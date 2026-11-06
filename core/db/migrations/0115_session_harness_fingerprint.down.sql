-- Revert 0115: drop the harness identity column (TG-575). Observability only; nothing gates on it.
ALTER TABLE session_triage
    DROP COLUMN IF EXISTS harness_fp;
