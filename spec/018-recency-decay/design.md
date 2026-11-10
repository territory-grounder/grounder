<!-- spec/018 — provenance tags: [F] foundation / [R] product reframe / [O] audit overlay. -->

# spec/018 — Design: shared recency, decay & periodic reconciliation

How the requirements map onto Go, and which existing primitives they compose over. Everything here is
competence-plane and additive: no existing decision path, safety gate, or estate write is changed.

## 1. Lessons — provenance + decay (REQ-1800/1701) — `core/lessons`

`ResolvedIncident` gains a `ResolvedAt time.Time` (`json:"resolved_at,omitempty"`). Because
`lessons.ParseResolved` decodes with `DisallowUnknownFields`, adding the field simply makes `resolved_at` a
KNOWN field — an existing feed without it still parses (zero value = undatable). `core/lessons/decay.go` adds:

- `HalfLifeWeight(resolvedAt, now, halfLife) float64` — `2^(-age/halfLife)`, clamped to `[0,1]`; a zero
  timestamp or non-positive half-life returns `1.0` (never down-weight an age we cannot prove). This is the
  influence QUANTIFIER a retrieval-ranking or dashboard can multiply relevance by.
- `Reconcile(resolved, now, maxAge) → {Fresh, StaleRefs}` — partitions the feed by provenance age (pure).
- `PruneStaleFromCorpus(corpus, staleRefs) → (kept, removed)` — removes the aged-out refs from the durable
  `knowledge.Incident` corpus (pure). The composition root rewrites the corpus atomically (temp + rename),
  serialized with the append path via one `lessonsMu`.

## 2. Learn — half-life on counts (REQ-1802) — `core/learn`

`CoOccurrenceLearner.counts`/`trials` become `float64` maps (so decay is continuous) and the struct gains a
`lastDecay time.Time` checkpoint. `Observe` still adds a whole `1.0`, so recent evidence keeps full weight;
`CoOccurrences` rounds to the `int` the `estate.CoOccurrence` snapshot expects (a pair rounding to 0 is
skipped). `Decay(now, halfLife)` multiplies every count and trial by `2^(-elapsed/halfLife)` measured from the
previous checkpoint (the first call only sets the baseline), dropping any that fall below one whole
observation (`countDecayFloor = 0.5`). Because counts AND trials decay by the same factor the base-rate ratio
`Count/PrimaryTrials` is preserved — decay shrinks the sample SIZE, not the confidence shape. `Decay` is a
maintenance op driven by the caller's clock, deliberately separate from the deterministic `Observe` path
(INV-08), and mutex-guarded (safe alongside `Observe`/`CoOccurrences`).

## 3. Estate — decay-on-disproof (REQ-1803) — `core/estate`

`core/estate/decay.go` adds `(g *Graph) DecayOnDisproof(Disproof, DecayOptions) (*Graph, DecayReport)`. It
`clone()`s the graph, then for every `Source == SourceIncident` edge incident (by canonical name) to a
disproved host multiplies the confidence by the factor (default `0.5`) and — if it reaches the floor — sets
`ValidUntil` to the observation time so the existing `fresh()` filter excludes it from every traversal. Ground
-truth live edges and declared edges are never decayed (they are re-seeded from their systems of record each
refresh). Working on a clone honours the estate's immutable-after-build discipline: the `Holder` swaps the new
graph atomically, so a concurrent prediction read is race-free. Durable persistence of learned-edge aging is
the `core/learn` half-life (the refresh rebuilds the learned tier from the decayed counts); this graph pass is
the immediate, targeted corrective on the live snapshot. The disproof reuses the typed
`core/verify.VerdictDetail` (surprise-hosts + rule-mismatches) mapped to bare hostnames by the composition
root, keeping `core/estate` decoupled from `core/verify`.

## 4. The periodic reconciliation cron (REQ-1804) — `cmd/worker/main.go` (spec/012-governed)

One decay cron (`TG_DECAY_INTERVAL`, off by default) fires a `runDecay()` pass modelled on the existing
judge/generator/escalation crons: (1) `reconcileLessons()` prunes stale precedent; (2) `learner.Decay`
half-lifes the counts; (3) `estateHolder.Graph().DecayOnDisproof(...)` decays the learned tier from the
discovery corpus's captured disproof hosts, and on any decay the new graph is swapped in via `Holder.Set` and
re-published. The whole pass runs under a `recover()` so a decay error can never crash the worker, and it never
constructs, arms, or consults an actuation path (mutation stays OFF). Knobs (`TG_LEARN_HALFLIFE`,
`TG_ESTATE_DECAY_FACTOR`, `TG_LESSONS_MAX_AGE`) are config-not-code. `cmd/worker/main.go` stays governed by
spec/012 and is re-stamped in `.lockstep.lock`; the store files (`core/lessons/{lessons,decay}.go`,
`core/learn/cooccurrence.go`, `core/estate/decay.go`) are bound to spec/018.

## 5. Discovery-corpus flush (REQ-1805) — `cmd/worker/main.go` + `eval` (call-only)

`falsify.NewMemDiscoveryCorpus` is constructed unconditionally and injected as the falsify Scorer's
`Discovery` writer (the Scorer already carries the optional field), so every live-scored deviation is captured.
A flush cron (`TG_DISCOVERY_FLUSH_INTERVAL`) drains it: it snapshots the corpus, feeds `eval.IngestCaptured`
only the per-signature reproduction DELTA since the last successful flush (the snapshot is cumulative, so
deltas avoid double-counting), and `Save`s `discovery-corpus.json`. `core/falsify` and `eval` are gate-frozen
and only CALLED here — no eval/falsify logic changes. A load/save error is logged and the loop continues; the
flush never mutates the estate.

## 6. Acceptance

`spec/018-recency-decay/acceptance/recency-decay.feature` drives the real code through godog: provenance
round-trip + down-weight + prune, the count half-life + age-out, the estate decay + ground-truth-untouched +
clone-immutability, and the end-to-end discovery-corpus drain (`MemDiscoveryCorpus.Capture` → `Snapshot` →
`eval.IngestCaptured` → `Save` → `LoadDiscoveryCorpus`). Every scenario is `present` (no `@pending`).
