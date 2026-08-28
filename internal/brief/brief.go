// Package brief renders the two-to-three-bullet Claude commentary under the top-3.
//
// Role: colour, never the critical path (spec §7). The engine is authoritative; the
// brief explains the shape of the decision. Everything here is asynchronous and
// failure-tolerant: a missing, slow or failed brief means the UI shows engine reasons
// only. Nothing blocks on the network.
//
// Speculative prefetch: when a pick lands within `window` picks of my turn, a brief is
// generated for the *projected* board at my turn (best-available-by-ADP removed for
// each intervening pick). When I actually reach the clock an exact brief is generated
// and replaces the projected one; until it lands the projected text is shown, marked.
package brief

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/wambozi/draft-copilot/internal/engine"
	"github.com/wambozi/draft-copilot/internal/httpapi"
	"github.com/wambozi/draft-copilot/internal/league"
	"github.com/wambozi/draft-copilot/internal/players"
	"github.com/wambozi/draft-copilot/internal/state"
	"github.com/wambozi/draft-copilot/internal/strategy"
)

// Generator produces brief text for a system prompt and a user message. Implemented by
// the Anthropic client; faked in tests.
type Generator interface {
	Generate(ctx context.Context, system, user string) (string, error)
}

// Service implements httpapi.Briefer.
type Service struct {
	gen    Generator
	eng    *engine.Engine
	lg     *league.League
	pool   *players.Pool
	cfg    *strategy.Config
	log    *slog.Logger
	poke   func()
	window int
	system string
	tmo    time.Duration

	mu    sync.Mutex
	cache map[int]httpapi.Brief // keyed by my live pick
	gen_  map[string]bool       // in-flight keys "live:version:projected"
}

// Options tune the service.
type Options struct {
	Window  int           // prefetch when within this many picks of my turn (default 4)
	Timeout time.Duration // per-generation timeout (default 25s)
	Poke    func()        // called when a brief lands so SSE clients refresh
	Log     *slog.Logger
}

// New builds a service. gen may not be nil.
func New(gen Generator, eng *engine.Engine, lg *league.League, pool *players.Pool, cfg *strategy.Config, o Options) *Service {
	s := &Service{gen: gen, eng: eng, lg: lg, pool: pool, cfg: cfg, log: o.Log, poke: o.Poke, window: o.Window, tmo: o.Timeout,
		cache: map[int]httpapi.Brief{}, gen_: map[string]bool{}}
	if s.log == nil {
		s.log = slog.Default()
	}
	if s.window == 0 {
		s.window = 4
	}
	if s.tmo == 0 {
		s.tmo = 25 * time.Second
	}
	if s.poke == nil {
		s.poke = func() {}
	}
	s.system = SystemPrompt(lg, cfg)
	return s
}

// Brief returns the brief for the live pick I am on (or next up for).
func (s *Service) Brief(snap state.Snapshot) (httpapi.Brief, bool) {
	ad := s.eng.Advise(snap)
	live := snap.LivePick
	if !ad.OnClock {
		live = ad.NextLivePick
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	b, ok := s.cache[live]
	return b, ok
}

// OnPick schedules generation. Safe to call from any goroutine; returns immediately.
func (s *Service) OnPick(snap state.Snapshot) {
	if snap.Complete {
		return
	}
	ad := s.eng.Advise(snap)
	switch {
	case ad.OnClock:
		s.spawn(snap.LivePick, snap, false)
	case ad.NextLivePick > 0 && ad.PicksUntil <= s.window:
		proj := s.project(snap, ad.NextLivePick)
		s.spawn(ad.NextLivePick, proj, true)
	}
}

func (s *Service) spawn(live int, snap state.Snapshot, projected bool) {
	key := fmt.Sprintf("%d:%d:%v", live, snap.Version, projected)
	s.mu.Lock()
	if s.gen_[key] {
		s.mu.Unlock()
		return
	}
	if cur, ok := s.cache[live]; ok && !cur.Projected && !projected && cur.Version == snap.Version {
		s.mu.Unlock()
		return // exact brief for this exact state already exists
	}
	s.gen_[key] = true
	s.mu.Unlock()

	go func() {
		defer func() {
			s.mu.Lock()
			delete(s.gen_, key)
			s.mu.Unlock()
		}()
		ad := s.eng.AdviseFor(snap, s.lg.MyTeam)
		user := UserMessage(snap, ad, s.lg, s.pool, projected)
		ctx, cancel := context.WithTimeout(context.Background(), s.tmo)
		defer cancel()
		t0 := time.Now()
		text, err := s.gen.Generate(ctx, s.system, user)
		if err != nil {
			s.log.Warn("brief failed", "live", live, "projected", projected, "err", err)
			return
		}
		text = strings.TrimSpace(text)
		if text == "" {
			return
		}
		s.mu.Lock()
		cur, ok := s.cache[live]
		// Never let a projected brief replace an exact one, or an older exact one replace a newer.
		if ok && (!cur.Projected && projected || (!cur.Projected && !projected && cur.Version > snap.Version)) {
			s.mu.Unlock()
			return
		}
		s.cache[live] = httpapi.Brief{Text: text, LivePick: live, Projected: projected, Version: snap.Version}
		s.mu.Unlock()
		s.log.Info("brief ready", "live", live, "projected", projected, "ms", time.Since(t0).Milliseconds())
		s.poke()
	}()
}

// project removes the best-available-by-ADP player for each opposing pick between
// now and `live`, assigning them to the teams on the clock so positional need still
// reads correctly. Deterministic; a coarse but honest guess at the board at my turn.
func (s *Service) project(snap state.Snapshot, live int) state.Snapshot {
	out := snap
	out.Taken = make(map[string]string, len(snap.Taken)+8)
	for k, v := range snap.Taken {
		out.Taken[k] = v
	}
	out.Rosters = make(map[string][]string, len(snap.Rosters))
	for k, v := range snap.Rosters {
		out.Rosters[k] = append([]string(nil), v...)
	}
	var board []*players.Player
	for _, p := range s.pool.Players {
		if _, gone := snap.Taken[p.ID]; gone || !p.Pos.Draftable() || p.Pos == players.DST || p.ADPMean <= 0 {
			continue
		}
		board = append(board, p)
	}
	sort.SliceStable(board, func(i, j int) bool { return board[i].ADPMean < board[j].ADPMean })
	i := 0
	for lp := snap.LivePick; lp < live && i < len(board); lp++ {
		team := s.lg.TeamOnClock(lp)
		p := board[i]
		i++
		out.Taken[p.ID] = team
		out.Rosters[team] = append(out.Rosters[team], p.ID)
		out.Picks = append(out.Picks, state.Pick{LivePick: lp, Team: team, PlayerID: p.ID, Source: "projected"})
	}
	out.LivePick = live
	out.OnClock = s.lg.MyTeam
	return out
}

// SystemPrompt is the cached, state-independent half of every request.
func SystemPrompt(lg *league.League, cfg *strategy.Config) string {
	var b strings.Builder
	fmt.Fprintf(&b, "You are the draft assistant for one manager (%q) in a %d-team head-to-head keeper league, full PPR, snake draft, %d rounds.\n", lg.MyTeam, len(lg.Teams), lg.Rounds())
	b.WriteString("Roster: ")
	poss := []players.Position{players.QB, players.RB, players.WR, players.TE, players.DST}
	for _, pos := range poss {
		fmt.Fprintf(&b, "%s×%d ", pos, cfg.Roster.Starters[pos])
	}
	fmt.Fprintf(&b, "FLEX×%d (%s), bench %d, no kicker. Position maxes: ", cfg.Roster.Flex.Count, joinPos(cfg.Roster.Flex.Eligible), cfg.Roster.Bench)
	for _, pos := range poss {
		fmt.Fprintf(&b, "%s %d ", pos, cfg.Roster.Max[pos])
	}
	b.WriteString(".\n")
	fmt.Fprintf(&b, "Keepers: %d slots are kept before pick one (%d live picks remain), which distorts the board — several elite RBs and TEs are gone before the draft starts. ", len(lg.Slots)-lg.NumLive(), lg.NumLive())
	fmt.Fprintf(&b, "Keeper economics: a year-1 keeper costs the round he was drafted in, floored at round %d, so rounds %d+ all have the same keeper cost; from round %d a young player with a real 2027 outlook is worth extra. Up to %d keepers.\n", cfg.Keeper.CostFloorRound, cfg.Keeper.CostFloorRound, cfg.Keeper.CostFloorRound, cfg.Keeper.MaxKeepers)
	b.WriteString("Objective: 8 of 12 teams make a single-elimination bracket, so weekly ceiling matters slightly more than floor.\n")
	if len(cfg.Gates) > 0 {
		b.WriteString("Strategy gates from the manager's plan:\n")
		for _, g := range cfg.Gates {
			fmt.Fprintf(&b, "- %s: ", g.Position)
			if g.MustDraftByLivePick > 0 {
				fmt.Fprintf(&b, "must have one by live pick %d; ", g.MustDraftByLivePick)
			}
			for d, n := range g.MinCountByLivePick {
				fmt.Fprintf(&b, "at least %d by live pick %d; ", n, d)
			}
			if g.Exactly > 0 {
				fmt.Fprintf(&b, "exactly %d; ", g.Exactly)
			}
			if g.NotBeforeRound > 0 {
				fmt.Fprintf(&b, "not before round %d; ", g.NotBeforeRound)
			}
			if g.Max > 0 {
				fmt.Fprintf(&b, "max %d; ", g.Max)
			}
			if g.Rationale != "" {
				fmt.Fprintf(&b, "(%s)", g.Rationale)
			}
			b.WriteString("\n")
		}
	}
	b.WriteString(`
A deterministic engine has ALREADY ranked the candidates (VOR = projected points over replacement; p_survive = chance the player is still there at the manager's next pick; regret = expected loss from waiting). Do not re-rank and do not invent numbers.

Your job: explain the SHAPE of this decision in at most 3 bullets, each under 15 words, no preamble, no headings. Prefer:
- what the numbers miss: bye-week collisions with the manager's roster, handcuff logic, positional runs visible in the recent picks, a player the room is drafting far from expert consensus
- which of the top candidates is the safe vs the upside choice, and why
- when a strategy gate is about to bind
If a brief is marked PROJECTED, the board is a prediction of the next few picks; say nothing about which specific players "just went".
Plain text bullets starting with "- ". Nothing else.`)
	return b.String()
}

func joinPos(ps []players.Position) string {
	ss := make([]string, len(ps))
	for i, p := range ps {
		ss[i] = string(p)
	}
	return strings.Join(ss, "/")
}

// UserMessage is the per-state JSON the model sees.
func UserMessage(snap state.Snapshot, ad *engine.Advice, lg *league.League, pool *players.Pool, projected bool) string {
	type cand struct {
		Name     string   `json:"name"`
		Pos      string   `json:"pos"`
		Team     string   `json:"team"`
		Bye      int      `json:"bye"`
		Proj     float64  `json:"proj"`
		VOR      float64  `json:"vor"`
		PSurvive float64  `json:"p_survive"`
		ADP      float64  `json:"adp"`
		ECR      float64  `json:"ecr,omitempty"`
		Age      int      `json:"age,omitempty"`
		Keeper   string   `json:"keeper_note,omitempty"`
		Reasons  []string `json:"reasons"`
	}
	mk := func(r engine.Recommendation) cand {
		p := r.Player
		c := cand{Name: p.Name, Pos: string(p.Pos), Team: p.Team, Bye: p.Bye, Proj: round1(p.ProjPoints), VOR: round1(r.VOR), PSurvive: round2(r.PSurvive), ADP: round1(p.ADPMean), ECR: p.ECR, Age: p.Age, Reasons: r.Reasons}
		if r.KeeperSpec {
			c.Keeper = fmt.Sprintf("2027 keeper asset at %s cost (P=%.0f%%)", r.KeeperCost, r.KeeperP*100)
		}
		return c
	}
	roster := map[string][]string{}
	for pos, ps := range ad.MyRoster {
		for _, p := range ps {
			roster[string(pos)] = append(roster[string(pos)], fmt.Sprintf("%s (bye %d)", p.Name, p.Bye))
		}
	}
	var recent []string
	picks := snap.Picks
	if len(picks) > 8 {
		picks = picks[len(picks)-8:]
	}
	for _, pk := range picks {
		if p, ok := pool.ByID[pk.PlayerID]; ok {
			recent = append(recent, fmt.Sprintf("#%d %s: %s %s", pk.LivePick, pk.Team, p.Name, p.Pos))
		}
	}
	top := make([]cand, 0, 3)
	for _, r := range ad.Top {
		top = append(top, mk(r))
	}
	others := make([]cand, 0, len(ad.Candidates))
	for _, r := range ad.Candidates {
		others = append(others, mk(r))
	}
	best := map[string]string{}
	for pos, r := range ad.ByPosition {
		best[string(pos)] = fmt.Sprintf("%s (VOR %.0f, %.0f%% survives)", r.Player.Name, r.VOR, r.PSurvive*100)
	}
	msg := map[string]any{
		"projected_board":    projected,
		"live_pick":          ad.LivePick,
		"round":              ad.Round,
		"on_clock":           ad.OnClock,
		"next_live_pick":     ad.NextLivePick,
		"picks_until_next":   ad.PicksUntil,
		"my_roster":          roster,
		"top3":               top,
		"other_candidates":   others,
		"best_by_position":   best,
		"gate_band":          ad.GateBand,
		"warnings":           ad.Warnings,
		"remaining_demand":   ad.Demand,
		"replacement_level":  ad.Replacement,
		"recent_picks":       recent,
		"keeper_speculative": ad.SpecCount,
	}
	b, _ := json.Marshal(msg)
	return string(b)
}

func round1(v float64) float64 { return float64(int(v*10+0.5)) / 10 }
func round2(v float64) float64 { return float64(int(v*100+0.5)) / 100 }
