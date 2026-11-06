-- 0108 — Ed25519 provenance signature on the action-verdict ledger (TG-81 borrow 3; core/verdictsig).
--
-- '' = unsigned (every pre-0108 row, and every row written while no signing seed is configured) — accepted
-- as pre-feature history by every reader. A NON-empty signature must verify against the deployment's public
-- key or the row is treated as an ABSENT verdict by the prior-verdict reader (evidence removed, review
-- raised): the row was not written through the interceptor's VerdictSink by a seed-holding process.
ALTER TABLE action_verdict ADD COLUMN signature text NOT NULL DEFAULT '';
