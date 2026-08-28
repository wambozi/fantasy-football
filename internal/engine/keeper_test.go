package engine

import (
	"testing"

	"github.com/wambozi/draft-copilot/internal/players"
)

func TestKeeperP(t *testing.T) {
	lg, pool, cfg := fixture(t)
	e := New(lg, pool, cfg, 1)
	cases := []struct {
		name string
		p    players.Player
		lo   float64
		hi   float64
	}{
		{"young rookie", players.Player{Name: "A", Age: 21, RookieRank: 3, ADPMean: 120}, 0.7, 1},
		{"young dynasty gap", players.Player{Name: "B", Age: 23, DynastyRank: 40, ADPMean: 130}, 0.7, 1},
		{"vet with gap", players.Player{Name: "C", Age: 30, DynastyRank: 40, ADPMean: 130}, 0, 0},
		{"young no signal", players.Player{Name: "D", Age: 22, DynastyRank: 150, ADPMean: 120}, 0, 0},
		{"no age data", players.Player{Name: "E", DynastyRank: 10, ADPMean: 200}, 0, 0},
	}
	for _, c := range cases {
		got := e.keeperP(&c.p)
		if got < c.lo || got > c.hi {
			t.Errorf("%s: P=%.2f, want [%.2f,%.2f]", c.name, got, c.lo, c.hi)
		}
	}
	cfg.Keeper.Targets = []string{"Some Guy"}
	if e.keeperP(&players.Player{Name: "some guy", Age: 31}) != 1 {
		t.Errorf("target override ignored")
	}
}

// The keeper term must not move anyone before the cost floor: the Top-3 at my picks in
// rounds 1–7 must be identical with the late weight zeroed.
func TestKeeperTermSilentBeforeFloor(t *testing.T) {
	lg, pool, cfg := fixture(t)
	off := *cfg
	off.Keeper.SurplusLate = 0
	st := newState(t, lg, pool)
	for _, live := range lg.MyLivePicks {
		slot, _ := lg.SlotForLive(live)
		if slot.Round >= cfg.Keeper.CostFloorRound {
			break
		}
		advanceBPA(t, st, pool, live)
		snap := st.Snapshot()
		a := New(lg, pool, cfg, 7).AdviseFor(snap, lg.MyTeam)
		b := New(lg, pool, &off, 7).AdviseFor(snap, lg.MyTeam)
		for i := range a.Top {
			if a.Top[i].Player.ID != b.Top[i].Player.ID || a.Top[i].Score != b.Top[i].Score {
				t.Errorf("#%d (round %d): keeper term changed the board: %s vs %s", live, slot.Round, a.Top[i].Player.Name, b.Top[i].Player.Name)
			}
			if a.Top[i].KeeperSpec {
				t.Errorf("#%d: %s flagged speculative before the floor", live, a.Top[i].Player.Name)
			}
		}
		if _, err := st.Pick(a.Top[0].Player.ID, "", "sim"); err != nil {
			t.Fatal(err)
		}
	}
}
