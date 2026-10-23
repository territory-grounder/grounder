<!-- spec/018 — provenance tags: [F] foundation / [R] product reframe / [O] audit overlay. -->

# spec/018 — Threat model: shared recency, decay & periodic reconciliation (STRIDE slice)

Per-feature threat slice for the competence-plane decay discipline. The system-wide model is
[`docs/THREAT-MODEL.md`](../../docs/THREAT-MODEL.md); this file scopes the decay pass's own trust boundary and
is the security half of the spec's definition-of-done.

**Trust boundary.** The decay pass runs INSIDE the read-only worker on a timer. Its inputs are the resolved
-incident feed (already operator-declared config, config-not-code), the co-occurrence learner's own
in-process counts, and the discovery corpus the falsify Scorer captured from verify-time deviations. Its
effects are confined to LEARNED state: it prunes precedents from the retrieval corpus, shrinks co-occurrence
counts, and reduces/ages learned estate-edge confidence — and it drains the discovery corpus to a durable
file. It NEVER writes the estate itself, actuates, or gates a decision. The asset is the fidelity of the
learned tier (a wrong decay could erase useful precedent or drop a real dependency edge). Adversaries of
interest: (a) a poisoned resolved-incident feed carrying forged/absent provenance; (b) a decay
mis-configuration that erases too much; (c) a decay error taking down the worker; (d) any attempt to reach an
actuation path through the decay hook.

**The deliberate posture (threat-modelled honestly).** Decay can only ever REDUCE a learned signal's weight —
it never raises confidence, mints an edge, or authorizes an action, so a hostile input can at worst degrade
the learned tier's ENRICHMENT (learned edges are already capped below the 0.80 suppression cutoff, so a
decayed or aged learned edge can never have been the thing that suppressed or auto-resolved). Ground-truth
live edges and operator-declared edges are excluded from decay by construction (`Source == SourceIncident`
only), so a disproof can never erode the authoritative topology. The estate pass works on a CLONE swapped
atomically, so it cannot corrupt a graph a concurrent prediction is reading. The whole pass is wrapped in a
`recover()` and every store/file operation is best-effort-and-logged, so a malformed feed or an unwritable
corpus degrades to "no decay this tick", never a crash. Undatable lessons are never aged out (fail toward
retention), so a feed that strips provenance cannot silently purge the corpus.

| STRIDE | Threat | Control | Requirement / invariant |
| --- | --- | --- | --- |
| **T**ampering | A poisoned resolved-incident feed forges old `resolved_at` values to purge good precedent | The feed is operator-declared config (config-not-code); pruning only ever removes precedent (never mints one), and an undatable record is never aged out (fail toward retention) | REQ-1800, REQ-1801 |
| **T**ampering | A crafted verify deviation drives disproof to erode the authoritative topology | Only `Source == SourceIncident` (learned) edges decay; ground-truth live + declared edges are never touched, and learned edges are already capped below the suppression cutoff | REQ-1803, INV-08 |
| **D**enial of Service | A decay error (bad feed, unwritable corpus, panic) crashes the read-only worker | The whole pass runs under `recover()`; every store/file op is best-effort-and-logged; a bad tick is a no-op, never fatal | REQ-1804, INV-21 |
| **E**levation of Privilege | The decay hook is abused to reach an actuation path | The pass constructs, arms, and consults no actuation path; it ages learned state only and the mechanical never-auto floor is untouched | REQ-1804, INV-09 |
| **I**nformation Disclosure | The drained `discovery-corpus.json` leaks secrets | `DiscoveryRecord` is non-secret by construction (host/rule/site slugs + hashes only — no argv, credential, or token material); the flush copies those typed fields, never raw payloads | REQ-1805, INV-13 |
| **R**epudiation | A silent decay hides which learned state was aged out | Every decay pass logs its counts (`Decayed`/`AgedOut`, pruned precedents, drained cases); nothing is aged out silently | REQ-1801, REQ-1803, REQ-1805 |
