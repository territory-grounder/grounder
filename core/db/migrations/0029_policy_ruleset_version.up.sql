-- 0029: immutable, versioned ruleset record so a past policy_decision joins the EXACT ACL document in force
-- when it decided (spec/020 T-020-6, REQ-2018), instead of the singleton latest-wins policy_ruleset (0019).
--
-- policy_ruleset holds ONE active rules-as-data document (latest-wins on the singleton PK), and every
-- policy_decision records the bundle_version (the content fingerprint of the rules-as-data, REQ-1522) it was
-- evaluated over. But once the operator changes the ruleset, the singleton is overwritten, so a PAST decision's
-- bundle_version could no longer resolve to the document that actually produced it — the decision tracer could
-- not show which exact ACL doc authorized/refused a historical action. This table keeps EVERY ruleset version
-- keyed by its bundle_version: PolicyRulesetStore.Save writes it (first-wins per version, since the same
-- bundle_version is the same content by construction), and a past decision JOINs
-- policy_decision.bundle_version -> policy_ruleset_version.bundle_version to recover the exact immutable doc.
-- NON-SECRET (rules-as-data: op-classes, host globs, verdicts, params — no credential or key material).
CREATE TABLE policy_ruleset_version (
  bundle_version text PRIMARY KEY CHECK (length(btrim(bundle_version)) > 0),  -- content fingerprint (REQ-1522) = the join key
  document       jsonb   NOT NULL,                                            -- the exact rules-as-data doc in force at this version
  rule_count     integer NOT NULL DEFAULT 0,
  saved_by       text    NOT NULL DEFAULT '',
  saved_at       timestamptz NOT NULL DEFAULT now(),
  schema_version integer NOT NULL DEFAULT 1 CHECK (schema_version > 0)
);

-- Immutable: a version, once recorded, is never rewritten (same bundle_version = same content). Revoke the DML
-- runtime role's UPDATE/DELETE so the versioned audit is tamper-resistant, exactly like the accountability
-- spine (0015) and policy_decision (0019). tg_migration (the table owner) is unaffected. The runtime role keeps
-- SELECT + INSERT (INSERT ... ON CONFLICT DO NOTHING is first-wins, never a rewrite).
REVOKE UPDATE, DELETE ON policy_ruleset_version FROM tg_runtime;
