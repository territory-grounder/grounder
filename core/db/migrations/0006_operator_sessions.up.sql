-- 0006 operator sessions — durable browser operator sessions (REQ-508) so a read-only console login
-- survives grounder restarts/redeploys instead of being wiped with the in-memory store (every deploy
-- forced a re-login). Server-side registration keeps logout authoritative; the cookie stays the signed
-- <id>.<hmac> — only the id→(operator,expiry) mapping lives here. Ephemeral operational state (not a
-- governed audit row): no schema_version. Expired rows are harmless (login verify re-checks expiry).
CREATE TABLE operator_sessions (
  session_id text PRIMARY KEY,
  operator   text NOT NULL,
  expires_at timestamptz NOT NULL,
  created_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX operator_sessions_expires_idx ON operator_sessions (expires_at);
