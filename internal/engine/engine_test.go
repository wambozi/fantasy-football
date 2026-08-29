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
	// This test is about roster state driving need — keepers filling starter and flex
	// slots — so switch off the per-manager lean that multiplies on top of it. That term
	// has its own coverage in TestManagerBias.
	c := *cfg
	c.ManagerBias.Weight = 0
	cfg = &c
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
		if got := onlyForced(g); got != c.wantForced {
			t.Errorf("%s: forced=%v want %q (warnings %v)", c.name, g.forced, c.wantForced, ad.Warnings)
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

// TestGatesResolveJointly covers deadlines that share a pick budget. Each requirement
// used to be judged on its own, so two gates whose last chance was the same pick both
// waited for it and then collided; whichever was listed first in strategy.yaml won.
// Now demand is summed per deadline: the pick before the collision binds to the set,
// value picks among them, and the second is taken on the deadline itself.
func TestGatesResolveJointly(t *testing.T) {
	lg, pool, cfg := fixture(t)
	me := lg.MyTeam
	mk := func(counts map[players.Position]int, live, left int) (*rosterCounts, *Advice) {
		rc := &rosterCounts{cfg: cfg, counts: map[string]map[players.Position]int{me: counts}, left: map[string]int{me: left}}
		slot, _ := lg.SlotForLive(live)
		return rc, &Advice{Round: slot.Round}
	}
	withGates := func(gs []strategy.Gate) *Engine {
		c := *cfg
		c.Gates = gs
		return New(lg, pool, &c, 1)
	}
	both := []strategy.Gate{
		{Position: players.QB, MustDraftByLivePick: 68},
		{Position: players.TE, MustDraftByLivePick: 68},
	}
	base := map[players.Position]int{players.WR: 2, players.RB: 2}

	// Picks 65 and 68 remain, two slots due by 68: #65 binds to {QB, TE}.
	t.Run("shared deadline binds one pick early to the set", func(t *testing.T) {
		e := withGates(both)
		rc, ad := mk(base, 65, 11)
		g := e.gates(rc, me, 65, ad)
		if !g.forced[players.QB] || !g.forced[players.TE] || len(g.forced) != 2 {
			t.Errorf("forced=%v want {QB TE}; band=%q", g.forced, ad.GateBand)
		}
		if g.allowed[players.WR] || g.allowed[players.RB] {
			t.Errorf("allowed=%v: only the binding positions may be drafted", g.allowed)
		}
		if !strings.HasPrefix(ad.GateBand, "QB/TE GATE") {
			t.Errorf("band=%q", ad.GateBand)
		}
	})

	// Having taken one of them at #65, #68 is the other's last pick.
	t.Run("then the remaining one binds on the deadline", func(t *testing.T) {
		e := withGates(both)
		rc, ad := mk(map[players.Position]int{players.WR: 2, players.RB: 2, players.TE: 1}, 68, 10)
		g := e.gates(rc, me, 68, ad)
		if onlyForced(g) != players.QB {
			t.Errorf("forced=%v want QB", g.forced)
		}
	})

	// Cumulative requirements at one position do not double count: TE by #65 plus
	// two TE by #90 is two TE slots, not three.
	t.Run("same position requirements are cumulative", func(t *testing.T) {
		e := withGates([]strategy.Gate{
			{Position: players.TE, MustDraftByLivePick: 65, MinCountByLivePick: map[int]int{90: 2}},
		})
		rc, ad := mk(base, 49, 12) // picks 49, 65 before 65; 49, 65, 68, 85, 90 before 90
		g := e.gates(rc, me, 49, ad)
		if len(g.forced) != 0 {
			t.Errorf("nothing should bind at #49 (forced=%v band=%q)", g.forced, ad.GateBand)
		}
	})

	// A deadline that has already passed cannot be saved by forcing, so a gate that
	// still can be met outranks it.
	t.Run("savable beats already-missed", func(t *testing.T) {
		e := withGates([]strategy.Gate{
			{Position: players.TE, MustDraftByLivePick: 26},
			{Position: players.QB, MustDraftByLivePick: 68},
		})
		rc, ad := mk(base, 68, 10)
		g := e.gates(rc, me, 68, ad)
		if onlyForced(g) != players.QB {
			t.Errorf("forced=%v want QB (TE deadline #26 is already gone)", g.forced)
		}
		if !hasWarning(ad, "TE GATE MISSED") {
			t.Errorf("missed TE gate not reported; warnings=%v", ad.Warnings)
		}
	})

	// Impossible on this pick: two slots, one pick. Reported, and the set still binds.
	t.Run("hole is reported, not silent", func(t *testing.T) {
		e := withGates(both)
		rc, ad := mk(base, 68, 10)
		g := e.gates(rc, me, 68, ad)
		if !hasWarning(ad, "GATE HOLE") {
			t.Errorf("warnings=%v", ad.Warnings)
		}
		if len(g.forced) != 2 {
			t.Errorf("forced=%v want {QB TE}", g.forced)
		}
	})

	// Same gate set, reversed in the file: the outcome must not move.
	t.Run("deterministic under reordering", func(t *testing.T) {
		rev := []strategy.Gate{both[1], both[0]}
		var got []string
		for _, gs := range [][]strategy.Gate{both, rev} {
			e := withGates(gs)
			rc, ad := mk(base, 65, 11)
			g := e.gates(rc, me, 65, ad)
			got = append(got, fmt.Sprint(g.forced, ad.GateBand))
		}
		if got[0] != got[1] {
			t.Errorf("reordering strategy.yaml changed the result: %q vs %q", got[0], got[1])
		}
	})
}

// TestCheckGates is the boot guard: the shipped strategy.yaml must be feasible, and a
// config that asks for more slots than there are picks before a deadline must not be.
func TestCheckGates(t *testing.T) {
	lg, _, cfg := fixture(t)
	if err := CheckGates(lg, cfg); err != nil {
		t.Fatalf("shipped strategy.yaml: %v", err)
	}
	c := *cfg
	c.Gates = []strategy.Gate{
		{Position: players.QB, MustDraftByLivePick: 8},
		{Position: players.TE, MustDraftByLivePick: 8},
	}
	if err := CheckGates(lg, &c); err == nil {
		t.Error("two slots due by my first pick should fail feasibility")
	}
	// Shared deadlines are fine when the picks are there.
	c.Gates = []strategy.Gate{
		{Position: players.QB, MustDraftByLivePick: 68},
		{Position: players.TE, MustDraftByLivePick: 68},
	}
	if err := CheckGates(lg, &c); err != nil {
		t.Errorf("QB and TE by #68 is feasible (picks 65, 68): %v", err)
	}
}

func onlyForced(g gateResult) players.Position {
	if len(g.forced) != 1 {
		return ""
	}
	for p := range g.forced {
		return p
	}
	return ""
}

func hasWarning(ad *Advice, sub string) bool {
	for _, w := range ad.Warnings {
		if strings.Contains(w, sub) {
			return true
		}
	}
	return false
}

// TestBenchIndexSharesFlexPool pins the fix for the RB pile-up: the flex slots are one
// shared pool, so a position stacked past its share is bench and gets decayed. The old
// code gave every flex-eligible position its own copy of Flex.Count.
func TestBenchIndexSharesFlexPool(t *testing.T) {
	_, _, cfg := fixture(t)
	me := "me"
	mk := func(c map[players.Position]int) *rosterCounts {
		return &rosterCounts{cfg: cfg, counts: map[string]map[players.Position]int{me: c}, left: map[string]int{me: 7}}
	}
	flexN := cfg.Roster.Flex.Count
	cases := []struct {
		name   string
		counts map[players.Position]int
		want   map[players.Position]int
	}{
		{
			// 3 RB + 2 TE: one RB and one TE surplus exactly fill the two flex slots,
			// so nobody is on the bench yet. This is the state at live #85.
			"surplus exactly fills flex",
			map[players.Position]int{players.RB: 3, players.WR: 2, players.TE: 2},
			map[players.Position]int{players.RB: 0, players.WR: 0, players.TE: 0},
		},
		{
			// 4 RB + 2 TE: three surplus for two slots, so one body is bench. Under the
			// old per-position capacity this read 0 and the 4th RB went undiscounted.
			"one past the pool",
			map[players.Position]int{players.RB: 4, players.WR: 2, players.TE: 2},
			map[players.Position]int{players.RB: 1, players.WR: 0, players.TE: 0},
		},
		{
			// 5 RB + 2 TE: the state that prompted this. Two bodies are bench.
			"stacked one position",
			map[players.Position]int{players.RB: 5, players.WR: 2, players.TE: 2},
			map[players.Position]int{players.RB: 1, players.WR: 0, players.TE: 1},
		},
		{
			"nothing beyond starters",
			map[players.Position]int{players.RB: 2, players.WR: 2, players.TE: 1},
			map[players.Position]int{players.RB: 0, players.WR: 0, players.TE: 0},
		},
	}
	for _, c := range cases {
		rc := mk(c.counts)
		for pos, want := range c.want {
			if got := rc.benchIndex(me, pos); got != want {
				t.Errorf("%s: benchIndex(%s)=%d want %d", c.name, pos, got, want)
			}
		}
	}

	// The invariant that makes the apportionment safe regardless of how ties fall:
	// across positions, benchIndex counts exactly the players who cannot start.
	for rb := 0; rb <= 8; rb++ {
		for wr := 0; wr <= 8; wr++ {
			for te := 0; te <= 4; te++ {
				rc := mk(map[players.Position]int{players.RB: rb, players.WR: wr, players.TE: te})
				surplus, bench := 0, 0
				for _, pos := range cfg.Roster.Flex.Eligible {
					if s := rc.count(me, pos) - cfg.Roster.Starters[pos]; s > 0 {
						surplus += s
					}
					bench += rc.benchIndex(me, pos)
				}
				filled := surplus
				if filled > flexN {
					filled = flexN
				}
				if bench != surplus-filled {
					t.Fatalf("RB %d WR %d TE %d: bench total %d, want %d (surplus %d, flex filled %d)", rb, wr, te, bench, surplus-filled, surplus, filled)
				}
			}
		}
	}

	// And it must agree with fillsStarter: if a position still fills a starter or flex
	// slot, nobody at that position is on the bench yet.
	for rb := 0; rb <= 6; rb++ {
		for te := 0; te <= 3; te++ {
			rc := mk(map[players.Position]int{players.RB: rb, players.WR: 2, players.TE: te})
			for _, pos := range cfg.Roster.Flex.Eligible {
				if rc.fillsStarter(me, pos) && rc.benchIndex(me, pos) > 0 {
					t.Errorf("RB %d TE %d: %s fills a slot but benchIndex is %d", rb, te, pos, rc.benchIndex(me, pos))
				}
			}
		}
	}
}

// TestWaiverLevelUsesRoomCounts: with engine.waiver_drafted set, the waiver baseline
// indexes by how many players THIS room still expects to draft at the position, not by
// how many have consensus ADP inside the draft — and it adapts as players are taken.
func TestWaiverLevelUsesRoomCounts(t *testing.T) {
	lg, pool, cfg := fixture(t)
	if len(cfg.Engine.WaiverDrafted) == 0 {
		t.Fatal("strategy.yaml has no engine.waiver_drafted — the room-measured counts did not load")
	}
	e := New(lg, pool, cfg, 1)
	var board []*players.Player
	for _, p := range pool.Players {
		if p.Pos.Draftable() {
			board = append(board, p)
		}
	}
	kth := func(pos players.Position, k int) float64 {
		var pts []float64
		for _, p := range board {
			if p.Pos == pos {
				pts = append(pts, p.ProjPoints)
			}
		}
		sortDesc(pts)
		if k >= len(pts) {
			k = len(pts) - 1
		}
		return pts[k]
	}
	w := e.waiverLevel(board, nil)
	for pos, k := range cfg.Engine.WaiverDrafted {
		if want := kth(pos, k); w[pos] != want {
			t.Errorf("%s waiver level %.1f, want %.1f (the %d-th best projection)", pos, w[pos], want, k)
		}
	}
	// Once the room has taken its expected share, the wire is the best remaining player.
	taken := map[players.Position]int{players.QB: cfg.Engine.WaiverDrafted[players.QB] + 3}
	if w2 := e.waiverLevel(board, taken); w2[players.QB] != kth(players.QB, 0) {
		t.Errorf("QB fully drafted: waiver level %.1f, want best available %.1f", w2[players.QB], kth(players.QB, 0))
	}
	// The room count must actually move the needle off the ADP-count fallback.
	cfg2 := *cfg
	eng2 := cfg2.Engine
	eng2.WaiverDrafted = nil
	cfg2.Engine = eng2
	if w3 := New(lg, pool, &cfg2, 1).waiverLevel(board, nil); w3[players.QB] >= w[players.QB] {
		t.Errorf("ADP-count QB level %.1f should sit below the room-count level %.1f — consensus drafts more QBs than this room", w3[players.QB], w[players.QB])
	}
}

// TestManagerBias covers the per-manager lean measured from past seasons: it must nudge
// the opponent model without replacing it, stay neutral for unknown teams, and switch off
// cleanly at weight 0 so the league-average model is always one config edit away.
func TestManagerBias(t *testing.T) {
	_, _, cfg := fixture(t)
	if len(cfg.ManagerBias.Teams) == 0 {
		t.Fatal("strategy.yaml has no manager_bias.teams — the measured tendencies did not load")
	}
	// Every team in draft-order.csv should have a profile, or the bias silently applies
	// to some managers and not others.
	lg, _, _ := fixture(t)
	for _, team := range lg.Teams {
		if _, ok := cfg.ManagerBias.Teams[team]; !ok {
			t.Errorf("no manager_bias entry for %q", team)
		}
	}

	b := cfg.ManagerBias
	t.Run("neutral when the pick fills a slot they need", func(t *testing.T) {
		// The lean is a bench habit. A team with a TE slot still open is filling a need,
		// not hoarding, so the multiplier must be 1 there.
		if got := b.For("Pollock Debacle", players.TE, 9, true); got != 1 {
			t.Errorf("got %v want 1 for a starter-filling pick", got)
		}
	})
	t.Run("unknown team is neutral", func(t *testing.T) {
		if got := b.For("Nobody FC", players.RB, 5, false); got != 1 {
			t.Errorf("got %v want 1", got)
		}
	})
	t.Run("weight 0 disables", func(t *testing.T) {
		off := b
		off.Weight = 0
		for _, team := range lg.Teams {
			for _, pos := range []players.Position{players.QB, players.RB, players.WR, players.TE, players.DST} {
				if got := off.For(team, pos, 3, false); got != 1 {
					t.Fatalf("weight 0: %s %s -> %v, want 1", team, pos, got)
				}
			}
		}
	})
	t.Run("weight scales the deviation, not the multiplier", func(t *testing.T) {
		half := b
		half.Weight = 0.5
		half.EarlyDamp = 0 // isolate the bias term from the timing term
		full := b
		full.EarlyDamp = 0
		// Pollock Debacle reaches for TE: bias 1.58, so half weight must land halfway.
		f := full.For("Pollock Debacle", players.TE, 9, false)
		h := half.For("Pollock Debacle", players.TE, 9, false)
		if f <= 1 {
			t.Fatalf("expected a TE lean for Pollock Debacle, got %v", f)
		}
		if want := 1 + (f-1)/2; math.Abs(h-want) > 1e-9 {
			t.Errorf("half weight = %v, want %v", h, want)
		}
	})
	t.Run("damps a position before that manager's usual round", func(t *testing.T) {
		// Svannah Alley Cats have not taken a QB before round 12 in three seasons.
		early := b.For("Svannah Alley Cats", players.QB, 4, false)
		late := b.For("Svannah Alley Cats", players.QB, 13, false)
		if !(early < late) {
			t.Errorf("QB weight round 4 (%v) should be below round 13 (%v)", early, late)
		}
	})
	t.Run("per-team lambda_rank nudges, never replaces", func(t *testing.T) {
		global := cfg.Engine.LambdaRank
		if got := b.LambdaFor("Nobody FC", global); got != global {
			t.Errorf("unknown team λ = %v, want global %v", got, global)
		}
		off := b
		off.LambdaWeight = 0
		for _, team := range lg.Teams {
			if got := off.LambdaFor(team, global); got != global {
				t.Fatalf("lambda_weight 0: %s λ = %v, want global %v", team, got, global)
			}
		}
		half := b
		half.LambdaWeight = 0.5
		full := b.LambdaFor("Lawson Country Lets Ride", global)
		if got, want := half.LambdaFor("Lawson Country Lets Ride", global), global+(full-global)/2; math.Abs(got-want) > 1e-9 {
			t.Errorf("half weight λ = %v, want %v", got, want)
		}
		// Every shipped value stays a nudge: within [0.5, 1.5]× the global steepness.
		for _, team := range lg.Teams {
			lam := b.LambdaFor(team, global)
			if lam < 0.5*global || lam > 1.5*global {
				t.Errorf("%s λ = %v — outside the nudge band around %v; remeasure before shipping", team, lam, global)
			}
		}
	})
	t.Run("the standout profiles survived the pipeline", func(t *testing.T) {
		// One sanity check per distinctive habit, so a bad regeneration is caught here
		// rather than in a draft. See data/draft-20*.json.
		if got := b.Teams["Ja'Marr & Jahmyr"].DST; got < 1.4 {
			t.Errorf("Ja'Marr & Jahmyr DST bias %v — they are the two-defence manager", got)
		}
		if got := b.Teams["Trash Pandas"].RB; got < 1.15 {
			t.Errorf("Trash Pandas RB bias %v — they hoard RB", got)
		}
		if got := b.Teams["Svannah Alley Cats"].FirstQB; got < 10 {
			t.Errorf("Svannah Alley Cats first_qb %v — they wait on QB", got)
		}
	})
}
