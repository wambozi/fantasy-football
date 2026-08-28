package httpapi

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/wambozi/draft-copilot/internal/state"
)

// FanDraft ingestion (spec §9). A userscript on the live board posts either raw
// WebSocket frames (/api/fandraft/frame — recon and, once a parser exists, ingestion)
// or already-extracted picks (/api/fandraft/pick — the DOM path, or a WS path parsed
// in the browser). Both end in the same idempotent state.PickAt. Automation never
// overwrites a manual entry: a disagreement becomes a conflict banner for the human.

// AutoPick is one pick as reported by automation. Any one of Overall, Round+Pick, or
// LivePick identifies the slot; with none, the pick is applied to the clock.
type AutoPick struct {
	Overall  int    `json:"overall,omitempty"`   // 1-indexed board position incl. keeper slots
	Round    int    `json:"round,omitempty"`     // with Pick
	Pick     int    `json:"pick,omitempty"`      // pick in round
	LivePick int    `json:"live_pick,omitempty"` // 1-indexed live pick (keepers excluded)
	Player   string `json:"player"`              // name as shown on the board
	Team     string `json:"team,omitempty"`      // drafting team, informational
	Source   string `json:"source,omitempty"`    // ws | dom (default dom)
}

// FrameParser turns one raw frame into zero or more picks. Nil until §9.1 recon
// tells us the wire shape; frames are still logged so the parser can be written.
type FrameParser func(raw string) []AutoPick

// AutomationStatus is reported in every state payload for the freshness indicator.
type AutomationStatus struct {
	Seen         bool            `json:"seen"` // any automated event since boot
	LastEventAt  time.Time       `json:"last_event_at"`
	Frames       int             `json:"frames"`
	Picks        int             `json:"picks"`
	Duplicates   int             `json:"duplicates"`
	Unmatched    []string        `json:"unmatched,omitempty"` // most recent unmatched names
	LastConflict *state.Conflict `json:"last_conflict,omitempty"`
	ConflictNote string          `json:"conflict_note,omitempty"`
}

type automation struct {
	mu     sync.Mutex
	st     AutomationStatus
	log    *os.File
	parser FrameParser
}

func (s *Server) automationStatus() AutomationStatus {
	if s.auto == nil {
		return AutomationStatus{}
	}
	s.auto.mu.Lock()
	defer s.auto.mu.Unlock()
	return s.auto.st
}

func (s *Server) touchAutomation() {
	s.auto.mu.Lock()
	s.auto.st.Seen = true
	s.auto.st.LastEventAt = time.Now()
	s.auto.mu.Unlock()
}

// handleFrame stores a raw frame and, when a parser exists, applies its picks.
func (s *Server) handleFrame(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Raw string `json:"raw"`
		At  int64  `json:"at"`
		URL string `json:"url"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	s.touchAutomation()
	s.auto.mu.Lock()
	s.auto.st.Frames++
	if s.auto.log != nil {
		rec, _ := json.Marshal(map[string]any{"t": time.Now().UTC().Format(time.RFC3339Nano), "url": body.URL, "raw": body.Raw})
		s.auto.log.Write(append(rec, '\n'))
	}
	parser := s.auto.parser
	s.auto.mu.Unlock()
	applied := 0
	if parser != nil {
		for _, ap := range parser(body.Raw) {
			ap.Source = "ws"
			if _, err := s.applyAutoPick(ap); err == nil {
				applied++
			}
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"stored": true, "applied": applied})
}

// handleAutoPick applies one extracted pick.
func (s *Server) handleAutoPick(w http.ResponseWriter, r *http.Request) {
	var ap AutoPick
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&ap); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	s.touchAutomation()
	res, err := s.applyAutoPick(ap)
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "error": err.Error(), "result": res})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "result": res})
}

func (s *Server) handleResolve(w http.ResponseWriter, _ *http.Request) {
	s.auto.mu.Lock()
	s.auto.st.LastConflict = nil
	s.auto.st.ConflictNote = ""
	s.auto.mu.Unlock()
	s.st.Poke()
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) handleAutoStatus(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, s.automationStatus())
}

// applyAutoPick resolves the slot and the player, then applies idempotently.
func (s *Server) applyAutoPick(ap AutoPick) (string, error) {
	src := state.Source(ap.Source)
	if src != state.SourceWS && src != state.SourceDOM {
		src = state.SourceDOM
	}
	name := strings.TrimSpace(ap.Player)
	if name == "" {
		return "", errors.New("player name required")
	}
	// Slot resolution.
	live := ap.LivePick
	switch {
	case live > 0:
	case ap.Overall > 0:
		if ap.Overall > len(s.lg.Slots) {
			return "", fmt.Errorf("overall %d beyond the board", ap.Overall)
		}
		slot := s.lg.Slots[ap.Overall-1]
		if slot.LivePick == 0 {
			return "keeper slot, ignored", nil
		}
		live = slot.LivePick
	case ap.Round > 0 && ap.Pick > 0:
		for _, slot := range s.lg.Slots {
			if slot.Round == ap.Round && slot.PickInRound == ap.Pick {
				if slot.LivePick == 0 {
					return "keeper slot, ignored", nil
				}
				live = slot.LivePick
			}
		}
		if live == 0 {
			return "", fmt.Errorf("no slot %d.%d", ap.Round, ap.Pick)
		}
	}
	// Player resolution: the same matcher the search box uses, taken players included
	// so a re-sent pick resolves to the same id and de-duplicates.
	hits := s.pool.Search(name, nil, 3)
	if len(hits) == 0 || (len(hits) > 1 && s.ambiguous(name, hits[0].Name, hits[1].Name)) {
		s.auto.mu.Lock()
		s.auto.st.Unmatched = append(s.auto.st.Unmatched, name)
		if len(s.auto.st.Unmatched) > 5 {
			s.auto.st.Unmatched = s.auto.st.Unmatched[len(s.auto.st.Unmatched)-5:]
		}
		s.auto.mu.Unlock()
		s.st.Poke()
		return "", fmt.Errorf("unmatched player %q", name)
	}
	p := hits[0]
	before := len(s.st.Snapshot().Picks)
	var (
		pk  state.Pick
		err error
	)
	if live > 0 {
		pk, err = s.st.PickAt(live, p.ID, src)
	} else {
		pk, err = s.st.Pick(p.ID, "", src)
	}
	var conflict *state.Conflict
	switch {
	case err == nil && len(s.st.Snapshot().Picks) == before:
		// state.Pick is idempotent on player: the board re-sent something we have.
		s.auto.mu.Lock()
		s.auto.st.Duplicates++
		s.auto.mu.Unlock()
		return "duplicate", nil
	case err == nil:
		s.auto.mu.Lock()
		s.auto.st.Picks++
		s.auto.mu.Unlock()
		s.log.Info("auto pick", "live", pk.LivePick, "player", p.Name, "source", src)
		s.afterMutation()
		return fmt.Sprintf("#%d %s", pk.LivePick, p.Name), nil
	case errors.As(err, &conflict):
		s.auto.mu.Lock()
		s.auto.st.LastConflict = conflict
		s.auto.st.ConflictNote = fmt.Sprintf("board says #%d is %s; you entered %s", conflict.LivePick, p.Name, s.name(conflict.Existing))
		s.auto.mu.Unlock()
		s.log.Warn("auto pick conflict", "live", conflict.LivePick, "existing", conflict.Existing, "incoming", p.ID)
		s.st.Poke()
		return "conflict", err
	case errors.Is(err, state.ErrTaken):
		s.auto.mu.Lock()
		s.auto.st.Duplicates++
		s.auto.mu.Unlock()
		return "duplicate", nil
	}
	s.log.Warn("auto pick rejected", "player", p.Name, "err", err)
	return "", err
}

func (s *Server) ambiguous(q, a, b string) bool {
	// Two different players both matching by full-name prefix is a real ambiguity;
	// a first-name-only query ("josh") is too. A full name that matches exactly wins.
	return strings.EqualFold(a, q) == false && strings.EqualFold(b, q) == false && len(strings.Fields(q)) < 2
}

func (s *Server) name(id string) string {
	if p, ok := s.pool.ByID[id]; ok {
		return p.Name
	}
	return id
}
