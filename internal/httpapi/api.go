// Package httpapi exposes the draft over JSON + SSE using only net/http.
package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"math"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/wambozi/draft-copilot/internal/league"
	"github.com/wambozi/draft-copilot/internal/players"
	"github.com/wambozi/draft-copilot/internal/state"
	"github.com/wambozi/draft-copilot/internal/strategy"
)

// Advisor produces the recommendation payload for a state version. Nil until M2.
type Advisor interface {
	// Advise must be cheap for a cached version and must never block on the network.
	Advise(snap state.Snapshot) any
}

// Briefer serves the latest Claude brief. Nil when no API key is configured.
type Briefer interface {
	// Brief returns the best brief for the snapshot's live pick and whether one exists.
	Brief(snap state.Snapshot) (Brief, bool)
	// OnPick is invoked after every state change for speculative prefetch.
	OnPick(snap state.Snapshot)
}

// Brief is one rendered commentary.
type Brief struct {
	Text      string `json:"text"`
	LivePick  int    `json:"live_pick"`
	Projected bool   `json:"projected"` // generated from a predicted board, not the actual one
	Version   int    `json:"version"`   // state version it was generated from
}

// Server wires handlers to state.
type Server struct {
	lg      *league.League
	pool    *players.Pool
	st      *state.DraftState
	advisor Advisor
	briefer Briefer
	static  fs.FS
	log     *slog.Logger
	search  func(q string, snap state.Snapshot, limit int) []*players.Player
	auto    *automation
	cfg     *strategy.Config

	vorMu   sync.Mutex
	pickVOR map[int]float64 // live pick -> VOR at the moment it was made (this process only)
}

// Options configure optional collaborators.
type Options struct {
	Advisor Advisor
	Briefer Briefer
	Static  fs.FS
	Search  func(q string, snap state.Snapshot, limit int) []*players.Player
	Log     *slog.Logger
	// Config, when set, is served on /api/league so the UI's roster-state logic reads
	// the same strategy.yaml the engine does (one source of truth, spec handoff).
	Config *strategy.Config
	// FrameLog is where raw FanDraft frames are appended (recon). "" disables.
	FrameLog string
	// FrameParser extracts picks from raw frames once the wire shape is known.
	FrameParser FrameParser
}

// New builds the mux.
func New(lg *league.League, pool *players.Pool, st *state.DraftState, o Options) *Server {
	s := &Server{lg: lg, pool: pool, st: st, advisor: o.Advisor, briefer: o.Briefer, static: o.Static, search: o.Search, log: o.Log}
	if s.log == nil {
		s.log = slog.Default()
	}
	s.cfg = o.Config
	s.pickVOR = map[int]float64{}
	s.auto = &automation{parser: o.FrameParser}
	if o.FrameLog != "" {
		f, err := os.OpenFile(o.FrameLog, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
		if err != nil {
			s.log.Warn("frame log unavailable", "path", o.FrameLog, "err", err)
		} else {
			s.auto.log = f
		}
	}
	return s
}

// Handler returns the routed http.Handler.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("ok\n")) })
	mux.HandleFunc("GET /api/state", s.handleState)
	mux.HandleFunc("GET /api/stream", s.handleStream)
	mux.HandleFunc("POST /api/pick", s.handlePick)
	mux.HandleFunc("POST /api/undo", s.handleUndo)
	mux.HandleFunc("GET /api/search", s.handleSearch)
	mux.HandleFunc("GET /api/brief", s.handleBrief)
	mux.HandleFunc("GET /api/league", s.handleLeague)
	mux.HandleFunc("POST /api/fandraft/frame", s.handleFrame)
	mux.HandleFunc("POST /api/fandraft/pick", s.handleAutoPick)
	mux.HandleFunc("POST /api/fandraft/resolve", s.handleResolve)
	mux.HandleFunc("GET /api/fandraft/status", s.handleAutoStatus)
	if s.static != nil {
		mux.Handle("/", http.FileServerFS(s.static))
	}
	return s.cors(mux)
}

// cors permits the FanDraft userscript (and a phone on the LAN) to call the API.
func (s *Server) cors(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// StatePayload is the GET /api/state body.
type StatePayload struct {
	State      state.Snapshot   `json:"state"`
	Advice     any              `json:"advice,omitempty"`
	Brief      *BriefPayload    `json:"brief,omitempty"`
	Automation AutomationStatus `json:"automation"`
	// PickVOR is the VOR each live pick carried when it was made, for the rail. Only
	// picks made since this process started are known; the UI falls back for the rest.
	PickVOR map[int]float64 `json:"pick_vor"`
}

// BriefPayload is the Claude commentary, if any.
type BriefPayload = Brief

func (s *Server) payload() StatePayload {
	snap := s.st.Snapshot()
	p := StatePayload{State: snap, Automation: s.automationStatus(), PickVOR: s.pickVORs()}
	if s.advisor != nil {
		p.Advice = s.advisor.Advise(snap)
	}
	if s.briefer != nil {
		if b, ok := s.briefer.Brief(snap); ok {
			p.Brief = &b
		}
	}
	return p
}

func (s *Server) handleState(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, s.payload())
}

func (s *Server) handleLeague(w http.ResponseWriter, _ *http.Request) {
	out := map[string]any{
		"teams":         s.lg.Teams,
		"my_team":       s.lg.MyTeam,
		"my_live_picks": s.lg.MyLivePicks,
		"num_live":      s.lg.NumLive(),
		"rounds":        s.lg.Rounds(),
		"slots":         s.lg.Slots,
		"players":       s.pool.Players,
	}
	if s.cfg != nil {
		out["roster"] = map[string]any{
			"starters":    s.cfg.Roster.Starters,
			"flex":        map[string]any{"count": s.cfg.Roster.Flex.Count, "eligible": s.cfg.Roster.Flex.Eligible},
			"bench":       s.cfg.Roster.Bench,
			"total_spots": s.cfg.Roster.TotalSpots,
			"max":         s.cfg.Roster.Max,
		}
		out["need"] = s.cfg.Engine.Need
		out["keeper"] = map[string]any{"cost_floor_round": s.cfg.Keeper.CostFloorRound}
	}
	writeJSON(w, http.StatusOK, out)
}

type pickReq struct {
	PlayerID string       `json:"player_id"`
	Team     string       `json:"team"`
	LivePick int          `json:"live_pick"`
	Source   state.Source `json:"source"`
}

func (s *Server) handlePick(w http.ResponseWriter, r *http.Request) {
	var req pickReq
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, fmt.Errorf("decode body: %w", err))
		return
	}
	req.PlayerID = strings.TrimSpace(req.PlayerID)
	if req.PlayerID == "" {
		writeErr(w, http.StatusBadRequest, errors.New("player_id required"))
		return
	}
	if req.Source == "" {
		req.Source = state.SourceManual
	}
	vor, vorOK := s.vorNow(req.PlayerID)
	var (
		p   state.Pick
		err error
	)
	if req.LivePick > 0 {
		p, err = s.st.PickAt(req.LivePick, req.PlayerID, req.Source)
	} else {
		p, err = s.st.Pick(req.PlayerID, req.Team, req.Source)
	}
	if err != nil {
		writeErr(w, statusFor(err), err)
		return
	}
	s.recordVOR(p.LivePick, vor, vorOK)
	s.log.Info("pick", "live", p.LivePick, "team", p.Team, "player", p.PlayerID, "source", p.Source)
	s.afterMutation()
	writeJSON(w, http.StatusOK, map[string]any{"pick": p, "state": s.payload()})
}

func (s *Server) handleUndo(w http.ResponseWriter, _ *http.Request) {
	p, err := s.st.Undo()
	if err != nil {
		writeErr(w, statusFor(err), err)
		return
	}
	s.forgetVOR(p.LivePick)
	s.log.Info("undo", "live", p.LivePick, "player", p.PlayerID)
	s.afterMutation()
	writeJSON(w, http.StatusOK, map[string]any{"undone": p, "state": s.payload()})
}

func (s *Server) afterMutation() {
	if s.briefer != nil {
		go s.briefer.OnPick(s.st.Snapshot())
	}
}

func (s *Server) handleSearch(w http.ResponseWriter, r *http.Request) {
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	if s.search == nil || q == "" {
		writeJSON(w, http.StatusOK, []any{})
		return
	}
	writeJSON(w, http.StatusOK, s.search(q, s.st.Snapshot(), 8))
}

func (s *Server) handleBrief(w http.ResponseWriter, _ *http.Request) {
	if s.briefer == nil {
		writeJSON(w, http.StatusOK, map[string]any{"enabled": false})
		return
	}
	snap := s.st.Snapshot()
	b, ok := s.briefer.Brief(snap)
	if !ok {
		writeJSON(w, http.StatusAccepted, map[string]any{"enabled": true, "version": snap.Version})
		return
	}
	writeJSON(w, http.StatusOK, b)
}

// handleStream pushes the full payload on every version bump, plus a keepalive.
func (s *Server) handleStream(w http.ResponseWriter, r *http.Request) {
	fl, ok := w.(http.Flusher)
	if !ok {
		writeErr(w, http.StatusInternalServerError, errors.New("streaming unsupported"))
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	ch, cancel := s.st.Subscribe()
	defer cancel()
	send := func() {
		b, err := json.Marshal(s.payload())
		if err != nil {
			s.log.Error("sse marshal", "err", err)
			return
		}
		fmt.Fprintf(w, "event: state\ndata: %s\n\n", b)
		fl.Flush()
	}
	send()
	tick := time.NewTicker(15 * time.Second)
	defer tick.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-ch:
			send()
		case <-tick.C:
			fmt.Fprint(w, ": keepalive\n\n")
			fl.Flush()
		}
	}
}

func statusFor(err error) int {
	switch {
	case errors.Is(err, state.ErrUnknownPlayer):
		return http.StatusNotFound
	case errors.Is(err, state.ErrConflict):
		return http.StatusConflict
	case errors.Is(err, state.ErrTaken), errors.Is(err, state.ErrWrongTeam), errors.Is(err, state.ErrDraftOver),
		errors.Is(err, state.ErrNothingToUndo), errors.Is(err, state.ErrPosMax), errors.Is(err, state.ErrNotDraftable):
		return http.StatusUnprocessableEntity
	}
	return http.StatusInternalServerError
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		slog.Error("write json", "err", err)
	}
}

func writeErr(w http.ResponseWriter, code int, err error) {
	writeJSON(w, code, map[string]string{"error": err.Error()})
}

// Serve runs the server until ctx is cancelled.
func Serve(ctx context.Context, addr string, h http.Handler, log *slog.Logger) error {
	srv := &http.Server{Addr: addr, Handler: h, ReadHeaderTimeout: 5 * time.Second}
	errc := make(chan error, 1)
	go func() { errc <- srv.ListenAndServe() }()
	log.Info("listening", "addr", addr)
	select {
	case err := <-errc:
		return fmt.Errorf("listen: %w", err)
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		return srv.Shutdown(shutdownCtx)
	}
}

// vorReplacement is the subset of engine.Advice the rail needs: replacement level per
// position at the current state. Read through the Advisor's untyped payload.
type vorReplacement struct {
	Waiver map[players.Position]float64 `json:"waiver"`
}

// vorNow is a player's value over the waiver level at the current state — what the
// pick is worth over free agency. That baseline stays meaningful for every team's
// pick all draft long, where the starter-demand replacement level goes negative for
// most mid-round picks. Cheap: the advisor caches per state version.
func (s *Server) vorNow(playerID string) (v float64, ok bool) {
	if s.advisor == nil {
		return 0, false
	}
	pl, found := s.pool.ByID[playerID]
	if !found || pl.ProjPoints == 0 {
		return 0, false
	}
	raw, err := json.Marshal(s.advisor.Advise(s.st.Snapshot()))
	if err != nil {
		return 0, false
	}
	var ad vorReplacement
	if json.Unmarshal(raw, &ad) != nil || ad.Waiver == nil {
		return 0, false
	}
	repl, has := ad.Waiver[pl.Pos]
	if !has {
		return 0, false
	}
	return math.Max(0, pl.ProjPoints-repl), true
}

func (s *Server) recordVOR(live int, v float64, ok bool) {
	if !ok {
		return
	}
	s.vorMu.Lock()
	s.pickVOR[live] = v
	s.vorMu.Unlock()
}

func (s *Server) forgetVOR(live int) {
	s.vorMu.Lock()
	delete(s.pickVOR, live)
	s.vorMu.Unlock()
}

func (s *Server) pickVORs() map[int]float64 {
	s.vorMu.Lock()
	defer s.vorMu.Unlock()
	out := make(map[int]float64, len(s.pickVOR))
	for k, v := range s.pickVOR {
		out[k] = v
	}
	return out
}
