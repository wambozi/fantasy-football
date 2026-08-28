package engine

import (
	"math"

	"github.com/wambozi/draft-copilot/internal/players"
)

// replacement computes the dynamic replacement level per position.
//
//	remaining_demand(P) = Σ_teams open starter slots at P
//	                    + flex_share(P) × Σ_teams open flex slots
//	replacement(P)      = ProjPoints of the ceil(demand)-th best available at P
//
// Static baselines ("RB24 is replacement") are wrong here because keepers already
// satisfied part of the starter demand. Counting only what is still unfilled makes the
// RB cliff (10 kept, few startable left) show up in the numbers instead of in memory.
// A position whose demand is fully satisfied still has demand floored at 1, so its best
// available player has VOR 0 and everyone else negative — bench value only.
func (e *Engine) replacement(board []*players.Player, rc *rosterCounts) (demand, repl map[players.Position]float64) {
	demand = map[players.Position]float64{}
	repl = map[players.Position]float64{}
	flexOpen := 0
	for team := range rc.counts {
		flexOpen += rc.flexOpen(team)
		for pos := range e.cfg.Roster.Starters {
			demand[pos] += float64(rc.starterOpen(team, pos))
		}
	}
	for pos, share := range e.cfg.Engine.FlexShare {
		demand[pos] += share * float64(flexOpen)
	}
	// board is sorted by ADP; within a position we want the k-th best by projection.
	byPos := map[players.Position][]float64{}
	for _, p := range board {
		byPos[p.Pos] = append(byPos[p.Pos], p.ProjPoints)
	}
	for pos, pts := range byPos {
		sortDesc(pts)
		k := int(math.Ceil(demand[pos]))
		if k < 1 {
			k = 1
		}
		if k > len(pts) {
			k = len(pts)
		}
		repl[pos] = pts[k-1]
	}
	return demand, repl
}

func sortDesc(v []float64) {
	for i := 1; i < len(v); i++ { // insertion sort: slices are ≤ ~190 and mostly sorted
		x := v[i]
		j := i - 1
		for j >= 0 && v[j] < x {
			v[j+1] = v[j]
			j--
		}
		v[j+1] = x
	}
}
