-- Reverting drops the only record TG keeps of how an alert reached it, returning to the state where the
-- ingest path could not be reconstructed from the running system (TG-372).
ALTER TABLE ingest_alert
  DROP COLUMN IF EXISTS delivery_peer,
  DROP COLUMN IF EXISTS delivery_host;
