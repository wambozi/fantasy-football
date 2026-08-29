package engine

import (
	"fmt"
	"math"
	"strings"
	"testing"

	"github.com/wambozi/draft-copilot/internal/league"
	"github.com/wambozi/draft-copilot/internal/players"
	"github.com/wambozi/draft-copilot/internal/state"
	"github.com/wambozi/draft-copilot/internal/strategy"
)

// fixture builds the real league from data/ and a synthetic projection curve so VOR
// tests are deterministic and independent of the projection file. Points are TEST DATA.
func fixture(t *testing.T) (*league.League, *players.Pool, *strategy.Config) {
	t.Helper()
	cfg, err := strategy.Load("../../data/strategy.yaml")
	if err != nil {
		t.Fatal(err)
	}
	lg, err := league.Load("../../data/draft-order.csv", "Sittin Purdy", cfg.LeagueRoster())
	if err != nil {
		t.Fatal(err)
	}
	pool, err := players.Load("../../data/players.json")
	if err != nil {
		t.Fatal(err)
	}
	if len(pool.Players) == 0 {
		t.Skip("data/players.json missing; run cmd/ingest")
	}
	pool.CurveProjections()
	for i := range lg.Slots {
		s := &lg.Slots[i]
		if s.KeeperID == "" {
			continue
		}
		if _, ok := pool.ByID[s.KeeperID]; !ok {
			found := false
			for _, p := range pool.Players {
				if players.NameKey(p.Name) == players.NameKey(s.KeeperName) && p.Pos == s.KeeperPos {
					s.KeeperID = p.ID
					found = true
					break
				}
			}
			if !found {
				t.Fatalf("keeper %s not in pool", s.KeeperName)
			}
		}
	}
	return lg, pool, cfg
}

func newState(t *testing.T, lg *league.League, pool *players.Pool) *state.DraftState {
	t.Helper()
	st, err := state.New(lg, pool, "")
	if err != nil {
		t.Fatal(err)
	}
	return st
}

// advanceBPA drafts the top available by ADP for every live pick < through.
func advanceBPA(t *testing.T, st *state.DraftState, pool *players.Pool, through int) {
	t.Helper()
	advanceBPAPos(t, st, pool, through, "")
}

// advanceBPAPos is advanceBPA restricted to one position when only != "".
func advanceBPAPos(t *testing.T, st *state.DraftState, pool *players.Pool, through int, only players.Position) {
	t.Helper()
	for {
		snap := st.Snapshot()
		if snap.LivePick >= through {
			return
		}
		var best *players.Player
		for _, p := range pool.Players {
			if _, gone := snap.Taken[p.ID]; gone || !p.Pos.Draftable() || p.Pos == players.DST {
				continue
			}
			if only != "" && p.Pos != only {
				continue
			}
			if best == nil || p.ADPMean < best.ADPMean {
				best = p
			}
		}
		if _, err := st.Pick(best.ID, "", state.SourceSim); err != nil {
			t.Fatal(err)
		}
	}
}

func TestNeedMultiplierFromKeepers(t *testing.T) {
	lg, pool, cfg := fixture(t)
	st := newState(t, lg, pool)
	rc := newRosterCounts(lg, pool, cfg, st.Snapshot())
	n := cfg.Engine.Need
	cases := []struct {
		team string
		pos  players.Position
		want float64
	}{
		{"Pollock Debacle", players.RB, n.StarterOpen}, // 0 keepers: every starter open
		{"Pollock Debacle", players.QB, n.StarterOpen},
		{"Time Stamps", players.RB, n.FlexOpen}, // kept CMC + Bijan: RB starters full, flex open
		{"Time Stamps", players.WR, n.StarterOpen},
		{"Ja'Marr & Jahmyr", players.RB, n.StarterOpen}, // kept Gibbs + Chase: one RB slot open
		{"Sittin Purdy", players.WR, n.FlexOpen},        // kept JSN + Odunze
		{"Sittin Purdy", players.DST, n.StarterOpen * n.DSTEarlyMult},
	}
	for _, c := range cases {
		if got := rc.need(c.team, c.pos, 1); math.Abs(got-c.want) > 1e-9 {
			t.Errorf("need(%s,%s)=%v want %v", c.team, c.pos, got, c.want)
		}
	}
	if rc.left["Pollock Debacle"] != 17 || rc.left["Tom"] != 16 || rc.left["Sittin Purdy"] != 15 {
		t.Errorf("picks left: %v", rc.left)
	}
}

func TestReplacementMovesDownAsStartersFill(t *testing.T) {
	lg, pool, cfg := fixture(t)
	e := New(lg, pool, cfg, 1)
	st := newState(t, lg, pool)
	var prev float64 = math.Inf(1)
	var prevDemand float64 = math.Inf(1)
	// Draft only RBs: each pick fills an RB starter/flex slot and removes an RB from the
	// board, so the replacement player must never move UP the board.
	for _, through := range []int{1, 10, 20, 30, 40, 50} {
		advanceBPAPos(t, st, pool, through, players.RB)
		snap := st.Snapshot()
		rc := newRosterCounts(lg, pool, cfg, snap)
		demand, repl := e.replacement(e.board(snap), rc)
		if demand[players.RB] > prevDemand {
			t.Errorf("RB demand rose at %d: %v -> %v", through, prevDemand, demand[players.RB])
		}
		if repl[players.RB] > prev+1e-9 {
			t.Errorf("RB replacement rose at %d: %.1f -> %.1f", through, prev, repl[players.RB])
		}
		prev, prevDemand = repl[players.RB], demand[players.RB]
	}
	snap := st.Snapshot()
	rc := newRosterCounts(lg, pool, cfg, snap)
	demand, _ := e.replacement(e.board(snap), rc)
	if demand[players.DST] != 12 {
		t.Errorf("DST demand should be 12 (none drafted), got %v", demand[players.DST])
	}
}

func TestSurvivalMonotoneInHorizon(t *testing.T) {
	lg, pool, cfg := fixture(t)
	cfg.Engine.Sims = 600
	e := New(lg, pool, cfg, 7)
	st := newState(t, lg, pool)
	snap := st.Snapshot()
	rc := newRosterCounts(lg, pool, cfg, snap)
	board := e.board(snap)
	// horizon 0: on the clock now with nextLive == current → everyone survives
	s0 := e.Survival(board, snap, rc, 1, "Patient Zeros Aids Epidemic")
	for _, p := range board[:20] {
		if s0[p.ID] != 1 {
			t.Fatalf("h=0 survival for %s = %v", p.Name, s0[p.ID])
		}
	}
	prev := map[string]float64{}
	for _, p := range board {
		prev[p.ID] = 1
	}
	for _, next := range []int{2, 4, 8, 12, 20, 30} {
		s := e.Survival(board, snap, rc, next, "nobody")
		for _, p := range board[:60] {
			if s[p.ID] > prev[p.ID]+0.03 { // MC noise tolerance
				t.Errorf("h→%d: %s survival rose %.2f -> %.2f", next-1, p.Name, prev[p.ID], s[p.ID])
			}
		}
		prev = s
	}
	// Sanity: the ADP #1 player is essentially never available after 7 picks.
	if s := prev; s[board[0].ID] > 0.02 {
		t.Errorf("%s survives 29 picks with p=%.2f", board[0].Name, s[board[0].ID])
	}
}

// A room full of teams needing RB must take more RBs than a room with RB starters filled.
func TestSurvivalCapturesPositionalRun(t *testing.T) {
	lg, pool, cfg := fixture(t)
	cfg.Engine.Sims = 800
	e := New(lg, pool, cfg, 3)
	st := newState(t, lg, pool)
	snap := st.Snapshot()
	board := e.board(snap)
	rbSurvival := func(rc *rosterCounts) float64 {
		// 23 opposing picks: enough for the room to reach RB9–RB25, where a run shows up.
		s := e.Survival(board, snap, rc, 24, "Sittin Purdy")
		sum, n := 0.0, 0
		// Skip the top 8: those go regardless of need. The run shows up in RB9–RB25.
		for _, p := range board[8:40] {
			if p.Pos == players.RB {
				sum += s[p.ID]
				n++
			}
		}
		return sum / float64(n)
	}
	real := newRosterCounts(lg, pool, cfg, snap)
	hungry := real.clone()
	sated := real.clone()
	for team := range hungry.counts {
		hungry.counts[team][players.RB] = 0
		sated.counts[team][players.RB] = 4 // starters and both flex slots already RB
	}
	h, s := rbSurvival(hungry), rbSurvival(sated)
	if !(h < s-0.05) {
		t.Errorf("RB run not captured: hungry-room RB survival %.2f should be well below sated-room %.2f", h, s)
	}
}

func TestGates(t *testing.T) {
	lg, pool, cfg := fixture(t)
	e := New(lg, pool, cfg, 1)
	me := lg.MyTeam
	mk := func(counts map[players.Position]int, live, left int) (*rosterCounts, *Advice) {
		rc := &rosterCounts{cfg: cfg, counts: map[string]map[players.Position]int{me: counts}, left: map[string]int{me: left}}
		slot, _ := lg.SlotForLive(live)
		return rc, &Advice{Round: slot.Round}
	}
	cases := []struct {
		name       string
		counts     map[players.Position]int
		live, left int
		wantForced players.Position
		wantBan    []players.Position
		wantBand   string
	}{
		{"opening: nothing forced, DST banned", map[players.Position]int{players.WR: 2}, 8, 15, "", []players.Position{players.DST}, ""},
		{"RB gate at #26 with 0 RB forces RB", map[players.Position]int{players.WR: 2}, 26, 13, players.RB, nil, "RB GATE"},
		{"RB gate at #11 with 0 RB: 2 picks for 2 RBs forces", map[players.Position]int{players.WR: 2}, 11, 14, players.RB, nil, "RB GATE"},
		{"TE gate last pick before #65", map[players.Position]int{players.WR: 2, players.RB: 2}, 65, 11, players.TE, nil, "TE GATE"},
		{"QB gate last pick before #68", map[players.Position]int{players.WR: 2, players.RB: 2, players.TE: 1}, 68, 10, players.QB, nil, "QB GATE"},
		{"QB max 2 bans QB", map[players.Position]int{players.QB: 2, players.WR: 2, players.RB: 2, players.TE: 1}, 90, 8, "", []players.Position{players.QB}, ""},
		{"DST last pick forced", map[players.Position]int{players.QB: 1, players.WR: 5, players.RB: 5, players.TE: 2}, 181, 1, players.DST, nil, "DST GATE"},
		{"endgame: only open starters allowed; overdue TE forced", map[players.Position]int{players.QB: 1, players.WR: 6, players.RB: 6, players.TE: 0}, 162, 2, players.TE, []players.Position{players.WR, players.RB, players.QB}, "TE GATE"},
		{"DST already have one → banned", map[players.Position]int{players.QB: 1, players.WR: 5, players.RB: 5, players.TE: 2, players.DST: 1}, 181, 1, "", []players.Position{players.DST}, ""},
	}
	for _, c := range cases {
		rc, ad := mk(c.counts, c.live, c.left)
		g := e.gates(rc, me, c.live, ad)
		if g.forced != c.wantForced {
			t.Errorf("%s: forced=%q want %q (warnings %v)", c.name, g.forced, c.wantForced, ad.Warnings)
		}
		for _, b := range c.wantBan {
			if g.allowed[b] {
				t.Errorf("%s: %s should be banned", c.name, b)
			}
		}
		if c.wantBand != "" && !strings.HasPrefix(ad.GateBand, c.wantBand) {
			t.Errorf("%s: band=%q want prefix %q", c.name, ad.GateBand, c.wantBand)
		}
	}
}

func TestAdviseNeverRecommendsKickerAndCaches(t *testing.T) {
	lg, pool, cfg := fixture(t)
	pool.Add(&players.Player{ID: "stray-k", Name: "Stray Kicker", Pos: players.K, ADPMean: 1, ADPStdDev: 3, ProjPoints: 999})
	cfg.Engine.Sims = 200
	e := New(lg, pool, cfg, 1)
	st := newState(t, lg, pool)
	advanceBPA(t, st, pool, 8)
	snap := st.Snapshot()
	ad := e.Advise(snap)
	if !ad.OnClock || ad.NextLivePick != 11 || ad.PicksUntil != 2 {
		t.Errorf("clock: %+v", ad)
	}
	for _, r := range ad.Candidates {
		if r.Player.Pos == players.K {
			t.Fatal("kicker recommended")
		}
	}
	if len(ad.Top) != TopN || (ad.ProjMode != players.ProjReal && ad.ProjMode != players.ProjCurve) {
		t.Errorf("top=%d mode=%s", len(ad.Top), ad.ProjMode)
	}
	if e.Advise(snap) != ad {
		t.Error("advice not cached by version")
	}
	if e.Advise(state.Snapshot{Version: snap.Version + 1, LivePick: snap.LivePick, Taken: snap.Taken, Rosters: snap.Rosters}) == ad {
		t.Error("cache not invalidated on version bump")
	}
}

func TestRegretPrefersScarceOverSafe(t *testing.T) {
	lg, pool, cfg := fixture(t)
	e := New(lg, pool, cfg, 1)
	recs := []Recommendation{
		{Player: &players.Player{ID: "a", Pos: players.RB, ADPStdDev: 3}, VOR: 30, PSurvive: 0.1}, // gone by my next pick
		{Player: &players.Player{ID: "b", Pos: players.RB, ADPStdDev: 3}, VOR: 31, PSurvive: 0.9}, // will be there
		{Player: &players.Player{ID: "c", Pos: players.RB, ADPStdDev: 3}, VOR: 20, PSurvive: 0.95},
	}
	e.score(recs, &Advice{ProjMode: "projections"})
	// a's fallback is b (31): regret 0. b's fallback is c (20): regret (0.1)(11)=1.1.
	if recs[0].Regret != 0 || math.Abs(recs[1].Regret-1.1) > 1e-9 {
		t.Errorf("regret: a=%.2f b=%.2f", recs[0].Regret, recs[1].Regret)
	}
	recs[1].PSurvive = 0.3 // now b likely gone too → a's fallback drops to c
	e.score(recs, &Advice{ProjMode: "projections"})
	if math.Abs(recs[0].Regret-0.9*10) > 1e-9 {
		t.Errorf("a regret with b gone = %.2f want 9.0", recs[0].Regret)
	}
}

// TestDemoMidDraft prints the M2 gate demo: a synthetic state at my live pick #26.
func TestDemoMidDraft(t *testing.T) {
	lg, pool, cfg := fixture(t)
	e := New(lg, pool, cfg, 42)
	st := newState(t, lg, pool)
	advanceBPA(t, st, pool, 8)
	snap := st.Snapshot()
	ad := e.Advise(snap)
	pick := func(id string) {
		if _, err := st.Pick(id, "", state.SourceSim); err != nil {
			t.Fatal(err)
		}
	}
	pick(ad.Top[0].Player.ID) // my #8
	advanceBPA(t, st, pool, 11)
	ad = e.Advise(st.Snapshot())
	pick(ad.Top[0].Player.ID) // my #11
	advanceBPA(t, st, pool, 26)
	ad = e.Advise(st.Snapshot())
	fmt.Println(Render(ad))
	if len(ad.Top) == 0 {
		t.Fatal("no recommendation")
	}
}

// TestGatePriorityIgnoresYAMLOrder covers the case two gates want the same pick. Before
// this, force() took whichever gate appeared first in strategy.yaml and silently dropped
// the other; the winner has to be the more urgent one, and the loser has to be visible.
func TestGatePriorityIgnoresYAMLOrder(t *testing.T) {
	lg, pool, cfg := fixture(t)
	me := lg.MyTeam
	mk := func(e *Engine, counts map[players.Position]int, live, left int) (*rosterCounts, *Advice) {
		rc := &rosterCounts{cfg: cfg, counts: map[string]map[players.Position]int{me: counts}, left: map[string]int{me: left}}
		slot, _ := lg.SlotForLive(live)
		return rc, &Advice{Round: slot.Round}
	}
	withGates := func(gs []strategy.Gate) *Engine {
		c := *cfg
		c.Gates = gs
		return New(lg, pool, &c, 1)
	}

	// Tighter slack wins even though it is declared second. QB has exactly one pick
	// left for one QB (slack 0); RB has one pick left for two RBs (slack -1).
	t.Run("tighter slack beats declaration order", func(t *testing.T) {
		e := withGates([]strategy.Gate{
			{Position: players.QB, MustDraftByLivePick: 68},
			{Position: players.RB, MinCountByLivePick: map[int]int{68: 2}},
		})
		rc, ad := mk(e, map[players.Position]int{players.WR: 2, players.TE: 1}, 68, 10)
		g := e.gates(rc, me, 68, ad)
		if g.forced != players.RB {
			t.Errorf("forced=%q want RB (QB is declared first but has more slack)", g.forced)
		}
		if !hasWarning(ad, "GATE CONFLICT") {
			t.Errorf("losing gate dropped silently; warnings=%v", ad.Warnings)
		}
	})

	// A deadline that has already passed cannot be saved by forcing, so a gate that
	// still can be met outranks it regardless of order.
	t.Run("savable beats already-missed", func(t *testing.T) {
		e := withGates([]strategy.Gate{
			{Position: players.TE, MustDraftByLivePick: 26},
			{Position: players.QB, MustDraftByLivePick: 68},
		})
		rc, ad := mk(e, map[players.Position]int{players.WR: 2, players.RB: 2}, 68, 10)
		g := e.gates(rc, me, 68, ad)
		if g.forced != players.QB {
			t.Errorf("forced=%q want QB (TE deadline #26 is already gone)", g.forced)
		}
	})

	// Same gate set, reversed in the file: the outcome must not move.
	t.Run("deterministic under reordering", func(t *testing.T) {
		a := []strategy.Gate{
			{Position: players.QB, MustDraftByLivePick: 68},
			{Position: players.RB, MinCountByLivePick: map[int]int{68: 2}},
		}
		b := []strategy.Gate{a[1], a[0]}
		var got []players.Position
		for _, gs := range [][]strategy.Gate{a, b} {
			e := withGates(gs)
			rc, ad := mk(e, map[players.Position]int{players.WR: 2, players.TE: 1}, 68, 10)
			got = append(got, e.gates(rc, me, 68, ad).forced)
		}
		if got[0] != got[1] {
			t.Errorf("reordering strategy.yaml changed the forced position: %q vs %q", got[0], got[1])
		}
	})
}

// TestShippedGatesDoNotCollide is the config guard: no two must-draft deadlines in the
// real strategy.yaml may share a last-chance pick, because only one of them can win.
func TestShippedGatesDoNotCollide(t *testing.T) {
	lg, _, cfg := fixture(t)
	lastChance := map[int][]players.Position{}
	for _, gt := range cfg.Gates {
		if gt.MustDraftByLivePick == 0 {
			continue
		}
		last := 0
		for _, lp := range lg.MyLivePicks {
			if lp <= gt.MustDraftByLivePick {
				last = lp
			}
		}
		if last == 0 {
			t.Errorf("%s gate deadline #%d is before my first pick", gt.Position, gt.MustDraftByLivePick)
			continue
		}
		lastChance[last] = append(lastChance[last], gt.Position)
	}
	for pick, pos := range lastChance {
		if len(pos) > 1 {
			t.Errorf("gates %v all have their last chance at pick #%d; only one can be forced", pos, pick)
		}
	}
}

func hasWarning(ad *Advice, sub string) bool {
	for _, w := range ad.Warnings {
		if strings.Contains(w, sub) {
			return true
		}
	}
	return false
}
