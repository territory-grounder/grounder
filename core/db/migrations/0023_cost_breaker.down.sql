-- Reverse 0023: drop the cross-process cost/budget spend-guard state. The cost breaker then falls back to
-- its in-process store (single-worker; a trip no longer force-Shadows sibling workers), or — if cost
-- tracking is unconfigured — is simply absent. Mutation defaults OFF regardless, and the append-only
-- governance_ledger 'cost:breaker-trip' records are unaffected (they live in a different table).
DROP TABLE IF EXISTS cost_breaker_state;
DROP TABLE IF EXISTS cost_accrual;
