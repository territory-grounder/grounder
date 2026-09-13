-- Reverse 0021: drop the cross-process mutation-breaker state. The armed breaker then falls back to its
-- in-process store, so a trip no longer force-Shadows sibling workers — safe only while a single worker runs
-- (mutation defaults OFF regardless; the governance_ledger 'safety:breaker-trip' record is unaffected).
DROP TABLE IF EXISTS mutation_breaker_state;
