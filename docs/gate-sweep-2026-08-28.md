# Gate sweep — 2026-08-28

Question: is the TE gate's `must_draft_by_live_pick: 49` well placed, and is the QB
gate's `68` load-bearing in a good way?

Method: one scratch copy of `data/` per arm, differing from the committed
`strategy.yaml` by **exactly one line** (verified by diff). `simdraft -n 500 -sims 400`,
base seed 1, replicated at base seed 20000. Every arm faces identical opponents, so the
`paired engine−baseline` line is the comparison that matters.

## Results (seed 1, n=500)

| arm  | TE gate | QB gate | violations | TE fired/changed | paired median | win % | p10 | p90 |
|------|---------|---------|------------|------------------|---------------|-------|-----|-----|
| base | 49      | 68      | 0          | 0.36 / 0.33      | +34           | 82.8  | −10 | +89 |
| te58 | 58      | 68      | 0          | 0.36 / 0.33      | +34           | 82.8  | −10 | +89 |
| te65 | **65**  | 68      | 0          | 0.33 / 0.33      | **+40**       | **86.8** | −5 | +95 |
| te68 | 68      | 68      | 163 (32.6% no TE by #68) | 0.33 / 0.33 | +38 | 86.4 | −5 | +93 |
| te90 | 90      | 68      | 0          | 0.33 / 0.33      | +37           | 86.0  | −7  | +91 |
| qb58 | 49      | 58      | 177 (35.4% no TE by #49) | 0.36 / **0.07** | +26 | 77.6 | −20 | +84 |
| qb90 | 49      | 90      | 0          | 0.36 / 0.33      | +28           | 81.2  | −15 | +88 |

Replication at seed 20000: base +36 / 83.0%, te65 +40 / 87.0%, te68 +39 / 86.4%.
The te65 edge reproduces; it is not a seed artifact.

## Findings

### 1. Move the TE gate from 49 to 65

+6 median paired points and +4pp win rate over status quo, reproduced on a second seed,
with a better tail in both directions (p10 −10 → −5, p90 +89 → +95). TE flex fills drop
196 → 160 and RB flex rises 352 → 379: the engine stops burning a mid pick on a TE it
did not want and takes the Kraft/Pitts/Fannin tier when it actually arrives.

This confirms the dead-zone hypothesis. At 49 the gate forced a reach in ~1/3 of drafts;
at 65 it still fires about as often, but what it forces is worth having.

### 2. Gate deadlines are quantized to MY pick boundaries

`engine.gates()` measures urgency as `myPicksThrough(deadline)` — my live picks inside
the window, never raw pick numbers. My picks jump 49 → 65, so **every deadline in
[49, 64] is the same gate.** `te58` came back byte-identical to `base`, including the
gate's own firing count.

Consequence: the TE gate has ~15 distinct settings, not 100, and the `rationale:` strings
that describe deadlines in ADP-tier terms can describe a tier the engine cannot see. Write
gate deadlines as one of my pick numbers, and say which pick is the last chance.

### 3. Simultaneous gates are resolved by YAML list order — latent bug

`force()` in `gates.go` takes the first gate in `cfg.Gates` that is allowed and ignores
every later one. When two gates bind on the same pick, the winner is whichever block was
typed first in `strategy.yaml`. QB is listed before TE, so QB always wins.

Both failing arms are this bug, not bad strategy:
- `qb58`: QB deadline moves into the ≤49 window, collides with TE at #49. TE's
  changed-the-pick collapses 0.33 → 0.07 and 35.4% of drafts finish with no TE.
- `te68`: TE deadline moves onto #68, where the QB gate already sits. 32.6% miss TE —
  even though the drafts that *do* get a TE score fine (+38).

Nothing warns. `te68` looks like a perfectly reasonable config and is silently broken.
Worth either an explicit priority field, tie-breaking by scarcity//urgency, or at minimum
a boot-time check that no two `must_draft_by_live_pick` deadlines share a last-chance pick.

### 4. Keep the QB gate at 68

It is 100% load-bearing (fires 0.89, changes the pick 0.89 — the engine never wants a QB
on value), but both directions tested are worse: 58 collides with TE (+26), 90 drifts to
+28 and collapses TE flex fills to 66. 68 it is.

## Recommended change

```yaml
  - position: TE
    must_draft_by_live_pick: 65   # was 49
    max: 2
    rationale: "Bowers/Loveland/Warren kept. #65 is my last pick before the Kraft/Pitts/Fannin tier is gone; #68 is the QB gate."
```

## Changes made (2026-08-28)

1. **`data/strategy.yaml`** — TE `must_draft_by_live_pick: 49 -> 65`, with the
   quantization and QB-collision reasoning in a comment above it.

2. **`internal/engine/gates.go`** — `force()` no longer takes the first gate in
   `cfg.Gates`. Gates now propose `gateCandidate`s and the winner is chosen after the
   whole loop by: still-savable before already-missed, then tightest slack (picks
   available before the deadline minus positions still needed), then earliest deadline,
   then declaration order as a deterministic last resort. Any other gate that also
   needed this exact pick is surfaced as a `GATE CONFLICT` warning instead of being
   dropped in silence. Collecting candidates first also fixes a smaller order bug: the
   old code tested `g.allowed[pos]` at call time, so a later gate banning a position
   could not un-force it.

3. **`internal/engine/engine_test.go`** — `TestGatePriorityIgnoresYAMLOrder` (tighter
   slack wins over declaration order; savable beats already-missed; reordering the YAML
   does not change the outcome) and `TestShippedGatesDoNotCollide`, which fails if any
   two must-draft gates in the real `strategy.yaml` share a last-chance pick. The
   existing `TestGates` TE case moved from #49 to #65 to match the new deadline.

Verification: all three new subtests fail against the old list-order semantics
(reproduced by disabling the sort) and pass with the fix. `go build ./...`, `go vet`
and `go test ./...` clean, gofmt clean.

`make sim` after both changes:

```
500 drafts, 400 sims/advise, 0 with violations
gate-forced picks per draft:  QB 0.92/0.92  RB 1.32/0.51  TE 0.33/0.33  DST 1.00/0.00
paired engine−baseline: median +40  mean +44  p10 -5  p90 +95  win 86.8%  loss 13.2%
[PASS] ×4
```

Identical to the `te65` sweep arm, which is the expected result: the shipped gates have
no collision, so the priority fix is behaviour-neutral today. It only changes what
happens in the case that used to fail silently.
