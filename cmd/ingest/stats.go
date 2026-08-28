package main

import (
	"encoding/csv"
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
)

// Last-season actuals from the FantasyPros "Statistics" export
// (FantasyPros_Fantasy_Football_Statistics_<POS>.csv). Two uses:
//
//  1. Player.Last for reason strings.
//  2. When no projection file is present, a finish-order value curve per position:
//     the k-th best player by ECR positional rank is valued at the k-th best actual
//     score last season (spec §4). Real projections, when they arrive, override this.
//
// The export is in whatever scoring the page was set to. We detect standard vs PPR
// from the columns and re-score to full PPR, so a wrong dropdown can't silently
// halve WR value.

const statsSeason = 2025

var nameTeam = regexp.MustCompile(`^(.*?)\s*\(([A-Z]{2,3})\)\s*$`)

type statRow struct {
	name, team string
	pts, ppr   float64
	games      int
	tgtPct     float64
}

func loadStats(dataDir string, log *slog.Logger) (map[players.Position][]statRow, error) {
	files, _ := filepath.Glob(filepath.Join(dataDir, "FantasyPros_Fantasy_Football_Statistics_*.csv"))
	out := map[players.Position][]statRow{}
	for _, f := range files {
		base := filepath.Base(f)
		posStr := strings.TrimSuffix(strings.TrimPrefix(base, "FantasyPros_Fantasy_Football_Statistics_"), ".csv")
		pos := players.NormPos(posStr)
		if !pos.Draftable() {
			continue
		}
		rows, mode, err := parseStatsFile(f, pos)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", base, err)
		}
		log.Info("stats", "file", base, "rows", len(rows), "export_scoring", mode)
		out[pos] = rows
	}
	return out, nil
}

// parseStatsFile handles the duplicated YDS/TD headers by position-specific ordinals.
func parseStatsFile(path string, pos players.Position) ([]statRow, string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, "", err
	}
	defer f.Close()
	cr := csv.NewReader(f)
	cr.FieldsPerRecord = -1
	cr.LazyQuotes = true
	hdr, err := cr.Read()
	if err != nil {
		return nil, "", err
	}
	idx := func(name string, nth int) int { // nth occurrence (0-based) of a header
		seen := 0
		for i, h := range hdr {
			if strings.EqualFold(strings.TrimSpace(h), name) {
				if seen == nth {
					return i
				}
				seen++
			}
		}
		return -1
	}
	col := map[string]int{"player": idx("Player", 0), "g": idx("G", 0), "fpts": idx("FPTS", 0), "fl": idx("FL", 0)}
	switch pos {
	case players.QB:
		col["passYds"], col["passTd"], col["int"] = idx("YDS", 0), idx("TD", 0), idx("INT", 0)
		col["rushYds"], col["rushTd"] = idx("YDS", 1), idx("TD", 1)
	case players.RB:
		col["rushYds"], col["rushTd"] = idx("YDS", 0), idx("TD", 0)
		col["rec"], col["recYds"], col["recTd"] = idx("REC", 0), idx("YDS", 1), idx("TD", 1)
	case players.WR, players.TE:
		col["rec"], col["recYds"], col["recTd"], col["tgtPct"] = idx("REC", 0), idx("YDS", 0), idx("TD", 0), idx("TGT %", 0)
		col["rushYds"], col["rushTd"] = idx("YDS", 1), idx("TD", 1)
	}
	for k, v := range col {
		if v < 0 && k != "tgtPct" && k != "int" {
			return nil, "", fmt.Errorf("column %s not found in header %v", k, hdr)
		}
	}
	num := func(rec []string, k string) float64 {
		i, ok := col[k]
		if !ok || i < 0 || i >= len(rec) {
			return 0
		}
		v, _ := strconv.ParseFloat(strings.ReplaceAll(strings.TrimSpace(rec[i]), ",", ""), 64)
		return v
	}

	var rows []statRow
	// Scoring-mode detection: compare the export's FPTS with our own standard-scoring
	// recomputation over the first rows; the offset tells us how receptions were scored.
	var diffs []float64
	for {
		rec, err := cr.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, "", err
		}
		if col["player"] >= len(rec) || col["fpts"] >= len(rec) {
			continue // blank/trailer rows
		}
		m := nameTeam.FindStringSubmatch(strings.TrimSpace(rec[col["player"]]))
		if m == nil {
			continue
		}
		r := statRow{name: m[1], team: m[2], pts: num(rec, "fpts"), games: int(num(rec, "g")), tgtPct: num(rec, "tgtPct")}
		std := 0.1*(num(rec, "rushYds")+num(rec, "recYds")) + 6*(num(rec, "rushTd")+num(rec, "recTd")) - 2*num(rec, "fl")
		if pos == players.QB {
			std += 0.04*num(rec, "passYds") + 4*num(rec, "passTd") - 2*num(rec, "int")
		}
		recs := num(rec, "rec")
		if len(diffs) < 15 && recs > 20 {
			diffs = append(diffs, (r.pts-std)/recs)
		}
		r.ppr = std + recs // full PPR in league scoring, from the raw columns
		if pos == players.QB {
			r.ppr = r.pts // QB export is scoring-independent enough; trust it
		}
		rows = append(rows, r)
	}
	mode := "standard"
	if len(diffs) > 0 {
		sort.Float64s(diffs)
		switch d := diffs[len(diffs)/2]; {
		case math.Abs(d-1) < 0.15:
			mode = "ppr"
		case math.Abs(d-0.5) < 0.15:
			mode = "half-ppr"
		case math.Abs(d) < 0.15:
			mode = "standard"
		default:
			mode = fmt.Sprintf("unknown(%.2f/rec)", d)
		}
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].ppr > rows[j].ppr })
	return rows, mode, nil
}

// attachStats sets Player.Last by name+position, then builds the finish curve for any
// player still without ProjPoints. Returns how many players got curve values.
func attachStats(stats map[players.Position][]statRow, list []*players.Player, log *slog.Logger) int {
	byKey := map[string]*players.Player{}
	for _, p := range list {
		byKey[players.NameKey(p.Name)+"|"+string(p.Pos)] = p
	}
	for pos, rows := range stats {
		for _, r := range rows {
			if p, ok := byKey[players.NameKey(r.name)+"|"+string(pos)]; ok {
				p.Last = &players.SeasonStats{Season: statsSeason, Points: math.Round(r.ppr*10) / 10, Games: r.games, TgtPct: r.tgtPct}
			}
		}
	}
	// Finish curve: value at positional rank k = k-th best PPR total last season,
	// smoothed by averaging with neighbours so one outlier season doesn't make a cliff.
	n := 0
	for pos, rows := range stats {
		curve := make([]float64, len(rows))
		for i := range rows {
			lo, hi := max(0, i-1), min(len(rows)-1, i+1)
			s := 0.0
			for j := lo; j <= hi; j++ {
				s += rows[j].ppr
			}
			curve[i] = s / float64(hi-lo+1)
		}
		// Rank EVERY player at the position (ECR positional rank, then ADP) so a gap
		// gets the value of its true slot; only players without a projection are written.
		var ranked []*players.Player
		for _, p := range list {
			if p.Pos == pos {
				ranked = append(ranked, p)
			}
		}
		sort.SliceStable(ranked, func(i, j int) bool {
			a, b := ranked[i], ranked[j]
			if (a.ECRPos > 0) != (b.ECRPos > 0) {
				return a.ECRPos > 0
			}
			if a.ECRPos != b.ECRPos {
				return a.ECRPos < b.ECRPos
			}
			return adpOrLast(a) < adpOrLast(b)
		})
		for i, p := range ranked {
			if p.ProjPoints > 0 || len(curve) == 0 {
				continue
			}
			k := min(i, len(curve)-1)
			p.ProjPoints = math.Round(curve[k]*10) / 10
			p.ProjSource = players.ProjFinish
			n++
		}
	}
	if n > 0 {
		log.Info("finish curve applied to players without a projection", "players", n)
	}
	return n
}

func adpOrLast(p *players.Player) float64 {
	if p.ADPMean <= 0 {
		return 1e9
	}
	return p.ADPMean
}
