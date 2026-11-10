-- Revert 0135: drop the raise/reset guards (TG-661). Reverting re-opens an unaudited mode raise and breaker
-- re-arm from any runtime role; the grants to tg_mode_admin are harmless and left in place.
DROP TRIGGER IF EXISTS policy_mode_raise_guard ON policy_mode;
DROP FUNCTION IF EXISTS tg_policy_mode_raise_guard();
DROP TRIGGER IF EXISTS breaker_reset_guard ON mutation_breaker_state;
DROP FUNCTION IF EXISTS tg_breaker_reset_guard();
