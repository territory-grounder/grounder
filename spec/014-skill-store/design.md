# spec/014 — design: the versioned skill store + graduation flywheel

Grounded in the 2026-07-17 seventeen-agent audit of the predecessor's prompt-patch-trial +
skill-versioning machinery (every mechanism adversarially verified against the live system). The port
keeps what survived production and designs out what failed there. ADR-0012 fixes the content strategy;
this spec is the delivery mechanism.

## What the predecessor proved (and where it failed)

**Proven under fire (ported verbatim):** deterministic `blake2b(issue|trial) % (N+1)` arm assignment
with control-last elision and read-before-hash idempotency; the one-active-trial partial unique index;
the timeout-sweep-before-finalize ordering; the one-sided Welch Student-t test against the *concurrent*
control (the normal approximation was anti-conservative at K=15–30); key normalization with a malformed
counter (its absence cost a 2.5-month dark pipeline); dead-man starvation metrics; a holdout-integrity
rail on promotion; rationale persisted at every transition.

**Verified failures (designed out):** zero trials ever completed in 3 months — online-only trials at
50/arm against real session volume starve (→ REQ-1307 offline admission + REQ-1309 traffic-aware
refusal); the finalizer was silently disabled behind a Cronicle/crontab split-brain (→ Temporal cron,
run state on the console); out-of-enum statuses written by raw SQL (→ CHECK constraints + API-only
`Transition`); `prompt-patches.json`-vs-sqlite split-brain with an unexercised rollback (→ rows + hash
chained ledger; breaker-driven auto-demote); trial telemetry smuggled through a mislabeled event type
(→ first-class metrics).

**Greenfield:** the predecessor has NO skill flywheel — its skill versioning is manual semver
bookkeeping. Porting the prompt-trial machinery onto skills is the sanctioned novelty of this spec.

## Schema (`core/db/migrations/00NN_skill_store`)

Four tables: `skill` (name PK, kind, pinned, position), `skill_version` (semver, CHECK'd status
draft/trial/production/retired/rejected, body ≤ 8 KiB, declarative `applies_when` JSONB, content hash,
author, source, rationale log, parent version, offline/online eval JSONB, ledger seq), `skill_trial`
(candidate ids, control version, target dimension, min-samples/lift/p, ends_at, CHECK'd status incl.
aborted_operator, per-arm result columns, note log), `skill_trial_assignment`
(external_ref + trial UNIQUE, variant index). Two partial unique indexes carry the invariants:
one-production-per-skill and one-active-trial-per-skill.

## Composition (INV-08 held)

`skillstore.Transition` is the single status mutator (state machine + rationale + ledger append via the
existing `audit.Ledger`). A `ComposeSeed` activity replaces the direct `skills.Default().Compose` call
in `temporal/runner/activities.go`: one production snapshot per session; declarative predicates
(phases × exec classes, validated at write time, deliberately not Turing-complete) evaluated in Go;
compiled `skill.position` ordering; pinned skills always composed from the compiled body; trial arms
assigned in-compose (REQ-1306) and persisted; the loaded (name, version, hash, arm) list recorded per
session. ANY store failure → the compiled registry composes exactly as today with the fallback reason
recorded (REQ-1304). A boot importer idempotently seeds the store from the compiled registry so the
console shows the real library from first start.

## Flywheel

Draft sources (REQ-1312, generate-only): eval-dimension regressions below threshold (defaults 3.5,
safety-analog 3.0, ≥3 samples — env-overridable) and confirmed-clean resolved-incident lessons; ≤3
rewrites per trigger across mutation lenses, deduped, 8 KiB cap, each row carrying its rationale +
source. Admission (REQ-1307): offline harness run — regression set must hold, target dimension must
improve on discovery; sealed holdout untouched. Online confirmation (REQ-1306/1309): `skill_trial`
with defaults 15/arm, 14 days, lift 0.05, p < 0.1, ≤2 concurrent trials, start refused when the trailing
judged-session rate cannot complete the trial in time. Graduation (REQ-1308): Temporal cron
`skill-trial-finalizer`, timeout sweep first, Welch one-sided vs concurrent control + safety-dim
no-regress + holdout-integrity rail; winner → production (structural supersede), losers → rejected,
everything ledger-recorded. Post-graduation (REQ-1310): a `core/breaker` named `skill:<name>` watches
judged sessions for a bounded window; a trip auto-retires the version, restores the prior production
row, ledgers and escalates.

## Surface

HTTP (all registered through the mandatory-auth router): `GET /v1/skills`, `GET /v1/skills/{name}`,
`GET /v1/skills/trials` (AuthReadOnly); `POST /v1/skills/{name}/versions`, `…/versions/{id}/trial`,
`…/versions/{id}/promote`, `…/versions/{id}/retire`, `…/trials/{id}/abort` (AuthSession; moves to the
admin tier when #27 Phase B lands). Console `frontend/src/panels/skills/`: library list (status chips,
pinned badge, trial badge, dimension sparklines, prominent compiled-fallback banner), editor (markdown
body + predicate checkboxes, mandatory rationale, save-is-new-draft), version diff with rationale log +
ledger links, trial dashboard (arm gauges, means + CI, p, projected completion, assignment-age dead-man
indicator, abort), flywheel feed (pending drafts with sources, admit/reject).

Metrics: `tg_skill_trials_active`, `tg_skill_trial_arm_samples`,
`tg_skill_trial_newest_assignment_age_seconds`, `tg_skill_trial_malformed_refs_total`,
`tg_skill_versions{status}`, `tg_skill_compose_fallback_total`, breaker state.

## Predecessor reference implementations

`claude-gateway/scripts/lib/prompt_patch_trial.py` (assignment, stats, state machine — transliterate;
golden-value tests pin the Go Welch implementation to the Python outputs),
`scripts/prompt-trial-assign.py` (key normalization), `scripts/write-trial-metrics.sh` (dead-man metric
set), `scripts/lib/gepa_generator.py` (generate-only pattern).
