# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## What this is

Draft Copilot: a single Go binary (`./server`) that recommends the next pick during a live
12-team full-PPR keeper fantasy draft. The authoritative design is
`docs/draft-copilot-spec.md`; code comments cite it by section (e.g. "spec §5.2", "§10
invariants"). Read the relevant spec section before changing engine, state, or API behavior.
The UI is plain HTML/CSS/JS embedded via `go:embed` — there is deliberately **no frontend
build step or toolchain**; don't introduce one.

## Commands

```sh
make build          # go build -o server ./cmd/server (UI embedded from web/)
make run            # build + serve on 0.0.0.0:8090 (PORT=, DATA= overridable)
make test           # go test ./...
go test ./internal/engine -run TestName   # single test
make ingest         # data/*.csv -> data/players.json (must re-run after CSV changes)
make sim            # 500 mock drafts; non-zero exit if a §10 invariant fails
make keepers        # print the 2027 keeper-speculative board
make brief-test     # one real Claude call (needs FANTASY_ANTHROPIC_API_KEY or ANTHROPIC_API_KEY)
./server -print-picks   # print derived live pick numbers and exit (sanity-check draft order)
```

`make sim` is the engine's real regression test — run it (not just `go test`) after touching
`internal/engine` or `data/strategy.yaml`.

Server flags: `-port`, `-data`, `-events` (event log path), `-team` (default "Sittin Purdy",
must match `data/draft-order.csv`), `-brief=false` to disable Claude.

## Architecture

Data flow: `cmd/ingest` turns the CSVs in `data/` into `data/players.json` → `cmd/server`
loads `strategy.yaml`, `draft-order.csv`, `players.json` → event-sourced state → engine →
HTTP/SSE → embedded SPA.

- `internal/strategy` — loads `data/strategy.yaml`. **Every tunable lives there, not in
  code** (roster limits, ADP source weights, sim counts, λ's, need multipliers, gates,
  keeper economics). Adding a knob means adding it to the YAML + struct, not a constant.
- `internal/league` — draft order, slots, keepers (pre-filled slots), which picks are
  "live". Keepers are resolved against the pool at boot; an unresolvable keeper fails boot
  on purpose (data bug, not something to paper over).
- `internal/players` — player pool, positions, fuzzy search, and a fitted ADP→value curve
  used as a fallback when projections are absent.
- `internal/state` — `DraftState`: append-only JSONL event log (`data/events.jsonl`)
  replayed on startup; `Pick`/`PickAt`/`Undo`; monotonically increasing `Version`;
  `Subscribe()` channel drives SSE. `Snapshot` is the immutable value passed to the engine.
  Delete `events.jsonl` for a clean board; keep it to resume a draft.
- `internal/engine` — pure, network-free, cached by state version. Pipeline per state
  change: board → dynamic replacement level → Monte Carlo survival to my next pick →
  score = VOR + λ·regret (+ variance) → hard gates from strategy.yaml → keeper annotation.
  `sim.go` is the mock-draft harness with opponent model and invariant checks used by
  `cmd/simdraft`. `AdviseFor(snap, team)` powers the league view for other teams.
- `internal/brief` — Claude commentary (anthropic-sdk-go). Colour only, never on the
  critical path: async, cached, speculative prefetch of the projected board before my turn;
  failures degrade to engine reasons. `httpapi.Briefer` is the interface it satisfies;
  `brief` imports `httpapi`, not the reverse.
- `internal/httpapi` — `GET /api/state|stream(SSE)|search|brief|league`, `POST
  /api/pick|undo`, and `/api/fandraft/*` for the Tampermonkey userscripts in
  `userscripts/`. Engine and briefer are injected via the `Advisor`/`Briefer` interfaces in
  `Options`. Automation never overwrites manual picks — conflicts surface as a banner.
- `web/` — `index.html`, `app.js` (single file), `draft-copilot.css`, vendored
  `broadsheet.css`. Two-column laptop layout (≥1013px), hotkeys documented in README.

## Conventions

- Position/roster semantics (no kicker, 2 flex RB/WR/TE, keeper cost floor R8) come from
  `strategy.yaml` and the spec — don't hardcode league facts.
- Ingest re-scores any-scoring FantasyPros projections to full PPR; ADP σ is a weighted MAD
  over per-source columns with best-ball sources excluded from σ (see `adp.sources`).
- Runtime data (`server` binary, `events*.jsonl`, `fandraft-frames*.jsonl`) is gitignored.
