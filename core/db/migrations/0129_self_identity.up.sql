-- 0129: self_identity — TG's own actor identity per evidence domain, PUBLISHED by the plane that holds the credential
-- (TG-684, owner ruling 2026-11-07).
--
-- The triage plane runs attribution but never holds the actuation private key, so it cannot derive TG's own SSH
-- identity (`root!SHA256:<fp>`) the way the actuation plane does (resolveSelfSSHActor). The owner's rule keeps the
-- identity DERIVED FROM THE KEY, never typed into config or a ruleset: the actuation plane derives it at boot and
-- appends it here; the triage plane reads the newest row per domain. A row is not trusted on its own — a journal
-- record naming this identity counts as TG's only when an action_execution on the same host explains it (the
-- journal reader's correlation step), so a forged row buys at most a login that a real TG execution sat beside.
-- Append-only (INSERT + SELECT only): a key rotation is a new row, the history of identities stays readable.
CREATE TABLE IF NOT EXISTS self_identity (
    seq            bigserial PRIMARY KEY,
    domain         text NOT NULL CHECK (domain <> ''),
    actor          text NOT NULL CHECK (actor <> ''),
    plane          text NOT NULL CHECK (plane <> ''),
    published_at   timestamptz NOT NULL DEFAULT now(),
    schema_version int NOT NULL DEFAULT 1 CHECK (schema_version > 0)
);
CREATE INDEX IF NOT EXISTS self_identity_domain_latest ON self_identity (domain, published_at DESC, seq DESC);
COMMENT ON TABLE self_identity IS 'plane: actuation';
GRANT SELECT, INSERT ON self_identity TO tg_runtime;
