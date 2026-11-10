-- 0124: TRIP ATTRIBUTION and the ONE admitted compensation on the mutation breaker (TG-665, G4A finding F7).
--
-- WHAT WAS WRONG. A forward `deviation` tripped the breaker (threshold 1) at the interceptor and the trip
-- force-Shadowed the owner-set mode; the commit-confirm window then fired the compensating inverse, which every
-- worker refused at gate 4e (breaker OPEN) and at 4f (mode Shadow) and at the leaf's own guard. The compensation
-- was lost on the one case it exists for, and the row said nothing about WHO tripped or WHY.
--
-- WHAT THIS ADDS (owner's fix-first prompt of 2026-11-07, coordinator ruling 1):
--   tripped_by_action_id      — the action whose failure OPENED the breaker (first trip wins);
--   trip_cause                — why: a closed vocabulary (core/safety names it). A deviation-caused trip opens the
--                               breaker DURABLY — every worker refuses every FORWARD at gate 4e while the owner-set
--                               mode row stays — and admits exactly ONE compensating inverse of that action; every
--                               other cause (kill-switch, operator, cost, chain-gap, revert-failed, unknown) still
--                               forces Shadow and blocks everything, compensation included. A later non-deviation
--                               trip ESCALATES the cause; a close clears all four columns;
--   compensation_action_id    — the one inverse admitted through the open breaker (set once by a CAS: open ∧
--                               trip_cause = 'deviation' ∧ tripped_by matches ∧ not yet set);
--   compensation_admitted_at  — when.
-- Latest-wins current-state row, exactly like the rest of this table (0021); the tamper-evident record is the
-- governance_ledger `safety:breaker-trip` / `actuate:compensation-under-trip` entries. NULL on every pre-0124 row.
-- The never-auto floor, the mode chokepoint and the leaf guards are untouched: the compensation still traverses
-- every other gate. tg_runtime keeps its existing UPDATE on this table (0021/0105 mutable working set).
ALTER TABLE mutation_breaker_state
    ADD COLUMN tripped_by_action_id text,
    ADD COLUMN trip_cause text CHECK (trip_cause IS NULL OR trip_cause IN
        ('deviation', 'chain-gap', 'revert-failed', 'kill-switch', 'operator', 'cost', 'unknown')),
    ADD COLUMN compensation_action_id text,
    ADD COLUMN compensation_admitted_at timestamptz;

COMMENT ON COLUMN mutation_breaker_state.tripped_by_action_id IS
    'TG-665: the action whose failure opened the breaker (first trip wins); NULL when closed or unattributed';
COMMENT ON COLUMN mutation_breaker_state.trip_cause IS
    'TG-665: why the breaker opened — deviation keeps the mode owner-set and admits one compensation; every other cause forces Shadow';
COMMENT ON COLUMN mutation_breaker_state.compensation_action_id IS
    'TG-665: the ONE compensating inverse admitted through the open breaker (set once by CAS); NULL = none yet';
COMMENT ON COLUMN mutation_breaker_state.compensation_admitted_at IS
    'TG-665: when the one compensation was admitted';
