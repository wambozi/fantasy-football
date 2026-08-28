package players

import (
	"math"
	"sort"
)

// Projection modes reported by the pool.
const (
	ProjReal   = "projections"  // real season projections were loaded
	ProjFinish = "finish-curve" // last season's finish-order points assigned by ECR rank
	ProjCurve  = "adp-curve"    // ProjPoints fitted from positional ADP rank
	ProjNone   = "adp-only"     // nothing: VOR is 0
)

// CurveProjections fills ProjPoints for every player from positional ADP rank when no
// real projections are loaded: points ≈ a·exp(−b·rank) + c per position (spec §4).
// The shape is what matters — a steep RB/TE cliff, a flatter WR curve, QB in between —
// scaled to roughly full-PPR season totals so gates and replacement levels stay in
// familiar units. Players with no ADP go to the back of their position.
// No-op (returns false) if any player already has a projection.
func (p *Pool) CurveProjections() bool {
	any, allFinish := false, true
	for _, pl := range p.Players {
		if pl.ProjPoints > 0 {
			any = true
			if pl.ProjSource != ProjFinish {
				allFinish = false
			}
		}
	}
	if any {
		p.ProjMode = ProjReal
		if allFinish {
			p.ProjMode = ProjFinish
		}
		return false
	}
	sorted := append([]*Player(nil), p.Players...)
	sort.SliceStable(sorted, func(i, j int) bool { return adpOrLast(sorted[i]) < adpOrLast(sorted[j]) })
	curve := map[Position][3]float64{
		QB: {160, 0.08, 240}, RB: {220, 0.07, 60}, WR: {200, 0.045, 70},
		TE: {150, 0.12, 50}, DST: {60, 0.1, 70},
	}
	rank := map[Position]int{}
	for _, pl := range sorted {
		c, ok := curve[pl.Pos]
		if !ok {
			continue
		}
		r := rank[pl.Pos]
		rank[pl.Pos]++
		pl.ProjPoints = c[0]*math.Exp(-c[1]*float64(r)) + c[2]
	}
	p.ProjMode = ProjCurve
	return true
}
