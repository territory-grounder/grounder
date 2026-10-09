-- Reverting re-orphans IncidentEnvelope.IP: four ingest modules keep populating it and nothing reads it
-- again, so an incident whose only identifier is an address becomes unattributable (TG-373).
ALTER TABLE ingest_alert DROP COLUMN IF EXISTS subject_ip;
