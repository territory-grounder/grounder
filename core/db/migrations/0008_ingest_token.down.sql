-- 0008 down: remove the ingest-push token column. Restoring NOT NULL on hmac_secret_ref requires every
-- remaining row to carry an HMAC ref (rows created token-only must be deleted or given a secret first).

ALTER TABLE sources DROP CONSTRAINT sources_has_credential;
ALTER TABLE sources DROP COLUMN ingest_token_ref;
ALTER TABLE sources ALTER COLUMN hmac_secret_ref SET NOT NULL;
