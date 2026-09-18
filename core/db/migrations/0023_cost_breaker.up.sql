-- 0023 cost_accrual + cost_breaker_state — the DURABLE, CROSS-PROCESS state of the cost/budget spend guard
-- (the $-ceiling breaker; the spend-guard sibling of the mutation breaker's mutation_breaker_state, 0021).
-- CONSTITUTION.md:130 ("named, observable circuit breakers with persisted state").
--
-- WHY THIS EXISTS: TG's per-call model spend (and, at the flip, per-actuation cost) must accrue into ONE
-- shared daily/session total across every worker, and a budget trip in ANY worker must be visible to ALL of
-- them. Two current-state surfaces do that:
--
--   cost_accrual        — the additive running spend, one row per (bucket_kind, bucket_key). bucket_kind
--                         'day' keyed by the UTC date (YYYY-MM-DD) is the daily-budget accumulator; 'session'
--                         keyed by external_ref is the per-session accumulator. Each worker ADDS its
--                         increment (usd_accrued = usd_accrued + delta), so the total is the shared spend.
--   cost_breaker_state  — the latest-wins breaker position (one row, name='cost'), set 'open' when a budget
--                         is exceeded. Every worker reads it on its next completion and force-Shadows its own
--                         mode, so one worker's trip force-Shadows every sibling (the read side lives in
--                         core/cost.Accountant).
--
-- Both are CURRENT-STATE (latest-wins / additive upsert), NOT append-only audit trails: the tamper-evident
-- record of a cost TRIP is the append-only governance_ledger 'cost:breaker-trip' entry (the worker's
-- costLedgerTripRecorder appends it) — these rows are only the live coordination state the guard reads/writes.
-- So — like mutation_breaker_state (0021) and policy_mode/policy_ruleset (0019), and UNLIKE the append-only
-- accountability spine (0015) / policy_decision (0019) — the DML runtime role KEEPS UPDATE here (the upserts
-- need it). This migration therefore does NOT revoke UPDATE/DELETE.
--
-- FAIL-OPEN by design (documented deliberately): this guards SPEND, not a safety floor. A row that cannot be
-- read is treated by core/cost as "not tripped" (never a halt) — the deliberate inverse of the mutation
-- breaker's fail-closed. The schema is identical in shape (a coordination row); only the reader's posture
-- differs, which is a code decision, not a schema one.
--
-- Single-organization (paradigm-rule 1): no tenant_id; one logical 'cost' breaker. NON-SECRET by
-- construction: only bucket keys (a UTC date or an external_ref slug), USD amounts, a state, a human reason,
-- and timestamps — no argv, host, or credential material can land here. schema_version guards the row shape.

CREATE TABLE cost_accrual (
  bucket_kind     text NOT NULL CHECK (bucket_kind IN ('day', 'session')),         -- the accumulator dimension
  bucket_key      text NOT NULL CHECK (length(btrim(bucket_key)) > 0),             -- UTC date (day) or external_ref (session)
  usd_accrued     double precision NOT NULL DEFAULT 0 CHECK (usd_accrued >= 0),    -- approximate running USD spend
  last_updated_at timestamptz NOT NULL DEFAULT now(),
  schema_version  int NOT NULL DEFAULT 1 CHECK (schema_version > 0),               -- reader-guard invariant
  PRIMARY KEY (bucket_kind, bucket_key)
);

CREATE TABLE cost_breaker_state (
  name            text PRIMARY KEY CHECK (length(btrim(name)) > 0),                -- the breaker slug (always 'cost')
  state           text NOT NULL CHECK (state IN ('closed', 'open')),               -- no half-open: over budget or not
  reason          text NOT NULL DEFAULT '',                                        -- the human trip reason (non-secret)
  usd_at_trip     double precision NOT NULL DEFAULT 0 CHECK (usd_at_trip >= 0),    -- the daily total at the trip
  opened_at       timestamptz,                                                     -- when it tripped; NULL unless state='open'
  last_updated_at timestamptz NOT NULL DEFAULT now(),
  schema_version  int NOT NULL DEFAULT 1 CHECK (schema_version > 0)                -- reader-guard invariant
);
