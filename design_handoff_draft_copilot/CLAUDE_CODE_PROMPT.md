# Claude Code prompt

Paste everything below into Claude Code from the root of the `draft-copilot` repo,
with this handoff folder available (e.g. copied to `docs/design_handoff_draft_copilot/`).

---

Rebuild the draft copilot's web UI from the design handoff in
`docs/design_handoff_draft_copilot/`.

**Read first, in this order:**

1. `docs/design_handoff_draft_copilot/README.md` — the full spec. Layout, every
   component, every token, the interaction table, and the data mapping. It is
   self-sufficient; treat it as the source of truth over your own instincts about how a
   draft app should look.
2. `docs/design_handoff_draft_copilot/draft-copilot.css` — the application design
   system, heavily commented. Ship it as-is.
3. `docs/design_handoff_draft_copilot/broadsheet.css` — the brand token sheet. Vendored,
   do not edit.
4. `docs/design_handoff_draft_copilot/reference/Draft Copilot.dc.html` — the
   interactive prototype. Open it in a browser and click through it before writing
   code. It is a design reference driven by mock state, **not** code to copy: it is a
   single-file component format that does not exist in this repo.

**Then read the code you are changing and the code you are reading from:**

- `web/index.html`, `web/app.js`, `web/style.css` — the current UI, being replaced
- `web/embed.go` — how `web/` is embedded and served
- `internal/httpapi/api.go` — the `/api/state`, `/api/stream`, `/api/league`,
  `/api/search`, `/api/pick`, `/api/undo` contracts
- `internal/engine/engine.go` — the `Advice` struct the UI renders
- `internal/engine/roster.go` — `starterOpen`, `flexOpen`, `atMax`, `need`; the league
  view's positional states are a port of these
- `internal/engine/sim.go` — the Monte Carlo loop; see the one server-side change below
- `data/strategy.yaml` — roster shape, gates, need multipliers
- `data/draft-order.csv` — the 12 teams and their pre-filled keepers

**Constraints, non-negotiable:**

- Vanilla ES2020 in `web/app.js`. No framework, no bundler, no npm, no build step. The
  binary serves `web/` from an embedded FS and that stays true.
- Replace `web/style.css` with `web/broadsheet.css` + `web/draft-copilot.css`, loaded in
  that order. All styling comes from those two files' classes and custom properties.
  **No hex literals and no font-family strings in `app.js` or `index.html`.** If you
  need a value that is not tokenised, add it to `draft-copilot.css` with a comment
  saying why.
- Do not restyle or reimplement anything `broadsheet.css` already provides — `.btn`,
  `.input`, `.seg`, `.card`, `.tag`, `.table` and their hover/active/focus states are
  done.
- The two-accent discipline in the README's Colors section is the design. Cyan for
  interactive and for my seat; magenta for urgency only. Do not reintroduce per-position
  colors — the current `--pos-QB`/`--pos-RB`/… scale is intentionally dropped.
- Everything is Source Serif 4. No sans-serif for UI chrome.
- Every figure is `tabular-nums`.
- No animation on the recommendation cards or the pick rail. Nothing may move under the
  eye mid-draft.
- Never call `scrollIntoView`.
- Preserve every existing behavior: search-to-mark, `1/2/3` gated on an empty search
  field, `Cmd/Ctrl+Z` undo, SSE with the 2s poll fallback, the automation freshness
  line, the conflict banner, the toast, focus returning to the search field with
  `preventScroll: true`.

**Server-side change (one, optional but preferred):**

The league view's `LIKELY NEXT MOVE` column wants per-team positional probabilities.
`internal/engine/sim.go` already runs 2000 Monte Carlo drafts and already decides, for
every opposing seat in every sim, which position that seat takes. Surface those
frequencies:

```go
// PosByTeam is, per team, the share of Monte Carlo sims in which that team's
// next pick was at each position. Derived from the same sims that produce
// PSurvive — no extra simulation cost.
PosByTeam map[string]map[players.Position]float64 `json:"pos_by_team"`
```

Add it to `Advice`, populate it in the sim loop, and render it. If threading it out of
the loop turns out to be invasive, fall back to the client-side
`need(team, pos, round) × bestAvailableVOR(pos)` proxy the prototype uses (a port of
`rosterCounts.need`) — and say so in a comment, because it is a heuristic, not a
simulation.

The positional counts and `OPEN/FLEX/SET/MAX` states need no server change, but they do
need the roster shape client-side. Serve `strategy.yaml`'s `roster` block on
`/api/league` rather than hard-coding the starter counts in JS — one source of truth.

**Work order:**

1. `web/index.html` — the new shell and the two stylesheet links. Semantic elements with
   the `dc-*` classes from `draft-copilot.css`; no inline styles.
2. `web/app.js` render functions, one per component, in this order: masthead and status
   band → gate band → the read → pick entry and results → the three cards → best by
   position → brief → roster strip → pick rail (on-deck, round dividers, pick rows) →
   league grid.
3. Interactions and keyboard, from the README's table.
4. Density persistence.
5. The `PosByTeam` change, or the documented fallback.

**Verify against the README's "Acceptance checks" section before you call it done.**
Run `make build && ./server`, replay a real event log if one is handy, and step through
a few picks: mark one from search, check the rail row and its ADP delta, flip to the
league view with `G`, confirm the seats it marks as picking before your next turn match
the rail's on-deck block, then advance to your own pick and confirm the status band goes
magenta and `1` drafts the first card.
