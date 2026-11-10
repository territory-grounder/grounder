-- 0134: workflow_auth_nonce — replay protection for the signed authorization envelope (TG-660, deep-review F1).
--
-- The grounder signs every admin-write workflow Request and every approval vote (temporal/wfauth: HMAC over the
-- request's authorization fields + a 16-byte random nonce + an expiry). The worker's first activity verifies the
-- MAC and then CLAIMS the nonce here with INSERT ... ON CONFLICT DO NOTHING: the first claim wins, a second
-- presentation of the same envelope (a captured Temporal payload replayed against the frontend) finds the row
-- and is refused as a replay. The claim is the LAST step of verification — a tampered or expired envelope never
-- reaches this table, so a flood of garbage cannot fill it.
--
-- plane: both — the triage plane verifies admin writes and votes (tg.runner); nothing on the actuation plane
-- writes here today, but the claim is not actuation AUTHORITY (the ledger row is) nor attacker-authored content,
-- so neither withheld-write list applies. INSERT-only for the runtime role (the 0105 default: SELECT+INSERT),
-- which is exactly the shape replay protection needs — a role that could DELETE a nonce could un-replay it.
--
-- Retention: one ~100-byte row per admin write or vote (a few hundred a day at most); rows past expires_at are
-- useless but harmless, and the history IS the protection. A purge function is a follow-up, declared in
-- core/db/retention_coverage_test.go.
CREATE TABLE workflow_auth_nonce (
    nonce      text        PRIMARY KEY CHECK (length(nonce) >= 16),
    kind       text        NOT NULL CHECK (kind <> ''),
    expires_at timestamptz NOT NULL,
    seen_at    timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX workflow_auth_nonce_expires_at ON workflow_auth_nonce (expires_at);
COMMENT ON TABLE workflow_auth_nonce IS 'plane: both';
