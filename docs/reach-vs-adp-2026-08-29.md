# Reach-vs-ADP per team, the bye-aware lineup metric, and the RB/WR waiver gap — 2026-08-29

Three related changes, one measurement session. All numbers from the committed
`data/draft-20*.json` history (2023–25, identity joined by `data/managers.csv`, keepers
excluded from every tendency fit — a kept player is last season's roster decision, not a
draft habit) and from `simdraft -n 800 -sims 400`, replicated at a second base seed.
800 per arm, not 500, is deliberate: the post-draft bar for trusting any number this
pipeline produces is n≥700, and the validation runs meet it even where the history
itself cannot (see caveats).

## 1. Per-team λ_rank (wired)

Question: managers differ in how far they reach past the best available player. The
opponent model draws picks with `w = exp(-λ_rank × rank)` and one global
`λ_rank: 0.25`. Can the three-season history support a per-team λ?

### Method

For each historical pick: rank of the chosen player, 0-based, among still-available
players sorted by that year's FantasyPros consensus ADP (keepers removed from the board
up front). Per-manager λ by MLE under the engine's own weight model, pooled λ the same
way over all 522 usable picks. SE from the Fisher information; empirical-Bayes
shrinkage toward the pooled fit (weight `τ²/(τ²+SE²)`); then re-anchored by ratio so
the room average stays exactly `engine.lambda_rank` — the raw MLEs sit near 0.03, not
0.25, because they are measured against noiseless ADP ranks while the engine applies λ
after its per-player σ draw, so only the *relative* steepness transfers.

### What the data can and cannot say

- At the engine's candidate pool (K=40), ~24% of drafted picks fall outside the
  window entirely and the fit shrinks **every** manager to the pool: between-manager
  variance of the MLEs (0.00023) is fully explained by sampling noise (mean SE²
  0.00024), τ² ≈ 0. Three seasons ≈ 44 picks per manager cannot distinguish anyone
  from the room average at that window.
- At K=200 (censoring nearly gone, 522 of 534 picks usable) real between-manager
  variance emerges: τ = 0.0073 against a pooled 0.0291, i.e. genuine ±25% spread in
  steepness across the room.
- The manager ORDERING is not stable across estimators. Woke Mob Warriors is the
  chalkiest manager under the K=40 fit, mid-pack under K=200; Patient Zeros is the
  flattest under K=40, second-steepest under K=200. Only the rough flat-vs-steep split
  survives. This is why the raw per-team MLEs must not be wired directly — the
  shrinkage is not a nicety, it is the part that keeps the estimator noise out of the
  opponent model.

### Wired values

K=200 fit, shrunk, anchored to 0.25 (`data/strategy.yaml`, `manager_bias.teams.*.lambda_rank`):

| team | n | MLE | SE | shrink w | wired λ |
|---|---|---|---|---|---|
| Sittin Purdy | 45 | 0.0196 | 0.0035 | 0.81 | 0.184 |
| Pollock Debacle | 42 | 0.0225 | 0.0040 | 0.78 | 0.206 |
| Ja'Marr & Jahmyr | 41 | 0.0235 | 0.0041 | 0.76 | 0.213 |
| Svannah Alley Cats | 45 | 0.0259 | 0.0042 | 0.75 | 0.229 |
| Woke Mob Warriors | 45 | 0.0260 | 0.0042 | 0.75 | 0.230 |
| Trash Pandas | 44 | 0.0291 | 0.0046 | 0.72 | 0.250 |
| Stopped The Steal! | 44 | 0.0304 | 0.0048 | 0.70 | 0.258 |
| The Comeback Story | 42 | 0.0316 | 0.0051 | 0.68 | 0.264 |
| Time Stamps | 43 | 0.0334 | 0.0052 | 0.66 | 0.274 |
| Patient Zeros Aids Epidemic | 43 | 0.0360 | 0.0056 | 0.63 | 0.288 |
| Tom | 44 | 0.0372 | 0.0057 | 0.62 | 0.293 |
| Lawson Country Lets Ride | 44 | 0.0531 | 0.0080 | 0.46 | 0.344 |

Wiring: `ManagerBias.LambdaFor(team, global)` in `internal/strategy`, applied in
`survival.go` (per window pick) and `sim.go` (`opponentPick`). `lambda_weight: 1.0`
scales the deviation exactly as `weight` does for the positional bias; 0 restores the
global model in one edit. Values are floored at 0.25× the global so no config can make
an opponent draw uniformly.

### Validation (n=800 × 2 seeds, bye-aware metric in both arms)

| arm | seed | violations | paired median | win % |
|---|---|---|---|---|
| λ off | 1 | 0 | +42 | 85.8 |
| λ off | 20000 | 0 | +42 | 83.4 |
| λ on | 1 | 0 | +45 | 85.2 |
| λ on | 20000 | 0 | +44 | 86.8 |

Read this as "does not hurt", not "helps": in simulation the opponents ARE the survival
model, so wiring per-team λ changes the world and the engine's beliefs together and the
paired edge cannot show the real payoff. The payoff, if any, is on draft night, where
the room's actual behavior is what the per-team values were fit to. The sim's job here
was to show zero invariant violations and no degradation, and it does, at n=800 twice.

### Caveats — read before trusting a specific number

~44 picks per manager is far below the n≥700 bar for standing behind any single
manager's λ. The shrunk values are a bounded nudge (0.18–0.34 around 0.25), which is
the most the data supports. Refit after the 2026 board lands (that adds ~15 picks per
manager — still provisional; the bar is honest about being years away for per-manager
claims). The instability across estimators is documented above on purpose: if a future
refit reorders managers, that is expected, not a regression.

## 2. Bye-aware lineup metric (harness)

`lineup()` in `internal/engine/sim.go` scored a roster as the season total of its best
starters — a roster with one QB booked 17 games from the slot no matter that week 7 (the
QB's bye) is a zero. The 1-QB/1-DST holes in engine rosters were found by a human
reading the board; the metric said nothing.

`lineup()` now fills the best legal lineup week by week over the 18-week season
(weekly points = season projection spread over games actually played), a backup is
worth exactly its fill-in weeks, and a backup sharing the starter's bye is worth
nothing. `SimResult.ByeHoles` counts starter/flex slot-weeks nobody rostered could
fill; simdraft prints it per arm:

```
unfillable bye slot-weeks per draft: engine 1.60  baseline 2.08     (n=800, seed 1)
```

A full-strength week still decides the flex-diversity invariant, so that check is
unchanged. This is the change that would have caught the QB and DST holes on its own:
they are the largest component of the engine's 1.60 (QB 1.7 and DST 1.0 average roster
counts leave most drafts with at least one naked bye at those slots), visible in every
run instead of discoverable by inspection.

### How a hole is priced (revised once, same session — see §3)

The first version priced an uncovered bye at zero. Validating §3 against that version
showed the zero is a fiction in this league and turns the metric into a demand for
insurance the FAAB wire makes unnecessary (details below). A hole is now priced at one
streamed week from the **end-of-draft waiver level** for the position (the flex hole
streams the best eligible wire), while `ByeHoles` still counts every hole unpriced —
a naked bye is never invisible, it is just costed at what streaming recovers rather
than at a zero nobody would actually take. `lineup(cfg, roster, nil)` keeps the
true-zero semantics for anyone who wants the harsher number.

## 3. The RB/WR waiver gap (wired: `engine.waiver_drafted`)

The waiver level — the baseline a pure-bench pick is measured against — indexed "how
many at this position are still to be drafted" by counting available players with
consensus ADP inside the draft. Consensus imports other formats' habits. Per position,
ADP-count vs what this room actually drafts (3 boards, n=604 picks, keepers included):

| pos | ADP≤204 says | room drafts (23/24/25) | boot waiver level, old → new |
|---|---|---|---|
| QB | 29 | 20 / 22 / 20 | 191.8 → 267.7 (+75.9) |
| RB | 63 | 62 / 67 / 71 | 73.9 → 60.7 (−13.2) |
| WR | 81 | 78 / 78 / 78 | 100.2 → 104.2 (+4.0) |
| TE | 27 | 24 / 24 / 22 | 117.1 → 126.2 (+9.1) |
| DST | 11 | 12 / 13 / 13 | 101.3 → 100.9 (−0.4) |

The QB and TE baselines sat 8 and 4 players too deep — every bench QB/TE carried
inflated VOR — and RB sat 4 too shallow. The RB−WR baseline gap *widens* from 26 to 44
points: this room hoards RBs (and keeps them: RB drafted counts trend 62→71 with the
keeper era), so the RB wire really is barren and a bench RB genuinely earns a premium
over a bench WR. The gap is not an artifact; its old size was.

Cross-check against what the wire actually delivered in 2025 (season points of the best
players our league left undrafted): top-3 undrafted RBs averaged ~162, WRs ~196 — a
realized gap of ~40, agreeing with the room-count direction, not the ADP-count one.
The same check shows every static level understates the wire (QB ~300, TE ~181
realized): in-season churn hands the wire more than the k-th projected player. That
churn premium is measured here but **deliberately unwired** — one season of actuals is
one draw, far under the n≥700 bar, and wiring it would shrink all bench VOR by a
roughly uniform ~90 points, which moves few decisions. Remeasure with 2026 actuals.

`waiver_drafted: {QB: 21, RB: 67, WR: 78, TE: 23, DST: 13}` in `strategy.yaml`;
`waiverLevel()` now takes the taken-count per position so the index adapts as the room
drafts (once the room has its expected share, the wire is the best remaining player).
Empty knob restores the ADP-count fallback.

### Validation, and the finding it forced (n=800 × 2 seeds, λ on in both arms)

Under the FIRST bye metric (holes priced at zero), the room-count arm looked worse:

| arm | seed | paired median | win % | bye slot-weeks (engine) | avg QB |
|---|---|---|---|---|---|
| ADP-count, zero-priced holes | 1 | +45 | 85.2 | 1.60 | 1.8 |
| ADP-count, zero-priced holes | 20000 | +44 | 86.8 | 1.57 | 1.8 |
| room-count, zero-priced holes | 1 | +34 | 80.6 | 2.10 | 1.0 |
| room-count, zero-priced holes | 20000 | +34 | 83.0 | 2.07 | 1.0 |

Both seeds agree, and the mechanism is exact: with the QB wire correctly at 267.7,
every late-round QB projects below it, so backup-QB bench VOR goes negative at ANY
`bench_discount_late` (the discount scales a negative number toward zero, it cannot
flip it) and the engine stops drafting QB2 entirely — avg QB 1.8 → 1.0, +0.5 QB bye
holes, and 0.5 × (≈350/17) ≈ 10.5 points ≈ the entire −11 paired drop. The engine got
MORE honest — this room's wire really does outscore any QB2 the engine could buy, so
skip-and-stream is the right play — and the zero-priced metric punished it for not
buying insurance against a fiction. The 2025 realized wire (Stafford 358, Herbert 300,
Darnold 249 left undrafted) is the ground truth here.

So the metric was revised to price holes at the end-of-draft waiver level (§2) and
both arms re-run at n=800 × 2 seeds:

| arm | seed | violations | paired median | win % | bye slot-weeks (engine) | avg QB |
|---|---|---|---|---|---|---|
| ADP-count, streamed holes | 1 | 0 | +41 | 85.4 | 1.60 | 1.8 |
| ADP-count, streamed holes | 20000 | 0 | +40 | 87.6 | 1.57 | 1.8 |
| room-count, streamed holes | 1 | 0 | +40 | 84.5 | 2.10 | 1.0 |
| room-count, streamed holes | 20000 | 0 | +39 | 87.2 | 2.07 | 1.0 |

The −11 gap collapses to −1, inside paired noise at this n, and the diagnosis is
confirmed: the whole regression was the metric, not the engine. The shipped
configuration is room-count waiver levels + streamed-hole pricing. The engine drafts
one QB and streams the bye (avg QB 1.0, the freed pick goes to RB depth), which is
what the corrected wire says this room's reality rewards; the bye holes remain
reported (2.10/draft, mostly the QB and DST byes) so draft-night eyes know they exist
and FAAB week plans cover them.

The `bench_discount_late: {QB: 0.35}` knob was swept against the OLD (too-deep) QB
waiver level; under the corrected level it is moot for QB2 (negative VOR regardless).
Leave it — it changes nothing while `waiver_drafted` is set and resumes meaning if the
knob is ever cleared.
