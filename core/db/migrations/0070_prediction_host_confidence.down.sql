-- 0070 down: drop the additive per-host confidence column (TG-189).
ALTER TABLE infragraph_prediction DROP COLUMN predicted_host_confidence;
