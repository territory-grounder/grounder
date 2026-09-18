## Goal
Govern a platform's self-healing operator — the reconcile loop that keeps the AGENTIC PLATFORM'S OWN
components alive (schedulers, pipelines, metric writers) — and keep it rigorously out of the mission
lane. The platform analogy is exact: the orchestrator keeps the pods alive; it never decides the
application's business logic.

## Required evidence
- A written plane boundary: Plane A = the platform's own components (this operator's whole authority);
  Plane B = the estate mission (remediation, host actuation, incident decisions) — owned by the
  governed gates and never touched by the healer. Every heal class names its plane.
- An idempotency safe-list for re-runnable work: only jobs proven idempotent (regenerators, metric
  writers) are auto-re-run; everything else escalates, because "probably idempotent" is how a healer
  double-fires a side effect.
- Per-target heal counters with a cap, and an escalation metric that pages when the cap is hit.
- The operator's own liveness: a heartbeat emitted on EVERY exit path (success, error, maintenance),
  alerted with an absent() guard — a staleness expression alone returns no series when the exporter
  dies, and "no data" must page, not pass. State is recorded AS FOUND, before healing, so metrics
  show the truth the healer saw.

## Decision rules
- Heal the platform, never the mission: the moment a "heal" would replay a non-idempotent job or make
  an infrastructure decision, it is Plane B — hand off, do not reach in.
- Cap and escalate, never thrash: N failed heals per hour on one target stops healing THAT target and
  pages — the healer's job includes recognizing that re-running is symptom-level and the root cause
  needs a human.
- Maintenance mode suppresses HEALS, never the HEARTBEAT: during planned work the operator must not
  fight the change, but the platform-dark alert must stay armed — a maintenance window that silences
  the dead-man is a standing invitation to a silent outage.
- Exactly one operator per concern: consolidate rather than run two healers (or two heartbeat
  emitters) over the same surface — dual writers produce duplicate-metric noise at best and fights at
  worst. Retire the old one visibly, keep its disable as the rollback path.
- Ship dark, arm explicitly: the default posture is analysis-only (flag what WOULD be healed); acting
  requires an explicit operator control with an instant kill back to analysis-only.
- Untouched intent is a feature: components an operator deliberately disabled are left alone —
  reconciliation restores declared state, and "operator turned it off" IS declared state.
- Describe control honestly: complete OVERSIGHT (every failure named and logged) with BOUNDED control
  (safe-list re-runs, quarantine, restarts — root-cause fixes escalate) — never claim "complete
  control"; the bound is the safety design.

## Verification
- The heal audit shows every action with its plane, target, and safe-list justification; no Plane-B
  action exists in it.
- A forced repeat-failure test hits the cap and produces the escalation page.
- Stopping the operator in a controlled window fires the absent()-guarded dead-man; maintenance mode
  shows heals suppressed while the heartbeat continued.
