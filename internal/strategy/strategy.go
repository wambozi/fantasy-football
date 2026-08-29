// Package strategy loads data/strategy.yaml: every tuning knob in one place.
package strategy

import (
	"fmt"
	"os"

	"gopkg.in/yaml.v3"

	"github.com/wambozi/draft-copilot/internal/league"
	"github.com/wambozi/draft-copilot/internal/players"
)

type Source struct {
	Weight    float64                      `yaml:"weight"`
	Sigma     bool                         `yaml:"sigma"`
	PosWeight map[players.Position]float64 `yaml:"pos_weight"`
}

// WeightFor returns the source's weight for a position (pos_weight override).
func (s Source) WeightFor(pos players.Position) float64 {
	if w, ok := s.PosWeight[pos]; ok {
		return w
	}
	return s.Weight
}

type ADP struct {
	Sources       map[string]Source           `yaml:"sources"`
	MinSources    int                         `yaml:"min_sources"`
	SigmaFloor    float64                     `yaml:"sigma_floor"`
	SigmaMADScale float64                     `yaml:"sigma_mad_scale"`
	ThinSigma     struct{ Min, Frac float64 } `yaml:"thin_sigma"`
	PosMap        map[string]string           `yaml:"pos_map"`
}

type Need struct {
	StarterOpen    float64 `yaml:"starter_open" json:"starter_open"`
	FlexOpen       float64 `yaml:"flex_open" json:"flex_open"`
	Full           float64 `yaml:"full" json:"full"`
	AtMax          float64 `yaml:"at_max" json:"at_max"`
	DSTBeforeRound int     `yaml:"dst_before_round" json:"dst_before_round"`
	DSTEarlyMult   float64 `yaml:"dst_early_mult" json:"dst_early_mult"`
}

type Engine struct {
	Sims             int                          `yaml:"sims"`
	CandidatePool    int                          `yaml:"candidate_pool"`
	LambdaRank       float64                      `yaml:"lambda_rank"`
	LambdaRegret     float64                      `yaml:"lambda_regret"`
	SurviveThreshold float64                      `yaml:"survive_threshold"`
	Need             Need                         `yaml:"need"`
	FlexShare        map[players.Position]float64 `yaml:"flex_share"`
	// BenchDiscount scales a candidate's (positive) VOR when the pick cannot fill one of
	// MY open starter or flex slots — i.e. it is pure bench. Positions absent from the
	// map are undiscounted. QB2 in a 1-QB league is the canonical case: league-level
	// replacement says it has value, my lineup says it does not.
	BenchDiscount map[players.Position]float64 `yaml:"bench_discount"`
	// From BenchLateRound the bench multiplier switches to BenchDiscountLate. A backup at
	// a one-slot position is worthless mid-draft and is bye insurance in the endgame; one
	// flat number cannot say both.
	BenchLateRound    int                          `yaml:"bench_late_round"`
	BenchDiscountLate map[players.Position]float64 `yaml:"bench_discount_late"`
	// BenchDecay multiplies bench value by decay^k for the k-th bench player at a
	// position (k=0 for the first). Diminishing returns keep the bench diversified
	// instead of stacking the position with the lowest waiver level.
	BenchDecay float64 `yaml:"bench_decay"`
	// WaiverDrafted is how many players THIS room drafts per position (keepers
	// included), measured from its own past boards. It sets the waiver-level index:
	// expected drafted minus already taken. Empty falls back to counting available
	// players with ADP inside the draft, which imports other formats' positional
	// habits — consensus ADP drafts far more QBs and TEs by pick 204 than this room.
	WaiverDrafted map[players.Position]int `yaml:"waiver_drafted"`
}

// BenchFactorAt is BenchFactor for a given round: from bench_late_round the late
// discount applies where one is configured, otherwise the ordinary one does.
func (e Engine) BenchFactorAt(pos players.Position, round int) float64 {
	if e.BenchLateRound > 0 && round >= e.BenchLateRound {
		if v, ok := e.BenchDiscountLate[pos]; ok {
			return v
		}
	}
	return e.BenchFactor(pos)
}

// BenchFactor returns the bench multiplier for pos (1.0 when unset).
func (e Engine) BenchFactor(pos players.Position) float64 {
	if v, ok := e.BenchDiscount[pos]; ok {
		return v
	}
	return 1.0
}

// ManagerTendency is one franchise's measured drafting habits: positional bias relative
// to the league mean, and the round they usually take their first QB/TE/DST. Derived from
// past seasons (data/draft-20*.json joined by data/managers.csv), not hand-tuned.
type ManagerTendency struct {
	QB       float64 `yaml:"qb"`
	RB       float64 `yaml:"rb"`
	WR       float64 `yaml:"wr"`
	TE       float64 `yaml:"te"`
	DST      float64 `yaml:"dst"`
	FirstQB  int     `yaml:"first_qb"`
	FirstTE  int     `yaml:"first_te"`
	FirstDST int     `yaml:"first_dst"`
	// LambdaRank is this manager's measured λ_rank — how steeply they prefer the best
	// available player over a deeper reach — on the same scale as engine.lambda_rank.
	// 0 means unmeasured: the team draws the global value.
	LambdaRank float64 `yaml:"lambda_rank"`
}

// Bias is the multiplier for pos, 1.0 when unknown so a missing team is simply neutral.
func (m ManagerTendency) Bias(pos players.Position) float64 {
	switch pos {
	case players.QB:
		return nz(m.QB)
	case players.RB:
		return nz(m.RB)
	case players.WR:
		return nz(m.WR)
	case players.TE:
		return nz(m.TE)
	case players.DST:
		return nz(m.DST)
	}
	return 1
}

// FirstRound is the round this manager usually takes their first player at pos, or 0 when
// there is no meaningful "first" for that position (RB and WR go in round 1 regardless).
func (m ManagerTendency) FirstRound(pos players.Position) int {
	switch pos {
	case players.QB:
		return m.FirstQB
	case players.TE:
		return m.FirstTE
	case players.DST:
		return m.FirstDST
	}
	return 0
}

func nz(v float64) float64 {
	if v == 0 {
		return 1
	}
	return v
}

// ManagerBias turns the per-team tendencies on. weight 0 falls back to the league-average
// opponent model, which is what every manager got before this existed.
type ManagerBias struct {
	Weight    float64 `yaml:"weight"`
	EarlyDamp float64 `yaml:"early_damp"`
	// LambdaWeight scales the per-team lambda_rank deviations the way Weight scales the
	// positional ones: 0 gives every opponent the global engine.lambda_rank, 1 applies
	// the measured per-team value in full. Separate knobs because the two tendencies
	// were measured by different pipelines and either can be switched off alone.
	LambdaWeight float64 `yaml:"lambda_weight"`
	// AppliesTo: "surplus" (the default) leans only on picks past a team's starter and
	// flex slots. Hoarding is a bench habit — scaling the starter-filling term instead
	// just models an opponent who is BETTER at building a roster, which is the opposite
	// of what the tendency describes. "all" keeps the naive reading measurable.
	AppliesTo string                     `yaml:"applies_to"`
	Teams     map[string]ManagerTendency `yaml:"teams"`
}

// For returns the multiplier to apply to a team's need at pos in this round.
func (b ManagerBias) For(team string, pos players.Position, round int, fillsStarter bool) float64 {
	if b.Weight <= 0 {
		return 1
	}
	if b.AppliesTo != "all" && fillsStarter {
		return 1 // filling a slot they genuinely need is not a habit, it is arithmetic
	}
	t, ok := b.Teams[team]
	if !ok {
		return 1
	}
	// Scale the deviation, not the multiplier: weight 0.5 applies half the observed lean.
	m := 1 + (t.Bias(pos)-1)*b.Weight
	if fr := t.FirstRound(pos); fr > 0 && round < fr && b.EarlyDamp > 0 {
		m *= b.EarlyDamp
	}
	return m
}

// LambdaFor is the λ_rank the opponent model should use for team: the global value
// nudged toward the team's measured one by lambda_weight. Neutral for unmeasured teams
// and at weight 0, exactly like For. Floored well above zero so no configuration can
// make an opponent draw uniformly from the whole candidate pool.
func (b ManagerBias) LambdaFor(team string, global float64) float64 {
	t, ok := b.Teams[team]
	if !ok || b.LambdaWeight <= 0 || t.LambdaRank <= 0 {
		return global
	}
	lam := global + (t.LambdaRank-global)*b.LambdaWeight
	if min := global * 0.25; lam < min {
		return min
	}
	return lam
}

type Gate struct {
	Position            players.Position `yaml:"position"`
	MustDraftByLivePick int              `yaml:"must_draft_by_live_pick"`
	Max                 int              `yaml:"max"`
	Exactly             int              `yaml:"exactly"`
	NotBeforeRound      int              `yaml:"not_before_round"`
	MinCountByLivePick  map[int]int      `yaml:"min_count_by_live_pick"`
	Rationale           string           `yaml:"rationale"`
}

// Keeper encodes the league's keeper economics (spec §14). Year-1 keeper cost is the
// round drafted, floored at CostFloorRound, so every round from the floor onward has
// identical keeper cost and speculation is "free" except for 2026 bench value.
type Keeper struct {
	MaxKeepers     int      `yaml:"max_keepers"`
	CostFloorRound int      `yaml:"cost_floor_round"`
	SurplusWeight  float64  `yaml:"surplus_weight"`      // rounds before the floor; keep 0
	SurplusLate    float64  `yaml:"surplus_weight_late"` // rounds >= floor
	MaxSpeculative int      `yaml:"max_speculative"`     // hard cap on keeper-speculative roster spots
	SpecThreshold  float64  `yaml:"spec_threshold"`      // P(hit) at/above which a pick is "speculative"
	R8Pick         int      `yaml:"round8_overall_pick"` // overall pick a floor-round pick is worth (12 teams × 8)
	Targets        []string `yaml:"targets"`             // optional hand-picked names: P(hit) forced to 1
}

type Config struct {
	Keeper Keeper `yaml:"keeper"`
	Draft  struct {
		SecondsPerPick   int `yaml:"seconds_per_pick"`
		ClockWarnSeconds int `yaml:"clock_warn_seconds"`
	} `yaml:"draft"`
	Roster struct {
		Starters map[players.Position]int `yaml:"starters"`
		Flex     struct {
			Count    int                `yaml:"count"`
			Eligible []players.Position `yaml:"eligible"`
		} `yaml:"flex"`
		Bench      int                      `yaml:"bench"`
		TotalSpots int                      `yaml:"total_spots"`
		Max        map[players.Position]int `yaml:"max"`
	} `yaml:"roster"`
	Scoring struct {
		Reception float64 `yaml:"reception"`
		TEPremium float64 `yaml:"te_premium"`
	} `yaml:"scoring"`
	Objective struct {
		VariancePreference float64 `yaml:"variance_preference"`
		KeeperHorizonBonus float64 `yaml:"keeper_horizon_bonus"`
	} `yaml:"objective"`
	ADP         ADP         `yaml:"adp"`
	Engine      Engine      `yaml:"engine"`
	Gates       []Gate      `yaml:"gates"`
	ManagerBias ManagerBias `yaml:"manager_bias"`
	FlexPolicy  string      `yaml:"flex_policy"`
}

// Load reads and validates strategy.yaml.
func Load(path string) (*Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read strategy: %w", err)
	}
	var c Config
	if err := yaml.Unmarshal(b, &c); err != nil {
		return nil, fmt.Errorf("parse strategy: %w", err)
	}
	if err := c.validate(); err != nil {
		return nil, fmt.Errorf("strategy.yaml: %w", err)
	}
	return &c, nil
}

func (c *Config) validate() error {
	if len(c.ADP.Sources) == 0 {
		return fmt.Errorf("adp.sources is empty")
	}
	if c.ADP.MinSources < 1 || c.ADP.SigmaFloor <= 0 || c.ADP.SigmaMADScale <= 0 {
		return fmt.Errorf("adp min_sources/sigma_floor/sigma_mad_scale must be positive")
	}
	if c.Keeper.SpecThreshold == 0 {
		c.Keeper.SpecThreshold = 0.5
	}
	if c.Keeper.R8Pick == 0 {
		c.Keeper.R8Pick = 96
	}
	if c.Engine.BenchDecay == 0 {
		c.Engine.BenchDecay = 1
	}
	if c.Engine.Sims < 1 || c.Engine.CandidatePool < 1 || c.Engine.LambdaRank <= 0 {
		return fmt.Errorf("engine sims/candidate_pool/lambda_rank must be positive")
	}
	for team, t := range c.ManagerBias.Teams {
		if t.LambdaRank < 0 {
			return fmt.Errorf("manager_bias.teams[%s].lambda_rank must be >= 0", team)
		}
	}
	if len(c.Engine.WaiverDrafted) > 0 {
		// Partial tables are a silent foot-gun: a missing position would pin its waiver
		// level to the best available player and zero out that position's bench value.
		for pos := range c.Roster.Starters {
			if _, ok := c.Engine.WaiverDrafted[pos]; !ok {
				return fmt.Errorf("engine.waiver_drafted is set but missing %s", pos)
			}
		}
	}
	if c.Roster.TotalSpots != sumStarters(c.Roster.Starters)+c.Roster.Flex.Count+c.Roster.Bench {
		return fmt.Errorf("roster.total_spots %d != starters+flex+bench", c.Roster.TotalSpots)
	}
	if _, hasK := c.Roster.Starters[players.K]; hasK {
		return fmt.Errorf("league has no kicker slot; remove K from roster.starters")
	}
	var sum float64
	for _, v := range c.Engine.FlexShare {
		sum += v
	}
	if sum < 0.99 || sum > 1.01 {
		return fmt.Errorf("engine.flex_share must sum to 1, got %.2f", sum)
	}
	return nil
}

func sumStarters(m map[players.Position]int) int {
	n := 0
	for _, v := range m {
		n += v
	}
	return n
}

// LeagueRoster converts the YAML roster section into the league type.
func (c *Config) LeagueRoster() league.Roster {
	return league.Roster{
		Starters:   c.Roster.Starters,
		FlexSlots:  c.Roster.Flex.Count,
		FlexElig:   c.Roster.Flex.Eligible,
		Bench:      c.Roster.Bench,
		TotalSpots: c.Roster.TotalSpots,
		PosMax:     c.Roster.Max,
	}
}
