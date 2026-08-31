-- 0008: per-source static ingest-push token (AuthIngestPush, spec/006).
--
-- Push sources that cannot HMAC-sign the body (e.g. Prometheus Alertmanager webhooks) authenticate the
-- ingest front door with a per-source static bearer token instead. The token is stored BY REFERENCE
-- (env:/file:, INV-13), never as a literal. A source row must carry at least one credential: an HMAC
-- secret ref, an ingest token ref, or both.

ALTER TABLE sources ALTER COLUMN hmac_secret_ref DROP NOT NULL;
ALTER TABLE sources ADD COLUMN ingest_token_ref text;
ALTER TABLE sources ADD CONSTRAINT sources_has_credential
  CHECK (hmac_secret_ref IS NOT NULL OR ingest_token_ref IS NOT NULL);
