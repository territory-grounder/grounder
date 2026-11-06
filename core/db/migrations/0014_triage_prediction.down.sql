-- Reverse 0014: drop the prediction columns. The triage round-trip degrades to blind
-- falsifiable_prediction scoring, exactly as before seq B.
ALTER TABLE session_triage
    DROP COLUMN IF EXISTS predicted,
    DROP COLUMN IF EXISTS prediction;
