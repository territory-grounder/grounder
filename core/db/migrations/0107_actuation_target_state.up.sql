-- 0107 — durable cross-process per-target actuation admission + cooldown (TG-81 borrow 2; spec/013 gate 4h2).
--
-- The interceptor's in-process lease (gate 4h, TG-166a) cannot see a sibling process; this row can. One row
-- per actuation TARGET, atomically claimed before the pre-effect sequence (baselines, necessity probe, Exec)
-- and released after it: claimed_by/claimed_at is the ACTIVE-SET claim ("an actuation is in flight against
-- this target, estate-wide"), cooldown_until is the COOLDOWN-ON-ERROR stamp (a disturbed effect — failed,
-- non-zero exit, or killed mid-flight — parks the target so the next hand waits out the dust). A crashed
-- worker's stale claim ages out via the claim TTL, enforced in the claim SQL, not by a reaper.
--
-- Plane: WRITTEN and READ only by the actuation chokepoint (core/actuate gate 4h2 via db.ActuationTargetStore).
-- A compromised TRIAGE worker must not be able to park targets (denial-of-remediation) or clear a cooldown, so
-- this is actuation-plane only, like action_prestate.
CREATE TABLE actuation_target_state (
  target         text PRIMARY KEY,                    -- the action's target (host/guest/resource), the admission key
  claimed_by     text NOT NULL DEFAULT '',            -- the claiming session's external ref; '' = unclaimed
  claimed_at     timestamptz,                         -- when the active claim was taken; NULL = unclaimed
  cooldown_until timestamptz,                         -- refuse admission until this instant; NULL = no cooldown
  last_error     text NOT NULL DEFAULT ''             -- the disturbance note behind the current/last cooldown
);

COMMENT ON TABLE actuation_target_state IS 'plane: actuation';
