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

**Read the table one comparison at a time.** Each cell is that arm's engine-vs-baseline
result, so arm-vs-arm differences are not paired and carry the noise of two independent
n=500 estimates. `49 -> 65` is established: it is a 6-point move, it replicates on a
second seed, and it has a mechanism (the dead-zone reach). `65 > 90` (+40 vs +37, 86.8%
vs 86.0%) is **not** established — that gap is inside noise at this n. 65 is still the
right pick, on the flex-fill shape and on staying clear of the QB gate's last-chance
pick, not because +40 beat +37.

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

But "broken" is the wrong diagnosis, and the fix below is weaker than it should be — see
**Follow-up: schedule the whole board** at the end of this document. `te68` is not
over-constrained at all; it has slack 2. It fails only because the gate rule notices
urgency at `n == 1`, by which point both gates are on their last pick and one must lose.

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


## Follow-up: schedule the whole board (prototyped 2026-08-29, NOT merged)

The priority fix above resolves a collision correctly. It does not prevent one, and the
collision is almost never real. Checking the deadlines against the picks actually left:

```
shipped (TE 65, QB 68)   by #26: need 2, have 3   by #65: need 3, have 5   by #68: need 4, have 6
te68    (TE 68, QB 68)   by #26: need 2, have 3   by #68: need 4, have 6            slack 2
qb58    (TE 49, QB 58)   by #26: need 2, have 3   by #58: need 4, have 4            tight
```

`te68` had room to spare. It failed 32.6% of drafts because nothing looked further than
the current pick. Replacing the per-gate `n == 1` trigger with a feasibility check every
pick — for each deadline, everything due by then must fit in the picks available by then
(Hall's condition over nested deadlines; earliest-deadline-first once something must be
scheduled) — forces early enough that both gates are met.

Prototype results, 500 drafts each, against the same arms:

| arm | violations before | violations after | paired median | win % |
|---|---|---|---|---|
| shipped (TE 65) | 0 | 0 | +40 (unchanged) | 86.8 (unchanged) |
| te68 | 163 (32.6%) | **0** | +38 -> +40 | 86.4 -> 87.0 |
| qb58 | 177 (35.4%) | **0** | +26 -> **−25** | 77.6 -> **26.4** |

Two things to take from this. The collision class of bug disappears — `te68` becomes a
non-issue rather than a warned one, and the exact TE deadline anywhere in [65, 68] stops
mattering. And `qb58` inverts: satisfying every gate reveals that the config itself is
bad, forcing all four of picks 8/11/26/49 into gates (gate-forced rises to 4.0 per draft)
and finishing 25 points *behind* naive BPA. The old silent failure had been flattering
that config by quietly discarding one of its constraints.

Status: prototyped and green (all existing tests, plus the priority tests), deliberately
**not merged before the draft**. The shipped config has no collision, so the change buys
nothing today and would alter the decision path in the ~1/3 of drafts where a gate fires.
Merge after the draft, then re-run the sweep.
## Follow-up (2026-08-28, later): joint deadline scheduler shipped

`engine.gates()` no longer judges each deadline on its own. Every gate reduces to
requirements `(position, deadline, need)`; for each distinct deadline D, ascending,
**demand** is the sum over positions of the largest outstanding need due by D (TE by
#65 plus 2 TE by #90 is two TE slots, not three) and **supply** is my picks through D.
That is Hall's condition over nested deadlines, checked on every pick; earliest-deadline
order then says *which* set binds:

- `demand == supply` → this pick binds to the positions due by D. `allowed` shrinks to
  that set and value chooses among them (`GateForced` on all of them; band reads
  `QB/TE GATE: 2 slots, 2 picks by #68` when the set is bigger than one).
- `demand > supply` → `GATE HOLE` warning; the set still binds so the damage is minimal.
- `demand == supply − 1` → one-pick-of-slack warning (the old "2 picks left").
- A deadline already passed never outranks one that can still be met; it binds only
  as a fallback (`GATE OVERDUE`).

`engine.CheckGates(lg, cfg)` runs the same test from an empty roster at boot, so a
`strategy.yaml` that demands more slots than there are picks before a deadline fails
at startup instead of on draft night. The `TestShippedGatesDoNotCollide` guard is gone:
shared last-chance picks are now a legitimate configuration.

| arm | violations, list-order | violations, scheduler | paired median | win % |
|-----|-----|-----|-----|-----|
| shipped (TE 65) | 0 | 0 | +40 (unchanged) | 86.8 (unchanged) |
| te68 | 163 | **0** | +40 | 86.8 |
| qb58 | 177 | **0** | +26 | 77.8 |

Shipped output is byte-identical (same gate-forced counts, same paired line), which is
the expected result: with no collision the scheduler and the list-order resolver make
the same call on every pick. `te68` becomes indistinguishable from `te65`; the TE
deadline anywhere in [65, 68] no longer matters. `qb58` is merely worse (+26), not
catastrophic — the scheduler admits a gate only when demand actually reaches supply,
so satisfying every deadline there still costs one pick (QB at #49), not four.
