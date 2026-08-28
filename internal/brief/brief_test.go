package brief

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wambozi/draft-copilot/internal/engine"
	"github.com/wambozi/draft-copilot/internal/league"
	"github.com/wambozi/draft-copilot/internal/players"
	"github.com/wambozi/draft-copilot/internal/state"
	"github.com/wambozi/draft-copilot/internal/strategy"
)

type fakeGen struct {
	mu    sync.Mutex
	calls []string
	delay time.Duration
	fail  bool
}

func (f *fakeGen) Generate(_ context.Context, system, user string) (string, error) {
	time.Sleep(f.delay)
	f.mu.Lock()
	f.calls = append(f.calls, user)
	f.mu.Unlock()
	if f.fail {
		return "", context.DeadlineExceeded
	}
	if !strings.Contains(system, "at most 3 bullets") {
		return "", context.Canceled
	}
	if strings.Contains(user, `"projected_board":true`) {
		return "- projected", nil
	}
	return "- exact", nil
}

func fixture(t *testing.T) (*league.League, *players.Pool, *strategy.Config) {
	t.Helper()
	cfg, err := strategy.Load("../../data/strategy.yaml")
	if err != nil {
		t.Fatal(err)
	}
	lg, err := league.Load("../../data/draft-order.csv", "Sittin Purdy", cfg.LeagueRoster())
	if err != nil {
		t.Fatal(err)
	}
	pool, err := players.Load("../../data/players.json")
	if err != nil {
		t.Fatal(err)
	}
	if len(pool.Players) == 0 {
		t.Skip("no players.json")
	}
	pool.CurveProjections()
	for i := range lg.Slots {
		s := &lg.Slots[i]
		if s.KeeperID == "" {
			continue
		}
		if _, ok := pool.ByID[s.KeeperID]; !ok {
			for _, p := range pool.Players {
				if players.NameKey(p.Name) == players.NameKey(s.KeeperName) && p.Pos == s.KeeperPos {
					s.KeeperID = p.ID
					break
				}
			}
		}
	}
	return lg, pool, cfg
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("condition not met in time")
}

func TestPrefetchThenExact(t *testing.T) {
	lg, pool, cfg := fixture(t)
	cfg.Engine.Sims = 100
	eng := engine.New(lg, pool, cfg, 1)
	st, _ := state.New(lg, pool, "")
	gen := &fakeGen{}
	pokes := 0
	var pm sync.Mutex
	svc := New(gen, eng, lg, pool, cfg, Options{Poke: func() { pm.Lock(); pokes++; pm.Unlock() }})

	// Pick 1 of 7 before my #8: outside the window, nothing generated.
	svc.OnPick(st.Snapshot())
	time.Sleep(50 * time.Millisecond)
	if _, ok := svc.Brief(st.Snapshot()); ok {
		t.Fatal("brief generated outside window")
	}
	// Advance to live pick 4 (4 picks until #8): projected brief expected.
	bpa := func() {
		snap := st.Snapshot()
		var best *players.Player
		for _, p := range pool.Players {
			if _, gone := snap.Taken[p.ID]; gone || p.Pos == players.DST || !p.Pos.Draftable() {
				continue
			}
			if best == nil || p.ADPMean < best.ADPMean {
				best = p
			}
		}
		if _, err := st.Pick(best.ID, "", state.SourceSim); err != nil {
			t.Fatal(err)
		}
	}
	for st.Snapshot().LivePick < 4 {
		bpa()
	}
	svc.OnPick(st.Snapshot())
	waitFor(t, func() bool { b, ok := svc.Brief(st.Snapshot()); return ok && b.Projected && b.LivePick == 8 })
	// Now reach my pick: exact brief replaces projected.
	for st.Snapshot().LivePick < 8 {
		bpa()
	}
	svc.OnPick(st.Snapshot())
	waitFor(t, func() bool { b, ok := svc.Brief(st.Snapshot()); return ok && !b.Projected && b.Text == "- exact" })
	pm.Lock()
	if pokes < 2 {
		t.Errorf("expected >=2 pokes, got %d", pokes)
	}
	pm.Unlock()
	if len(gen.calls) < 2 {
		t.Errorf("expected >=2 generations, got %d", len(gen.calls))
	}
	if !strings.Contains(gen.calls[len(gen.calls)-1], `"on_clock":true`) {
		t.Errorf("exact call should be on clock: %s", gen.calls[len(gen.calls)-1][:80])
	}
}

func TestFailureIsSilent(t *testing.T) {
	lg, pool, cfg := fixture(t)
	cfg.Engine.Sims = 100
	eng := engine.New(lg, pool, cfg, 1)
	st, _ := state.New(lg, pool, "")
	svc := New(&fakeGen{fail: true}, eng, lg, pool, cfg, Options{})
	for st.Snapshot().LivePick < 5 {
		snap := st.Snapshot()
		for _, p := range pool.Players {
			if _, gone := snap.Taken[p.ID]; !gone && p.Pos == players.WR {
				st.Pick(p.ID, "", state.SourceSim)
				break
			}
		}
	}
	svc.OnPick(st.Snapshot())
	time.Sleep(100 * time.Millisecond)
	if _, ok := svc.Brief(st.Snapshot()); ok {
		t.Fatal("failed generation must not produce a brief")
	}
}

func TestSystemPromptStable(t *testing.T) {
	lg, _, cfg := fixture(t)
	a, b := SystemPrompt(lg, cfg), SystemPrompt(lg, cfg)
	if a != b {
		t.Fatal("system prompt must be byte-stable for prompt caching")
	}
}
