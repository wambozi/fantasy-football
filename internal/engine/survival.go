package engine

import (
	"math"
	"math/rand/v2"
	"sort"

	"github.com/wambozi/draft-copilot/internal/players"
	"github.com/wambozi/draft-copilot/internal/state"
)

// Survival estimates, for every player on the board, the probability they are still
// available at my next live pick (nextLive). Returns 1.0 for everyone when I have no
// opposing picks to wait through.
//
// Why Monte Carlo and not a normal CDF: a closed form treats each opposing pick as
// independent, so it cannot represent a positional run. Simulating the room pick by
// pick, with each opponent's need updated after every simulated selection, captures
// runs — and a run at RB is exactly what ten kept RBs guarantee in round 1.
//
// Per sim:
//
//	each available player gets a noisy ADP: adp_i + N(0, σ_i)   (σ from source spread)
//	for each opposing pick until my turn:
//	    candidates = top `candidate_pool` still-available players by noisy ADP
//	    w_i = exp(-λ_rank × rank_i) × need(team, pos_i, round)
//	    draw one by weight, mark taken, update that team's roster counts
//	survivors are tallied; p_survive = survivals / sims
func (e *Engine) Survival(board []*players.Player, snap state.Snapshot, rc *rosterCounts, nextLive int, me string) map[string]float64 {
	out, _ := e.survival(board, snap, rc, nextLive, me)
	return out
}

// survival is Survival plus, per opposing team, the share of sims in which that team's
// first pick inside the window went to each position — the "likely next move" the
// league view shows. Same sims, no extra cost.
func (e *Engine) survival(board []*players.Player, snap state.Snapshot, rc *rosterCounts, nextLive int, me string) (map[string]float64, map[string]map[players.Position]float64) {
	out := make(map[string]float64, len(board))
	posByTeam := map[string]map[players.Position]float64{}
	// Opposing picks between now and my next turn.
	var opp []int
	start := snap.LivePick
	if e.lg.TeamOnClock(start) == me {
		start++
	}
	if nextLive > 0 {
		for live := start; live < nextLive; live++ {
			opp = append(opp, live)
		}
	}
	if len(opp) == 0 {
		for _, p := range board {
			out[p.ID] = 1
		}
		return out, posByTeam
	}

	// Only the top K by ADP can realistically go in the next h picks; the rest survive.
	// K = pool + 3h leaves room for σ to pull deep players up into contention.
	h := len(opp)
	K := e.cfg.Engine.CandidatePool + 3*h
	if K > len(board) {
		K = len(board)
	}
	cand := board[:K]
	for _, p := range board[K:] {
		out[p.ID] = 1
	}

	sims := e.cfg.Engine.Sims
	survived := make([]int, K)
	noisy := make([]float64, K)
	order := make([]int, K)
	taken := make([]bool, K)
	weights := make([]float64, e.cfg.Engine.CandidatePool)
	picked := make([]int, 0, h)
	rng := rand.New(rand.NewPCG(e.seed, uint64(snap.Version)))
	// λ_rank is per opposing team (measured reach-vs-ADP, docs/reach-vs-adp-2026-08-29.md);
	// resolved once per window pick, not per sim.
	lamByTeam := map[string]float64{}
	for _, live := range opp {
		slot, _ := e.lg.SlotForLive(live)
		if _, ok := lamByTeam[slot.Team]; !ok {
			lamByTeam[slot.Team] = e.cfg.ManagerBias.LambdaFor(slot.Team, e.cfg.Engine.LambdaRank)
		}
	}
	firstPick := map[string]bool{} // per sim: has this team's first window pick been tallied
	tally := map[string]map[players.Position]int{}

	for s := 0; s < sims; s++ {
		clear(firstPick)
		for i, p := range cand {
			noisy[i] = p.ADPMean + rng.NormFloat64()*p.ADPStdDev
			order[i] = i
			taken[i] = false
		}
		sort.Slice(order, func(a, b int) bool { return noisy[order[a]] < noisy[order[b]] })
		sim := rc.clone()
		picked = picked[:0]
		for _, live := range opp {
			slot, _ := e.lg.SlotForLive(live)
			team := slot.Team
			lam := lamByTeam[team]
			// Walk the noisy order collecting the first `candidate_pool` untaken players.
			total := 0.0
			n := 0
			for _, idx := range order {
				if taken[idx] {
					continue
				}
				w := math.Exp(-lam*float64(n)) * sim.need(team, cand[idx].Pos, slot.Round)
				weights[n] = w
				picked = append(picked, idx)
				total += w
				n++
				if n == len(weights) {
					break
				}
			}
			base := len(picked) - n
			chosen := -1
			if total > 0 {
				r := rng.Float64() * total
				for i := 0; i < n; i++ {
					r -= weights[i]
					if r <= 0 {
						chosen = picked[base+i]
						break
					}
				}
			}
			if chosen < 0 && n > 0 { // all weights zero (e.g. everything at max): take BPA
				chosen = picked[base]
			}
			picked = picked[:base]
			if chosen < 0 {
				continue
			}
			taken[chosen] = true
			sim.add(team, cand[chosen].Pos)
			if !firstPick[team] {
				firstPick[team] = true
				if tally[team] == nil {
					tally[team] = map[players.Position]int{}
				}
				tally[team][cand[chosen].Pos]++
			}
		}
		for i := range cand {
			if !taken[i] {
				survived[i]++
			}
		}
	}
	for i, p := range cand {
		out[p.ID] = float64(survived[i]) / float64(sims)
	}
	for team, m := range tally {
		shares := make(map[players.Position]float64, len(m))
		for pos, n := range m {
			shares[pos] = float64(n) / float64(sims)
		}
		posByTeam[team] = shares
	}
	return out, posByTeam
}
