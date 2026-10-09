-- WORKER-PUBLISHED CREDENTIAL-BINDING PROJECTION (TG-274).
--
-- The AWX inventory source resolves which named credential governs which inventory — the job-template walk
-- knows the credential name, the inventory, the host count and whether the operator mapped it — and then
-- discarded all of it. Source.Skipped() recorded only the failures and `grep -rn '\.Skipped()'` over
-- non-test code returned NOTHING: computed every sync, read by no one.
--
-- Measured on this estate 2026-08-04: AWX holds 11 Machine credentials and TG_AWX_CRED_REF_MAP maps ONE.
-- The other ten were invisible, so a deployment blind to most of the fleet looked configured. Worse, there
-- was no surface on which to say "this credential needs its key" — which is why the key had to be a FILE an
-- operator placed by hand, in a bind mount readable by the worker's uid (TG-153).
--
-- SAME SHAPE AS 0051, FOR THE SAME REASON: the source lives in the WORKER and the console is served by the
-- GROUNDER, so the worker publishes and the API reads. observed_at is load-bearing — the reader treats a
-- row older than its window as ABSENT, because a projection that outlives its publisher serves a remembered
-- answer, which is the defect one layer down.
--
-- REFERENCES ONLY, NEVER MATERIAL (INV-13). secret_ref holds a SecretRef STRING ("bao:secret/data/..."),
-- and there is deliberately no column a secret value could be written to.
CREATE TABLE credential_binding_projection (
  source_id      text        NOT NULL CHECK (length(btrim(source_id)) > 0),  -- the inventory source ("awx")
  credential     text        NOT NULL CHECK (length(btrim(credential)) > 0), -- the name AS THE SOURCE SPELLS IT
  scope          text        NOT NULL DEFAULT '',                            -- inventory/group governed
  via            text        NOT NULL DEFAULT '',                            -- what binds them (a job template)
  hosts          integer     NOT NULL DEFAULT 0 CHECK (hosts >= 0),          -- blast radius, and cost of NOT mapping
  mapped         boolean     NOT NULL DEFAULT false,                         -- TG holds a SecretRef for this name
  secret_ref     text        NOT NULL DEFAULT '',                            -- the REFERENCE; never a secret
  observed_at    timestamptz NOT NULL DEFAULT now(),
  schema_version integer     NOT NULL DEFAULT 1 CHECK (schema_version > 0),
  PRIMARY KEY (source_id, credential, scope)
);

CREATE INDEX credential_binding_projection_unmapped ON credential_binding_projection (mapped, hosts DESC);

COMMENT ON TABLE credential_binding_projection IS
  'Worker-published: which named credential each inventory source binds to which hosts, mapped and UNMAPPED alike (TG-274). Unmapped rows are the work list, not an error state. References only, never key material.';
