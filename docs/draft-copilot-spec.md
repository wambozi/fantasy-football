# Draft Copilot — Technical Spec

A single-binary Go service + SPA that tells you who to draft, in under five seconds of reading, on a 60–90 second clock.

---

## 1. Context and goals

**Problem.** 12-team keeper league, snake, 17 rounds. 21 players are already kept, which distorts the board badly — 10 RBs and 3 elite TEs are gone before a live pick happens. Standard cheat sheets and ADP-sorted lists are wrong in this environment because they assume a full player pool. During the draft there is no time to cross-reference a strategy doc against a live board.

**Goal.** At any moment during the draft, answer one question in a glance: *given who's gone, who I have, and how many picks until my next turn, who should I take?*

**Non-goals.**
- Not a general-purpose fantasy platform. One league, one season, one user.
- Not multi-user. No auth, no accounts.
- Not cloud-hosted. Runs on a laptop on the same LAN as a phone.
- Not authoritative. FanDraft remains the system of record for the actual draft.

**Constraints.**
- Total data: ~600 players, ~204 draft slots. The entire problem fits in memory.
- Draft night reliability matters more than elegance. Every dependency is a thing that can fail at 7pm.
- Single developer, evenings, one week.
- **Draft is in person** (Saturday Aug 29, 6:30 PM) but run through FanDraft's web app, with a remote option for owners who can't attend. The live board will be open in a browser on your laptop, so automated ingestion is possible — see §9.

---

## 1.1 League configuration (confirmed)

League: *That'll move the chains!!!!!!!!!* — ESPN, ID 557790, 12 teams, head-to-head points, league-manager format.

**Roster — 17 spots, and the draft fills them exactly.**

| Slot | Starters | Position max |
|---|---|---|
| QB | 1 | 4 |
| RB | 2 | 9 |
| WR | 2 | 8 |
| TE | 1 | 3 |
| FLEX (RB/WR/TE) | 2 | — |
| D/ST | 1 | 3 |
| Bench | 8 | — |
| IR | 1 | — |
| **K** | **0** | **none** |

9 starters + 8 bench = 17. Two are keepers, so **15 live picks fill exactly 15 remaining spots.** There is no room for a throwaway pick — every selection occupies a roster spot you'll carry into week 1.

**Scoring — full PPR.**

- Receptions: 1.0 (this is the single most important config fact)
- Receiving yards 0.1, receiving TD 6, 2PT 2
- Fumbles lost −2
- Any return TD 6
- D/ST: sack 1, INT 2, FR 2, safety 2, any TD 6, blocked kick 2; points-allowed and yards-allowed tiers both scored, with negatives down to −7
- Kicking values are still defined in settings but are vestigial — there is no K roster slot

**Playoffs: 8 of 12 teams qualify, 1-week matchups, no reseeding, seeding tiebreak on Total Points For.**

**Keepers: 2 per team in 2026 *and* 2027.** Lock one hour before draft.

**In-season:** FAAB with no season acquisition limit, 1-day waivers, waiver order resets weekly to inverse standings. Trades unlimited, deadline 11/18, 7 votes to veto.

### Strategic consequences

These follow from the config and should be encoded, not left to memory:

1. **Full PPR flattens the RB/WR flex distinction.** Pass-catching backs and slot receivers are near-interchangeable for the 2 flex slots. The RB-first opening still holds — it's driven by the ten kept RBs, not by format — but flex slots should go to best-available pass catcher, never to a forced position.
2. **67% playoff odds change the objective function.** Making the playoffs is nearly free. What's scarce is winning a single-elimination, one-week bracket. That argues for **ceiling over floor** on every close call: prefer the volatile high-upside player to the safe 12-points-every-week guy. Encode as a variance preference in the scoring function, not just expected points.
3. **Total Points For is the seeding tiebreak**, which mildly reinforces the same thing — accumulate points, don't manage to a floor.
4. **Two keepers again in 2027.** A late-round pick who breaks out has value beyond this season. On genuinely close calls, break the tie toward youth and ascending role.
5. **Only one mandatory low-value pick (D/ST) and no kicker.** Your endgame is one round of obligation instead of two, so the last several picks are all upside swings.
6. **8 bench spots against 9 starters** is a shallow bench for a 12-team league. Handcuffs and dart throws compete directly with usable depth. Don't spend more than one or two spots on pure lottery tickets.

---

## 2. Architecture

```
┌─────────────────────────────────────────────────────┐
│  Browser (laptop) / Phone (LAN)                     │
│  ┌───────────────────────────────────────────────┐  │
│  │  SPA — Preact + TS + Vite                     │  │
│  │  • recommendation panel   • player search      │  │
│  │  • roster + board state   • undo               │  │
│  └───────────▲───────────────────────┬───────────┘  │
└──────────────│───────────────────────│──────────────┘
               │ SSE (state push)      │ POST /api/pick
               │                       │
┌──────────────┴───────────────────────▼──────────────┐
│  Go service (single binary, embed.FS for SPA)       │
│                                                      │
│  ┌────────────┐  ┌──────────────┐  ┌─────────────┐  │
│  │ DraftState │──│ Recommender  │──│ BriefCache  │  │
│  │ (in-mem)   │  │ • MonteCarlo │  │ (Claude,    │  │
│  │            │  │ • VOR        │  │  speculative)│ │
│  └─────┬──────┘  └──────────────┘  └──────┬──────┘  │
│        │                                   │         │
│  ┌─────▼───────────┐              ┌────────▼──────┐ │
│  │ events.jsonl    │              │ Anthropic API │ │
│  │ (append-only)   │              │ (optional)    │ │
│  └─────────────────┘              └───────────────┘ │
└──────────────────────────────────────────────────────┘
               ▲
               │ POST /api/pick (optional)
┌──────────────┴──────────────────────────────────────┐
│  Userscript in FanDraft tab (Phase 5, optional)      │
│  MutationObserver → diff → localhost:8080            │
└──────────────────────────────────────────────────────┘
```

**Why no database.** State is 204 events. An append-only JSONL log rebuilt on boot gives free undo, free crash recovery, and free replay for tests. Postgres adds an external process that can be down when it matters and buys nothing at this scale. If you later want SQL for pre-draft analysis, use `modernc.org/sqlite` — pure Go, no cgo, single file.

---

## 3. Data model

### 3.1 Static inputs (loaded at boot, never mutated)

```go
type Position string // QB, RB, WR, TE, DST, K

type Player struct {
    ID          string    // slug: "jeremiyah-love-ari"
    Name        string
    Team        string
    Pos         Position
    Bye         int
    ADPMean     float64   // consensus across sources
    ADPStdDev   float64   // computed from per-source spread
    ADPSources  []float64 // raw per-source values, for diagnostics
    ProjPoints  float64   // season projection in league scoring; 0 if unavailable
    Tier        int       // optional, from tiered rankings
}

type DraftSlot struct {
    Round      int
    PickInRound int
    Team       string
    KeeperID   string // "" if live pick
}

type League struct {
    Teams          []string
    MyTeam         string
    Slots          []DraftSlot
    StarterSlots   map[Position]int  // QB:1 RB:2 WR:2 TE:1 DST:1
    FlexSlots      int               // 2, eligible RB/WR/TE
    BenchSlots     int
    Scoring        ScoringConfig
}
```

**Derived at boot, not hardcoded:** the live-pick sequence. Walk `Slots` in order, skip any with a `KeeperID`, and number the remainder 1..N. `League.MyLivePicks` falls out of this. For the current setup that yields **#8, #11, #26, #49, #65, #68, #85, #90, …** — but if a keeper changes or a pick is traded, re-deriving keeps everything correct.

### 3.2 Mutable state

```go
type Pick struct {
    Seq       int       // monotonic
    LivePick  int       // 1-indexed live pick number
    SlotIdx   int       // index into League.Slots
    Team      string
    PlayerID  string
    At        time.Time
}

type DraftState struct {
    mu        sync.RWMutex
    Picks     []Pick
    Taken     map[string]string // playerID -> team
    Rosters   map[string][]string
    version   int               // bumped on every mutation, drives SSE
}
```

### 3.3 Event log

`events.jsonl`, one JSON object per line, append-only:

```json
{"seq":1,"type":"pick","live_pick":1,"team":"Patient Zeros","player_id":"justin-jefferson-min","at":"2026-08-30T19:02:11Z"}
{"seq":2,"type":"undo","undoes":1,"at":"2026-08-30T19:02:19Z"}
```

Rebuild on boot by replaying. Undo is a new event, never a deletion — you want the audit trail when something goes wrong mid-draft.

---

## 4. ADP ingestion

Source: FootballGuys consensus ADP export (or FantasyPros). The value is the **per-source columns**, not the consensus number — that spread is what makes the survival model work.

Pipeline (`cmd/ingest`):
1. Parse CSV → per-player slice of source ADPs.
2. Drop sources with >40% missing coverage.
3. `ADPMean` = median across sources (median, not mean — resistant to one outlier site).
4. `ADPStdDev` = 1.4826 × MAD, floored at 3.0 picks. The floor matters: a player every site ranks identically still has real draft-day variance, and a σ of 0.5 makes the model absurdly confident.
5. Emit `players.json`.

**Projections.** If you can get season point projections in your scoring format, load them — VOR is much sharper with real points. If not, fall back to a fitted value curve: for each position, fit `points ≈ a · exp(-b · pos_rank) + c` against any partial projection data, or use last season's finish-order points as the shape. Flag clearly in the UI which mode is active, because ADP-only VOR is materially weaker and you should know when you're trusting it.

**Refresh.** Re-run ingest the morning of the draft. ADP moves on injury news and preseason week 3.

---

## 5. The recommendation engine

This is the part with real subtlety. Everything else is plumbing.

### 5.1 Replacement level (dynamic)

Static positional baselines ("RB24 is replacement") are wrong in a keeper league because the starter demand has already been partly satisfied. Compute it live:

```
For position P:
  remaining_demand(P) = Σ over all teams of (required starters at P - already rostered at P)
                        + flex_share(P) × remaining flex slots
  replacement(P)      = ProjPoints of the remaining_demand(P)-th best available player at P
  VOR(p)              = p.ProjPoints - replacement(p.Pos)
```

`flex_share` splits remaining flex demand by how often each position historically fills flex in this league's scoring — RB/WR heavy, TE light. Recompute on every pick. This is what makes the RB cliff appear in the numbers automatically instead of you having to remember it's there.

### 5.2 Survival probability (Monte Carlo)

For each available player, how likely are they to still be there at your next turn?

```
h = your_next_live_pick - current_live_pick     // picks until you're up

for sim in 1..N (N = 2000):
    board = copy(available)
    for each of the h upcoming opposing picks:
        team = team_on_clock(pick)
        candidates = top 40 of board by (ADPMean + noise), where
            noise ~ Normal(0, ADPStdDev)
        weight each candidate by:
            w = exp(-λ · adjusted_rank) × need_multiplier(team, candidate.Pos)
        draw one by weight, remove from board
    record which players survived

p_survive(p) = survivals(p) / N
```

**Why Monte Carlo and not a closed form.** A normal-CDF approximation is faster but treats picks as independent, so it cannot represent a positional run. Positional runs are precisely the failure mode in this draft — ten kept RBs guarantees one in round 1. Simulating opposing teams with need-awareness captures it. At 600 players × 2000 sims × ~25 picks of horizon, this is single-digit milliseconds in Go. Run it on every state change and cache the result against `state.version`.

`need_multiplier` should be modest (1.0 → ~2.5), not absolute. Managers reach and go best-available; don't model them as perfectly rational.

### 5.3 Scoring

```
For each available player p:
    immediate  = VOR(p)
    fallback   = VOR of the best player at p.Pos expected available at my next pick
                 (= highest-VOR player at that position with p_survive > 0.5)
    regret(p)  = (1 - p_survive(p)) × (VOR(p) - fallback)
    score(p)   = immediate + λ_regret × regret(p)
```

`λ_regret` defaults to 1.0. Higher makes it more aggressive about scarcity, lower makes it more BPA. Expose it as a slider — you may want to feel it out during mock drafts.

### 5.4 Strategy gates

Applied as hard constraints on top of the score, configurable in `strategy.yaml`:

```yaml
roster:
  starters: {QB: 1, RB: 2, WR: 2, TE: 1, DST: 1}
  flex: {count: 2, eligible: [RB, WR, TE]}
  bench: 8
  total_spots: 17
  # No kicker slot in this league.

scoring:
  reception: 1.0          # full PPR
  te_premium: 0.0

objective:
  variance_preference: 0.15   # >0 favors ceiling; 8/12 teams make a 1-week bracket
  keeper_horizon_bonus: 0.05  # tiebreak toward youth; 2 keepers again in 2027

gates:
  - position: QB
    must_draft_by_live_pick: 68
    max: 2
    rationale: "0 QBs kept; 12 teams × 1 QB. QB12 by ADP is ~Stafford/Purdy."
  - position: TE
    must_draft_by_live_pick: 49
    max: 2
    rationale: "Bowers/Loveland/Warren kept. Kraft/LaPorta/Pitts/Fannin tier lands here."
  - position: DST
    exactly: 1
    not_before_round: 16
    rationale: "One slot, no kicker. Only mandatory endgame pick."
  - position: RB
    min_count_by_live_pick: {26: 2}
    rationale: "10 RBs kept. Two startable RBs by end of round 3 or you stream all year."

flex_policy: best_available   # RB/WR/TE by VOR — never force a position into flex
```

**Note on `variance_preference`.** With 8 of 12 teams reaching a single-elimination bracket, expected points is the wrong sole objective — you're optimizing for a high-scoring week in December, not a stable median. Implement as a bonus proportional to the player's outcome spread (use ADP σ as a rough proxy for uncertainty of role, or a boom/bust rate if your projection source provides one). Keep the coefficient small; this is a tiebreaker between comparable players, not a mandate to draft chaos.

When a gate is about to bind, the UI escalates: the recommendation panel shows a red band reading `QB GATE: 3 picks left` and the top recommendation is filtered to that position.

### 5.5 Output contract

```go
type Recommendation struct {
    Player      Player
    Score       float64
    VOR         float64
    PSurvive    float64   // to my next pick
    Reasons     []string  // ["71% gone by #49", "RB2 need", "tier break after him"]
    GateForced  bool
}

type Advice struct {
    OnClock      bool
    LivePick     int
    NextLivePick int
    PicksUntil   int
    Top          []Recommendation   // 3
    ByPosition   map[Position]Recommendation
    Warnings     []string           // gate pressure, roster holes, bye stacking
}
```

---

## 6. HTTP API

All JSON, no auth, bound to `0.0.0.0:8080` so your phone can reach it.

| Method | Path | Purpose |
|---|---|---|
| `GET` | `/api/state` | Full draft state + current `Advice` |
| `GET` | `/api/stream` | SSE; pushes `Advice` on every version bump |
| `POST` | `/api/pick` | `{player_id, team?}` — team defaults to whoever is on the clock |
| `POST` | `/api/undo` | Reverts last pick |
| `GET` | `/api/search?q=` | Fuzzy player search over available players |
| `GET` | `/api/brief` | Latest Claude brief for the current state (may be `202` if still generating) |
| `GET` | `/healthz` | Liveness |

`POST /api/pick` is idempotent on `(live_pick, player_id)` so a duplicate submit from the userscript and the manual box can't double-advance the board.

---

## 7. Claude integration

**Role: colour commentary, never the critical path.** The deterministic engine is authoritative. Claude writes the two-sentence "here's the shape of this decision" brief that's faster to read than a table of numbers.

**Speculative generation.** When a pick lands and you are within 4 picks of your turn, kick off a background generation for the *projected* state at your turn. Store in `BriefCache` keyed by state version. By the time you're on the clock it's already rendered. If it hasn't returned or the call failed, the UI shows engine reasons only and nothing is blocked.

**Auth.** This needs an API key from the Claude Console (`console.claude.com`) in `ANTHROPIC_API_KEY`. A Claude.ai Pro/Max subscription does not authenticate the Messages API — separate product, separate billing. If the key is absent, the service starts normally with briefs disabled.

**Endpoint.** `POST https://api.anthropic.com/v1/messages`, headers `x-api-key` and `anthropic-version: 2023-06-01`.

**Model.** `claude-sonnet-5` for quality. If briefs feel slow even with prefetch, drop to `claude-haiku-4-5-20251001`. Verify current model IDs against `https://platform.claude.com/docs/en/about-claude/models/overview` at build time — IDs are pinned snapshots, not evergreen pointers.

**Prompt caching.** League rules, strategy doc, and roster construction principles go in a cached system block. Only the live board state varies. Cache reads bill at a fraction of standard input.

**Prompt shape:**

```
System (cached):
  You are a draft assistant for a 12-team keeper league.
  <league rules, scoring, roster requirements>
  <strategy.yaml rendered as prose>
  Output at most 3 bullets. Each under 15 words. No preamble.
  The numeric engine has already ranked players — do not re-rank.
  Explain the *shape* of the decision and flag anything the numbers miss
  (injury news, bye collisions, handcuff logic, opponent tendencies).

User:
  <JSON: current pick, picks until next turn, my roster,
         top 8 candidates with VOR / p_survive / reasons,
         positional scarcity summary, active gates>
```

Constrain hard on brevity. A three-paragraph answer is useless at 60 seconds.

**Cost.** A few hundred calls of a couple thousand tokens each. Under a dollar for the whole draft.

---

## 8. Frontend

**Stack.** Preact + TypeScript + Vite. Built assets embedded via `embed.FS` so the binary is self-contained.

**One screen. No scrolling. No tabs.**

```
┌──────────────────────────────────────────────────────────┐
│  LIVE PICK 24  ·  YOU'RE UP IN 2  ·  ⚠ TE GATE IN 25    │  ← status bar
├──────────────────────────────────────────────────────────┤
│  1  Quinshon Judkins   RB  CLE      VOR 41.2             │
│     71% gone by #49 · fills RB2 · tier break after       │
│                                                           │
│  2  Bucky Irving       RB  TB       VOR 38.7             │
│  3  Jaylen Waddle      WR  DEN      VOR 36.1             │
├──────────────────────────────────────────────────────────┤
│  Claude: RB run started — three off the board since your │
│  last pick. Waddle survives to #49; Judkins won't.       │
├──────────────────────────────────────────────────────────┤
│  BEST BY POS   QB Herbert · RB Judkins · WR Waddle       │
│                TE Kraft · DST —                          │
├──────────────────────────────────────────────────────────┤
│  [ search: jud▌                    ]   ⌫ undo            │
│    → Quinshon Judkins  RB CLE                             │
├──────────────────────────────────────────────────────────┤
│  MY ROSTER  QB– RB1 RB– WR:JSN,Odunze TE– FLEX– DST–     │
└──────────────────────────────────────────────────────────┘
```

**Interaction rules:**
- Search box is autofocused always, and refocuses after every action. You should never touch the mouse.
- Type 3 characters → arrow keys → Enter marks a player taken by the team on the clock. Target: under 2 seconds per opposing pick.
- `Cmd/Ctrl+Z` for undo, globally bound.
- `1` / `2` / `3` drafts the corresponding recommendation to your own roster when you're on the clock.
- When you're on the clock, the whole status bar changes colour. Peripheral vision should tell you.

**Design.** Dark, high contrast, large type — you'll be reading this on a phone at arm's length in someone's living room. Numbers are the interface; keep chrome to nearly zero. Never animate a transition on the recommendation panel; a moving list is unreadable under time pressure.

---

## 9. FanDraft ingestion (Phase 6)

FanDraft is a web app at `fandraft.app`. The draft is run in person by the commissioner, but remote owners share the identical live board, so **you will have the live board open in a browser on your laptop.** That makes automated ingestion viable again.

Three input paths, in descending order of preference. All of them write to the same idempotent `POST /api/pick`.

### 9.1 Recon first — do this before writing any code

Run a mock draft on FanDraft (the free tier allows unlimited mocks, capped at two rounds — enough to capture a pick event) and inspect how picks arrive:

1. DevTools → Network → **WS**. A live shared board is almost certainly WebSocket-driven. Watch the frames as a pick lands and capture the payload shape.
2. If there's no WebSocket, check XHR/Fetch for a polled JSON endpoint.
3. Only if both come up empty, fall back to reading the DOM.

**Do this tonight.** Everything below branches on what you find, and you have two days.

### 9.2 Preferred — WebSocket interception

Far more stable than DOM scraping: the wire format is a real contract, while CSS selectors are incidental. Inject at `document_start`, before the app opens its socket:

```js
// ==UserScript== @run-at document-start @match https://fandraft.app/* ==/UserScript==
const Native = window.WebSocket;
window.WebSocket = function (...args) {
  const ws = new Native(...args);
  ws.addEventListener('message', (e) => {
    GM_xmlhttpRequest({
      method: 'POST',
      url: 'http://localhost:8080/api/fandraft/frame',
      headers: {'Content-Type': 'application/json'},
      data: JSON.stringify({raw: e.data, at: Date.now()}),
    });
  });
  return ws;
};
window.WebSocket.prototype = Native.prototype;
```

The backend parses frames in `internal/ingest/fandraft`, discards everything that isn't a pick event (chat, clock ticks, ticker updates), and maps player names to your `Player.ID` via the same fuzzy matcher the search box uses. Unmatched names raise a UI warning rather than being silently dropped.

**Use `GM_xmlhttpRequest`, not `fetch`.** It bypasses both CORS and mixed-content restrictions. A plain `fetch` from an HTTPS page to `http://localhost` usually works in Chrome (localhost is treated as a trustworthy origin) but "usually" is not a property you want on draft night.

### 9.3 Fallback — DOM MutationObserver

If there's no usable socket, observe the board container and diff rows. Same POST target, same de-duplication by pick key. Selectors need re-verification the day of.

### 9.4 Always — manual entry

The search box remains the source of truth and must be sufficient on its own. Automation is an accelerator. Test a full mock with automation *disabled* and confirm you can keep up by hand.

### 9.5 Reconciliation

Two input paths mean two chances to disagree. `POST /api/pick` is idempotent on `(live_pick, player_id)`, so a duplicate from both sources is harmless. Beyond that:

- Maintain a `source` field on each pick event (`manual` | `ws` | `dom`).
- If an automated event arrives for a live pick that manual entry already filled with a *different* player, do not overwrite. Raise a conflict banner and let the user resolve it. Silent divergence mid-draft is the worst failure mode in the system.
- Show a small freshness indicator: seconds since the last automated event. If the scraper dies, you want to notice within one pick, not five.

### 9.6 Setup import

FanDraft can pull league setup from ESPN, Yahoo, Sleeper, CBS, Fantrax, MFL and Fleaflicker, so the draft order and keeper assignments in FanDraft should already match your ESPN league. Export or screenshot the FanDraft order and diff it against `draft-order.csv` before the draft — a mismatch there invalidates every live-pick number the engine computes.

---

## 10. Testing

**Unit:**
- Live-pick derivation against the real draft CSV. Assert `MyLivePicks[0..7] == [8,11,26,49,65,68,85,90]`.
- Replacement level: as picks fill starter slots, `replacement(RB)` must move monotonically down the board.
- Survival: `p_survive` must be monotonically decreasing in `h`, and → 1.0 as `h` → 0.

**Simulation harness (`cmd/simdraft`).** The highest-value piece of test code in the project.

Run 500 full mock drafts where 11 opponents draft by noisy ADP with need-weighting and your seat is drafted by the engine. Then assert on aggregate invariants:

- ≥95% of drafts end with 2+ startable RBs by live pick 26
- 100% of drafts have a QB by live pick 68
- 100% have a TE by live pick 49
- 100% end with exactly one D/ST, drafted in round 16 or 17
- 100% never draft a kicker (no slot exists; guard against a stray K in the player pool)
- 100% end with exactly 15 drafted players and every starter slot fillable
- 0% exceed a position max (QB 4, RB 9, WR 8, TE 3, DST 3)
- Median roster VOR beats a naive best-ADP-available baseline
- Flex slots are filled by whichever of RB/WR/TE had the higher VOR, not by a fixed position

Bugs in the survival math are invisible in a single draft and obvious across 500. Run this before draft night, not on it.

**Dress rehearsal.** Do one full mock with the real UI on your actual phone, timing yourself. If entering an opposing pick takes more than 2 seconds, the UI has failed regardless of how good the engine is.

---

## 11. Build and run

```
draft-copilot/
├── cmd/
│   ├── server/       # main binary
│   ├── ingest/       # ADP CSV → players.json
│   └── simdraft/     # simulation harness
├── internal/
│   ├── league/       # config, slot/live-pick derivation
│   ├── state/        # DraftState, event log
│   ├── engine/       # VOR, montecarlo, scoring, gates
│   ├── brief/        # Anthropic client, cache, prefetch
│   └── httpapi/      # handlers, SSE
├── web/              # Preact SPA
├── data/
│   ├── players.json
│   ├── draft-order.csv
│   └── strategy.yaml
└── Makefile
```

```bash
make ingest    # data/adp.csv -> data/players.json
make build     # vite build -> embed -> single binary
make sim       # 500 mock drafts, print invariant report
./draft-copilot --port 8080 --data ./data
```

Go 1.22+, stdlib `net/http` only for routing. Dependencies limited to a fuzzy-match library and a YAML parser.

---

## 12. Milestones

| Phase | Deliverable | Done when |
|---|---|---|
| **M1** | Ingest + state + event log + manual pick entry, no UI | `curl POST /api/pick` advances the board; restart replays correctly |
| **M2** | Engine: VOR, Monte Carlo survival, scoring, gates | `GET /api/state` returns sane top-3 for a mid-draft fixture |
| **M3** | SPA + SSE + keyboard flow | Full mock draft entered by hand in under 2s/pick |
| **M4** | Simulation harness | 500 drafts, all invariants green |
| **M5** | Claude briefs with speculative prefetch | Brief is present the moment you're on the clock, ≥90% of turns |
| **M6** | FanDraft ingestion (§9) | A mock draft's picks land in the tool with no typing, and killing the userscript mid-mock degrades cleanly to manual |

M1–M4 is the shippable product. M5 and M6 are enhancements that must never become load-bearing.

**Do the §9.1 recon tonight**, before M1. It's fifteen minutes in DevTools and it determines whether M6 is an afternoon or a dead end — you want to know that while there's still time to spend the hours elsewhere.

**Timeline:** the draft is Saturday Aug 29 at 6:30 PM. Freeze features Friday night. Whatever isn't working by then, cut — a tool you half-trust is worse at the table than a tool that does less but is right.

---

## 13. Trade-offs and known weaknesses

**ADP-derived value without projections is the weakest link.** VOR needs points. If projections aren't available in your scoring format, the fitted curve is a real approximation and the recommendations degrade from "correct" to "reasonable." Getting projections is the single highest-leverage data improvement.

**Opponent model is crude.** Noisy-ADP-plus-positional-need approximates a room of humans, but your leaguemates have tendencies — the guy who always takes his own team's players, the one who panics on QB in round 5. If you have last year's draft log, fitting per-manager positional biases would sharpen `p_survive` meaningfully.

**No trade handling.** If picks get traded during the draft, `draft-order.csv` is stale. Mitigation: an admin endpoint to reassign a slot's team, which triggers live-pick re-derivation.

**σ floor is a judgment call.** Set at 3.0 picks. Too low and the model is overconfident about scarcity and makes you reach; too high and everything looks equally likely to survive and the regret term goes flat. Worth tuning against the sim harness.

**What I'd revisit as it grows.** If you ran this for multiple leagues or multiple seasons, the in-memory-plus-JSONL model stops being the right call and SQLite earns its place. For one draft, it does not.

---

## 14. Addendum (2026-08-27): keeper economics

**Sequenced after M2–M4 are green; cut if not done by the Friday freeze.**

Rules: keep up to 2. Year-1 keeper costs the round drafted, floored at round 8. Year-2+ keeper costs a 1st or 2nd. Current keepers JSN (R4) and Odunze (R5) escalate to 1st/2nd in 2027.

Derived: rounds 8–17 have identical keeper cost. Ten of my fifteen picks (#68, 85, 90, 109, 114, 133, 138, 157, 162, 181) are in that flat zone. Rounds 1–3 scale 1:1; rounds 6–7 are slightly worse than 8.

`strategy.yaml`:
```yaml
keeper:
  max_keepers: 2
  cost_floor_round: 8
  escalation: [1st_or_2nd]     # year 2+
  surplus_weight: 0.0          # rounds 1–7: no arbitrage, keep at zero
  surplus_weight_late: <tune>  # rounds 8+ only
  max_speculative: 3           # hard cap on keeper-speculative roster spots
```

`keeper_surplus(p)`, rounds ≥ 8 only: P(p is a top-2 keeper candidate in 2027) × (expected 2027 value − value of a round-8 pick). P(...) driven by the annotation fields (age — weighted heavily because of the year-2 escalation — target-share trend, NFL draft capital, ascending role).

Tests/invariants: the term must never move a player up at #49 or #65 (assert); no simulated draft ends with more than `max_speculative` keeper-speculative players.

UI: show "keeper: R8" on every recommendation from round 8; flag keeper-speculative candidates distinctly from 2026 contributors.

Reviewer notes: (1) Because an R8-cost hit dominates a 1st/2nd-cost JSN/Odunze, P(top-2) ≈ P(player hits) — any real breakout displaces an escalating keeper. (2) Within the flat zone the opportunity cost is already captured by VOR falling with round, so no explicit "later is better" term is needed. (3) P(...) depends on the nflverse annotations; a v1 without them is a curated `keeper.targets:` list in strategy.yaml plus the fixed "keeper: R8" label.
