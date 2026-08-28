# Handoff: Draft Copilot v2 — dense draft-night UI

## Overview

The draft copilot's web UI (`web/index.html`, `web/app.js`, `web/style.css` in the
`draft-copilot` Go repo) is a single 760px-wide dark column built for a phone. This
handoff replaces it with a full-width, two-column laptop layout in a newsprint visual
system, and adds two features the current UI does not have:

1. **A pick rail** down the right side — every pick made, newest first, with round
   dividers, the team that made it, VOR at that pick, and how far the player fell past
   or was reached ahead of ADP. Above it, an on-deck block naming the next five seats,
   the position each is likely to take, and any keeper already locked to that slot.
2. **A league view** — a 12-row grid of every team's positional counts with each
   position's roster state (`OPEN / FLEX / SET / MAX`), their open-starter total, and a
   weighted read of the position they are likely to take next. This is what lets the
   manager anticipate opponents' moves.

Everything the current UI does is preserved: search-to-mark-a-pick, top-3
recommendations, the Claude brief, best-available-by-position, my roster, the
on-the-clock state, and the strategy gate band.

## About the design files

The files in `reference/` are **design references authored in HTML** — a prototype of
the intended look and behavior, not production code to lift wholesale. The task is to
recreate this design inside the existing app: vanilla ES2020 in `web/app.js`, no
framework, no build step, served from the Go binary's embedded `web/` FS
(`web/embed.go`). Keep that architecture. Do not introduce React, a bundler, or npm.

`reference/Draft Copilot.dc.html` is a self-contained prototype driven by hard-coded
mock state (a snapshot of the real league mid-draft at live pick 31). The real app gets
its state from `GET /api/state` and the `/api/stream` SSE feed. Every value the
prototype hard-codes already exists in the live payload — the mapping is in
**Data sources** below.

## Fidelity

**High fidelity.** Colors, type sizes, spacing, and the density scale are final. Match
them. The two CSS files in this bundle are the design system — ship them as-is rather
than re-deriving values from the prototype's inline styles.

## Files in this bundle

| File | What it is | Where it goes |
|---|---|---|
| `broadsheet.css` | The brand token sheet + generic components (`.btn`, `.input`, `.seg`, `.card`, `.tag`, `.table`). Vendored unchanged. | `web/broadsheet.css` |
| `draft-copilot.css` | The application design system: density scale, page shell, the eight app components. Documented inline. | `web/draft-copilot.css` |
| `reference/Draft Copilot.dc.html` | The interactive prototype. Open it in a browser. | reference only — do not ship |
| `CLAUDE_CODE_PROMPT.md` | A ready-to-paste prompt for the implementation session. | — |

`web/style.css` is replaced by these two files, loaded in this order:

```html
<link rel="stylesheet" href="/broadsheet.css">
<link rel="stylesheet" href="/draft-copilot.css">
```

Source Serif 4 is pulled from Google Fonts by an `@import` at the top of
`broadsheet.css`. Draft night runs on a laptop on the same LAN as the server, so an
outbound font request is acceptable; if you want it offline, self-host the two weights
(400, 600) plus the 400 italic and replace the `@import` with `@font-face` rules.

---

## Design tokens

### Colors

All from `broadsheet.css`. **Never hard-code a hex in app code.**

| Token | Value | Role |
|---|---|---|
| `--color-bg` | `#f3f2f2` | the paper ground; the whole page |
| `--color-surface` | `#eae9e9` | `.card` fill — the only filled surface |
| `--color-text` | `#201e1d` | body ink |
| `--color-accent` | `#0088b0` | cyan: interactive fills, markers, meters |
| `--color-accent-700` | `#006786` | cyan at body-copy contrast — use for cyan *text* |
| `--color-accent-2` | `#d6006c` | magenta: urgency fills and markers |
| `--color-accent-2-700` | `#aa0b56` | magenta at body-copy contrast — use for magenta *text* |
| `--color-accent-100` | `#e9f8ff` | cyan tint: my-seat row background |
| `--color-accent-2-100` | `#fff1f4` | magenta tint: gate band, upcoming-seat row background |
| `--color-neutral-200` | `#eae7e7` | row hairlines |
| `--color-neutral-300` | `#d7d3d3` | meter track, round-divider rule |
| `--color-neutral-600` | `#7d7979` | faint labels |
| `--color-neutral-700` | `#605d5d` | dimmed labels, kickers |
| `--color-neutral-800` | `#444141` | secondary body copy |
| `--color-divider` | `mix(ink 16%)` | section hairlines |

`draft-copilot.css` remaps these to semantic names (`--dc-live`, `--dc-urgent`,
`--dc-dim`, …). Use the semantic names in new code.

**The two-accent rule, which the whole UI hangs on:**

- **Cyan** = interactive, value, *my* seat. Search focus, tab underline, primary
  buttons, the my-seat row marker, "+8 value" on the rail, a keeper already known.
- **Magenta** = urgency, and only urgency. My turn on the clock, a binding strategy
  gate, an unfilled starter slot (`OPEN`), a seat that picks before my next turn, "−6
  reach", a player ≥45% likely to be gone by my next pick.
- Never both in the same small component. Never magenta for decoration.

Positions are **not** color-coded — deliberately. The current UI gives each position a
hue (`--pos-QB` etc.); this design drops that, because with only two accents in the
system a five-hue scale would collide with the semantic meaning above. Positions read
as small-caps letters. Do not reintroduce position colors.

### Typography

Source Serif 4 throughout — headings and body both, weight 600 for headings and 400 for
body, with the true italic at 400. **No sans-serif anywhere.** The serif is the chrome.

| Token | Dense (default) | Airy | Used for |
|---|---|---|---|
| `--dc-brand` | 19px | 23px | masthead wordmark |
| `--dc-status` | 27px | 38px | on-the-clock team; picks-until figure |
| `--dc-recname` | 19px | 25px | recommendation card player name |
| `--dc-recvor` | 25px | 34px | recommendation card VOR figure |
| `--dc-rail` | 12.5px | 14px | pick-rail player name |
| `--dc-search` | 16px | 19px | the search field |
| `--dc-kick` | 10px | 11px | small-caps kickers and section labels |

Fixed sizes (not density-scaled): tab 13px, card meta 11px, card why 13px, needs team
15px, needs cell figure 15px, needs cell status 8.5px, needs note 11.5px, rail team
11.5px, rail VOR 12.5px, rail delta 10px, roster slot 14px, legend/keys 11px.

Letter-spacing: `-0.022em` on the two display sizes, `-0.012em` to `-0.015em` on
mid-size headings, `+0.1em` to `+0.14em` on every uppercase small-caps run.

**Every figure is `font-variant-numeric: tabular-nums`.** Pick numbers, VOR, ADP,
percentages, roster counts — all of them. They are read as columns.

### Spacing and rhythm

| Token | Dense | Airy | Role |
|---|---|---|---|
| `--dc-gap-main` | 26px | 44px | gutter between main column and rail |
| `--dc-card-pad` | 13px | 20px | recommendation card padding |
| `--dc-row-pad` | 4px | 9px | vertical padding in a rail / needs row |
| `--dc-section` | 17px | 30px | space above a new section |

Fixed: page padding `20px 34px 34px`, rail width `352px`, rail left padding `20px`,
card grid gap `14px`, masthead gap `26px`, tab gap `22px`, minimum page width `1013px`.

Radius is effectively zero — `--radius-md` is 2px and only `.card`, `.btn` and `.seg`
take it. **No rounded containers.** Shadow: `--shadow-sm` on the three cards, nothing
else.

### Density

Dense is the default and the draft-night setting. Airy is a preference, not a
breakpoint: `<body data-density="airy">` swaps the seven type tokens and four rhythm
tokens above, and hides `.dc-card-stats`. Persist the choice in `localStorage` under
`dc.density`; default to `dense` when unset.

---

## Layout

```
┌──────────────────────────────────────────────────────────────────────────┐
│ ████████████████████████████████████████████████████████  4px ink rule   │
│ The Draft Copilot   ROUND 3 · LIVE PICK 31 · 12 TEAMS…    [Airy|Dense] ⌫ │
│ ──────────────────────────────────────────────────────────  1px ink rule │
│                                                                          │
│ ON THE CLOCK                                            YOU'RE UP IN     │
│ Ja'Marr & Jahmyr                                              3 · #34    │
│ ▓ RB GATE · TWO STARTABLE RBS WERE DUE BY #26 — YOU HAVE 1 ▓             │
│ Expect RB then WR off the board before #34 — Love and Jacobs are…        │
│                                                                          │
│ ┌────────────────────────────────────────────┬─────────────────────────┐ │
│ │ MARK A PICK — TYPE A NAME, ENTER           │ THE BOARD    30 PICKS IN│ │
│ │ ________________________________________   │ ────────────────────────│ │
│ │                                            │ ON DECK                 │ │
│ │ THE SHORTLIST   THE LEAGUE — NEEDS…        │ #31 Ja'Marr…  likely RB │ │
│ │ ───────────                                │ #32 Lawson…   keeper:…  │ │
│ │ ┌────────┐ ┌────────┐ ┌────────┐           │ #33 Svannah…  likely WR │ │
│ │ │ 1 GATE │ │ 2      │ │ 3      │           │ #34 You           your… │ │
│ │ │ Jeremi │ │ Josh   │ │ Travis │           │ ────────────────────────│ │
│ │ │ 70.8   │ │ 66.0   │ │ 62.4   │           │ ROUND 3 ───────────     │ │
│ │ └────────┘ └────────┘ └────────┘           │ #30 McMillan WR   +4 val│ │
│ │ BEST AVAILABLE BY POSITION                 │ #29 Jackson  QB   at adp│ │
│ │ CLAUDE — THE READ AT #31                   │ …                       │ │
│ │ MY ROSTER  6 of 17 · 5 starters open       │                         │ │
│ └────────────────────────────────────────────┴─────────────────────────┘ │
└──────────────────────────────────────────────────────────────────────────┘
```

`.dc-main` is `grid-template-columns: minmax(0,1fr) 352px`. The rail is separated by a
1px ink `border-left`, not a background — this system has no dark surfaces and no
panels.

**No responsive fallback below 1013px.** The page sets `min-width` and scrolls
horizontally. That is deliberate for the laptop target. If a phone layout is wanted
later it is a separate stacked design, not a media query on this one.

---

## Screens / views

The app is one screen with two swappable centre views.

### 1. Masthead — always visible

Thick 4px ink rule, then a baseline-aligned row: wordmark "The Draft Copilot" at
`--dc-brand`; a dateline of small-caps facts (`ROUND 3` · `LIVE PICK 31` ·
`12 TEAMS · FULL PPR` · `KEEPER · 17 ROUNDS` · `SITTIN PURDY`); then right-aligned, a
`.seg` density control and a `.btn.btn-secondary` undo button (`⌫ UNDO`, 11px
uppercase). Then a 1px ink rule.

The thick-thin rule pair is the only place this system prints rules as furniture. Do
not add rules between other sections.

### 2. Status band — always visible

Left: kicker `ON THE CLOCK`, then the team name at `--dc-status`. Right, baseline-aligned:
kicker `YOU'RE UP IN`, then `3 · #34`.

**On my clock** (`.dc-status.is-mine`): kicker becomes `YOUR PICK`, the figure becomes
`You're up — #31`, the right side becomes `BOARD LEADER` / `70.8 VOR`, and both the
kicker and the figure turn magenta.

**Draft complete:** figure reads `Draft complete`, right side blank.

### 3. Gate band — conditional

A single magenta-tinted bar, shown when a `strategy.yaml` gate is binding. Text is
`gate_band` from the advice payload, upper-cased. At most one at a time; warnings
concatenate onto the same line with ` · `.

### 4. The read — always visible

One italic serif line synthesising what happens before my next turn. Built from the
league view's per-team predictions, not written by Claude:

> Expect RB then WR off the board before #34 — Love and Jacobs are the exposed names.
> Walker III is a keeper locked at #32.

Rules: list a predicted position per unknown intervening pick, in order; name at most
two exposed players; call out any keeper-locked intervening slot by last name and pick
number. On my clock it reads `You are on the clock at #31. Jeremiyah Love leads the
board at 70.8 VOR.`

### 5. Pick entry — always visible

A ruled writing line, not a boxed input: `border-bottom: 2px solid ink`, transparent
fill, no radius, 44px tall, `--dc-search` in the heading serif, italic placeholder.
Focus turns the rule cyan. Label above it: `MARK A PICK — TYPE A NAME, ENTER`.

Results appear below as up to 6 rows, `18px | 1fr | 62px | 54px | 64px`: caret,
name (16px heading serif), `POS · TM`, `ADP nn`, `nn.n VOR`. Selected/hovered row takes
the cyan tint. `↑/↓` moves, `Enter` marks, `Escape` clears, click marks.

### 6. View tabs

`THE SHORTLIST` and `THE LEAGUE — NEEDS & NEXT MOVES`, 13px heading serif uppercase,
0.12em tracking. Active tab is ink with a 3px cyan `inset 0 -3px 0` underline; inactive
is `--color-neutral-600` and goes cyan on hover. To the right, faint: `PRESS G TO FLIP`.

### 7. THE SHORTLIST view

**Three recommendation cards**, equal thirds, 14px gap, `.card` + `--shadow-sm`.
Contents top to bottom:

1. Head row: cyan rank figure (14px), then a flag — `BOARD LEADER` (cyan) or
   `GATE FORCED` (magenta) — then right-aligned the slot it fills: `RB STARTER`,
   `FLEX SLOT`, or `BENCH` (10px uppercase, faint).
2. Player name at `--dc-recname`, `text-wrap: balance`.
3. `RB · ARI · BYE 14` — 11px uppercase, 0.09em, dimmed.
4. VOR: the figure at `--dc-recvor` beside a faint `VOR` label.
5. Survival: `9%` + `GONE BY #34`, then a 3px meter. Three risk bands — under 20%
   neutral, 20–44% cyan (`--color-accent-600`), 45%+ magenta.
6. The why line, 13px: `RB1 by VOR · your RB starter is empty · 9% gone by #34`.
7. Dense only: `ADP 41 · σ 5.3 · REGRET 0.5 · SCORE 71.3` above a hairline.
8. On my clock: `.btn.btn-primary.btn-block` reading `DRAFT — PRESS 1`.

A gate-forced card carries `inset 3px 0 0 magenta` alongside its shadow.

**Best available by position** — a 5-column row under a hairline. Each: position in
small caps (magenta if that starter slot is open, cyan if only flex is open, dimmed if
set), the player's last name at 16px, then `71 VOR · ADP 41 · 91% SURVIVES`.

**The Claude brief** — under a hairline, kicker `CLAUDE — THE READ AT #31` in cyan,
body 15px/1.6, `white-space: pre-line`, max 62ch. Leading `- ` becomes `• `. When
`brief.projected` is true the kicker reads `CLAUDE ≈ PROJECTED`.

### 8. THE LEAGUE view

A 12-row grid, one row per team, ordered by draft slot.
Columns: `46px | 1.6fr | 38px ×4 | 44px | 1.6fr`

| Column | Contents |
|---|---|
| `NEXT` | that team's next live pick, `#33`. Cyan for my seat, magenta if it falls before my next turn, else dimmed. |
| `TEAM` | name, 15px heading serif, truncated at 22 chars with an ellipsis |
| `QB RB WR TE` | count over status word, centred: `2` / `SET`. Colors: `OPEN` magenta, `FLEX` cyan, `SET` faint, `MAX` dimmed |
| `OPEN` | total unfilled starter slots including flex |
| `LIKELY NEXT MOVE` | `RB 36% · WR 31%`, and under it an italic note: `needs QB, TE · +1 WR past the starters` |

DST is not a column — it is meaningless before round 15 and the width is better spent
on the note. Roll it into the note when a team is still missing one late.

Row states: my seat takes the cyan tint + a 3px cyan inset marker and its note reads
`your seat`; a team picking before my next turn takes the magenta tint + magenta
marker. Everything else is plain paper.

Below, a legend: `OPEN starter slot unfilled · FLEX covered, flex still live · SET
starters & flex satisfied · MAX positional cap reached`.

### 9. My roster strip — always visible

Under a hairline: kicker `MY ROSTER`, then `6 OF 17 · 5 STARTERS OPEN`. Then a wrapping
row of slots in league order — `QB`, `RB1`, `RB2`, `WR1`, `WR2`, `TE`, `DST`, `FLEX`,
`FLEX` — each a 10px uppercase label beside a 14px last name. An unfilled slot has a
magenta label and a faint `open`.

### 10. The pick rail — always visible

Head: `THE BOARD` (13px uppercase heading serif) and, right-aligned, `30 PICKS IN`.

**On deck** — the next five slots. Each row `34px | 1fr | auto`: pick number, team name
(`You — Sittin Purdy` in cyan for my seat, ink for the seat on the clock now, dimmed
beyond), then an italic tag: `keeper: Walker III` in cyan when that slot is already
committed, `likely RB` in magenta otherwise, `your pick` in cyan for my seat.

**The board** — every pick made, newest first, grouped by round. A round divider is a
small-caps `ROUND 3` beside a 1px neutral rule. Each pick row is `30px | 1fr | 46px`:

- pick number, faint
- player name at `--dc-rail` with the position as a 10px uppercase suffix; under it the
  team name at 11.5px, plus `· keeper` when it was a keeper slot
- right-aligned: VOR at 12.5px, and under it the ADP delta — `+8 value` in cyan when
  the player fell 3+ picks past ADP, `−6 reach` in magenta when taken 3+ early,
  `at adp` faint within ±2, and `kept` faint for keeper slots. **Keepers are never
  scored as reaches** — a keeper's ADP has nothing to do with the round it costs.

My own picks take the cyan tint and a 3px cyan inset marker.

Rail length: 22 rows in dense, 13 in airy, then scroll.

---

## Interactions & behavior

| Input | Effect |
|---|---|
| type ≥2 chars | `GET /api/search?q=` → up to 6 results |
| `↑` / `↓` | move selection |
| `Enter` | `POST /api/pick` for the selected player, marked against the team on the clock; clears the field |
| click a result | same as Enter |
| `Escape` | clear the field |
| `1` `2` `3` | draft that card — **only** when I am on the clock and the search field is empty |
| click a card's Draft button | same |
| `G` | flip shortlist ↔ league (ignored while an input has focus) |
| `Cmd/Ctrl+Z`, or the masthead undo | `POST /api/undo` |
| click a tab | flip view |
| density segment | swap `data-density` on `<body>`, persist to `localStorage` |

`1/2/3` must stay gated on an empty search field, or typing a jersey number into a name
search fires a pick. The current `app.js` already gets this right — keep the guard.

Advancing the live pick must **skip slots that already hold a keeper**. In the
prototype, marking #31 jumps to #33 because #32 is Walker III, pre-filled from
`draft-order.csv`. The server already owns this; the client just renders it.

**No animation on the recommendation list.** The existing comment in `app.js` —
"Never animated — the list must not move under the eye" — still holds. Nothing on this
page transitions except hover tints and the density swap.

**Focus** returns to the search field after any pick, undo, or background click, with
`preventScroll: true`. Never call `scrollIntoView`.

Retained from the current UI and unchanged in behavior: the automation freshness line
(`auto 12s ago · 30 picks`, green, red past 90s), the red conflict banner with its
dismiss button, and the toast. Restyle them onto these tokens — the freshness line goes
in the dateline row, the conflict banner becomes a second magenta-tinted band directly
under the gate band, and the toast keeps its position but takes `--color-surface` with
ink text (success cyan-tinted, error magenta-tinted).

---

## State management

Client state, all of it:

```js
{ cur,            // last /api/state payload: {state, advice, brief, automation}
  league,         // /api/league: teams, my_team, slots, players
  byId,           // Map(player_id -> player)
  hits, sel,      // current search results and the selected index
  view,           // "shortlist" | "league"
  density,        // "dense" | "airy", mirrored to localStorage
  busy }          // in-flight pick/undo guard
```

No new server state. No new endpoints. The transport stays as it is: `GET /api/state`
on load, then `/api/stream` SSE pushing the whole payload on every version bump, with a
2s poll fallback when the stream drops.

---

## Data sources

Everything on the page comes from the existing payloads. The two new views need one
server addition, called out at the end.

| UI element | Source |
|---|---|
| round, live pick | `advice.round`, `state.live_pick` |
| on-the-clock team | `state.on_clock` |
| picks until / next pick | `advice.picks_until`, `advice.next_live_pick` |
| gate band | `advice.gate_band`, `advice.warnings` |
| three cards | `advice.top[0..2]` — `player`, `vor`, `p_survive`, `reasons`, `gate_forced`, `keeper_spec` |
| best by position | `advice.by_position[pos]` |
| Claude brief | `brief.text`, `brief.projected` |
| my roster | `state.rosters[my_team]` → `byId`, slotted against `strategy.yaml` roster shape |
| pick rail | `state.picks[]` — `live_pick`, `team`, `player_id`, `source`; ADP delta from `byId[player_id].adp_mean − live_pick` |
| round dividers | `Math.floor((live_pick − 1) / 12) + 1` |
| keeper rows | a pick whose `live_pick` was pre-filled in `draft-order.csv` — expose this as a boolean on the pick, or derive it client-side from `league.keepers_by_team` |
| on deck | `league.slots[live_pick − 1 …]` for the next five |
| league grid counts | `state.rosters[team]` → `byId[].pos`, per team |
| roster state per position | ported from `internal/engine/roster.go`: `starterOpen`, `flexOpen`, `atMax` against `strategy.yaml` `roster.starters` / `roster.flex` / `roster.max` |
| likely next move | see below |

### The `LIKELY NEXT MOVE` column

The prototype computes it client-side as
`need(team, pos, round) × bestAvailableVOR(pos)`, normalised across positions, showing
the top two. `need()` is a direct port of `rosterCounts.need` in
`internal/engine/roster.go`, reading `engine.need` from `strategy.yaml`
(`starter_open 1.8`, `flex_open 1.25`, `full 0.6`, `at_max 0`, `dst_before_round 15`,
`dst_early_mult 0.05`).

**That is a proxy, and the engine already has something better.** `internal/engine/sim.go`
runs 2000 Monte Carlo drafts over the opposing picks to compute `p_survive`; every one
of those sims already decides, for each opposing seat, which position it takes. Those
frequencies are the real answer to "what will this team do next".

Preferred implementation: extend `engine.Advice` with

```go
// PosByTeam is, per team, the share of Monte Carlo sims in which that team's
// next pick was at each position. Derived from the same 2000 sims that
// produce PSurvive — no extra simulation cost.
PosByTeam map[string]map[players.Position]float64 `json:"pos_by_team"`
```

and have the client render those shares directly. Fall back to the need×VOR proxy only
if that turns out to be awkward to thread out of the sim loop. If you ship the proxy,
label the column honestly — it is a heuristic, not a simulation.

The positional counts, `OPEN/FLEX/SET/MAX` states, and the note are pure functions of
`state.rosters` plus `strategy.yaml`; they need no server change. Either port
`roster.go`'s helpers to JS, or serve the roster shape on `/api/league` and compute
client-side — the latter avoids two implementations drifting.

---

## Assets

None. No images, no icon font, no SVG. Broadsheet nominates Phosphor duotone for icons
and this design uses none — the two glyphs on the page (`⌫` in the undo button, `→` in
the search results) are text characters. Do not add an icon set for this screen.

---

## Acceptance checks

- Page renders at 1440×900 with no horizontal scroll; the rail is 352px and the main
  column takes the rest.
- Dense is the default on a fresh profile. Flipping to airy changes the seven type
  tokens and hides the card stats line, and survives a reload.
- Marking a pick from search advances the live pick past any keeper-filled slot.
- On my clock: status band magenta, three Draft buttons present, `1/2/3` fires only
  with the search field empty.
- The league view's row markers agree with the rail's on-deck block about which seats
  pick before my next turn.
- A keeper row in the rail reads `kept`, never a reach.
- `Cmd+Z` and the masthead undo button both revert the last pick.
- No `--color-*` fallback is missing; no hex literal appears in `app.js`.
- No sans-serif renders anywhere; every numeric column is tabular.
