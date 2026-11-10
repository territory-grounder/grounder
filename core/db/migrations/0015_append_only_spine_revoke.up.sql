-- 0015: enforce append-only tamper-RESISTANCE on the accountability spine (readiness-review G4, INV-19).
--
-- Provenance: [O] INV-19 (append-only, hash-chained audit spine); #23 Phase-2 readiness review G4.
--
-- THE GAP: deploy/postgres-init/00-roles.sh grants tg_runtime SELECT,INSERT,UPDATE,DELETE on every table
-- via a blanket `ALTER DEFAULT PRIVILEGES ... GRANT ... ON TABLES TO tg_runtime`. Migration 0003 relied on
-- the governance_ledger hash chain for tamper-EVIDENCE "regardless of grant" -- but tg_runtime is the very
-- identity that performs mutations, and with UPDATE/DELETE it can DELETE + re-INSERT a self-consistent chain
-- that still passes VerifyChain. So the accountability record the mutation-ON canary relies on for post-hoc
-- honesty was silently rewritable BY THE APP ITSELF. Verified on dc1tg01: has_table_privilege(
-- 'tg_runtime', <spine>, 'UPDATE'/'DELETE') = t on all three.
--
-- THE FIX: revoke UPDATE + DELETE for tg_runtime on the three genuinely append-only spine tables, making
-- them tamper-RESISTANT (strictly stronger than tamper-evident). INSERT + SELECT remain, so the runtime can
-- still append audit rows and read them. tg_migration (the table owner / DDL role) is unaffected and can
-- still run this REVOKE (an owner may always revoke privileges on its own tables).
--
-- SCOPE: infragraph_prediction is intentionally EXCLUDED -- core/db/falsify.go legitimately UPDATEs a
-- prediction row with its scored outcome, so it is a mutable working-set, not part of the immutable spine.
-- If a retention job ever prunes the ledger, it MUST run as a privileged role (tg_migration / dedicated),
-- never as tg_runtime -- the app must not be able to erase its own audit trail. Future append-only tables
-- should REVOKE UPDATE,DELETE in the same migration that CREATEs them (this migration is the retrofit).
REVOKE UPDATE, DELETE ON governance_ledger  FROM tg_runtime;
REVOKE UPDATE, DELETE ON session_risk_audit FROM tg_runtime;
REVOKE UPDATE, DELETE ON action_verdict     FROM tg_runtime;
