package state

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/wambozi/draft-copilot/internal/league"
	"github.com/wambozi/draft-copilot/internal/players"
)

func fixture(t *testing.T) (*league.League, *players.Pool) {
	t.Helper()
	slots := []league.DraftSlot{
		{Round: 1, PickInRound: 1, Team: "A"},
		{Round: 1, PickInRound: 2, Team: "B", KeeperID: "keep-b"},
		{Round: 2, PickInRound: 1, Team: "B"},
		{Round: 2, PickInRound: 2, Team: "A"},
	}
	r := league.DefaultRoster()
	r.TotalSpots = 2
	r.PosMax = map[players.Position]int{players.QB: 1}
	lg, err := league.New(slots, "A", r)
	if err != nil {
		t.Fatal(err)
	}
	pool := &players.Pool{ByID: map[string]*players.Player{}}
	for _, p := range []*players.Player{
		{ID: "p1", Pos: players.RB}, {ID: "p2", Pos: players.WR}, {ID: "p3", Pos: players.RB},
		{ID: "q1", Pos: players.QB}, {ID: "q2", Pos: players.QB}, {ID: "k1", Pos: players.K},
		{ID: "keep-b", Pos: players.RB},
	} {
		pool.Add(p)
	}
	return lg, pool
}

func TestPickFlow(t *testing.T) {
	lg, pool := fixture(t)
	s, err := New(lg, pool, "")
	if err != nil {
		t.Fatal(err)
	}
	if lg.MyLivePicks[0] != 1 || lg.NumLive() != 3 {
		t.Fatalf("fixture: %v %d", lg.MyLivePicks, lg.NumLive())
	}
	cases := []struct {
		name   string
		player string
		team   string
		want   error
	}{
		{"wrong team", "p1", "B", ErrWrongTeam},
		{"kicker rejected", "k1", "", ErrNotDraftable},
		{"unknown", "nope", "", ErrUnknownPlayer},
		{"keeper already taken", "keep-b", "", ErrTaken},
		{"ok A takes p1", "p1", "", nil},
		{"idempotent resubmit", "p1", "", nil},
		{"B takes p2", "p2", "B", nil},
		{"A takes p3", "p3", "A", nil},
		{"draft over", "q1", "", ErrDraftOver},
	}
	for _, c := range cases {
		_, err := s.Pick(c.player, c.team, SourceManual)
		if !errors.Is(err, c.want) {
			t.Errorf("%s: err=%v want %v", c.name, err, c.want)
		}
	}
	snap := s.Snapshot()
	if len(snap.Picks) != 3 || !snap.Complete || snap.Rosters["B"][0] != "keep-b" {
		t.Errorf("snapshot: %+v", snap)
	}
	if _, err := s.Undo(); err != nil {
		t.Fatal(err)
	}
	if snap := s.Snapshot(); snap.LivePick != 3 || snap.Taken["p3"] != "" {
		t.Errorf("after undo: %+v", snap)
	}
}

func TestPosMax(t *testing.T) {
	lg, pool := fixture(t)
	s, _ := New(lg, pool, "")
	if _, err := s.Pick("q1", "", SourceManual); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Pick("p2", "", SourceManual); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Pick("q2", "", SourceManual); !errors.Is(err, ErrPosMax) {
		t.Errorf("want ErrPosMax got %v", err)
	}
}

func TestPickAtConflict(t *testing.T) {
	lg, pool := fixture(t)
	s, _ := New(lg, pool, "")
	if _, err := s.Pick("p1", "", SourceManual); err != nil {
		t.Fatal(err)
	}
	if _, err := s.PickAt(1, "p1", SourceWS); err != nil {
		t.Errorf("duplicate should be harmless: %v", err)
	}
	var c *Conflict
	if _, err := s.PickAt(1, "p2", SourceWS); !errors.As(err, &c) || c.Existing != "p1" {
		t.Errorf("want conflict, got %v", err)
	}
}

func TestReplay(t *testing.T) {
	lg, pool := fixture(t)
	path := filepath.Join(t.TempDir(), "events.jsonl")
	s, err := New(lg, pool, path)
	if err != nil {
		t.Fatal(err)
	}
	s.now = func() time.Time { return time.Unix(0, 0) }
	must := func(_ Pick, err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(s.Pick("p1", "", SourceManual))
	must(s.Pick("p2", "", SourceWS))
	must(s.Undo())
	must(s.Pick("p3", "", SourceManual))
	before := s.Snapshot()

	s2, err := New(lg, pool, path)
	if err != nil {
		t.Fatal(err)
	}
	after := s2.Snapshot()
	if len(after.Picks) != 2 || after.Picks[1].PlayerID != "p3" || after.Picks[1].Source != SourceManual {
		t.Errorf("replayed picks: %+v", after.Picks)
	}
	if after.LivePick != before.LivePick || after.Taken["p2"] != "" {
		t.Errorf("replay mismatch: %+v vs %+v", after, before)
	}
	// Log must have 4 lines (3 picks + 1 undo): undo is an event, never a deletion.
	b, _ := os.ReadFile(path)
	if n := len(splitLines(b)); n != 4 {
		t.Errorf("log lines = %d, want 4", n)
	}
	// Continuing after replay must not reuse a seq.
	p, err := s2.Pick("q1", "A", SourceManual)
	if err != nil || p.Seq != 5 {
		t.Errorf("post-replay pick: %+v %v", p, err)
	}
}

func splitLines(b []byte) [][]byte {
	var out [][]byte
	start := 0
	for i, c := range b {
		if c == '\n' {
			if i > start {
				out = append(out, b[start:i])
			}
			start = i + 1
		}
	}
	return out
}

func TestSubscribeCoalesces(t *testing.T) {
	lg, pool := fixture(t)
	s, _ := New(lg, pool, "")
	ch, cancel := s.Subscribe()
	defer cancel()
	_, _ = s.Pick("p1", "", SourceManual)
	_, _ = s.Pick("p2", "", SourceManual)
	select {
	case v := <-ch:
		if v != 2 {
			t.Errorf("want latest version 2, got %d", v)
		}
	default:
		t.Error("no notification")
	}
}
