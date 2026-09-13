-- Reverse 0016: drop the runtime posture projection. The grounder then falls back to its OWN read-only
-- gate for /v1/whoami + /v1/governance (always OFF) — exactly the pre-fix behavior — but flags the posture
-- as unknown because no worker row can be read.
DROP TABLE IF EXISTS runtime_posture;
