package engine

import (
	"math"
	"testing"

	"github.com/wambozi/draft-copilot/internal/players"
)

// lineup must price byes: a slot with no cover books a zero for that week, and a
// backup earns exactly its one fill-in week. This is the metric that makes a
// 1-QB/1-DST roster measurably worse instead of a hole a human has to notice.
func TestLineupByeAware(t *testing.T) {
	_, _, cfg := fixture(t)
	mk := func(id string, pos players.Position, proj float64, bye int) *players.Player {
		return &players.Player{ID: id, Name: id, Pos: pos, ProjPoints: proj, Bye: bye}
	}
	// Starters covered everywhere, flex held by rb3/wr3 — but QB and DST have no cover.
	base := []*players.Player{
		mk("qb1", players.QB, 340, 7),
		mk("rb1", players.RB, 280, 5), mk("rb2", players.RB, 240, 6), mk("rb3", players.RB, 180, 9),
		mk("wr1", players.WR, 290, 8), mk("wr2", players.WR, 250, 10), mk("wr3", players.WR, 190, 11),
		mk("te1", players.TE, 200, 12), mk("te2", players.TE, 120, 13),
		mk("dst1", players.DST, 110, 14),
	}
	pts1, flex1, holes1 := lineup(cfg, base, nil)
	if holes1 != 2 {
		t.Fatalf("naked QB and DST byes: %d holes, want 2", holes1)
	}
	if len(flex1) != cfg.Roster.Flex.Count {
		t.Fatalf("flex at full strength: %v, want %d slots", flex1, cfg.Roster.Flex.Count)
	}

	// A backup QB with a different bye closes the hole and is worth its one week.
	qb2 := mk("qb2", players.QB, 250, 9)
	pts2, _, holes2 := lineup(cfg, append(append([]*players.Player{}, base...), qb2), nil)
	if holes2 != 1 {
		t.Errorf("QB covered: %d holes, want 1 (DST)", holes2)
	}
	if want := pts1 + 250.0/(seasonWeeks-1); math.Abs(pts2-want) > 1e-9 {
		t.Errorf("backup QB worth %.2f, want %.2f (one week's fill)", pts2-pts1, want-pts1)
	}

	// A backup sharing the starter's bye covers nothing.
	qb3 := mk("qb3", players.QB, 250, 7)
	if _, _, holes3 := lineup(cfg, append(append([]*players.Player{}, base...), qb3), nil); holes3 != 2 {
		t.Errorf("same-bye backup: %d holes, want 2", holes3)
	}

	// With a wire, a hole streams: priced at the waiver level's week, still counted as
	// a hole — and a backup QB is now only worth its edge over the stream.
	wire := map[players.Position]float64{players.QB: 200, players.DST: 90}
	ptsW, _, holesW := lineup(cfg, base, wire)
	if holesW != 2 {
		t.Errorf("streamed holes still count: %d, want 2", holesW)
	}
	if want := pts1 + (200.0+90.0)/(seasonWeeks-1); math.Abs(ptsW-want) > 1e-9 {
		t.Errorf("streamed season worth %.2f over zero-priced, want %.2f", ptsW-pts1, want-pts1)
	}
	ptsW2, _, _ := lineup(cfg, append(append([]*players.Player{}, base...), qb2), wire)
	if want := (250.0 - 200.0) / (seasonWeeks - 1); math.Abs(ptsW2-ptsW-want) > 1e-9 {
		t.Errorf("backup QB over a 200-point wire worth %.2f, want %.2f", ptsW2-ptsW, want)
	}
}

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
