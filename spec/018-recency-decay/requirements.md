<!-- spec/018 — provenance tags: [F] foundation / [R] product reframe / [O] audit overlay. -->

# spec/018 — Shared recency, decay & periodic reconciliation of the learned stores

**Owning behavior family:** — (competence-plane maintenance; no BEH row — it composes over the existing
learned stores).
**Constitution / invariants:** INV-08 (deterministic learning), INV-09 (mechanical never-auto floor unaffected),
INV-21 (read-only worker), INV-22 (no undeclared test-gap).
**Phase:** Phase 1 (competence-plane; read-only; mutation stays OFF).
**Status:** Approved.

The three learned stores accrue knowledge but never FORGET: `core/lessons` distils resolved incidents into
citable precedent, `core/learn` accumulates incident co-occurrence counts, and `core/estate`'s confidence
ratchet only ever goes UP (`Upsert` MAX-merges). Without a decay discipline a stale lesson keeps being cited,
an ancient co-occurrence keeps enriching prediction, and a learned edge reality has since disproved lingers at
full confidence forever. This spec adds a SHARED recency/decay discipline — provenance timestamps + a
half-life + decay-on-disproof — plus the periodic reconciliation cron that fires it (Gulli ch14). It also
completes the deferred wiring hop of the discovery-corpus flywheel (design-wisdom #10): draining the in-memory
discovery corpus to the durable `eval/discovery-corpus.json` on a cron.

> **Competence-plane only.** Every requirement here ages LEARNED state (the retrieval corpus, the co-occurrence
> counts, the learned estate tier). None touches the estate itself, actuates, or gates a decision — the
> mechanical never-auto floor (INV-09) and the read-only worker posture (INV-21) are untouched, and mutation
> stays OFF. Decay only ever REDUCES a learned signal's weight; it can never raise confidence or authorize an
> action.

## Requirements

- **REQ-1800** — [O] Gulli ch14 · [R] paradigm-rule 1.
  The teacher's resolved-incident record SHALL carry a `resolved_at` PROVENANCE timestamp that round-trips
  through `lessons.ParseResolved` (a JSON feed), so a lesson's AGE is known to the reconciliation; a record
  with no `resolved_at` SHALL be treated as undatable and SHALL NOT be aged out on an age that cannot be
  proven.

- **REQ-1801** — [O] Gulli ch14.
  The lessons store SHALL expose a recency weight (`HalfLifeWeight`) that decays a lesson's influence
  exponentially with its provenance age — 1.0 at age zero, 0.5 at one half-life — AND a reconciliation
  (`Reconcile` + `PruneStaleFromCorpus`) that removes from the retrieval corpus every lesson whose provenance
  age exceeds an operator-declared retention horizon, WHILE keeping a fresh lesson and an undatable lesson.

- **REQ-1802** — [O] Gulli ch14 · [O] INV-08.
  The co-occurrence learner SHALL apply an exponential HALF-LIFE to its accumulated counts and per-host trial
  counts so a count halves over one half-life and a pair that stops recurring decays below the learned-edge
  threshold and is dropped — WHILE the base-rate ratio is preserved (counts and trials decay by the same
  factor). Decay SHALL be a maintenance operation driven by an explicit caller clock, leaving the deterministic
  replay-safe `Observe` path unchanged (INV-08).

- **REQ-1803** — [O] Gulli ch14.
  The estate graph SHALL expose `DecayOnDisproof`: a fresh observation that contradicts the learned tier
  (verify's surprise-hosts + rule-mismatches, off the typed `core/verify.VerdictDetail`) SHALL reduce the
  confidence of the LEARNED (`Source == incident`) edges incident to the named hosts and SHALL age out (expire)
  any that reach the floor, WHILE ground-truth live edges and operator-declared edges are NEVER decayed, and
  the receiver graph is NEVER mutated in place (the pass works on a clone the caller swaps atomically).

- **REQ-1804** — [O] INV-21 · [O] INV-09.
  WHERE the worker arms the periodic decay/reconciliation schedule (`TG_DECAY_INTERVAL`), the schedule SHALL
  age the three learned stores only and SHALL NOT touch the estate itself, actuate, or gate a decision; a decay
  error SHALL be recovered and logged and SHALL NOT crash the worker; and mutation SHALL stay OFF throughout.

- **REQ-1805** — [F] OpenAI Evals three-set flywheel · [O] Gulli ch14.
  The worker SHALL periodically DRAIN the in-memory discovery corpus (`falsify.MemDiscoveryCorpus`, the buffer
  the falsify Scorer captures scored deviations into) to the durable `discovery-corpus.json` via the pure
  `eval.IngestCaptured` → `DiscoveryCorpus.Save`, so a captured deviation survives the rolling cap and the
  process; a flush error SHALL be logged and SHALL NOT crash the worker, and the flush SHALL NOT mutate the
  estate (measurement-plane only).
