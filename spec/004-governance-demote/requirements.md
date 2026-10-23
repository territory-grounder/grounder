<!-- spec/004 — provenance tags: [F] foundation / [R] product reframe / [O] audit overlay. -->

# spec/004 — Governance auto-demote + judge-death detection

**Owning behavior family:** BEH-4 (see [`docs/GOVERNED-BEHAVIORS.md`](../../docs/GOVERNED-BEHAVIORS.md)).
**Constitution / invariants:** INV-12, INV-15, INV-19, INV-22 · **[R]** paradigm-rules 1/4/5.
**Phase:** the governance-metrics worker and the judge-liveness monitor land in Phase 2–3.
**Status:** Approved. Corrected 2026-07-31 (TG-222): this header claimed "no Go implementation exists yet"
long after `core/governance` shipped and every acceptance oracle became `present` — the stale line is
recorded here rather than quietly deleted, because a spec that overstates its own immaturity is the mirror
of one that overstates its maturity, and both were live in this file at once.

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
  whose recurrence is by design is not treated as an offender. This carve-out scopes the RECURRENCE-COUNTING
  trigger of REQ-302 only; it does not scope the evidence trigger described below, whose input is a
  demonstrated suppression miss rather than a recurrence count (spec/005 REQ-411).

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

## Two triggers, one demotion mechanism

A demotion row can be produced by two independent triggers, and both write the SAME org-global
analysis-only row with the same audit record and the same 30-day expiry (REQ-301/304), so the consulting
side has one thing to read:

1. **Recurrence** (REQ-302, this spec): a tuple that recurs three or more times in the rolling window is a
   genuine repeat-offender.
2. **Evidence** (spec/005 REQ-411, owned there): PROOF that a LEARNED suppression pattern silenced an
   incident that then needed action. Its threshold is one, because the input is a demonstrated miss rather
   than a count — waiting for a second proof means silencing a second real incident to earn the right to
   stop. This trigger exists because spec/005's learned lane must not be a one-way ratchet.

- **REQ-307** — [F] `judge-frontier-crosscheck.py` · [O] INV-15/INV-22 ·
  [R] a single self-referential liveness metric is insufficient (docs/PORT-FIDELITY-AUDIT §3-8).
  The system SHALL re-judge a sample of recently-ended sessions with a model INDEPENDENT of the local judge,
  over the same rubric, and SHALL compare the two verdicts. IF the fraction of sessions the independent model
  scored and the local judge did not exceeds one half, THEN the system SHALL raise a confirmed judge-death
  warning with no minimum-sample gate. IF more than five pairs are sampled, at least one pair carries both
  verdicts, and their agreement rate falls below the agreement floor, THEN the system SHALL raise a
  judge-drift warning. The independent model SHALL be configured on a tier distinct from the local judge's,
  and a sample the system cannot read SHALL surface as an error rather than as an empty comparison.
  *Rationale:* the local judged fraction is computable only from what the local judge wrote, so a judge that
  writes nothing at all can be indistinguishable from a window in which nothing was judgeable. An independent
  re-judgment resolves that ambiguity because it scores the same sessions. An empty comparison evaluates to a
  clean bill of health, so a broken sampler must never be allowed to produce one.

- **REQ-308** — [O] INV-19/INV-22 · [R] detection that stops nothing is a message someone must read.
  The system SHALL register each governance monitor as a Temporal Schedule at worker boot, and SHALL register
  a schedule ONLY for a monitor whose collaborators are wired and whose workflow exists. WHEN judge death is
  confirmed, the system SHALL halt judged accrual through a named breaker whose state is persisted in the
  shared cross-process breaker store, SHALL append that halt to the immutable audit spine, and SHALL refuse
  the skill-trial graduation pass with an error while the halt stands, leaving every trial row unchanged. The
  halt SHALL NOT clear on a cooldown; only a deliberate re-arm SHALL clear it. IF the halt's state cannot be
  read, THEN judged accrual SHALL be treated as halted. The ABSENCE of this wiring SHALL itself be detectable
  by a gate that fails when a schedule names a workflow that does not exist, when the boot path does not arm
  the schedules, or when the graduation pass does not consult the halt.
  *Rationale:* both monitors were code-complete, oracle-tested and lockstep-clean while having no
  constructor, no caller, and workflows defined nowhere — every green signal the project had stayed green
  while the control did nothing. Nothing watched the watchers, so the watchers' absence needs its own gate.

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
