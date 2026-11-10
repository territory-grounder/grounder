-- 0128: an index for the per-incident reads over session_triage (TG-667, fresh-eyes review 2026-11-07).
--
-- The hold watch (cmd/worker/hold_watch.go) joins every open hold to session_triage on (host, alert_rule) with
-- created_at > decided_at once a minute, and FailedInvestigationsUnretried runs the same (host, alert_rule,
-- created_at) anti-join on every GET /v1/holds. session_triage carried only a partial created_at index and the
-- synthetic-leak index, so both reads were sequential scans over a table that grows with every alert.
-- Index-only, idempotent, no data change; nothing on the decision path reads it.
CREATE INDEX IF NOT EXISTS session_triage_host_rule_created ON session_triage (host, alert_rule, created_at);
