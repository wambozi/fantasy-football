package main

import (
	"encoding/csv"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/wambozi/draft-copilot/internal/players"
)

// applyProjections attaches season point projections and expert consensus rank.
//
//	<data>/projections/*.csv  any CSV with a player-name column and a points column
//	                          (FPTS / FPTS. / POINTS / PTS / PROJ). Position comes from a
//	                          POS column if present, else from the file name (qb.csv,
//	                          fantasypros_rb_projections.csv, ...). One file per position
//	                          is the FantasyPros export shape; a single combined file works too.
//	<data>/ecr.csv            FantasyPros ECR export: player-name column + AVG (or ECR/RK).
//
// Matching is by normalised name + position, then by normalised name alone when unique.
// Returns the number of players that received a projection.
func applyProjections(dataDir string, list []*players.Player, log *slog.Logger) (int, error) {
	byKey := map[string][]*players.Player{}
	byName := map[string][]*players.Player{}
	for _, p := range list {
		k := players.NameKey(p.Name)
		byKey[k+"|"+string(p.Pos)] = append(byKey[k+"|"+string(p.Pos)], p)
		byName[k] = append(byName[k], p)
	}
	resolve := func(name string, pos players.Position) *players.Player {
		k := players.NameKey(name)
		if pos != "" {
			if c := byKey[k+"|"+string(pos)]; len(c) == 1 {
				return c[0]
			}
		}
		if c := byName[k]; len(c) == 1 {
			return c[0]
		}
		return nil
	}

	files, _ := filepath.Glob(filepath.Join(dataDir, "projections", "*.csv"))
	fp, _ := filepath.Glob(filepath.Join(dataDir, "FantasyPros_Fantasy_Football_Projections_*.csv"))
	for _, f := range fp {
		if !strings.Contains(f, "_FLX") { // FLX duplicates RB/WR/TE
			files = append(files, f)
		}
	}
	sort.Strings(files)
	n := 0
	for _, f := range files {
		m, miss, mode, err := loadPointsFile(f, resolve)
		if err != nil {
			return n, fmt.Errorf("%s: %w", f, err)
		}
		n += m
		log.Info("projections", "file", filepath.Base(f), "matched", m, "unmatched", len(miss), "export_scoring", mode)
		if len(miss) > 0 {
			sort.Strings(miss)
			if len(miss) > 12 {
				miss = append(miss[:12], "…")
			}
			log.Warn("projections: names not in ADP pool", "file", filepath.Base(f), "names", strings.Join(miss, ", "))
		}
	}

	ecrFiles, _ := filepath.Glob(filepath.Join(dataDir, "FantasyPros_*_Draft_ALL_Rankings.csv"))
	if ef := filepath.Join(dataDir, "ecr.csv"); fileExists(ef) {
		ecrFiles = append(ecrFiles, ef)
	}
	for _, ef := range ecrFiles {
		m, miss, err := loadECR(ef, resolve)
		if err != nil {
			return n, fmt.Errorf("%s: %w", ef, err)
		}
		log.Info("ecr", "file", filepath.Base(ef), "matched", m, "unmatched", len(miss))
		if len(miss) > 0 {
			sort.Strings(miss)
			if len(miss) > 12 {
				miss = append(miss[:12], "…")
			}
			log.Warn("ecr: names not in ADP pool", "names", strings.Join(miss, ", "))
		}
	}
	for kind, pat := range map[string]string{"dynasty": "FantasyPros_*_Dynasty_ALL_Rankings.csv", "rookie": "FantasyPros_*_Rookies_ALL_Rankings.csv"} {
		files, _ := filepath.Glob(filepath.Join(dataDir, pat))
		for _, f := range files {
			m, miss, err := loadRanking(f, kind, resolve)
			if err != nil {
				return n, fmt.Errorf("%s: %w", f, err)
			}
			log.Info(kind+" rankings", "file", filepath.Base(f), "matched", m, "unmatched", len(miss))
		}
	}
	for _, p := range list {
		if p.ProjPoints > 0 {
			p.ProjSource = "projection"
		}
	}
	stats, err := loadStats(dataDir, log)
	if err != nil {
		return n, err
	}
	n += attachStats(stats, list, log)
	return n, nil
}

var (
	nameCols   = []string{"player name", "player", "name"}
	pointsCols = []string{"fpts", "fpts.", "points", "pts", "proj", "proj pts", "fantasy points"}
	posCols    = []string{"pos", "position"}
	ecrCols    = []string{"avg", "avg.", "ecr", "rk", "rank"}
	posInFile  = regexp.MustCompile(`(?i)(?:^|[^a-z])(qb|rb|wr|te|dst|def|d/st|k)(?:[^a-z]|$)`)
)

// header locates columns by case-insensitive name, trying candidates in order.
type header struct{ col map[string]int }

func readHeader(cr *csv.Reader) (header, error) {
	h, err := cr.Read()
	if err != nil {
		return header{}, fmt.Errorf("read header: %w", err)
	}
	return headerOf(h), nil
}

func headerOf(h []string) header {
	m := map[string]int{}
	for i, c := range h {
		k := strings.ToLower(strings.TrimSpace(strings.Trim(c, `"`)))
		if _, dup := m[k]; !dup { // first occurrence wins (FPTS is unique; YDS is not)
			m[k] = i
		}
	}
	return header{m}
}

func (h header) find(cands ...string) (int, bool) {
	for _, c := range cands {
		if i, ok := h.col[c]; ok {
			return i, true
		}
	}
	return 0, false
}

// loadPointsFile reads one projections CSV. Because the FantasyPros export is in
// whatever scoring the page dropdown was on, points are re-scored to full PPR from the
// raw columns: we recompute standard scoring per row, infer the export's per-reception
// credit from the offset, and add whatever is missing. Returns the detected mode.
func loadPointsFile(path string, resolve func(string, players.Position) *players.Player) (int, []string, string, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, nil, "", err
	}
	defer f.Close()
	cr := csv.NewReader(f)
	cr.FieldsPerRecord = -1
	cr.LazyQuotes = true
	rawHdr, err := cr.Read()
	if err != nil {
		return 0, nil, "", fmt.Errorf("read header: %w", err)
	}
	h := headerOf(rawHdr)
	nameI, ok := h.find(nameCols...)
	if !ok {
		return 0, nil, "", fmt.Errorf("no player-name column (want one of %v)", nameCols)
	}
	ptsI, ok := h.find(pointsCols...)
	if !ok {
		return 0, nil, "", fmt.Errorf("no points column (want one of %v)", pointsCols)
	}
	posI, hasPos := h.find(posCols...)
	var filePos players.Position
	if m := posInFile.FindStringSubmatch(filepath.Base(path)); m != nil {
		filePos = players.NormPos(strings.ToUpper(m[1]))
	}
	score := rowScorer(rawHdr, filePos)

	type row struct {
		p         *players.Player
		pts, recs float64
	}
	var rows []row
	var miss []string
	var diffs []float64
	for {
		rec, err := cr.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			return 0, miss, "", err
		}
		if nameI >= len(rec) || ptsI >= len(rec) {
			continue
		}
		name := strings.TrimSpace(rec[nameI])
		if name == "" {
			continue
		}
		pts, err := strconv.ParseFloat(strings.ReplaceAll(strings.TrimSpace(rec[ptsI]), ",", ""), 64)
		if err != nil {
			continue // subtotal rows, blanks
		}
		pos := filePos
		if hasPos && posI < len(rec) {
			if pp := players.NormPos(strings.TrimRight(strings.TrimSpace(rec[posI]), "0123456789")); pp != "" {
				pos = pp
			}
		}
		std, recs := score(rec)
		if recs > 20 && len(diffs) < 15 {
			diffs = append(diffs, (pts-std)/recs)
		}
		p := resolve(name, pos)
		if p == nil {
			miss = append(miss, name)
			continue
		}
		rows = append(rows, row{p, pts, recs})
	}
	mode, credit := "n/a", 1.0
	if len(diffs) > 0 {
		sort.Float64s(diffs)
		d := diffs[len(diffs)/2]
		switch {
		case abs(d-1) < 0.15:
			mode, credit = "ppr", 1
		case abs(d-0.5) < 0.15:
			mode, credit = "half-ppr", 0.5
		case abs(d) < 0.15:
			mode, credit = "standard", 0
		default:
			return 0, miss, "", fmt.Errorf("cannot infer export scoring (%.2f pts/reception); check the columns", d)
		}
	}
	n := 0
	for _, r := range rows {
		pts := r.pts + (1-credit)*r.recs // full PPR
		if pts > r.p.ProjPoints {        // duplicates across files: keep the larger
			if r.p.ProjPoints == 0 {
				n++
			}
			r.p.ProjPoints = float64(int(pts*10+0.5)) / 10
		}
	}
	return n, miss, mode, nil
}

// rowScorer returns a per-row (standardPoints, receptions) function for the file's
// column layout. FantasyPros repeats YDS/TDS headers; which block comes first depends
// on position: QB pass→rush, RB rush→rec, WR/TE rec→rush. We detect from whether REC
// precedes the first YDS column, so the FLX and per-position exports both work.
func rowScorer(hdr []string, pos players.Position) func([]string) (float64, float64) {
	nth := func(name string, k int) int {
		seen := 0
		for i, h := range hdr {
			hh := strings.ToUpper(strings.TrimSpace(h))
			if hh == name || (name == "TDS" && hh == "TD") || (name == "INTS" && hh == "INT") {
				if seen == k {
					return i
				}
				seen++
			}
		}
		return -1
	}
	recI := nth("REC", 0)
	yds0, yds1, td0, td1 := nth("YDS", 0), nth("YDS", 1), nth("TDS", 0), nth("TDS", 1)
	flI, intI := nth("FL", 0), nth("INTS", 0)
	get := func(rec []string, i int) float64 {
		if i < 0 || i >= len(rec) {
			return 0
		}
		v, _ := strconv.ParseFloat(strings.ReplaceAll(strings.TrimSpace(rec[i]), ",", ""), 64)
		return v
	}
	return func(rec []string) (float64, float64) {
		recs := get(rec, recI)
		var std float64
		switch {
		case pos == players.QB:
			std = 0.04*get(rec, yds0) + 4*get(rec, td0) - 2*get(rec, intI) + 0.1*get(rec, yds1) + 6*get(rec, td1)
		case pos == players.DST || recI < 0:
			return get(rec, nth("FPTS", 0)), 0
		default: // rush/rec blocks in either order both contribute 0.1/yd, 6/TD
			std = 0.1*(get(rec, yds0)+get(rec, yds1)) + 6*(get(rec, td0)+get(rec, td1))
		}
		return std - 2*get(rec, flI), recs
	}
}

func abs(x float64) float64 {
	if x < 0 {
		return -x
	}
	return x
}

func loadECR(path string, resolve func(string, players.Position) *players.Player) (int, []string, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, nil, err
	}
	defer f.Close()
	cr := csv.NewReader(f)
	cr.FieldsPerRecord = -1
	cr.LazyQuotes = true
	h, err := readHeader(cr)
	if err != nil {
		return 0, nil, err
	}
	nameI, ok := h.find(nameCols...)
	if !ok {
		return 0, nil, fmt.Errorf("no player-name column (want one of %v)", nameCols)
	}
	ecrI, ok := h.find(ecrCols...)
	if !ok {
		return 0, nil, fmt.Errorf("no ECR column (want one of %v)", ecrCols)
	}
	posI, hasPos := h.find(posCols...)
	n := 0
	var miss []string
	for {
		rec, err := cr.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			return n, miss, err
		}
		if nameI >= len(rec) || ecrI >= len(rec) {
			continue
		}
		name := strings.TrimSpace(rec[nameI])
		v, err := strconv.ParseFloat(strings.TrimSpace(rec[ecrI]), 64)
		if name == "" || err != nil {
			continue
		}
		var pos players.Position
		posRank := 0
		if hasPos && posI < len(rec) {
			raw := strings.TrimSpace(rec[posI])
			pos = players.NormPos(strings.TrimRight(raw, "0123456789"))
			posRank, _ = strconv.Atoi(strings.TrimLeft(raw, "ABCDEFGHIJKLMNOPQRSTUVWXYZ/"))
		}
		p := resolve(name, pos)
		if p == nil {
			miss = append(miss, name)
			continue
		}
		p.ECR = v
		p.ECRPos = posRank
		n++
	}
	return n, miss, nil
}

func fileExists(p string) bool {
	st, err := os.Stat(p)
	return err == nil && !st.IsDir()
}

// loadRanking reads a FantasyPros dynasty or rookie ranking export: RK, PLAYER NAME,
// POS, AGE. Sets Age (either file) and DynastyRank or RookieRank.
func loadRanking(path, kind string, resolve func(string, players.Position) *players.Player) (int, []string, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, nil, err
	}
	defer f.Close()
	cr := csv.NewReader(f)
	cr.FieldsPerRecord = -1
	cr.LazyQuotes = true
	h, err := readHeader(cr)
	if err != nil {
		return 0, nil, err
	}
	nameI, ok := h.find(nameCols...)
	if !ok {
		return 0, nil, fmt.Errorf("no player-name column")
	}
	rkI, ok := h.find("rk", "rank")
	if !ok {
		return 0, nil, fmt.Errorf("no RK column")
	}
	posI, hasPos := h.find(posCols...)
	ageI, hasAge := h.find("age")
	n := 0
	var miss []string
	for {
		rec, err := cr.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			return n, miss, err
		}
		if nameI >= len(rec) || rkI >= len(rec) {
			continue
		}
		name := strings.TrimSpace(rec[nameI])
		rk, err := strconv.Atoi(strings.TrimSpace(rec[rkI]))
		if name == "" || err != nil {
			continue
		}
		var pos players.Position
		if hasPos && posI < len(rec) {
			pos = players.NormPos(strings.TrimRight(strings.TrimSpace(rec[posI]), "0123456789"))
		}
		p := resolve(name, pos)
		if p == nil {
			miss = append(miss, name)
			continue
		}
		if hasAge && ageI < len(rec) {
			if a, err := strconv.Atoi(strings.TrimSpace(rec[ageI])); err == nil && a > 0 {
				p.Age = a
			}
		}
		if kind == "dynasty" {
			p.DynastyRank = rk
		} else {
			p.RookieRank = rk
		}
		n++
	}
	return n, miss, nil
}
