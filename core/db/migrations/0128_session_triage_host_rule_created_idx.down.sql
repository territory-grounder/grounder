-- Revert 0128: drop the (host, alert_rule, created_at) index on session_triage (TG-667). Reads fall back to scans.
DROP INDEX IF EXISTS session_triage_host_rule_created;
