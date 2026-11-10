-- Revert 0134: drop the envelope replay-protection table (TG-660). Reverting re-opens replay of captured
-- admin-write / vote envelopes within their 5-minute window; the MAC check itself lives in the worker and stays.
DROP TABLE IF EXISTS workflow_auth_nonce;
