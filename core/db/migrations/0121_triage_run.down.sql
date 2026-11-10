-- Revert 0121: drop the per-run triage record (TG-633). Observability only; nothing gates on it.
DROP TABLE IF EXISTS triage_run;
