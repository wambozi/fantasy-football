package engine

import "testing"

// A handful of full mock drafts must satisfy every hard invariant. The 500-draft
// statistical run lives in cmd/simdraft; this guards the plumbing.
func TestSimulateDraftInvariants(t *testing.T) {
	lg, pool, cfg := fixture(t)
	for seed := uint64(1); seed <= 4; seed++ {
		r, err := SimulateDraft(lg, pool, cfg, seed, SimOptions{Sims: 150})
		if err != nil {
			t.Fatal(err)
		}
		if len(r.Mine) != len(lg.MyLivePicks) {
			t.Errorf("seed %d: %d picks, want %d", seed, len(r.Mine), len(lg.MyLivePicks))
		}
		for _, v := range r.Violations {
			t.Errorf("seed %d: %s", seed, v)
		}
		b, err := SimulateDraft(lg, pool, cfg, seed, SimOptions{Baseline: true, Sims: 150})
		if err != nil {
			t.Fatal(err)
		}
		t.Logf("seed %d: engine lineup %.0f, baseline %.0f, flex %v", seed, r.Lineup, b.Lineup, r.FlexPos)
	}
}

// A second QB has no lineup value in a 1-QB league; the bench discount must keep the
// engine from spending a mid-round pick on one.
func TestNoEarlyBackupQB(t *testing.T) {
	lg, pool, cfg := fixture(t)
	if cfg.Engine.BenchFactor("QB") >= 1 {
		t.Skip("bench_discount for QB not configured")
	}
	for seed := uint64(1); seed <= 6; seed++ {
		r, err := SimulateDraft(lg, pool, cfg, seed, SimOptions{Sims: 150})
		if err != nil {
			t.Fatal(err)
		}
		qbs := 0
		for _, p := range r.Mine {
			if p.Player.Pos == "QB" {
				qbs++
				if qbs == 2 && p.Round < 13 {
					t.Errorf("seed %d: second QB %s taken in round %d", seed, p.Player.Name, p.Round)
				}
			}
		}
	}
}
