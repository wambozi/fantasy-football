# Draft Copilot

A single Go binary that answers one question during a live fantasy draft: *given who's
gone, who I have, and how many picks until my next turn, who should I take?* Built for
one 12-team full-PPR keeper league (spec: [docs/draft-copilot-spec.md](docs/draft-copilot-spec.md)).

## Draft night

```sh
make build
rm -f data/events.jsonl            # clean board (keep the file if resuming a draft)
export FANTASY_ANTHROPIC_API_KEY=… # optional: Claude briefs
./server                           # http://<laptop-ip>:8090 on the phone
```

The UI is a two-column laptop layout (minimum 1013px wide; no phone layout) built on the
design handoff in [docs/design_handoff_draft_copilot/](docs/design_handoff_draft_copilot/).

- Type a name, `Enter` marks the pick for the team on the clock. `↑/↓` to choose, `Esc` clears.
- `1`–`6` drafts that shortlist card when you're on the clock (search field empty).
- `G` flips between the shortlist and the league view (needs & likely next moves).
- `Cmd/Ctrl+Z` or the masthead `⌫ Undo` reverts the last pick. The event log replays on restart.
- Magenta = urgency: your turn, a binding gate, an open starter slot, a seat picking before you.
  Cyan = interactive and your own seat. Positions are not colour-coded.
- Dense is the default density; `Airy` is a preference, remembered per browser.

## Before draft night

| Step | Command | What it checks |
|---|---|---|
| Rebuild the player pool | `make ingest` | FantasyPros projections (auto re-scored to full PPR), ECR, dynasty/rookie ranks, 2025 stats |
| Validate the engine | `make sim` | 500 mock drafts against the §10 invariants |
| Eyeball the keeper board | `make keepers` | who the 2027 model likes |
| Verify the Claude key | `make brief-test` | one real call, prints the brief |
| Diff the draft order | — | FanDraft's order vs `data/draft-order.csv` (§9.6) — a mismatch breaks every live-pick number |

## FanDraft automation (optional, §9)

Manual entry is the source of truth; automation is an accelerator.

1. Install Tampermonkey, add `userscripts/fandraft-recon.user.js`, run a FanDraft mock
   with `./server` up (port 8090). Frames land in `data/fandraft-frames.jsonl`.
2. From those frames, fill in `extractPickFromFrame` (or the DOM `cellSelector`/`extract`)
   in `userscripts/fandraft-ingest.user.js`, install it, disable the recon script.
3. The status bar shows `auto Ns ago`; it turns red after 90 s of silence. If the board
   disagrees with a manual entry a red conflict banner appears — automation never overwrites.

## Data inputs (`data/`)

| File | Source |
|---|---|
| `adp.csv` | FootballGuys ADP export (per-source columns drive the survival σ) |
| `FantasyPros_Fantasy_Football_Projections_{QB,RB,WR,TE,DST}.csv` | FantasyPros projections (any scoring; re-scored) |
| `FantasyPros_2026_Draft_ALL_Rankings.csv` | overall ECR |
| `FantasyPros_2026_{Dynasty,Rookies}_ALL_Rankings.csv` | age, dynasty and rookie ranks (keeper model) |
| `FantasyPros_Fantasy_Football_Statistics_*.csv` | 2025 actuals (reason strings, finish-curve fallback) |
| `draft-order.csv` | slot → team, keepers pre-filled |
| `strategy.yaml` | every tunable: gates, need model, bench discount, keeper economics |

## Layout

```
cmd/server     main binary        internal/engine   VOR, Monte Carlo survival, gates, keeper
cmd/ingest     CSVs → players.json internal/brief    Claude commentary (prefetch + cache)
cmd/simdraft   simulation harness  internal/httpapi  JSON + SSE + FanDraft endpoints
web/           embedded UI (vanilla JS + vendored Broadsheet CSS)
                                   internal/state    event-sourced draft state
userscripts/   FanDraft recon/ingest
```
