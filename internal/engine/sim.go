package engine

import (
	"fmt"
	"math"
	"math/rand/v2"
	"sort"

	"github.com/wambozi/draft-copilot/internal/league"
	"github.com/wambozi/draft-copilot/internal/players"
	"github.com/wambozi/draft-copilot/internal/state"
)

// SimPick is one live pick in a simulated draft.
type SimPick struct {
	Live   int
	Round  int
	Team   string
	Player *players.Player
	Mine   bool
}

// SimResult is one full mock draft from my seat's point of view.
type SimResult struct {
	Seed       uint64
	Picks      []SimPick // every live pick, in order
	Mine       []SimPick // my picks only
	Roster     []*players.Player
	Lineup     float64            // ProjPoints of my optimal starting lineup
	FlexPos    []players.Position // positions that ended up in my flex slots
	Violations []string           // §10 invariants this draft broke
	SpecCount  int                // keeper-speculative players on my final roster
	Counts     map[players.Position]int
	// GateFired counts my picks where a gate band was active and forced a position.
	// GateBinding counts the subset where that changed the pick: the best-scoring
	// eligible player was at a different position, so the deadline actually bit.
	GateFired   map[players.Position]int
	GateBinding map[players.Position]int
}

// SimOptions tune one mock draft.
type SimOptions struct {
	// Baseline drafts my seat by best available ADP (subject to legality) instead of
	// the engine, for the "median VOR beats naive" comparison.
	Baseline bool
	// Sims overrides cfg.Engine.Sims for the engine's survival Monte Carlo (speed).
	Sims int
}

// SimulateDraft runs one complete mock draft: the eleven opponents draft by noisy ADP
// with positional-need weighting (the same model Survival uses), my seat is drafted
// by the engine (or by naive BPA when opts.Baseline). Deterministic in seed.
func SimulateDraft(lg *league.League, pool *players.Pool, cfg0 *strategyConfig, seed uint64, opts SimOptions) (SimResult, error) {
	cfg := *cfg0
	if opts.Sims > 0 {
		cfg.Engine.Sims = opts.Sims
	}
	e := New(lg, pool, &cfg, seed)
	st, err := state.New(lg, pool, "")
	if err != nil {
		return SimResult{}, err
	}
	rng := rand.New(rand.NewPCG(seed, 0x5eed))
	res := SimResult{Seed: seed, Counts: map[players.Position]int{}, GateFired: map[players.Position]int{}, GateBinding: map[players.Position]int{}}
	me := lg.MyTeam

	for {
		snap := st.Snapshot()
		if snap.Complete {
			break
		}
		slot, _ := lg.SlotForLive(snap.LivePick)
		var choice *players.Player
		if slot.Team == me {
			if opts.Baseline {
				choice = e.baselinePick(snap, me)
			} else {
				ad := e.AdviseFor(snap, me)
				if len(ad.Top) == 0 {
					return res, fmt.Errorf("seed %d live %d: engine returned no recommendation", seed, snap.LivePick)
				}
				choice = ad.Top[0].Player
				if ad.Top[0].GateForced {
					res.GateFired[choice.Pos]++
					if ad.GateChanged {
						res.GateBinding[choice.Pos]++
					}
				}
			}
		} else {
			choice = e.opponentPick(rng, snap, slot)
		}
		if choice == nil {
			return res, fmt.Errorf("seed %d live %d: no legal player for %s", seed, snap.LivePick, slot.Team)
		}
		if _, err := st.Pick(choice.ID, "", state.SourceSim); err != nil {
			return res, fmt.Errorf("seed %d live %d %s -> %s: %w", seed, snap.LivePick, slot.Team, choice.Name, err)
		}
		sp := SimPick{Live: snap.LivePick, Round: slot.Round, Team: slot.Team, Player: choice, Mine: slot.Team == me}
		res.Picks = append(res.Picks, sp)
		if sp.Mine {
			res.Mine = append(res.Mine, sp)
		}
	}
	final := st.Snapshot()
	for _, id := range final.Rosters[me] {
		p := pool.ByID[id]
		res.Roster = append(res.Roster, p)
		res.Counts[p.Pos]++
	}
	res.Lineup, res.FlexPos = lineup(&cfg, res.Roster)
	res.Violations = checkInvariants(e, lg, &cfg, &res)
	return res, nil
}

// opponentPick draws one player for an opposing team exactly as Survival models it.
func (e *Engine) opponentPick(rng *rand.Rand, snap state.Snapshot, slot league.DraftSlot) *players.Player {
	board := e.board(snap)
	rc := newRosterCounts(e.lg, e.pool, e.cfg, snap)
	K := e.cfg.Engine.CandidatePool
	if K > len(board) {
		K = len(board)
	}
	type cand struct {
		p     *players.Player
		noisy float64
	}
	cs := make([]cand, 0, len(board))
	for _, p := range board {
		cs = append(cs, cand{p, p.ADPMean + rng.NormFloat64()*p.ADPStdDev})
	}
	sort.Slice(cs, func(a, b int) bool { return cs[a].noisy < cs[b].noisy })
	cs = cs[:K]
	lam := e.cfg.Engine.LambdaRank
	weights := make([]float64, K)
	total := 0.0
	for i, c := range cs {
		weights[i] = math.Exp(-lam*float64(i)) * rc.need(slot.Team, c.p.Pos, slot.Round)
		total += weights[i]
	}
	if total > 0 {
		r := rng.Float64() * total
		for i := range cs {
			r -= weights[i]
			if r <= 0 {
				return cs[i].p
			}
		}
	}
	// Everything weighted zero (rare endgame): first legal player.
	for _, c := range cs {
		if !rc.atMax(slot.Team, c.p.Pos) {
			return c.p
		}
	}
	for _, p := range board {
		if !rc.atMax(slot.Team, p.Pos) {
			return p
		}
	}
	return nil
}

// baselinePick is the naive strategy: best available by ADP that is legal and, in the
// endgame, still lets every starter slot be filled. Takes DST only when forced.
func (e *Engine) baselinePick(snap state.Snapshot, me string) *players.Player {
	board := e.board(snap)
	rc := newRosterCounts(e.lg, e.pool, e.cfg, snap)
	left := rc.left[me]
	mustFill := rc.openStarters(me) >= left
	for _, p := range board {
		if rc.atMax(me, p.Pos) {
			continue
		}
		if mustFill && !rc.fillsStarter(me, p.Pos) {
			continue
		}
		if p.Pos == players.DST && !(mustFill && rc.starterOpen(me, players.DST) > 0) {
			continue
		}
		return p
	}
	for _, p := range board {
		if !rc.atMax(me, p.Pos) {
			return p
		}
	}
	return nil
}

// lineup returns the ProjPoints of the best legal starting lineup and which positions
// fill the flex slots.
func lineup(cfg *strategyConfig, roster []*players.Player) (float64, []players.Position) {
	byPos := map[players.Position][]*players.Player{}
	for _, p := range roster {
		byPos[p.Pos] = append(byPos[p.Pos], p)
	}
	total := 0.0
	var surplus []*players.Player
	for pos, list := range byPos {
		sort.Slice(list, func(a, b int) bool { return list[a].ProjPoints > list[b].ProjPoints })
		n := cfg.Roster.Starters[pos]
		for i, p := range list {
			if i < n {
				total += p.ProjPoints
			} else if isFlex(cfg, pos) {
				surplus = append(surplus, p)
			}
		}
	}
	sort.Slice(surplus, func(a, b int) bool { return surplus[a].ProjPoints > surplus[b].ProjPoints })
	var flex []players.Position
	for i := 0; i < cfg.Roster.Flex.Count && i < len(surplus); i++ {
		total += surplus[i].ProjPoints
		flex = append(flex, surplus[i].Pos)
	}
	return total, flex
}

// checkInvariants evaluates the §10 per-draft invariants.
func checkInvariants(e *Engine, lg *league.League, cfg *strategyConfig, r *SimResult) []string {
	var v []string
	countBy := func(pos players.Position, live int) int {
		n := 0
		for _, p := range r.Mine {
			if p.Player.Pos == pos && p.Live <= live {
				n++
			}
		}
		return n
	}
	for _, g := range cfg.Gates {
		if g.MustDraftByLivePick > 0 && countBy(g.Position, g.MustDraftByLivePick) == 0 {
			v = append(v, fmt.Sprintf("no %s by #%d", g.Position, g.MustDraftByLivePick))
		}
		for deadline, want := range g.MinCountByLivePick {
			if n := countBy(g.Position, deadline); n < want {
				v = append(v, fmt.Sprintf("%d/%d %s by #%d", n, want, g.Position, deadline))
			}
		}
		if g.Exactly > 0 && r.Counts[g.Position] != g.Exactly {
			v = append(v, fmt.Sprintf("%d %s, want exactly %d", r.Counts[g.Position], g.Position, g.Exactly))
		}
		if g.NotBeforeRound > 0 {
			for _, p := range r.Mine {
				if p.Player.Pos == g.Position && p.Round < g.NotBeforeRound {
					v = append(v, fmt.Sprintf("%s in round %d (not before %d)", g.Position, p.Round, g.NotBeforeRound))
				}
			}
		}
	}
	if k := cfg.Keeper; k.MaxSpeculative > 0 && k.CostFloorRound > 0 {
		spec := 0
		for _, p := range r.Mine {
			if p.Round >= k.CostFloorRound && e.keeperP(p.Player) >= k.SpecThreshold {
				spec++
			}
		}
		r.SpecCount = spec
		if spec > k.MaxSpeculative {
			v = append(v, fmt.Sprintf("%d keeper-speculative players, cap %d", spec, k.MaxSpeculative))
		}
	}
	if r.Counts[players.K] > 0 {
		v = append(v, "drafted a kicker")
	}
	if n := len(r.Mine); n != len(lg.MyLivePicks) {
		v = append(v, fmt.Sprintf("%d picks made, want %d", n, len(lg.MyLivePicks)))
	}
	for pos, m := range cfg.Roster.Max {
		if r.Counts[pos] > m {
			v = append(v, fmt.Sprintf("%d %s exceeds max %d", r.Counts[pos], pos, m))
		}
	}
	for pos, n := range cfg.Roster.Starters {
		if r.Counts[pos] < n {
			v = append(v, fmt.Sprintf("starter slot unfillable: %d %s for %d slots", r.Counts[pos], pos, n))
		}
	}
	if len(r.FlexPos) < cfg.Roster.Flex.Count {
		v = append(v, fmt.Sprintf("only %d flex-eligible surplus for %d flex slots", len(r.FlexPos), cfg.Roster.Flex.Count))
	}
	return v
}
