// Command ingest converts data/adp.csv (+ optional projections) into data/players.json.
//
// Per-source ADP columns are the whole point: the spread across sources is the σ that
// drives the survival model. The consensus column is only used as a thin-data fallback.
package main

import (
	"encoding/csv"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/wambozi/draft-copilot/internal/players"
	"github.com/wambozi/draft-copilot/internal/strategy"
)

func main() {
	if err := run(); err != nil {
		slog.Error("ingest failed", "err", err)
		os.Exit(1)
	}
}

func run() error {
	dataDir := flag.String("data", "./data", "data directory")
	flag.Parse()
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))

	cfg, err := strategy.Load(filepath.Join(*dataDir, "strategy.yaml"))
	if err != nil {
		return err
	}
	f, err := os.Open(filepath.Join(*dataDir, "adp.csv"))
	if err != nil {
		return fmt.Errorf("open adp.csv: %w", err)
	}
	defer f.Close()
	list, err := ParseADP(f, cfg.ADP, log)
	if err != nil {
		return err
	}
	if n, err := applyProjections(*dataDir, list, log); err != nil {
		return err
	} else if n == 0 {
		log.Warn("no projections found; ProjPoints=0 for all players (VOR unavailable)")
	}
	out, err := json.MarshalIndent(list, "", " ")
	if err != nil {
		return fmt.Errorf("marshal: %w", err)
	}
	dst := filepath.Join(*dataDir, "players.json")
	if err := os.WriteFile(dst, out, 0o644); err != nil {
		return fmt.Errorf("write %s: %w", dst, err)
	}
	log.Info("wrote players.json", "players", len(list))
	return nil
}

var posRank = regexp.MustCompile(`^([A-Za-z/]+)(\d*)$`)

// ParseADP reads the FootballGuys export. Column positions are discovered from the header;
// only sources named in cfg.Sources are used.
func ParseADP(r io.Reader, cfg strategy.ADP, log *slog.Logger) ([]*players.Player, error) {
	cr := csv.NewReader(r)
	cr.FieldsPerRecord = -1
	header, err := cr.Read()
	if err != nil {
		return nil, fmt.Errorf("read header: %w", err)
	}
	col := map[string]int{}
	for i, h := range header {
		col[strings.TrimSpace(h)] = i
	}
	for _, need := range []string{"Consensus", "Player", "Pos", "Team", "Bye"} {
		if _, ok := col[need]; !ok {
			return nil, fmt.Errorf("adp.csv missing column %q (header: %v)", need, header)
		}
	}
	type src struct {
		name string
		idx  int
		cfg  strategy.Source
	}
	var srcs []src
	for name, sc := range cfg.Sources {
		i, ok := col[name]
		if !ok {
			return nil, fmt.Errorf("strategy.yaml adp source %q not a column in adp.csv", name)
		}
		srcs = append(srcs, src{name, i, sc})
	}
	sort.Slice(srcs, func(i, j int) bool { return srcs[i].name < srcs[j].name })
	log.Info("adp sources in use", "n", len(srcs), "ignored_columns", len(header)-5-len(srcs))

	var out []*players.Player
	seen := map[string]bool{}
	line := 1
	for {
		rec, err := cr.Read()
		if err == io.EOF {
			break
		}
		line++
		if err != nil {
			return nil, fmt.Errorf("adp.csv line %d: %w", line, err)
		}
		name := strings.TrimSpace(rec[col["Player"]])
		m := posRank.FindStringSubmatch(strings.TrimSpace(rec[col["Pos"]]))
		if m == nil {
			log.Warn("dropped row: unparseable position", "line", line, "player", name, "pos", rec[col["Pos"]])
			continue
		}
		rawPos := m[1]
		if mapped, ok := cfg.PosMap[rawPos]; ok {
			if mapped != rawPos && mapped != "DST" && mapped != "K" {
				// Routine TD->DST / PK->K remaps are noise; log only the surprising ones.
				log.Info("position remapped", "player", name, "from", rawPos, "to", mapped)
			}
			rawPos = mapped
		}
		pos := players.NormPos(rawPos)
		if !pos.Draftable() {
			log.Info("dropped row: no roster slot for position", "player", name, "pos", pos)
			continue
		}
		team := players.NormTeam(rec[col["Team"]])
		bye, _ := strconv.Atoi(strings.TrimSpace(rec[col["Bye"]]))
		consensus, _ := strconv.ParseFloat(strings.TrimSpace(rec[col["Consensus"]]), 64)

		var vals, weights, sigmaVals, raw []float64
		for _, s := range srcs {
			v, ok := parseADPCell(rec, s.idx)
			if !ok {
				raw = append(raw, 0) // 0 = missing; keeps column alignment for diagnostics
				continue
			}
			raw = append(raw, v)
			vals = append(vals, v)
			weights = append(weights, s.cfg.WeightFor(pos))
			if s.cfg.Sigma {
				sigmaVals = append(sigmaVals, v)
			}
		}
		p := &players.Player{ID: players.Slug(name, team), Name: name, Team: team, Pos: pos, Bye: bye, ADPSources: raw}
		p.ADPMean, p.ADPStdDev = Summarize(vals, weights, sigmaVals, consensus, cfg)
		if seen[p.ID] {
			log.Warn("duplicate player id; keeping first", "id", p.ID, "line", line)
			continue
		}
		seen[p.ID] = true
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ADPMean < out[j].ADPMean })
	return out, nil
}

func parseADPCell(rec []string, i int) (float64, bool) {
	if i >= len(rec) {
		return math.NaN(), false
	}
	s := strings.TrimSpace(rec[i])
	if s == "" || s == "-" {
		return math.NaN(), false
	}
	v, err := strconv.ParseFloat(s, 64)
	if err != nil || v <= 0 {
		return math.NaN(), false
	}
	return v, true
}

// Summarize computes (ADPMean, ADPStdDev).
//
// Mean is a weighted median: sources closer to our format (full PPR, 1QB, redraft) get
// more say, and a median resists one outlier site. σ is 1.4826×MAD over the σ-eligible
// sources only — best-ball sites disagree with redraft sites for format reasons, and
// counting that as uncertainty would make survival look too relaxed. Both are floored:
// σ at sigma_floor always, and at max(thin.min, thin.frac×mean) when data is thin,
// because a player only two sites bother to rank is genuinely uncertain.
func Summarize(vals, weights, sigmaVals []float64, consensus float64, cfg strategy.ADP) (mean, sigma float64) {
	thin := func(m float64) float64 { return math.Max(cfg.ThinSigma.Min, cfg.ThinSigma.Frac*m) }
	if len(vals) < cfg.MinSources {
		if len(vals) > 0 {
			mean = WeightedMedian(vals, weights)
		} else {
			mean = consensus
		}
		if mean <= 0 {
			mean = 999
		}
		return mean, thin(mean)
	}
	mean = WeightedMedian(vals, weights)
	if len(sigmaVals) < cfg.MinSources {
		return mean, thin(mean)
	}
	med := median(sigmaVals)
	dev := make([]float64, len(sigmaVals))
	for i, v := range sigmaVals {
		dev[i] = math.Abs(v - med)
	}
	sigma = cfg.SigmaMADScale * median(dev)
	return mean, math.Max(sigma, cfg.SigmaFloor)
}

// WeightedMedian returns the value at which cumulative weight first reaches half the total.
func WeightedMedian(vals, weights []float64) float64 {
	idx := make([]int, len(vals))
	for i := range idx {
		idx[i] = i
	}
	sort.Slice(idx, func(a, b int) bool { return vals[idx[a]] < vals[idx[b]] })
	var total float64
	for _, w := range weights {
		total += w
	}
	var cum float64
	for _, i := range idx {
		cum += weights[i]
		if cum >= total/2 {
			return vals[i]
		}
	}
	return vals[idx[len(idx)-1]]
}

func median(v []float64) float64 {
	s := append([]float64(nil), v...)
	sort.Float64s(s)
	n := len(s)
	if n%2 == 1 {
		return s[n/2]
	}
	return (s[n/2-1] + s[n/2]) / 2
}
