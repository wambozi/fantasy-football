package engine

import (
	"fmt"
	"sort"
	"strings"

	"github.com/wambozi/draft-copilot/internal/league"
	"github.com/wambozi/draft-copilot/internal/players"
	"github.com/wambozi/draft-copilot/internal/strategy"
)

type gateResult struct {
	allowed map[players.Position]bool
	// forced is the set of positions a binding deadline restricts this pick to. Usually
	// one position; more when several deadlines share the same pick budget, in which
	// case value decides among them. Empty when nothing binds.
	forced map[players.Position]bool
	// unbound is allowed before any deadline restricted it: what the board could have
	// drafted had no gate bound. The sim uses it to tell a gate that merely confirmed
	// the value pick from one that changed it.
	unbound map[players.Position]bool
}

// gateReq is one outstanding roster requirement: still need `need` more at pos by my
// live pick `deadline`. Both must_draft_by_live_pick and min_count_by_live_pick reduce
// to this shape; the resolver never sees the difference.
type gateReq struct {
	pos      players.Position
	deadline int
	need     int
}

// gateReqs lists what strategy.yaml still demands of my roster given current counts.
// Requirements already met are omitted.
func gateReqs(gates []strategy.Gate, have func(players.Position) int) []gateReq {
	var reqs []gateReq
	for _, gt := range gates {
		h := have(gt.Position)
		if gt.MustDraftByLivePick > 0 && h == 0 {
			reqs = append(reqs, gateReq{gt.Position, gt.MustDraftByLivePick, 1})
		}
		for deadline, want := range gt.MinCountByLivePick {
			if need := want - h; need > 0 {
				reqs = append(reqs, gateReq{gt.Position, deadline, need})
			}
		}
	}
	return reqs
}

// demandThrough is how many of my picks the requirements with deadline ≤ D consume,
// and which positions they are for. Requirements at one position are cumulative
// (TE by #65 and 2 TE by #90 is two TEs, not three), so per position it is the max.
func demandThrough(reqs []gateReq, D int) (int, map[players.Position]bool) {
	perPos := map[players.Position]int{}
	for _, r := range reqs {
		if r.deadline <= D && r.need > perPos[r.pos] {
			perPos[r.pos] = r.need
		}
	}
	total, set := 0, map[players.Position]bool{}
	for pos, n := range perPos {
		total += n
		set[pos] = true
	}
	return total, set
}

func posList(set map[players.Position]bool) string {
	ps := make([]string, 0, len(set))
	for p := range set {
		ps = append(ps, string(p))
	}
	sort.Strings(ps)
	return strings.Join(ps, "/")
}

// gates applies strategy.yaml constraints to my seat. Two outputs: the set of positions
// I may draft right now, and (optionally) the positions a binding deadline restricts this
// pick to. Warnings and the red band are written onto ad.
//
// Every rule reasons in "my picks left before the deadline", not raw pick counts —
// with a snake and traded picks, that is the only honest measure of urgency.
//
// Deadlines are resolved jointly, earliest first (spec §10). For each deadline D the
// demand is every outstanding requirement due by D and the supply is my picks through D.
// The first D where demand reaches supply binds: this pick must go to one of the
// positions due by D, and value picks among them. Two gates that share a last-chance
// pick therefore bind one pick earlier instead of colliding on the same one, and a
// configuration that cannot be met at all is reported as a GATE HOLE rather than
// discovered when the deadline has already passed.
func (e *Engine) gates(rc *rosterCounts, me string, live int, ad *Advice) gateResult {
	g := gateResult{allowed: map[players.Position]bool{}, forced: map[players.Position]bool{}}
	for pos := range e.cfg.Roster.Starters {
		g.allowed[pos] = !rc.atMax(me, pos)
	}
	myPicksThrough := func(deadline int) int {
		n := 0
		for _, lp := range e.lg.MyLivePicks {
			if lp >= live && lp <= deadline {
				n++
			}
		}
		return n
	}
	picksLeft := rc.left[me]

	// Endgame: every starter slot must still be fillable with the picks I have left.
	if open := rc.openStarters(me); open >= picksLeft {
		for pos := range g.allowed {
			if !rc.fillsStarter(me, pos) {
				g.allowed[pos] = false
			}
		}
		if open > picksLeft {
			ad.Warnings = append(ad.Warnings, fmt.Sprintf("ROSTER HOLE: %d starter slots open, %d picks left", open, picksLeft))
		}
	}

	// Static bans and the mandatory endgame slot.
	var band string
	bind := func(set map[players.Position]bool, msg string) {
		if band != "" {
			return
		}
		ok := map[players.Position]bool{}
		for pos := range set {
			if g.allowed[pos] {
				ok[pos] = true
			}
		}
		if len(ok) == 0 {
			return
		}
		g.unbound = map[players.Position]bool{}
		for pos, a := range g.allowed {
			g.unbound[pos] = a
		}
		for pos := range g.allowed {
			g.allowed[pos] = ok[pos]
		}
		g.forced, band = ok, msg
	}
	for _, gt := range e.cfg.Gates {
		pos := gt.Position
		have := rc.count(me, pos)
		if gt.Max > 0 && have >= gt.Max {
			g.allowed[pos] = false
		}
		if gt.Exactly > 0 && have >= gt.Exactly {
			g.allowed[pos] = false
		}
		if gt.NotBeforeRound > 0 && ad.Round < gt.NotBeforeRound {
			g.allowed[pos] = false
		}
		if gt.Exactly > 0 && have == 0 && picksLeft == 1 && ad.Round >= gt.NotBeforeRound {
			g.allowed[pos] = true
			bind(map[players.Position]bool{pos: true}, fmt.Sprintf("%s GATE: last pick", pos))
		}
	}

	// Deadline requirements, split into still-savable and already-missed. A missed
	// deadline cannot be rescued by this pick, so it never outranks one that can be.
	var pending, overdue []gateReq
	for _, r := range gateReqs(e.cfg.Gates, func(p players.Position) int { return rc.count(me, p) }) {
		if live > r.deadline {
			overdue = append(overdue, r)
			ad.Warnings = append(ad.Warnings, fmt.Sprintf("%s GATE MISSED: still need %d %s after #%d", r.pos, r.need, r.pos, r.deadline))
		} else {
			pending = append(pending, r)
		}
	}
	deadlines := map[int]bool{}
	for _, r := range pending {
		deadlines[r.deadline] = true
	}
	ds := make([]int, 0, len(deadlines))
	for d := range deadlines {
		ds = append(ds, d)
	}
	sort.Ints(ds)
	for _, D := range ds {
		demand, set := demandThrough(pending, D)
		supply := myPicksThrough(D)
		if supply == 0 {
			continue // none of my picks fall before D; nothing to decide yet
		}
		switch slack := supply - demand; {
		case slack < 0:
			ad.Warnings = append(ad.Warnings, fmt.Sprintf("GATE HOLE: %d slots (%s) due by #%d, %d pick(s) left", demand, posList(set), D, supply))
			bind(set, fmt.Sprintf("%s GATE: %d slots, %d pick(s) by #%d", posList(set), demand, supply, D))
		case slack == 0:
			if len(set) == 1 && demand == 1 {
				bind(set, fmt.Sprintf("%s GATE: last pick before #%d", posList(set), D))
			} else {
				bind(set, fmt.Sprintf("%s GATE: %d slots, %d picks by #%d", posList(set), demand, supply, D))
			}
		case slack == 1:
			ad.Warnings = append(ad.Warnings, fmt.Sprintf("%s GATE: %d slot(s) by #%d, %d picks left", posList(set), demand, D, supply))
		}
		if band != "" {
			break
		}
	}
	if band == "" && len(overdue) > 0 {
		_, set := demandThrough(overdue, live)
		bind(set, fmt.Sprintf("%s GATE OVERDUE", posList(set)))
	}
	ad.GateBand = band
	return g
}

// CheckGates reports whether the deadline gates in cfg can all be met from an empty
// roster with my live picks. It is the boot-time guard for the configuration class that
// used to fail silently: two deadlines whose demand outruns the picks before them.
func CheckGates(lg *league.League, cfg *strategy.Config) error {
	if len(lg.MyLivePicks) == 0 {
		return nil
	}
	first := lg.MyLivePicks[0]
	reqs := gateReqs(cfg.Gates, func(players.Position) int { return 0 })
	seen := map[int]bool{}
	for _, r := range reqs {
		if seen[r.deadline] {
			continue
		}
		seen[r.deadline] = true
		demand, set := demandThrough(reqs, r.deadline)
		supply := 0
		for _, lp := range lg.MyLivePicks {
			if lp >= first && lp <= r.deadline {
				supply++
			}
		}
		if demand > supply {
			return fmt.Errorf("gates: %d slots (%s) due by #%d but only %d of my picks fall before it", demand, posList(set), r.deadline, supply)
		}
	}
	return nil
}
