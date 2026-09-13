-- Revert 0074: drop the append-only occurrence log. The canonical ingest_alert record is untouched.
DROP TABLE IF EXISTS ingest_alert_occurrence;
