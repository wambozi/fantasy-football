package engine

import (
	"github.com/wambozi/draft-copilot/internal/league"
	"github.com/wambozi/draft-copilot/internal/players"
	"github.com/wambozi/draft-copilot/internal/state"
	"github.com/wambozi/draft-copilot/internal/strategy"
)

// rosterCounts tracks per-team positional counts (keepers included) and picks remaining.
// It is cloned per Monte Carlo sim so simulated picks update opponent needs as they go.
type rosterCounts struct {
	cfg    *strategy.Config
	counts map[string]map[players.Position]int
	left   map[string]int // live picks remaining for the team, including the current one
}

func newRosterCounts(lg *league.League, pool *players.Pool, cfg *strategy.Config, snap state.Snapshot) *rosterCounts {
	rc := &rosterCounts{cfg: cfg, counts: map[string]map[players.Position]int{}, left: map[string]int{}}
	for _, t := range lg.Teams {
		rc.counts[t] = map[players.Position]int{}
	}
	for team, ids := range snap.Rosters {
		for _, id := range ids {
			if p, ok := pool.ByID[id]; ok {
				rc.counts[team][p.Pos]++
			}
		}
	}
	for live := snap.LivePick; live <= lg.NumLive(); live++ {
		rc.left[lg.TeamOnClock(live)]++
	}
	return rc
}

func (rc *rosterCounts) clone() *rosterCounts {
	c := &rosterCounts{cfg: rc.cfg, counts: make(map[string]map[players.Position]int, len(rc.counts)), left: make(map[string]int, len(rc.left))}
	for t, m := range rc.counts {
		mm := make(map[players.Position]int, len(m))
		for p, n := range m {
			mm[p] = n
		}
		c.counts[t] = mm
	}
	for t, n := range rc.left {
		c.left[t] = n
	}
	return c
}

func (rc *rosterCounts) count(team string, pos players.Position) int { return rc.counts[team][pos] }

func (rc *rosterCounts) add(team string, pos players.Position) {
	rc.counts[team][pos]++
	rc.left[team]--
}

// starterOpen is how many required starter slots at pos the team has not filled.
func (rc *rosterCounts) starterOpen(team string, pos players.Position) int {
	n := rc.cfg.Roster.Starters[pos] - rc.count(team, pos)
	if n < 0 {
		return 0
	}
	return n
}

// flexOpen is how many flex slots the team has not yet covered by surplus RB/WR/TE.
func (rc *rosterCounts) flexOpen(team string) int {
	used := 0
	for _, pos := range rc.cfg.Roster.Flex.Eligible {
		if s := rc.count(team, pos) - rc.cfg.Roster.Starters[pos]; s > 0 {
			used += s
		}
	}
	n := rc.cfg.Roster.Flex.Count - used
	if n < 0 {
		return 0
	}
	return n
}

// openStarters is the total of unfilled starter slots (including flex) for the team.
func (rc *rosterCounts) openStarters(team string) int {
	n := rc.flexOpen(team)
	for pos := range rc.cfg.Roster.Starters {
		n += rc.starterOpen(team, pos)
	}
	return n
}

func (rc *rosterCounts) atMax(team string, pos players.Position) bool {
	m, ok := rc.cfg.Roster.Max[pos]
	return ok && rc.count(team, pos) >= m
}

// fillsStarter reports whether a pick at pos would fill an open starter or flex slot.
func (rc *rosterCounts) fillsStarter(team string, pos players.Position) bool {
	return rc.starterOpen(team, pos) > 0 || (rc.flexOpen(team) > 0 && isFlex(rc.cfg, pos))
}

// need is the opponent model's positional multiplier, derived from the team's actual
// roster (keepers included): a 0-keeper team is near-pure BPA, a team that kept two RBs
// leans WR. Modest by design — managers reach and go best-available.
func (rc *rosterCounts) need(team string, pos players.Position, round int) float64 {
	n := rc.cfg.Engine.Need
	if rc.atMax(team, pos) {
		return n.AtMax
	}
	var m float64
	switch {
	case rc.starterOpen(team, pos) > 0:
		m = n.StarterOpen
	case rc.flexOpen(team) > 0 && isFlex(rc.cfg, pos):
		m = n.FlexOpen
	default:
		m = n.Full
	}
	// When every remaining pick is needed for a starter, bench depth is off the table.
	if left := rc.left[team]; left > 0 && rc.openStarters(team) >= left && !rc.fillsStarter(team, pos) {
		m = n.AtMax
	}
	if pos == players.DST && round < n.DSTBeforeRound {
		m *= n.DSTEarlyMult
	}
	return m
}

// benchIndex is how many players at pos the team already holds beyond what its
// starter and flex slots can absorb — 0 for the first pure-bench player.
func (rc *rosterCounts) benchIndex(team string, pos players.Position) int {
	cap := rc.cfg.Roster.Starters[pos]
	if isFlex(rc.cfg, pos) {
		cap += rc.cfg.Roster.Flex.Count
	}
	if n := rc.count(team, pos) - cap; n > 0 {
		return n
	}
	return 0
}

// byPos groups a team's rostered players for display.
func byPos(snap state.Snapshot, team string, pool *players.Pool) map[players.Position][]*players.Player {
	out := map[players.Position][]*players.Player{}
	for _, id := range snap.Rosters[team] {
		if p, ok := pool.ByID[id]; ok {
			out[p.Pos] = append(out[p.Pos], p)
		}
	}
	return out
}
