// Package players defines the static player pool loaded at boot.
package players

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"regexp"
	"strings"
)

// Position is a roster position. K is recognised only so it can be rejected.
type Position string

const (
	QB  Position = "QB"
	RB  Position = "RB"
	WR  Position = "WR"
	TE  Position = "TE"
	DST Position = "DST"
	K   Position = "K"
)

// Draftable reports whether the position can occupy a roster spot in this league.
// There is no kicker slot, so K is never draftable.
func (p Position) Draftable() bool {
	switch p {
	case QB, RB, WR, TE, DST:
		return true
	}
	return false
}

// Player is a static, never-mutated entry in the pool.
type Player struct {
	ID         string    `json:"id"`
	Name       string    `json:"name"`
	Team       string    `json:"team"`
	Pos        Position  `json:"pos"`
	Bye        int       `json:"bye"`
	ADPMean    float64   `json:"adp_mean"`
	ADPStdDev  float64   `json:"adp_stddev"`
	ADPSources []float64 `json:"adp_sources,omitempty"`
	ProjPoints float64   `json:"proj_points"`
	// ECR is the expert-consensus rank (FantasyPros AVG), 0 if absent. The gap
	// between ECR and ADPMean is the "room undervalues them" signal.
	ECR float64 `json:"ecr,omitempty"`
	// ECRPos is the positional rank from the ECR export (WR12 -> 12), 0 if absent.
	ECRPos int `json:"ecr_pos,omitempty"`
	// ProjSource says where ProjPoints came from: "projection" (a real forecast) or
	// "finish-curve" (last season's finish-order points assigned by ECR rank).
	ProjSource string `json:"proj_source,omitempty"`
	// Keeper-economics inputs from the FantasyPros dynasty/rookie rankings.
	Age         int `json:"age,omitempty"`
	DynastyRank int `json:"dynasty_rank,omitempty"` // overall dynasty ECR, 0 if unranked
	RookieRank  int `json:"rookie_rank,omitempty"`  // 2026 rookie ECR, 0 if not a rookie
	// Last is last season's actuals, for reason strings. nil if the player had none.
	Last *SeasonStats `json:"last,omitempty"`
	Tier int          `json:"tier,omitempty"`
	// Keeper is true when the player was rostered before pick one.
	Keeper bool `json:"keeper,omitempty"`
}

// SeasonStats is one season of actuals in league (full-PPR) scoring.
type SeasonStats struct {
	Season int     `json:"season"`
	Points float64 `json:"points"`
	Games  int     `json:"games"`
	TgtPct float64 `json:"tgt_pct,omitempty"` // team target share, WR/TE only
}

// Pool is the loaded player set, indexed by ID.
type Pool struct {
	Players []*Player
	ByID    map[string]*Player
	// ProjMode is ProjReal, ProjCurve or ProjNone (see curve.go). Set at load/curve time.
	ProjMode string
}

// Load reads players.json. A missing file yields an empty pool (M1 runs without ADP data);
// any other error is returned.
func Load(path string) (*Pool, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return &Pool{ByID: map[string]*Player{}}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read players: %w", err)
	}
	var list []*Player
	if err := json.Unmarshal(b, &list); err != nil {
		return nil, fmt.Errorf("parse players: %w", err)
	}
	p := &Pool{ByID: make(map[string]*Player, len(list))}
	for _, pl := range list {
		if !pl.Pos.Draftable() {
			// Guard against a stray kicker (or unknown position) ever entering the board.
			continue
		}
		if _, dup := p.ByID[pl.ID]; dup {
			return nil, fmt.Errorf("duplicate player id %q", pl.ID)
		}
		p.ByID[pl.ID] = pl
		p.Players = append(p.Players, pl)
	}
	return p, nil
}

// Add inserts a player not present in the source data (e.g. a keeper missing from ADP).
func (p *Pool) Add(pl *Player) {
	if _, ok := p.ByID[pl.ID]; ok {
		return
	}
	p.ByID[pl.ID] = pl
	p.Players = append(p.Players, pl)
}

var nonSlug = regexp.MustCompile(`[^a-z0-9]+`)

// Slug builds the canonical ID: "jahmyr-gibbs-det". Team is normalised via NormTeam.
func Slug(name, team string) string {
	n := strings.ReplaceAll(NameKey(name), " ", "-")
	return n + "-" + strings.ToLower(NormTeam(team))
}

// NormalizeName turns "Last, First" into "First Last" and strips suffixes/punctuation
// so ESPN, FootballGuys and FanDraft spellings collapse to one key.
func NormalizeName(name string) string {
	name = strings.TrimSpace(name)
	if i := strings.Index(name, ","); i >= 0 {
		name = strings.TrimSpace(name[i+1:]) + " " + strings.TrimSpace(name[:i])
	}
	fields := strings.Fields(name)
	out := fields[:0]
	for _, f := range fields {
		switch strings.ToLower(strings.TrimRight(f, ".")) {
		case "jr", "sr", "ii", "iii", "iv", "v":
			continue
		}
		out = append(out, f)
	}
	return strings.Join(out, " ")
}

// NameKey is the exact-match key: lowercase normalised name with punctuation removed,
// so "Ja'Marr Chase", "JaMarr Chase" and "Chase, Ja'Marr" all collapse to one key.
func NameKey(name string) string {
	n := strings.ToLower(NormalizeName(name))
	n = strings.ReplaceAll(n, "'", "") // apostrophes join, they don't separate: Ja'Marr -> jamarr
	n = strings.Trim(nonSlug.ReplaceAllString(n, " "), " ")
	// Sites disagree on nicknames ("Ken Walker" vs "Kenneth Walker"); canonicalise the first name.
	if i := strings.IndexByte(n, ' '); i > 0 {
		if full, ok := nicknames[n[:i]]; ok {
			n = full + n[i:]
		}
	}
	return n
}

// nicknames maps short first names to the long form used as the canonical key.
var nicknames = map[string]string{
	"ken": "kenneth", "mike": "michael", "matt": "matthew", "chris": "christopher",
	"josh": "joshua", "zach": "zachary", "nick": "nicholas", "alex": "alexander",
	"cam": "cameron", "dan": "daniel", "will": "william", "tom": "thomas",
	"jon": "jonathan", "ben": "benjamin", "rob": "robert", "jake": "jacob",
}

// teamAliases maps the various abbreviations sites use onto one canonical form.
var teamAliases = map[string]string{
	"SFO": "SF", "KCC": "KC", "LVR": "LV", "TBB": "TB", "GBP": "GB", "NEP": "NE",
	"NOS": "NO", "JAC": "JAX", "WSH": "WAS", "LAR": "LAR", "LAC": "LAC", "OAK": "LV",
	"SD": "LAC", "STL": "LAR", "ARZ": "ARI", "HST": "HOU", "BLT": "BAL", "CLV": "CLE",
}

// NormTeam canonicalises an NFL team abbreviation.
func NormTeam(t string) string {
	t = strings.ToUpper(strings.TrimSpace(t))
	if a, ok := teamAliases[t]; ok {
		return a
	}
	return t
}

// NormPos canonicalises a position string ("D/ST", "DEF" -> DST, "PK" -> K).
func NormPos(s string) Position {
	s = strings.ToUpper(strings.TrimSpace(s))
	switch s {
	case "D/ST", "DEF", "DST", "D":
		return DST
	case "PK", "K":
		return K
	}
	return Position(s)
}
