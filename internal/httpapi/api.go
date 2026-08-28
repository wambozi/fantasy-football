// Package httpapi exposes the draft over JSON + SSE using only net/http.
package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/wambozi/draft-copilot/internal/league"
	"github.com/wambozi/draft-copilot/internal/players"
	"github.com/wambozi/draft-copilot/internal/state"
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
}

// Options configure optional collaborators.
type Options struct {
	Advisor Advisor
	Briefer Briefer
	Static  fs.FS
	Search  func(q string, snap state.Snapshot, limit int) []*players.Player
	Log     *slog.Logger
}

// New builds the mux.
func New(lg *league.League, pool *players.Pool, st *state.DraftState, o Options) *Server {
	s := &Server{lg: lg, pool: pool, st: st, advisor: o.Advisor, briefer: o.Briefer, static: o.Static, search: o.Search, log: o.Log}
	if s.log == nil {
		s.log = slog.Default()
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
	State  state.Snapshot `json:"state"`
	Advice any            `json:"advice,omitempty"`
	Brief  *BriefPayload  `json:"brief,omitempty"`
}

// BriefPayload is the Claude commentary, if any.
type BriefPayload = Brief

func (s *Server) payload() StatePayload {
	snap := s.st.Snapshot()
	p := StatePayload{State: snap}
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
	writeJSON(w, http.StatusOK, map[string]any{
		"teams":         s.lg.Teams,
		"my_team":       s.lg.MyTeam,
		"my_live_picks": s.lg.MyLivePicks,
		"num_live":      s.lg.NumLive(),
		"slots":         s.lg.Slots,
		"players":       s.pool.Players,
	})
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
