-- Reverse 0042. Propose-path scores return to sharing action_verdict with executed-action verdicts (and the
-- reported verified-match rate returns to pooling two different meanings — see the up-migration).
DROP INDEX IF EXISTS prediction_verdict_created;
DROP TABLE IF EXISTS prediction_verdict;
