package engine

import (
	"fmt"
	"sort"

	"github.com/wambozi/draft-copilot/internal/players"
)

type gateResult struct {
	allowed map[players.Position]bool
	forced  players.Position
}

// gateCandidate is one gate asking to force a position on this pick. Candidates are
// collected across every gate and resolved by urgency after the loop, never by the
// order the blocks happen to appear in strategy.yaml (spec §10).
type gateCandidate struct {
	pos players.Position
	msg string
	// slack is my picks available before the deadline minus how many I still need.
	// 0 = this is the last pick that satisfies it; negative = already short.
	slack    int
	deadline int
	// missed means the deadline has already passed. Forcing cannot save it, so a
	// still-savable gate outranks it.
	missed bool
	order  int // index in cfg.Gates: final, deterministic tie-break only
}

// gates applies strategy.yaml constraints to my seat. Two outputs: the set of positions
// I may draft right now, and (optionally) a single forced position when a deadline is
// about to bind. Warnings and the red band are written onto ad.
//
// Every rule reasons in "my picks left before the deadline", not raw pick counts —
// with a snake and traded picks, that is the only honest measure of urgency.
//
// Two gates can want the same pick (e.g. a TE deadline and a QB deadline whose last
// chance is the same live pick). Only one can win — a pick cannot fill two slots — so
// the tightest one does, and the loser is surfaced as a GATE CONFLICT warning rather
// than silently dropped.
func (e *Engine) gates(rc *rosterCounts, me string, live int, ad *Advice) gateResult {
	g := gateResult{allowed: map[players.Position]bool{}}
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

	var cands []gateCandidate
	for i, gt := range e.cfg.Gates {
		pos := gt.Position
		have := rc.count(me, pos)
		propose := func(msg string, slack, deadline int, missed bool) {
			cands = append(cands, gateCandidate{pos: pos, msg: msg, slack: slack, deadline: deadline, missed: missed, order: i})
		}
		if gt.Max > 0 && have >= gt.Max {
			g.allowed[pos] = false
		}
		if gt.Exactly > 0 && have >= gt.Exactly {
			g.allowed[pos] = false
		}
		if gt.NotBeforeRound > 0 && ad.Round < gt.NotBeforeRound {
			g.allowed[pos] = false
		}
		if gt.MustDraftByLivePick > 0 && have == 0 {
			n := myPicksThrough(gt.MustDraftByLivePick)
			switch {
			case n == 1:
				propose(fmt.Sprintf("%s GATE: last pick before #%d", pos, gt.MustDraftByLivePick), 0, gt.MustDraftByLivePick, false)
			case n == 2:
				ad.Warnings = append(ad.Warnings, fmt.Sprintf("%s GATE: 2 picks left (by #%d)", pos, gt.MustDraftByLivePick))
			case n == 0 && live <= gt.MustDraftByLivePick:
				// no picks before the deadline at all; nothing to do here
			case n == 0:
				ad.Warnings = append(ad.Warnings, fmt.Sprintf("%s GATE MISSED: still no %s after #%d", pos, pos, gt.MustDraftByLivePick))
				propose(fmt.Sprintf("%s GATE OVERDUE", pos), -1, gt.MustDraftByLivePick, true)
			}
		}
		for deadline, want := range gt.MinCountByLivePick {
			need := want - have
			if need <= 0 {
				continue
			}
			n := myPicksThrough(deadline)
			switch {
			case n > 0 && need >= n:
				propose(fmt.Sprintf("%s GATE: need %d more by #%d, %d pick(s) left", pos, need, deadline, n), n-need, deadline, false)
			case n > 0 && n-need == 1:
				ad.Warnings = append(ad.Warnings, fmt.Sprintf("%s GATE: need %d more by #%d, %d picks left", pos, need, deadline, n))
			case n == 0 && live > deadline:
				ad.Warnings = append(ad.Warnings, fmt.Sprintf("%s GATE MISSED: %d/%d by #%d", pos, have, want, deadline))
			}
		}
		if gt.Exactly > 0 && have == 0 && picksLeft == 1 && ad.Round >= gt.NotBeforeRound {
			g.allowed[pos] = true
			propose(fmt.Sprintf("%s GATE: last pick", pos), 0, live, false)
		}
	}

	// Resolve. Savable before missed, then tightest slack, then earliest deadline,
	// then declaration order so the choice is deterministic.
	sort.SliceStable(cands, func(a, b int) bool {
		x, y := cands[a], cands[b]
		switch {
		case x.missed != y.missed:
			return !x.missed
		case x.slack != y.slack:
			return x.slack < y.slack
		case x.deadline != y.deadline:
			return x.deadline < y.deadline
		default:
			return x.order < y.order
		}
	})
	var band string
	won := -1
	for i, c := range cands {
		if g.allowed[c.pos] {
			g.forced, band, won = c.pos, c.msg, i
			break
		}
	}
	// Anything else that also needed this very pick cannot be satisfied now. Say so.
	for i, c := range cands {
		if i == won || c.missed || c.slack > 0 || !g.allowed[c.pos] {
			continue
		}
		if won >= 0 {
			ad.Warnings = append(ad.Warnings, fmt.Sprintf("GATE CONFLICT: %s forced, so %s cannot also be met (%s)", cands[won].pos, c.pos, c.msg))
		}
	}
	ad.GateBand = band
	return g
}
