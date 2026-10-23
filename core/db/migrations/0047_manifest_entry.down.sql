-- Reverses 0047. The manifest is a reviewable projection of discovery; dropping it does not touch the
-- boot-frozen env allowlists, which remain the other half of the union and the sole source when this
-- table is absent (the pre-Stage-3 posture).
DROP INDEX IF EXISTS manifest_entry_materializing;
DROP INDEX IF EXISTS manifest_entry_live_identity;
DROP TABLE IF EXISTS manifest_entry;
