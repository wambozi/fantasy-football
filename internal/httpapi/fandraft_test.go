package httpapi

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/wambozi/draft-copilot/internal/league"
	"github.com/wambozi/draft-copilot/internal/players"
	"github.com/wambozi/draft-copilot/internal/state"
)

func autoFixture(t *testing.T) (*Server, *state.DraftState, *players.Pool) {
	t.Helper()
	pool := &players.Pool{ByID: map[string]*players.Player{}}
	pool.Add(&players.Player{ID: "a", Name: "Alpha Adams", Pos: players.RB, ADPMean: 1})
	pool.Add(&players.Player{ID: "b", Name: "Bravo Brown", Pos: players.WR, ADPMean: 2})
	pool.Add(&players.Player{ID: "c", Name: "Charlie Cole", Pos: players.WR, ADPMean: 3})
	pool.Add(&players.Player{ID: "k", Name: "Kept Guy", Pos: players.TE, ADPMean: 4})
	slots := []league.DraftSlot{
		{Round: 1, PickInRound: 1, Team: "T1"},
		{Round: 1, PickInRound: 2, Team: "T2", KeeperID: "k", KeeperName: "Kept Guy", KeeperPos: players.TE},
		{Round: 1, PickInRound: 3, Team: "T3"},
		{Round: 2, PickInRound: 1, Team: "T3"},
		{Round: 2, PickInRound: 2, Team: "T2"},
		{Round: 2, PickInRound: 3, Team: "T1"},
	}
	r := league.DefaultRoster()
	r.TotalSpots = 2
	lg, err := league.New(slots, "T1", r)
	if err != nil {
		t.Fatal(err)
	}
	st, err := state.New(lg, pool, "")
	if err != nil {
		t.Fatal(err)
	}
	return New(lg, pool, st, Options{}), st, pool
}

func post(t *testing.T, h http.Handler, path string, body any) map[string]any {
	t.Helper()
	b, _ := json.Marshal(body)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", path, bytes.NewReader(b)))
	var out map[string]any
	json.Unmarshal(rec.Body.Bytes(), &out)
	return out
}

func TestAutoPickFlow(t *testing.T) {
	s, st, _ := autoFixture(t)
	h := s.Handler()
	// Overall 1 -> live 1: applies.
	if r := post(t, h, "/api/fandraft/pick", AutoPick{Overall: 1, Player: "alpha ad", Source: "ws"}); r["ok"] != true {
		t.Fatalf("first pick: %v", r)
	}
	if st.Snapshot().LivePick != 2 {
		t.Fatalf("live should be 2, got %d", st.Snapshot().LivePick)
	}
	// Overall 2 is a keeper slot: ignored, no error.
	if r := post(t, h, "/api/fandraft/pick", AutoPick{Overall: 2, Player: "Kept Guy"}); r["ok"] != true || r["result"] != "keeper slot, ignored" {
		t.Fatalf("keeper slot: %v", r)
	}
	// Re-sent pick 1: duplicate, harmless.
	if r := post(t, h, "/api/fandraft/pick", AutoPick{Overall: 1, Player: "Alpha Adams"}); r["ok"] != true || r["result"] != "duplicate" {
		t.Fatalf("duplicate: %v", r)
	}
	// Manual entry fills live 2 with Bravo; automation then claims Charlie: conflict, not overwrite.
	if _, err := st.Pick("b", "", state.SourceManual); err != nil {
		t.Fatal(err)
	}
	r := post(t, h, "/api/fandraft/pick", AutoPick{Overall: 3, Player: "Charlie Cole"})
	if r["ok"] != false || r["result"] != "conflict" {
		t.Fatalf("conflict expected: %v", r)
	}
	if st.Snapshot().Picks[1].PlayerID != "b" {
		t.Fatal("manual pick was overwritten")
	}
	if st := s.automationStatus(); st.LastConflict == nil || st.Picks != 1 || st.Duplicates != 1 || !st.Seen {
		t.Fatalf("status: %+v", st)
	}
	post(t, h, "/api/fandraft/resolve", nil)
	if s.automationStatus().LastConflict != nil {
		t.Fatal("resolve did not clear the conflict")
	}
	// Unmatched name is reported, not dropped silently.
	r = post(t, h, "/api/fandraft/pick", AutoPick{Player: "Zed Nobody"})
	if r["ok"] != false || len(s.automationStatus().Unmatched) != 1 {
		t.Fatalf("unmatched: %v %+v", r, s.automationStatus())
	}
}

func TestFrameStoredWithoutParser(t *testing.T) {
	s, _, _ := autoFixture(t)
	r := post(t, s.Handler(), "/api/fandraft/frame", map[string]any{"raw": `{"type":"pick"}`, "at": 1})
	if r["stored"] != true || s.automationStatus().Frames != 1 {
		t.Fatalf("frame: %v", r)
	}
}
