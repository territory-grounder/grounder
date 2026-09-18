-- 0079 down: drop the guest power-state projection (TG-378). "Is guest X running?" becomes unanswerable
-- estate-wide again — the pre-0079 behaviour.
DROP TABLE IF EXISTS guest_liveness;
