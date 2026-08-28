// Package league holds the immutable league configuration: teams, the draft slot order,
// roster shape, and — derived, never hardcoded — the live-pick sequence.
package league

import (
	"encoding/csv"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"

	"github.com/wambozi/draft-copilot/internal/players"
)

// DraftSlot is one of the 204 (17×12) board positions.
type DraftSlot struct {
	Round       int    `json:"round"`
	PickInRound int    `json:"pick_in_round"`
	Overall     int    `json:"overall"` // 1-indexed board position
	Team        string `json:"team"`
	KeeperID    string `json:"keeper_id,omitempty"` // "" if live pick
	// KeeperName/Team/Pos are carried from the draft-order file so a keeper missing from
	// the ADP pool can still be represented.
	KeeperName string           `json:"keeper_name,omitempty"`
	KeeperTeam string           `json:"keeper_team,omitempty"`
	KeeperPos  players.Position `json:"keeper_pos,omitempty"`
	// LivePick is the 1-indexed live pick number, 0 for keeper slots.
	LivePick int `json:"live_pick"`
}

// Roster shape. Position maxes exist so the engine never over-drafts a position
// into a roster it cannot legally hold.
type Roster struct {
	Starters   map[players.Position]int
	FlexSlots  int
	FlexElig   []players.Position
	Bench      int
	TotalSpots int
	PosMax     map[players.Position]int
}

// League is the immutable configuration.
type League struct {
	Name   string
	Teams  []string
	MyTeam string
	Slots  []DraftSlot
	Roster Roster

	// LiveSlots maps live pick number (1-indexed) -> index into Slots.
	LiveSlots []int
	// MyLivePicks is the ascending list of live pick numbers where MyTeam is on the clock.
	MyLivePicks []int
	// Keepers per team, as slot indices.
	KeepersByTeam map[string][]int
}

// DefaultRoster is the confirmed §1.1 configuration.
func DefaultRoster() Roster {
	return Roster{
		Starters:   map[players.Position]int{players.QB: 1, players.RB: 2, players.WR: 2, players.TE: 1, players.DST: 1},
		FlexSlots:  2,
		FlexElig:   []players.Position{players.RB, players.WR, players.TE},
		Bench:      8,
		TotalSpots: 17,
		PosMax:     map[players.Position]int{players.QB: 4, players.RB: 9, players.WR: 8, players.TE: 3, players.DST: 3},
	}
}

// Load parses the draft-order CSV and derives the live-pick sequence.
func Load(path, myTeam string, roster Roster) (*League, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open draft order: %w", err)
	}
	defer f.Close()
	slots, err := ParseDraftOrder(f)
	if err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	return New(slots, myTeam, roster)
}

// New builds a League from slots. It validates that the board is a contiguous
// round/pick grid and that MyTeam appears in it.
func New(slots []DraftSlot, myTeam string, roster Roster) (*League, error) {
	if len(slots) == 0 {
		return nil, fmt.Errorf("no draft slots")
	}
	lg := &League{MyTeam: myTeam, Roster: roster, KeepersByTeam: map[string][]int{}}
	seen := map[string]bool{}
	live := 0
	for i := range slots {
		s := &slots[i]
		s.Overall = i + 1
		if !seen[s.Team] {
			seen[s.Team] = true
			lg.Teams = append(lg.Teams, s.Team)
		}
		if s.KeeperID != "" {
			s.LivePick = 0
			lg.KeepersByTeam[s.Team] = append(lg.KeepersByTeam[s.Team], i)
			continue
		}
		live++
		s.LivePick = live
		lg.LiveSlots = append(lg.LiveSlots, i)
		if s.Team == myTeam {
			lg.MyLivePicks = append(lg.MyLivePicks, live)
		}
	}
	lg.Slots = slots
	if !seen[myTeam] {
		return nil, fmt.Errorf("team %q not found in draft order (teams: %s)", myTeam, strings.Join(lg.Teams, "; "))
	}
	if err := lg.validate(); err != nil {
		return nil, err
	}
	return lg, nil
}

func (lg *League) validate() error {
	teams := len(lg.Teams)
	if teams == 0 {
		return fmt.Errorf("no teams")
	}
	rounds := len(lg.Slots) / teams
	if rounds*teams != len(lg.Slots) {
		return fmt.Errorf("%d slots is not a multiple of %d teams", len(lg.Slots), teams)
	}
	// Every team must own exactly TotalSpots slots (keepers + live); otherwise a trade
	// or export glitch has produced a roster that cannot be filled exactly.
	owned := map[string]int{}
	for _, s := range lg.Slots {
		owned[s.Team]++
	}
	for _, t := range lg.Teams {
		if owned[t] != lg.Roster.TotalSpots {
			return fmt.Errorf("team %q owns %d slots, roster has %d spots", t, owned[t], lg.Roster.TotalSpots)
		}
	}
	for i := 1; i < len(lg.Slots); i++ {
		a, b := lg.Slots[i-1], lg.Slots[i]
		ok := (b.Round == a.Round && b.PickInRound == a.PickInRound+1) || (b.Round == a.Round+1 && b.PickInRound == 1)
		if !ok {
			return fmt.Errorf("slot order breaks at %d.%d -> %d.%d", a.Round, a.PickInRound, b.Round, b.PickInRound)
		}
	}
	return nil
}

// NumLive is the number of live picks in the draft.
func (lg *League) NumLive() int { return len(lg.LiveSlots) }

// Rounds is the number of draft rounds.
func (lg *League) Rounds() int { return len(lg.Slots) / len(lg.Teams) }

// SlotForLive returns the slot for a 1-indexed live pick; ok=false past the end.
func (lg *League) SlotForLive(live int) (DraftSlot, bool) {
	if live < 1 || live > len(lg.LiveSlots) {
		return DraftSlot{}, false
	}
	return lg.Slots[lg.LiveSlots[live-1]], true
}

// TeamOnClock returns the team owning live pick n.
func (lg *League) TeamOnClock(live int) string {
	s, ok := lg.SlotForLive(live)
	if !ok {
		return ""
	}
	return s.Team
}

// NextLivePickFor returns the first live pick >= from that belongs to team, or 0.
func (lg *League) NextLivePickFor(team string, from int) int {
	for i := from; i <= len(lg.LiveSlots); i++ {
		if lg.Slots[lg.LiveSlots[i-1]].Team == team {
			return i
		}
	}
	return 0
}

// ParseDraftOrder reads a Round/Pick,Team,Player[,...] CSV. Columns are located by
// header name so extra ESPN export columns (Amount, Player Team, Player Position) are fine.
func ParseDraftOrder(r io.Reader) ([]DraftSlot, error) {
	cr := csv.NewReader(r)
	cr.FieldsPerRecord = -1
	cr.LazyQuotes = true
	header, err := cr.Read()
	if err != nil {
		return nil, fmt.Errorf("read header: %w", err)
	}
	col := map[string]int{}
	for i, h := range header {
		col[strings.ToLower(strings.TrimSpace(h))] = i
	}
	need := func(names ...string) (int, error) {
		for _, n := range names {
			if i, ok := col[n]; ok {
				return i, nil
			}
		}
		return -1, fmt.Errorf("missing column %q in header %v", names[0], header)
	}
	rpCol, err := need("round/pick", "pick", "slot")
	if err != nil {
		return nil, err
	}
	teamCol, err := need("team", "owner", "franchise")
	if err != nil {
		return nil, err
	}
	playerCol, err := need("player", "keeper")
	if err != nil {
		return nil, err
	}
	ptCol, _ := col["player team"]
	ppCol, _ := col["player position"]
	hasPT := colHas(col, "player team")
	hasPP := colHas(col, "player position")

	var slots []DraftSlot
	line := 1
	for {
		rec, err := cr.Read()
		if err == io.EOF {
			break
		}
		line++
		if err != nil {
			return nil, fmt.Errorf("line %d: %w", line, err)
		}
		if len(rec) <= rpCol || strings.TrimSpace(rec[rpCol]) == "" {
			continue
		}
		round, pick, err := parseRoundPick(rec[rpCol])
		if err != nil {
			return nil, fmt.Errorf("line %d: %w", line, err)
		}
		s := DraftSlot{Round: round, PickInRound: pick, Team: strings.TrimSpace(field(rec, teamCol))}
		if s.Team == "" {
			return nil, fmt.Errorf("line %d: empty team", line)
		}
		if name := strings.TrimSpace(field(rec, playerCol)); name != "" {
			s.KeeperName = players.NormalizeName(name)
			if hasPT {
				s.KeeperTeam = players.NormTeam(field(rec, ptCol))
			}
			if hasPP {
				s.KeeperPos = players.NormPos(field(rec, ppCol))
			}
			s.KeeperID = players.Slug(name, s.KeeperTeam)
		}
		slots = append(slots, s)
	}
	return slots, nil
}

func colHas(col map[string]int, name string) bool { _, ok := col[name]; return ok }

func field(rec []string, i int) string {
	if i < 0 || i >= len(rec) {
		return ""
	}
	return rec[i]
}

// parseRoundPick handles "1.10" (round 1, pick 10). It must NOT be parsed as a float:
// "1.1" and "1.10" are different slots.
func parseRoundPick(s string) (int, int, error) {
	s = strings.TrimSpace(s)
	parts := strings.FieldsFunc(s, func(r rune) bool { return r == '.' || r == '-' || r == '/' })
	if len(parts) != 2 {
		return 0, 0, fmt.Errorf("bad round/pick %q", s)
	}
	r, err := strconv.Atoi(parts[0])
	if err != nil || r < 1 {
		return 0, 0, fmt.Errorf("bad round in %q", s)
	}
	p, err := strconv.Atoi(parts[1])
	if err != nil || p < 1 {
		return 0, 0, fmt.Errorf("bad pick in %q", s)
	}
	return r, p, nil
}
