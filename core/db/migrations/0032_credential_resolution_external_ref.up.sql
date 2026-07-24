-- 0032: add the external_ref correlation key to credential_resolution (spec/020 REQ-2015, depth projection).
--
-- credential_resolution (migration 0018) records one NON-SECRET row per machine-plane identity resolution
-- (target, resolved user, connection scheme, and the SCHEME of the key reference only — never key material,
-- INV-13). It carried only the target label, so the decision tracer could not join a resolution back to the
-- session that triggered it — the credential-identity boundary the walk promised had no session key. This one
-- additive, defaulted, NON-SECRET column closes it: external_ref is the same normalized incident id the rest of
-- the spine keys on (the resolver reads it from the investigation activity's context), so a persisted
-- resolution joins straight into the per-incident walk. Backward-compatible: pre-fix rows read as an empty
-- external_ref (the correlation was simply absent, exactly as before). credential_resolution is append-only
-- (0018 REVOKE UPDATE,DELETE from tg_runtime); this is a DDL add by the migration role, not a runtime write.
ALTER TABLE credential_resolution
    ADD COLUMN external_ref text NOT NULL DEFAULT '';

-- Index the correlation key so the tracer's per-session read is not a full scan.
CREATE INDEX IF NOT EXISTS credential_resolution_ref_idx ON credential_resolution (external_ref);
