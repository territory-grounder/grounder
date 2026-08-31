-- 0021 mutation_breaker_state — the CROSS-PROCESS durable state of the safety mutation breaker
-- (design-wisdom #3; CONSTITUTION.md:130 "named, observable circuit breakers with persisted state").
--
-- WHY THIS EXISTS: the armed mutation breaker was backed by an in-process store, so a deviation trip in one
-- worker could NOT force-Shadow its sibling workers — the shared-kill guarantee the multi-worker canary
-- depends on was not global. This table is the single cross-process source of truth for each named breaker's
-- three-state position: a trip in ANY worker upserts the row to 'open', and every worker reads that row
-- before it actuates, so one worker's trip force-Shadows every sibling (the read side of the system-wide
-- kill lives in core/actuate.Interceptor + core/safety.MutationBreaker.Tripped).
--
-- It is CURRENT-STATE, one row per breaker name (single logical state per name, latest-wins upsert) — NOT an
-- append-only audit trail. The tamper-evident record of a breaker TRIP already lives in the append-only
-- governance_ledger (the worker's ledgerTripRecorder appends 'safety:breaker-trip'); this row is only the
-- live coordination state the breaker reads/writes. So — unlike the append-only accountability spine (0015)
-- and policy_decision (0019) — the DML runtime role KEEPS UPDATE here (the upsert needs it), exactly like the
-- latest-wins policy_mode / policy_ruleset current-state rows.
--
-- Single-organization (paradigm-rule 1): no tenant_id; the DML-only runtime role is the privilege boundary.
-- NON-SECRET by construction: only a breaker name (a slug), its state, and counters — no argv, host, or
-- credential material can land here. schema_version guards the row shape for a forward-compatible reader.

CREATE TABLE mutation_breaker_state (
  name                text PRIMARY KEY CHECK (length(btrim(name)) > 0),   -- the breaker slug (e.g. 'mutation')
  state               text NOT NULL CHECK (state IN ('closed', 'open', 'half_open')),
  failure_count       int NOT NULL DEFAULT 0 CHECK (failure_count >= 0),  -- consecutive failures toward the trip
  opened_at           timestamptz,                                        -- when it tripped; NULL unless state='open'
  half_open_successes int NOT NULL DEFAULT 0 CHECK (half_open_successes >= 0),
  last_transition_at  timestamptz,                                        -- last state change (NULL if never)
  last_updated_at     timestamptz NOT NULL DEFAULT now(),
  schema_version      int NOT NULL DEFAULT 1 CHECK (schema_version > 0)   -- reader-guard invariant
);
