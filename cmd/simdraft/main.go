// Command simdraft is the M4 harness: N full mock drafts with the engine in my seat,
// opponents drafting by noisy ADP with positional need. Reports the §10 invariants and
// exits non-zero if any fails. Run it before draft night, not on it.
package main

import (
	"flag"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"

	"github.com/wambozi/draft-copilot/internal/engine"
	"github.com/wambozi/draft-copilot/internal/league"
	"github.com/wambozi/draft-copilot/internal/players"
	"github.com/wambozi/draft-copilot/internal/strategy"
)

func main() {
	var (
		n       = flag.Int("n", 500, "number of mock drafts")
		sims    = flag.Int("sims", 400, "survival Monte Carlo sims per advise (speed/accuracy)")
		dataDir = flag.String("data", "./data", "data directory")
		myTeam  = flag.String("team", "Sittin Purdy", "my team")
		seed    = flag.Uint64("seed", 1, "base seed")
		verbose = flag.Bool("v", false, "print every violating draft")
		show    = flag.Int("show", 1, "print this many full engine drafts")
	)
	flag.Parse()
	if err := run(*n, *sims, *dataDir, *myTeam, *seed, *verbose, *show); err != nil {
		slog.Error("simdraft", "err", err)
		os.Exit(1)
	}
}

func run(n, sims int, dataDir, myTeam string, seed uint64, verbose bool, show int) error {
	cfg, err := strategy.Load(filepath.Join(dataDir, "strategy.yaml"))
	if err != nil {
		return err
	}
	lg, err := league.Load(filepath.Join(dataDir, "draft-order.csv"), myTeam, cfg.LeagueRoster())
	if err != nil {
		return err
	}
	pool, err := players.Load(filepath.Join(dataDir, "players.json"))
	if err != nil {
		return err
	}
	if err := attachKeepers(lg, pool); err != nil {
		return err
	}
	if pool.CurveProjections() {
		fmt.Println("note: no projections loaded; VOR from fitted ADP curve")
	}

	type job struct {
		seed     uint64
		baseline bool
	}
	jobs := make(chan job)
	engRes := make([]engine.SimResult, n)
	baseRes := make([]engine.SimResult, n)
	var firstErr error
	var mu sync.Mutex
	var wg sync.WaitGroup
	for w := 0; w < runtime.NumCPU(); w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := range jobs {
				r, err := engine.SimulateDraft(lg, pool, cfg, j.seed, engine.SimOptions{Baseline: j.baseline, Sims: sims})
				mu.Lock()
				if err != nil && firstErr == nil {
					firstErr = err
				}
				if j.baseline {
					baseRes[j.seed-seed] = r
				} else {
					engRes[j.seed-seed] = r
				}
				mu.Unlock()
			}
		}()
	}
	for i := 0; i < n; i++ {
		jobs <- job{seed + uint64(i), false}
		jobs <- job{seed + uint64(i), true}
	}
	close(jobs)
	wg.Wait()
	if firstErr != nil {
		return firstErr
	}

	// Aggregate.
	viol := map[string]int{}
	bad := 0
	flexPos := map[players.Position]int{}
	posTotals := map[players.Position]int{}
	var engLineup, baseLineup []float64
	for i, r := range engRes {
		if len(r.Violations) > 0 {
			bad++
			if verbose {
				fmt.Printf("seed %d: %s\n", r.Seed, strings.Join(r.Violations, "; "))
			}
		}
		for _, v := range r.Violations {
			viol[normalise(v)]++
		}
		for _, p := range r.FlexPos {
			flexPos[p]++
		}
		for pos, c := range r.Counts {
			posTotals[pos] += c
		}
		engLineup = append(engLineup, r.Lineup)
		baseLineup = append(baseLineup, baseRes[i].Lineup)
	}
	for i := 0; i < show && i < len(engRes); i++ {
		printDraft(engRes[i])
	}
	fmt.Printf("\n%d drafts, %d sims/advise, %d with violations\n", n, sims, bad)
	keys := make([]string, 0, len(viol))
	for k := range viol {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		fmt.Printf("  %5.1f%%  %s\n", 100*float64(viol[k])/float64(n), k)
	}
	fmt.Printf("avg roster:")
	for _, pos := range []players.Position{players.QB, players.RB, players.WR, players.TE, players.DST} {
		fmt.Printf("  %s %.1f", pos, float64(posTotals[pos])/float64(n))
	}
	fmt.Printf("\nflex fills:")
	for _, pos := range []players.Position{players.RB, players.WR, players.TE} {
		fmt.Printf("  %s %d", pos, flexPos[pos])
	}
	em, bm := median(engLineup), median(baseLineup)
	fmt.Printf("\nmedian lineup points: engine %.0f  baseline(BPA) %.0f  (%+.1f%%)\n", em, bm, 100*(em-bm)/bm)

	// §10 thresholds.
	fail := false
	check := func(ok bool, msg string) {
		mark := "PASS"
		if !ok {
			mark = "FAIL"
			fail = true
		}
		fmt.Printf("[%s] %s\n", mark, msg)
	}
	rbMiss := 0
	for _, r := range engRes {
		for _, v := range r.Violations {
			if strings.Contains(v, "RB by #26") {
				rbMiss++
				break
			}
		}
	}
	check(float64(n-rbMiss)/float64(n) >= 0.95, fmt.Sprintf("≥95%% drafts have 2+ RB by #26 (%.1f%%)", 100*float64(n-rbMiss)/float64(n)))
	hard := 0
	for _, r := range engRes {
		for _, v := range r.Violations {
			if !strings.Contains(v, "RB by #26") {
				hard++
				break
			}
		}
	}
	check(hard == 0, fmt.Sprintf("100%% of drafts satisfy every hard invariant (%d failing)", hard))
	check(em > bm, "median lineup beats naive BPA baseline")
	check(flexPos[players.RB] > 0 && flexPos[players.WR] > 0, "flex is filled by value, not a fixed position")
	if fail {
		return fmt.Errorf("invariants failed")
	}
	return nil
}

func normalise(v string) string {
	// Collapse counts so "3/2 RB by #26" and "1/2 RB by #26" aggregate; keep the rule text.
	if i := strings.Index(v, " "); i > 0 && strings.ContainsAny(v[:i], "0123456789") {
		return "…" + v[i:]
	}
	return v
}

func median(v []float64) float64 {
	s := append([]float64(nil), v...)
	sort.Float64s(s)
	if len(s) == 0 {
		return 0
	}
	return s[len(s)/2]
}

func printDraft(r engine.SimResult) {
	fmt.Printf("--- engine draft, seed %d, lineup %.0f\n", r.Seed, r.Lineup)
	for _, p := range r.Mine {
		fmt.Printf("  R%-2d #%-3d %-24s %-3s %-4s adp %5.1f  proj %5.0f\n", p.Round, p.Live, p.Player.Name, p.Player.Pos, p.Player.Team, p.Player.ADPMean, p.Player.ProjPoints)
	}
	if len(r.Violations) > 0 {
		fmt.Printf("  violations: %s\n", strings.Join(r.Violations, "; "))
	}
}

func attachKeepers(lg *league.League, pool *players.Pool) error {
	byName := map[string]*players.Player{}
	for _, p := range pool.Players {
		byName[players.NameKey(p.Name)+"|"+string(p.Pos)] = p
	}
	for i := range lg.Slots {
		s := &lg.Slots[i]
		if s.KeeperID == "" {
			continue
		}
		if p, ok := pool.ByID[s.KeeperID]; ok {
			p.Keeper = true
			continue
		}
		if p, ok := byName[players.NameKey(s.KeeperName)+"|"+string(s.KeeperPos)]; ok {
			s.KeeperID = p.ID
			p.Keeper = true
			continue
		}
		return fmt.Errorf("keeper %s not in pool", s.KeeperName)
	}
	return nil
}
