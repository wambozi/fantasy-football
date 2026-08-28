package engine

import (
	"fmt"

	"github.com/wambozi/draft-copilot/internal/players"
)

type gateResult struct {
	allowed map[players.Position]bool
	forced  players.Position
}

// gates applies strategy.yaml constraints to my seat. Two outputs: the set of positions
// I may draft right now, and (optionally) a single forced position when a deadline is
// about to bind. Warnings and the red band are written onto ad.
//
// Every rule reasons in "my picks left before the deadline", not raw pick counts —
// with a snake and traded picks, that is the only honest measure of urgency.
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

	var band string
	force := func(pos players.Position, msg string) {
		if g.forced == "" && g.allowed[pos] {
			g.forced = pos
			band = msg
		}
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
		if gt.MustDraftByLivePick > 0 && have == 0 {
			n := myPicksThrough(gt.MustDraftByLivePick)
			switch {
			case n == 1:
				force(pos, fmt.Sprintf("%s GATE: last pick before #%d", pos, gt.MustDraftByLivePick))
			case n == 2:
				ad.Warnings = append(ad.Warnings, fmt.Sprintf("%s GATE: 2 picks left (by #%d)", pos, gt.MustDraftByLivePick))
			case n == 0 && live <= gt.MustDraftByLivePick:
				// no picks before the deadline at all; nothing to do here
			case n == 0:
				ad.Warnings = append(ad.Warnings, fmt.Sprintf("%s GATE MISSED: still no %s after #%d", pos, pos, gt.MustDraftByLivePick))
				force(pos, fmt.Sprintf("%s GATE OVERDUE", pos))
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
				force(pos, fmt.Sprintf("%s GATE: need %d more by #%d, %d pick(s) left", pos, need, deadline, n))
			case n > 0 && n-need == 1:
				ad.Warnings = append(ad.Warnings, fmt.Sprintf("%s GATE: need %d more by #%d, %d picks left", pos, need, deadline, n))
			case n == 0 && live > deadline:
				ad.Warnings = append(ad.Warnings, fmt.Sprintf("%s GATE MISSED: %d/%d by #%d", pos, have, want, deadline))
			}
		}
		if gt.Exactly > 0 && have == 0 && picksLeft == 1 && ad.Round >= gt.NotBeforeRound {
			g.allowed[pos] = true
			force(pos, fmt.Sprintf("%s GATE: last pick", pos))
		}
	}
	ad.GateBand = band
	return g
}
