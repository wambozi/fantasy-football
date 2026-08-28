package httpapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/wambozi/draft-copilot/internal/league"
	"github.com/wambozi/draft-copilot/internal/players"
	"github.com/wambozi/draft-copilot/internal/state"
)

func newTestServer(t *testing.T) http.Handler {
	t.Helper()
	slots := []league.DraftSlot{
		{Round: 1, PickInRound: 1, Team: "A"}, {Round: 1, PickInRound: 2, Team: "B"},
		{Round: 2, PickInRound: 1, Team: "B"}, {Round: 2, PickInRound: 2, Team: "A"},
	}
	r := league.DefaultRoster()
	r.TotalSpots = 2
	lg, err := league.New(slots, "A", r)
	if err != nil {
		t.Fatal(err)
	}
	pool := &players.Pool{ByID: map[string]*players.Player{}}
	pool.Add(&players.Player{ID: "p1", Pos: players.RB})
	pool.Add(&players.Player{ID: "p2", Pos: players.WR})
	st, err := state.New(lg, pool, "")
	if err != nil {
		t.Fatal(err)
	}
	return New(lg, pool, st, Options{}).Handler()
}

func TestSmoke(t *testing.T) {
	h := newTestServer(t)
	do := func(method, path, body string) (int, map[string]any) {
		t.Helper()
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		var out map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
		return rec.Code, out
	}
	cases := []struct {
		method, path, body string
		want               int
	}{
		{"GET", "/healthz", "", 200},
		{"GET", "/api/state", "", 200},
		{"POST", "/api/pick", `{}`, 400},
		{"POST", "/api/pick", `{"player_id":"zzz"}`, 404},
		{"POST", "/api/pick", `{"player_id":"p1","team":"B"}`, 422},
		{"POST", "/api/pick", `{"player_id":"p1"}`, 200},
		{"POST", "/api/pick", `{"player_id":"p1"}`, 200}, // idempotent
		{"POST", "/api/pick", `{"player_id":"p2","live_pick":1,"source":"ws"}`, 409},
		{"POST", "/api/undo", "", 200},
		{"POST", "/api/undo", "", 422},
		{"GET", "/api/brief", "", 200},
	}
	for _, c := range cases {
		code, _ := do(c.method, c.path, c.body)
		if code != c.want {
			t.Errorf("%s %s %s: got %d want %d", c.method, c.path, c.body, code, c.want)
		}
	}
	_, out := do("GET", "/api/state", "")
	if st := out["state"].(map[string]any); st["live_pick"].(float64) != 1 {
		t.Errorf("state after undo: %v", st)
	}
}
