// Package engine turns a draft snapshot into a recommendation.
//
// Pipeline on every state change:
//  1. board       — available, draftable players
//  2. replacement — dynamic per-position replacement level from remaining starter demand
//  3. survival    — Monte Carlo over the opposing picks until my next turn
//  4. score       — VOR + λ·regret (+ small variance bonus)
//  5. gates       — hard roster/strategy constraints from strategy.yaml
//
// Results are cached against the state version; Advise is safe to call from any
// goroutine and never touches the network.
package engine

import (
	"fmt"
	"math"
	"sort"
	"strings"
	"sync"

	"github.com/wambozi/draft-copilot/internal/league"
	"github.com/wambozi/draft-copilot/internal/players"
	"github.com/wambozi/draft-copilot/internal/state"
	"github.com/wambozi/draft-copilot/internal/strategy"
)

// Recommendation is one ranked candidate.
type Recommendation struct {
	Player     *players.Player `json:"player"`
	Score      float64         `json:"score"`
	VOR        float64         `json:"vor"`
	PSurvive   float64         `json:"p_survive"`
	Regret     float64         `json:"regret"`
	Reasons    []string        `json:"reasons"`
	GateForced bool            `json:"gate_forced"`
}

// Advice is the output contract for the UI and the Claude brief.
type Advice struct {
	Version      int                                    `json:"version"`
	OnClock      bool                                   `json:"on_clock"`
	LivePick     int                                    `json:"live_pick"`
	Round        int                                    `json:"round"`
	NextLivePick int                                    `json:"next_live_pick"` // 0 when I have no picks left
	PicksUntil   int                                    `json:"picks_until"`    // opposing picks before my next turn
	Top          []Recommendation                       `json:"top"`
	ByPosition   map[players.Position]Recommendation    `json:"by_position"`
	Warnings     []string                               `json:"warnings"`
	GateBand     string                                 `json:"gate_band,omitempty"` // red band text, e.g. "QB GATE: 1 pick left"
	Replacement  map[players.Position]float64           `json:"replacement"`
	Waiver       map[players.Position]float64           `json:"waiver"` // best available beyond the last draft slot
	Demand       map[players.Position]float64           `json:"demand"`
	ProjMode     string                                 `json:"proj_mode"` // "projections" | "adp-only"
	MyRoster     map[players.Position][]*players.Player `json:"my_roster"`
	Candidates   []Recommendation                       `json:"candidates"` // top N by score, unfiltered, for the brief
	Params       struct {
		Sims         int
		LambdaRegret float64
	} `json:"params"`
}

// Engine is safe for concurrent use.
type Engine struct {
	lg   *league.League
	pool *players.Pool
	cfg  *strategy.Config
	seed uint64

	mu      sync.Mutex
	cacheV  int
	cacheAd *Advice
}

// New builds an engine. seed makes Monte Carlo deterministic for tests/sims.
func New(lg *league.League, pool *players.Pool, cfg *strategy.Config, seed uint64) *Engine {
	return &Engine{lg: lg, pool: pool, cfg: cfg, seed: seed}
}

// Advise returns the cached advice for snap.Version or computes it.
func (e *Engine) Advise(snap state.Snapshot) *Advice {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.cacheAd != nil && e.cacheV == snap.Version {
		return e.cacheAd
	}
	ad := e.compute(snap)
	e.cacheV, e.cacheAd = snap.Version, ad
	return ad
}

// AdviseFor computes advice for an arbitrary team's seat (used by the simulator for
// the engine-driven seat and by the brief prefetch for a projected state). Not cached.
func (e *Engine) AdviseFor(snap state.Snapshot, team string) *Advice {
	return e.computeFor(snap, team)
}

func (e *Engine) compute(snap state.Snapshot) *Advice { return e.computeFor(snap, e.lg.MyTeam) }

func (e *Engine) computeFor(snap state.Snapshot, me string) *Advice {
	ad := &Advice{Version: snap.Version, LivePick: snap.LivePick, ByPosition: map[players.Position]Recommendation{}}
	if snap.Complete {
		ad.Warnings = append(ad.Warnings, "draft complete")
		return ad
	}
	slot, _ := e.lg.SlotForLive(snap.LivePick)
	ad.Round = slot.Round
	ad.OnClock = slot.Team == me

	// My next turn and the opposing picks between now and then.
	from := snap.LivePick
	if ad.OnClock {
		from = snap.LivePick + 1
	}
	ad.NextLivePick = e.lg.NextLivePickFor(me, from)
	if ad.NextLivePick > 0 {
		ad.PicksUntil = ad.NextLivePick - snap.LivePick
		if ad.OnClock {
			ad.PicksUntil--
		}
	}

	board := e.board(snap)
	if len(board) == 0 {
		ad.Warnings = append(ad.Warnings, "no available players")
		return ad
	}
	switch {
	case !hasProjections(board):
		ad.ProjMode = players.ProjNone
		ad.Warnings = append(ad.Warnings, "NO PROJECTIONS LOADED — ranking by ADP only, VOR is 0")
	case e.pool.ProjMode == players.ProjCurve:
		ad.ProjMode = players.ProjCurve
		ad.Warnings = append(ad.Warnings, "VOR from fitted ADP curve, not real projections")
	case e.pool.ProjMode == players.ProjFinish:
		ad.ProjMode = players.ProjFinish
		ad.Warnings = append(ad.Warnings, "VOR from ECR × 2025 finish curve")
	default:
		ad.ProjMode = players.ProjReal
	}

	rc := newRosterCounts(e.lg, e.pool, e.cfg, snap)
	ad.MyRoster = byPos(snap, me, e.pool)
	ad.Demand, ad.Replacement = e.replacement(board, rc)

	// Survival over the opposing picks before my next turn.
	surv := e.Survival(board, snap, rc, ad.NextLivePick, me)

	// Score every candidate.
	ad.Waiver = e.waiverLevel(board)
	// Two baselines. A pick that fills one of MY starter/flex slots is measured against
	// the league's dynamic replacement level (what the last team to fill that slot gets).
	// A pick that can only sit on my bench is measured against the waiver wire — the
	// best player nobody will draft — scaled by how likely that bench spot ever starts.
	// Without the split, every mid-round candidate is negative against replacement and
	// a backup QB's small positive VOR wins by default.
	vor := func(p *players.Player) float64 {
		if rc.fillsStarter(me, p.Pos) {
			return p.ProjPoints - ad.Replacement[p.Pos]
		}
		f := e.cfg.Engine.BenchFactor(p.Pos) * math.Pow(e.cfg.Engine.BenchDecay, float64(rc.benchIndex(me, p.Pos)))
		return (p.ProjPoints - ad.Waiver[p.Pos]) * f
	}
	recs := make([]Recommendation, 0, len(board))
	for _, p := range board {
		recs = append(recs, Recommendation{Player: p, VOR: vor(p), PSurvive: surv[p.ID]})
	}
	e.score(recs, ad)

	// Hard constraints for my seat.
	g := e.gates(rc, me, snap.LivePick, ad)
	var eligible []Recommendation
	for _, r := range recs {
		if g.allowed[r.Player.Pos] {
			eligible = append(eligible, r)
		}
	}
	if len(eligible) == 0 { // should not happen; never leave the user with nothing
		eligible = recs
		ad.Warnings = append(ad.Warnings, "gates excluded every position; showing unfiltered board")
	}
	sortByScore(eligible)
	for _, r := range eligible {
		if _, ok := ad.ByPosition[r.Player.Pos]; !ok {
			ad.ByPosition[r.Player.Pos] = r
		}
	}
	if n := 8; len(eligible) > n {
		ad.Candidates = eligible[:n]
	} else {
		ad.Candidates = eligible
	}

	top := eligible
	if g.forced != "" {
		var forced []Recommendation
		for _, r := range eligible {
			if r.Player.Pos == g.forced {
				r.GateForced = true
				forced = append(forced, r)
			}
		}
		if len(forced) > 0 {
			top = forced
		}
	}
	if len(top) > 3 {
		top = top[:3]
	}
	for i := range top {
		top[i].Reasons = e.reasons(top[i], ad, rc, me)
	}
	ad.Top = top
	ad.Params.Sims, ad.Params.LambdaRegret = e.cfg.Engine.Sims, e.cfg.Engine.LambdaRegret
	return ad
}

// waiverLevel is, per position, what a bench spot is worth relative to free: the
// projection of the k-th best available player at the position, where k is how many
// at that position are still expected to be drafted (available players with ADP inside
// the draft). Ranking by projection rather than ADP keeps the baseline on a real
// projection instead of on a deep player valued by the fallback curve.
func (e *Engine) waiverLevel(board []*players.Player) map[players.Position]float64 {
	limit := float64(len(e.lg.Slots))
	k := map[players.Position]int{}
	proj := map[players.Position][]float64{}
	for _, p := range board {
		if p.ADPMean > 0 && p.ADPMean <= limit {
			k[p.Pos]++
		}
		proj[p.Pos] = append(proj[p.Pos], p.ProjPoints)
	}
	out := map[players.Position]float64{}
	for pos, pts := range proj {
		sortDesc(pts)
		i := k[pos]
		if i >= len(pts) {
			i = len(pts) - 1
		}
		out[pos] = pts[i]
	}
	return out
}

// board returns available draftable players sorted by ADPMean.
func (e *Engine) board(snap state.Snapshot) []*players.Player {
	out := make([]*players.Player, 0, len(e.pool.Players))
	for _, p := range e.pool.Players {
		if !p.Pos.Draftable() { // belt and braces: kickers never reach the board
			continue
		}
		if _, gone := snap.Taken[p.ID]; gone {
			continue
		}
		out = append(out, p)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].ADPMean < out[j].ADPMean })
	return out
}

func hasProjections(board []*players.Player) bool {
	for _, p := range board {
		if p.ProjPoints > 0 {
			return true
		}
	}
	return false
}

// score fills Score and Regret.
//
//	fallback(pos) = best VOR at pos among OTHER players likely (> threshold) to survive
//	regret(p)     = (1 - p_survive) × max(0, VOR(p) - fallback)
//	score(p)      = VOR + λ_regret × regret + variance_preference × σ_adp
//
// Regret is what I lose by passing on p now and settling for the fallback at my next
// pick, weighted by how likely that loss is to materialise. The variance term is a
// small ceiling tiebreak: ADP σ proxies role uncertainty, and with 8/12 teams in a
// one-week bracket, upside beats floor on close calls.
func (e *Engine) score(recs []Recommendation, ad *Advice) {
	th := e.cfg.Engine.SurviveThreshold
	// best and second-best surviving VOR per position, so p can be excluded from its own fallback.
	type pair struct {
		best, second float64
		bestID       string
	}
	fb := map[players.Position]pair{}
	for _, r := range recs {
		if r.PSurvive <= th {
			continue
		}
		q := fb[r.Player.Pos]
		if _, ok := fb[r.Player.Pos]; !ok {
			q = pair{best: math.Inf(-1), second: math.Inf(-1)}
		}
		if r.VOR > q.best {
			q.second, q.best, q.bestID = q.best, r.VOR, r.Player.ID
		} else if r.VOR > q.second {
			q.second = r.VOR
		}
		fb[r.Player.Pos] = q
	}
	vp := e.cfg.Objective.VariancePreference
	for i := range recs {
		r := &recs[i]
		fallback := 0.0 // nothing survives → replacement level (VOR 0)
		if q, ok := fb[r.Player.Pos]; ok {
			fallback = q.best
			if q.bestID == r.Player.ID {
				fallback = q.second
			}
			if math.IsInf(fallback, -1) {
				fallback = 0
			}
		}
		r.Regret = (1 - r.PSurvive) * math.Max(0, r.VOR-fallback)
		r.Score = r.VOR + e.cfg.Engine.LambdaRegret*r.Regret + vp*math.Min(r.Player.ADPStdDev, 30)
		if ad.ProjMode == players.ProjNone {
			// No points: fall back to ADP order with survival as the tiebreak so the
			// tool is still usable. Flagged loudly in Warnings.
			r.Score = -r.Player.ADPMean - 10*r.PSurvive
		}
	}
}

func sortByScore(rs []Recommendation) {
	sort.SliceStable(rs, func(i, j int) bool {
		if rs[i].Score != rs[j].Score {
			return rs[i].Score > rs[j].Score
		}
		return rs[i].Player.ADPMean < rs[j].Player.ADPMean
	})
}

func (e *Engine) reasons(r Recommendation, ad *Advice, rc *rosterCounts, me string) []string {
	var out []string
	if ad.NextLivePick > 0 && ad.PicksUntil > 0 {
		out = append(out, fmt.Sprintf("%d%% gone by #%d", int(math.Round((1-r.PSurvive)*100)), ad.NextLivePick))
	}
	pos := r.Player.Pos
	have := rc.count(me, pos)
	need := e.cfg.Roster.Starters[pos]
	switch {
	case have < need:
		out = append(out, fmt.Sprintf("fills %s%d", pos, have+1))
	case rc.flexOpen(me) > 0 && isFlex(e.cfg, pos):
		out = append(out, "flex")
	default:
		out = append(out, "bench depth")
	}
	if r.GateForced {
		out = append(out, "gate")
	} else if r.Regret > 0 && ad.ProjMode != players.ProjNone {
		out = append(out, fmt.Sprintf("regret +%.1f", r.Regret))
	}
	if l := r.Player.Last; l != nil && l.Games > 0 {
		s := fmt.Sprintf("%d: %.0f pts/%dg", l.Season, l.Points, l.Games)
		if l.TgtPct > 0 {
			s += fmt.Sprintf(" · %.0f%% tgt", l.TgtPct)
		}
		out = append(out, s)
	}
	// Keeper zone (round >= cost floor): the 2027 view matters, so show age and
	// dynasty/rookie standing. Before the floor it is noise on a 60-second clock.
	if p := r.Player; ad.Round >= 8 && (p.Age > 0 || p.RookieRank > 0) {
		var bits []string
		if p.Age > 0 {
			bits = append(bits, fmt.Sprintf("age %d", p.Age))
		}
		if p.RookieRank > 0 {
			bits = append(bits, fmt.Sprintf("rookie #%d", p.RookieRank))
		}
		if p.DynastyRank > 0 {
			bits = append(bits, fmt.Sprintf("dyn #%d", p.DynastyRank))
		}
		out = append(out, strings.Join(bits, " "))
	}
	// ECR vs ADP gap: experts rank them well ahead of where the room drafts them (or
	// the reverse). No modelling — the gap itself is the signal.
	if p := r.Player; p.ECR > 0 && p.ADPMean > 0 {
		switch gap := p.ADPMean - p.ECR; {
		case gap >= 8:
			out = append(out, fmt.Sprintf("ECR %.0f vs ADP %.0f — room undervalues", p.ECR, p.ADPMean))
		case gap <= -8:
			out = append(out, fmt.Sprintf("ECR %.0f vs ADP %.0f — reach", p.ECR, p.ADPMean))
		}
	}
	return out
}

func isFlex(cfg *strategy.Config, pos players.Position) bool {
	for _, p := range cfg.Roster.Flex.Eligible {
		if p == pos {
			return true
		}
	}
	return false
}
