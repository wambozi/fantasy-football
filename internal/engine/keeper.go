package engine

import (
	"fmt"
	"io"
	"math"
	"sort"

	"github.com/wambozi/draft-copilot/internal/league"
	"github.com/wambozi/draft-copilot/internal/players"
	"github.com/wambozi/draft-copilot/internal/state"
)

// Keeper economics (spec §14).
//
// A year-1 keeper costs the round he was drafted in, floored at round 8, so every pick
// from the floor onward carries the same keeper cost. From there a young player with a
// real 2027 outlook is worth more than his 2026 projection says:
//
//	keeper_surplus(p) = P(hit) × max(0, value2027(p) − valueR8) × surplus_weight_late
//
// P(hit) — will he be one of my two keepers next year? — comes from age, rookie rank
// and the gap between dynasty rank and ADP (all from the FantasyPros exports). Because
// an R8-cost hit beats keeping JSN for a 1st or Odunze for a 2nd, P(top-2) collapses
// to P(player hits). value2027 is the projection of the player currently sitting at
// his dynasty rank on the ADP board; valueR8 the projection at a floor-round slot.
// Before the floor the term is zero: rounds 1–7 have no arbitrage.

// keeperP estimates P(hit) in [0,1]. Exported through Recommendation.KeeperP.
func (e *Engine) keeperP(p *players.Player) float64 {
	for _, t := range e.cfg.Keeper.Targets {
		if players.NameKey(t) == players.NameKey(p.Name) {
			return 1
		}
	}
	if p.Age == 0 {
		return 0
	}
	youth := clamp((27-float64(p.Age))/4, 0, 1) // 23 → 1, 27 → 0
	if youth == 0 {
		return 0
	}
	// Dynasty signal needs both: the room drafting him well below his dynasty rank
	// (gap) AND a dynasty rank that is actually keeper-worthy (quality). A 22-year-old
	// ranked 211 in dynasty is not a 2027 asset however cheap he is.
	signal := 0.0
	if p.DynastyRank > 0 && p.ADPMean > 0 {
		gap := clamp((p.ADPMean-float64(p.DynastyRank))/120, 0, 1)
		quality := clamp((150-float64(p.DynastyRank))/120, 0, 1)
		signal = math.Sqrt(gap * quality)
	}
	if p.RookieRank > 0 {
		signal = math.Max(signal, 0.8*clamp((30-float64(p.RookieRank))/28, 0, 1))
	}
	return youth * signal
}

// keeperSurplus is the score bonus for r at the current round.
func (e *Engine) keeperSurplus(r Recommendation, ad *Advice) float64 {
	k := e.cfg.Keeper
	if k.CostFloorRound == 0 || ad.Round < k.CostFloorRound || k.SurplusLate == 0 {
		return 0
	}
	p := r.Player
	if r.KeeperP <= 0 || p.DynastyRank == 0 {
		return 0
	}
	v2027 := e.valueAtADPRank(p.DynastyRank)
	vR8 := e.valueAtADPRank(k.R8Pick)
	// A 2027 asset still occupies a 2026 roster spot: the same bench multiplier that
	// discounts his VOR (QB2, a 6th RB) discounts his keeper upside.
	return r.KeeperP * math.Max(0, v2027-vR8) * k.SurplusLate * r.bench
}

// annotateKeeper fills KeeperCost/KeeperP/KeeperSpec on every candidate for the round.
func (e *Engine) annotateKeeper(recs []Recommendation, round int) {
	k := e.cfg.Keeper
	if k.CostFloorRound == 0 {
		return
	}
	for i := range recs {
		r := &recs[i]
		r.KeeperP = e.keeperP(r.Player)
		// Only shown from the floor on: before it the cost is just the round, and the
		// screen is for a 60-second read.
		if round >= k.CostFloorRound {
			r.KeeperCost = fmt.Sprintf("R%d", k.CostFloorRound)
			r.KeeperSpec = r.KeeperP >= k.SpecThreshold
		}
	}
}

// specCount is how many keeper-speculative players (P ≥ threshold, drafted at or after
// the floor round) are already on my roster. Keepers and early picks don't count.
func (e *Engine) specCount(snap state.Snapshot, me string) int {
	k := e.cfg.Keeper
	if k.CostFloorRound == 0 {
		return 0
	}
	n := 0
	for _, pk := range snap.Picks {
		if pk.Team != me {
			continue
		}
		slot, ok := e.lg.SlotForLive(pk.LivePick)
		if !ok || slot.Round < k.CostFloorRound {
			continue
		}
		if p, ok := e.pool.ByID[pk.PlayerID]; ok && e.keeperP(p) >= k.SpecThreshold {
			n++
		}
	}
	return n
}

// valueAtADPRank returns the projection of the player at the given overall ADP rank
// (1-based) as value over waiver, from a table built once per engine. Beyond the table it returns the tail.
func (e *Engine) valueAtADPRank(rank int) float64 {
	e.rankOnce.Do(func() {
		var ps []*players.Player
		for _, p := range e.pool.Players {
			if p.ADPMean > 0 && p.Pos.Draftable() {
				ps = append(ps, p)
			}
		}
		sort.SliceStable(ps, func(i, j int) bool { return ps[i].ADPMean < ps[j].ADPMean })
		// Value over the position's waiver level, so QB points and WR points compare.
		// Full pool, nobody taken: with waiver_drafted set the index is the table itself.
		waiver := e.waiverLevel(ps, nil)
		e.valueByRank = make([]float64, len(ps))
		for i, p := range ps {
			e.valueByRank[i] = math.Max(0, p.ProjPoints-waiver[p.Pos])
		}
		// Smooth: 5-wide moving average so one mis-projected player doesn't spike a rank.
		sm := make([]float64, len(ps))
		for i := range ps {
			lo, hi := max(0, i-2), min(len(ps)-1, i+2)
			s := 0.0
			for j := lo; j <= hi; j++ {
				s += e.valueByRank[j]
			}
			sm[i] = s / float64(hi-lo+1)
		}
		e.valueByRank = sm
	})
	if len(e.valueByRank) == 0 {
		return 0
	}
	i := rank - 1
	if i < 0 {
		i = 0
	}
	if i >= len(e.valueByRank) {
		i = len(e.valueByRank) - 1
	}
	return e.valueByRank[i]
}

func clamp(v, lo, hi float64) float64 { return math.Max(lo, math.Min(hi, v)) }

// PrintKeeperCandidates lists the highest-P(hit) draftable players with their keeper
// surplus at the floor round — a sanity check on the 2027 model before draft night.
func PrintKeeperCandidates(w io.Writer, lg *league.League, pool *players.Pool, cfg *strategyConfig, n int) {
	e := New(lg, pool, cfg, 1)
	type row struct {
		p       *players.Player
		P, surp float64
	}
	var rows []row
	for _, p := range pool.Players {
		if !p.Pos.Draftable() || p.Keeper {
			continue
		}
		P := e.keeperP(p)
		if P <= 0 {
			continue
		}
		surp := P * math.Max(0, e.valueAtADPRank(p.DynastyRank)-e.valueAtADPRank(cfg.Keeper.R8Pick)) * cfg.Keeper.SurplusLate
		rows = append(rows, row{p, P, surp})
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].P != rows[j].P {
			return rows[i].P > rows[j].P
		}
		return rows[i].p.ADPMean < rows[j].p.ADPMean
	})
	fmt.Fprintf(w, "%-24s %-3s %5s %4s %6s %6s %5s %7s\n", "player", "pos", "adp", "age", "dyn", "rookie", "P", "bonus@R8")
	for i, r := range rows {
		if i >= n {
			break
		}
		fmt.Fprintf(w, "%-24s %-3s %5.0f %4d %6d %6d %5.2f %7.1f\n", r.p.Name, r.p.Pos, r.p.ADPMean, r.p.Age, r.p.DynastyRank, r.p.RookieRank, r.P, r.surp)
	}
}
