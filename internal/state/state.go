// Package state holds the mutable draft state and its append-only event log.
//
// The log is the source of truth: every mutation is an event, undo is an event,
// and boot replays the log. Nothing is ever deleted from it.
package state

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"sync"
	"time"

	"github.com/wambozi/draft-copilot/internal/league"
	"github.com/wambozi/draft-copilot/internal/players"
)

// Source identifies which input path produced a pick.
type Source string

const (
	SourceManual Source = "manual"
	SourceWS     Source = "ws"
	SourceDOM    Source = "dom"
	SourceSim    Source = "sim"
)

// Event is one JSONL line.
type Event struct {
	Seq      int       `json:"seq"`
	Type     string    `json:"type"` // "pick" | "undo"
	LivePick int       `json:"live_pick,omitempty"`
	Team     string    `json:"team,omitempty"`
	PlayerID string    `json:"player_id,omitempty"`
	Source   Source    `json:"source,omitempty"`
	Undoes   int       `json:"undoes,omitempty"`
	At       time.Time `json:"at"`
}

// Pick is an applied live pick.
type Pick struct {
	Seq      int       `json:"seq"`
	LivePick int       `json:"live_pick"`
	SlotIdx  int       `json:"slot_idx"`
	Team     string    `json:"team"`
	PlayerID string    `json:"player_id"`
	Source   Source    `json:"source"`
	At       time.Time `json:"at"`
}

// Sentinel errors mapped to HTTP statuses by the API layer.
var (
	ErrDraftOver     = errors.New("draft is complete")
	ErrUnknownPlayer = errors.New("unknown player")
	ErrTaken         = errors.New("player already taken")
	ErrWrongTeam     = errors.New("team is not on the clock")
	ErrNothingToUndo = errors.New("nothing to undo")
	ErrConflict      = errors.New("live pick already filled with a different player")
	ErrNotDraftable  = errors.New("position has no roster slot")
	ErrPosMax        = errors.New("position max reached")
)

// Conflict is returned (wrapping ErrConflict) when an automated source disagrees with
// what is already on the board.
type Conflict struct {
	LivePick int
	Existing string
	Incoming string
	Source   Source
}

func (c *Conflict) Error() string {
	return fmt.Sprintf("live pick %d already has %s, %s sent %s", c.LivePick, c.Existing, c.Source, c.Incoming)
}
func (c *Conflict) Unwrap() error { return ErrConflict }

// DraftState is the in-memory board.
type DraftState struct {
	mu      sync.RWMutex
	lg      *league.League
	pool    *players.Pool
	picks   []Pick
	taken   map[string]string   // playerID -> team
	rosters map[string][]string // team -> playerIDs (keepers first)
	seq     int
	version int
	logW    io.Writer
	logMu   sync.Mutex
	now     func() time.Time

	subsMu sync.Mutex
	subs   map[chan int]struct{}
}

// Snapshot is an immutable copy for JSON/engine consumption.
type Snapshot struct {
	Version   int                 `json:"version"`
	LivePick  int                 `json:"live_pick"` // the pick currently on the clock; NumLive+1 when done
	OnClock   string              `json:"on_clock"`
	Picks     []Pick              `json:"picks"`
	Taken     map[string]string   `json:"taken"`
	Rosters   map[string][]string `json:"rosters"`
	Complete  bool                `json:"complete"`
	Conflicts []Conflict          `json:"conflicts,omitempty"`
}

// New builds a state seeded with keepers. If logPath is non-empty, the log is replayed
// then opened for append.
func New(lg *league.League, pool *players.Pool, logPath string) (*DraftState, error) {
	s := &DraftState{
		lg:      lg,
		pool:    pool,
		taken:   map[string]string{},
		rosters: map[string][]string{},
		now:     time.Now,
		subs:    map[chan int]struct{}{},
	}
	for _, t := range lg.Teams {
		s.rosters[t] = []string{}
	}
	for _, slot := range lg.Slots {
		if slot.KeeperID == "" {
			continue
		}
		s.taken[slot.KeeperID] = slot.Team
		s.rosters[slot.Team] = append(s.rosters[slot.Team], slot.KeeperID)
	}
	if logPath == "" {
		s.logW = io.Discard
		return s, nil
	}
	if err := s.replay(logPath); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return nil, fmt.Errorf("open event log: %w", err)
	}
	s.logW = f
	return s, nil
}

func (s *DraftState) replay(path string) error {
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("open event log: %w", err)
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<16), 1<<20)
	line := 0
	for sc.Scan() {
		line++
		if len(sc.Bytes()) == 0 {
			continue
		}
		var ev Event
		if err := json.Unmarshal(sc.Bytes(), &ev); err != nil {
			return fmt.Errorf("event log line %d: %w", line, err)
		}
		if err := s.apply(ev); err != nil {
			return fmt.Errorf("event log line %d (seq %d): %w", line, ev.Seq, err)
		}
	}
	if err := sc.Err(); err != nil {
		return fmt.Errorf("read event log: %w", err)
	}
	return nil
}

// apply mutates state for an event without logging. Caller holds s.mu.
func (s *DraftState) apply(ev Event) error {
	switch ev.Type {
	case "pick":
		slot, ok := s.lg.SlotForLive(ev.LivePick)
		if !ok {
			return fmt.Errorf("live pick %d out of range", ev.LivePick)
		}
		s.picks = append(s.picks, Pick{
			Seq: ev.Seq, LivePick: ev.LivePick, SlotIdx: slot.Overall - 1,
			Team: ev.Team, PlayerID: ev.PlayerID, Source: ev.Source, At: ev.At,
		})
		s.taken[ev.PlayerID] = ev.Team
		s.rosters[ev.Team] = append(s.rosters[ev.Team], ev.PlayerID)
	case "undo":
		if len(s.picks) == 0 {
			return ErrNothingToUndo
		}
		last := s.picks[len(s.picks)-1]
		if ev.Undoes != 0 && ev.Undoes != last.Seq {
			return fmt.Errorf("undo targets seq %d but last pick is seq %d", ev.Undoes, last.Seq)
		}
		s.picks = s.picks[:len(s.picks)-1]
		delete(s.taken, last.PlayerID)
		r := s.rosters[last.Team]
		s.rosters[last.Team] = r[:len(r)-1]
	default:
		return fmt.Errorf("unknown event type %q", ev.Type)
	}
	if ev.Seq > s.seq {
		s.seq = ev.Seq
	}
	s.version++
	return nil
}

func (s *DraftState) record(ev Event) error {
	b, err := json.Marshal(ev)
	if err != nil {
		return fmt.Errorf("marshal event: %w", err)
	}
	s.logMu.Lock()
	defer s.logMu.Unlock()
	if _, err := s.logW.Write(append(b, '\n')); err != nil {
		return fmt.Errorf("append event log: %w", err)
	}
	if f, ok := s.logW.(*os.File); ok {
		_ = f.Sync()
	}
	return nil
}

// CurrentLive returns the live pick on the clock (NumLive+1 once complete). Caller holds s.mu.
func (s *DraftState) currentLive() int { return len(s.picks) + 1 }

// Pick applies a pick for the team on the clock. team may be "" (defaults to on-clock).
// Idempotent on (live_pick, player_id): a duplicate submit returns the existing pick.
func (s *DraftState) Pick(playerID, team string, src Source) (Pick, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	// Idempotency / conflict detection: was this player already picked, and at which live pick?
	for _, p := range s.picks {
		if p.PlayerID == playerID {
			return p, nil
		}
	}
	live := s.currentLive()
	if live > s.lg.NumLive() {
		return Pick{}, ErrDraftOver
	}
	slot, _ := s.lg.SlotForLive(live)
	if team == "" {
		team = slot.Team
	} else if team != slot.Team {
		return Pick{}, fmt.Errorf("%w: %q is on the clock at live pick %d, got %q", ErrWrongTeam, slot.Team, live, team)
	}
	pl, ok := s.pool.ByID[playerID]
	if !ok {
		return Pick{}, fmt.Errorf("%w: %q", ErrUnknownPlayer, playerID)
	}
	if !pl.Pos.Draftable() {
		return Pick{}, fmt.Errorf("%w: %s is %s", ErrNotDraftable, pl.Name, pl.Pos)
	}
	if owner, gone := s.taken[playerID]; gone {
		return Pick{}, fmt.Errorf("%w: %s rostered by %q", ErrTaken, pl.Name, owner)
	}
	if max, ok := s.lg.Roster.PosMax[pl.Pos]; ok && s.countPos(team, pl.Pos) >= max {
		return Pick{}, fmt.Errorf("%w: %s already has %d %s", ErrPosMax, team, max, pl.Pos)
	}
	ev := Event{Seq: s.seq + 1, Type: "pick", LivePick: live, Team: team, PlayerID: playerID, Source: src, At: s.now().UTC()}
	if err := s.record(ev); err != nil {
		return Pick{}, err
	}
	if err := s.apply(ev); err != nil {
		return Pick{}, err
	}
	s.notify()
	return s.picks[len(s.picks)-1], nil
}

// PickAt is used by automated sources that know the live pick number. If that pick is
// already filled by a different player it returns a *Conflict rather than overwriting.
func (s *DraftState) PickAt(live int, playerID string, src Source) (Pick, error) {
	s.mu.RLock()
	if live >= 1 && live <= len(s.picks) {
		existing := s.picks[live-1]
		s.mu.RUnlock()
		if existing.PlayerID == playerID {
			return existing, nil
		}
		return Pick{}, &Conflict{LivePick: live, Existing: existing.PlayerID, Incoming: playerID, Source: src}
	}
	cur := s.currentLive()
	s.mu.RUnlock()
	if live != cur {
		return Pick{}, fmt.Errorf("live pick %d is not on the clock (current %d)", live, cur)
	}
	return s.Pick(playerID, "", src)
}

func (s *DraftState) countPos(team string, pos players.Position) int {
	n := 0
	for _, id := range s.rosters[team] {
		if pl, ok := s.pool.ByID[id]; ok && pl.Pos == pos {
			n++
		}
	}
	return n
}

// Undo reverts the most recent pick by appending an undo event.
func (s *DraftState) Undo() (Pick, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.picks) == 0 {
		return Pick{}, ErrNothingToUndo
	}
	last := s.picks[len(s.picks)-1]
	ev := Event{Seq: s.seq + 1, Type: "undo", Undoes: last.Seq, At: s.now().UTC()}
	if err := s.record(ev); err != nil {
		return Pick{}, err
	}
	if err := s.apply(ev); err != nil {
		return Pick{}, err
	}
	s.notify()
	return last, nil
}

// Snapshot returns a copy safe to read without the lock.
func (s *DraftState) Snapshot() Snapshot {
	s.mu.RLock()
	defer s.mu.RUnlock()
	live := s.currentLive()
	snap := Snapshot{
		Version:  s.version,
		LivePick: live,
		OnClock:  s.lg.TeamOnClock(live),
		Picks:    append([]Pick(nil), s.picks...),
		Taken:    make(map[string]string, len(s.taken)),
		Rosters:  make(map[string][]string, len(s.rosters)),
		Complete: live > s.lg.NumLive(),
	}
	for k, v := range s.taken {
		snap.Taken[k] = v
	}
	for k, v := range s.rosters {
		snap.Rosters[k] = append([]string(nil), v...)
	}
	return snap
}

// Version returns the current state version.
func (s *DraftState) Version() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.version
}

// Subscribe returns a channel that receives the new version after each mutation.
// The channel is buffered(1) and coalesces: a slow reader sees only the latest version.
func (s *DraftState) Subscribe() (<-chan int, func()) {
	ch := make(chan int, 1)
	s.subsMu.Lock()
	s.subs[ch] = struct{}{}
	s.subsMu.Unlock()
	return ch, func() {
		s.subsMu.Lock()
		delete(s.subs, ch)
		s.subsMu.Unlock()
	}
}

// Poke re-sends the current version to subscribers without a mutation — used when
// something derived (a Claude brief) becomes available for the same state.
func (s *DraftState) Poke() {
	s.mu.RLock()
	defer s.mu.RUnlock()
	s.notify()
}

// notify is called with s.mu held.
func (s *DraftState) notify() {
	v := s.version
	s.subsMu.Lock()
	defer s.subsMu.Unlock()
	for ch := range s.subs {
		select {
		case <-ch: // drop stale
		default:
		}
		ch <- v
	}
}
