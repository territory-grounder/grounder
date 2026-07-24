<!-- spec/004 — provenance tags: [F] foundation / [R] product reframe / [O] audit overlay. -->

# spec/004 — Governance auto-demote + judge-death detection

**Owning behavior family:** BEH-4 (see [`docs/GOVERNED-BEHAVIORS.md`](../../docs/GOVERNED-BEHAVIORS.md)).
**Constitution / invariants:** INV-12, INV-15, INV-19, INV-22 · **[R]** paradigm-rules 1/4/5.
**Phase:** the governance-metrics worker and the judge-liveness monitor land in Phase 2–3.
**Status:** Draft (every acceptance oracle is `@pending`; no Go implementation exists yet).

Two self-monitoring controls share this family. The **governance-metrics worker** (a Temporal Schedule)
demotes a genuine repeat-offender `(host, alert_rule)` tuple to analysis-only, so a pattern
that keeps recurring after an auto-resolve stops being eligible for suppression or auto-resolve and is
escalated instead — a circuit-breaker built from a metric, an audit record, and an auto-expiry, never a
manual-review queue. The **judge-liveness monitor** measures whether the local LLM judge is actually
scoring recently-ended sessions, computed from tables the judge does not write so a dead judge cannot
certify itself alive. This document is the requirement source of record; the design is in `design.md`,
the runnable acceptance oracles are in `acceptance/`, and the engineering tasks are in `tasks.json`.

## Requirements

- **REQ-301** — [F] spec/004.
  WHEN a `(host, alert_rule)` tuple is classified a genuine repeat-offender, the system
  SHALL auto-demote that tuple to **analysis-only** — revoking its suppression and auto-resolve
  eligibility so Tier-1 suppression escalates it instead — as a circuit-breaker realized by a metric,
  an audit record, and a 30-day expiry, with no manual-review step.

- **REQ-302** — [F] spec/004.
  WHEN a `(host, alert_rule)` tuple recurs three or more times within a rolling 30-day
  window, the system SHALL classify that tuple as a demote candidate.

- **REQ-303** — [F] spec/004.
  IF a demote candidate is an intentional known-transient — a tuple tagged expected or known-benign for
  the organization — THEN the system SHALL exclude that tuple from demotion, so a declared transient pattern
  whose recurrence is by design is not treated as an offender.

- **REQ-304** — [F] spec/004 · [R] paradigm-rule 4.
  The system SHALL auto-expire each demotion 30 days after it is written, holding the demotion state as
  an org-global policy row in the policy store rather than a hardcoded prompt or a host-local flag,
  with no manual-review step.

- **REQ-305** — [F] spec/004 · [O] INV-15/INV-22.
  The judge-liveness monitor SHALL compute the fraction of recently-ended sessions that carry a real
  local judgment using ONLY tables the judge process does not write, so a dead
  judge cannot certify its own liveness.

- **REQ-306** — [F] spec/004.
  IF fewer than 50% of more than three eligible recently-ended sessions carry a real local judgment,
  THEN the monitor SHALL raise a judge-death warning routed through the escalation module.

## Persistence contract

The retention model is split by design and the two stores are never conflated. The **governance
demotion decisions** and the **judge-liveness facts** (the org-global judged-fraction reading and each
judge-death event) land on the **immutable audit spine** — append-only, integrity-preserving, and
hash-chained into the governance ledger (INV-19); a demotion is an org-global policy row carrying
`host`, `alert_rule`, `demotion_reason`, `valid_from`, and `valid_until`, and its state is
a required output of the decision function. The **raw judged transcripts and their scores** are
**purgeable operational memory** governed by a retention TTL and right-to-erasure; a transcript purge
SHALL NOT remove any audit-spine record. Every row in both stores is authority-checked against the
acting user/role under RBAC (INV-12), and every governed row is `schema_version`-stamped against the canonical
registry (INV-15). See [`docs/DATA-MODEL.md`](../../docs/DATA-MODEL.md).

## Judge-independence invariant

The judged-fraction denominator and numerator SHALL be drawn from session-outcome tables the judge role
holds no write grant on. The monitor's metrics are generated from one typed source per entity (INV-15)
and are exercised by a test that drives the real code path (INV-22) — a judge that stops scoring drives
the fraction down and trips REQ-306 rather than reporting itself healthy from its own tail.
