-- 0059 — THE DATABASE HALF OF THE CREDENTIAL-PLANE SPLIT (TG-164, follow-on to TG-153).
--
-- WHAT WAS STILL SHARED. TG-153 split the worker into two processes under two OpenBao AppRoles, and OpenBao
-- verifiably refuses each plane the other's secrets (403/200 measured in both directions, 2026-08-04). The
-- split stopped at the secret store. Measured on the live box the same day:
--
--     postgres roles: postgres(super) | tg_migration | tg_runtime
--     worker           -> tg_runtime
--     worker-actuate   -> tg_runtime
--
-- So a compromised TRIAGE worker — the process that reads untrusted alert bodies, device syslog, host command
-- output and ticket text — held the ACTUATION worker's full database authority. OpenBao said it could not
-- fetch the actuation KEY; the shared DB role said it could still forge the RECORD of an actuation, or corrupt
-- the state the gates read back. An intruder who cannot mutate the estate but can write action_verdict,
-- action_execution and policy_decision can manufacture a clean execution history, and this codebase READS that
-- history (prior verdicts, the graduation ladder's evidence, the console's audit surface).
--
-- WHAT THIS MIGRATION IS. It installs ONE function, tg_apply_plane_grants(), which derives a plane role's
-- table privileges FROM tg_runtime's and withholds writes on the named off-plane tables. It creates no role
-- (a role needs a password, which must never live in a migration) and it grants nothing on its own: the
-- composition root calls it after Migrate (cmd/grounder/main.go → db.ApplyPlaneGrants). If the roles do not
-- exist the call is a no-op and the deployment is byte-identical to today.
--
-- WHY DERIVE FROM tg_runtime RATHER THAN ENUMERATE. Fourteen migrations have carved append-only posture into
-- tg_runtime by REVOKE (0015 governance_ledger/session_risk_audit/action_verdict, 0018, 0019, 0020, 0022,
-- 0029, 0030, 0031, 0033, 0034, 0042, 0043, 0048, 0049, 0050, 0053, 0055, 0058). A hand-written GRANT list
-- for the new roles would silently re-grant every one of those UPDATE/DELETE privileges — a privilege
-- ESCALATION shipped inside a hardening change, which is the worst possible way to get this wrong. Deriving
-- means the plane roles can never hold a privilege tg_runtime does not, today or after the next migration.
--
-- WHY IT IS RE-RUNNABLE RATHER THAN A ONE-SHOT DDL BLOCK. Migrations run once, by version. The plane roles
-- are created by the operator (deploy/postgres-init/01-plane-roles.sh at first init, or by hand on an
-- existing database) and that may happen LONG after 0059 applied. A one-shot GRANT block would then have run
-- against roles that did not exist yet and never run again — the grants would be silently absent and the
-- split worker would fail with a permission error deep inside an activity. Calling the function on every
-- grounder boot makes the privilege state converge: create the roles whenever you like, restart, done. It
-- also picks up tables added by LATER migrations, which a frozen grant list could not.
--
-- READ THIS BEFORE ADDING A TABLE THAT RECORDS OR AUTHORISES AN ACTUATION. The withheld-table lists live in
-- Go (core/db/plane_roles.go, ActuationAuthorityTables / TriageContentTables) and are passed in as arrays.
-- A new table is granted to BOTH planes by default — fail-OPEN by design, because the alternative is a
-- worker that boots green and dies inside an activity — so a new actuation-record table MUST be added to
-- that list. TestPlaneWithheldTablesAreRealAndClassified pins the lists against the live schema.

-- p_role          : the plane role to (re)apply privileges for; a no-op returning -1 when it does not exist.
-- p_source_role   : the role whose privileges are mirrored (tg_runtime — the un-split posture).
-- p_withhold_write: tables this plane may READ but not INSERT/UPDATE/DELETE.
-- Returns the number of table privileges GRANTED (not the number withheld) so the caller can refuse a
-- vacuous application — see ApplyPlaneGrants' floor.
CREATE OR REPLACE FUNCTION tg_apply_plane_grants(
  p_role text,
  p_source_role text,
  p_withhold_write text[]
) RETURNS integer
LANGUAGE plpgsql
AS $fn$
DECLARE
  t         record;
  f         record;
  priv      text;
  granted   integer := 0;
  mirrored  boolean;
  withheld  boolean;
BEGIN
  IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = p_role) THEN
    RETURN -1;  -- the operator has not opted in; changing nothing is the correct answer
  END IF;
  IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = p_source_role) THEN
    RAISE EXCEPTION 'tg_apply_plane_grants: source role % does not exist — there is nothing to mirror, and '
      'granting a guessed privilege set to % would be an escalation dressed as a hardening',
      p_source_role, p_role;
  END IF;

  EXECUTE format('GRANT USAGE ON SCHEMA public TO %I', p_role);

  -- Walked over pg_class by OID rather than pg_tables by name. has_table_privilege(role, name, priv) has to
  -- resolve the name, and Postgres does not promise to apply the schema filter first -- the name form dies
  -- with 42P01 on 'public.pg_statistic' while the database is perfectly healthy. relkind r/p = ordinary and
  -- partitioned tables; views and sequences are handled by their own grants below.
  FOR t IN
    SELECT c.oid AS oid, c.relname AS relname
    FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
    WHERE n.nspname = 'public' AND c.relkind IN ('r', 'p')
    ORDER BY c.relname
  LOOP
    FOREACH priv IN ARRAY ARRAY['SELECT', 'INSERT', 'UPDATE', 'DELETE'] LOOP
      -- Does the un-split runtime role hold this privilege here? If not, neither plane may.
      mirrored := has_table_privilege(p_source_role, t.oid, priv);
      -- Is this an off-plane WRITE? Reads are never withheld: both planes read each other's records (the
      -- triage workflow reads back the verdict the actuation plane wrote, and refusing that read would
      -- break verification rather than harden anything).
      withheld := (priv <> 'SELECT') AND (t.relname = ANY (p_withhold_write));
      IF mirrored AND NOT withheld THEN
        EXECUTE format('GRANT %s ON public.%I TO %I', priv, t.relname, p_role);
        granted := granted + 1;
      ELSE
        -- REVOKE the complement, not merely "skip the grant". This function's job is to make the privilege
        -- state CONVERGE on every boot: a table moved onto the withheld list after a previous run must lose
        -- the privilege it was already granted, or the list would document a control that is not in force.
        EXECUTE format('REVOKE %s ON public.%I FROM %I', priv, t.relname, p_role);
      END IF;
    END LOOP;
  END LOOP;

  -- Sequences: mirror deploy/postgres-init/00-roles.sh's blanket USAGE,SELECT. Every append path in this
  -- schema uses a bigserial id, so a role with INSERT and no sequence USAGE fails at the first write with a
  -- permission error that names the SEQUENCE, not the table — an hour of the wrong hypothesis.
  EXECUTE format('GRANT USAGE, SELECT ON ALL SEQUENCES IN SCHEMA public TO %I', p_role);

  -- Functions: mirror the source role's EXECUTE. This is what carries reap_agent_step_evidence (0055) to the
  -- plane roles — the evidence reaper's loop runs in BOTH worker processes, and the SECURITY DEFINER path is
  -- the only deletion path either of them has.
  FOR f IN
    SELECT p.oid AS oid, p.oid::regprocedure AS sig
    FROM pg_proc p JOIN pg_namespace n ON n.oid = p.pronamespace
    WHERE n.nspname = 'public'
  LOOP
    IF has_function_privilege(p_source_role, f.oid, 'EXECUTE') THEN
      EXECUTE format('GRANT EXECUTE ON FUNCTION %s TO %I', f.sig, p_role);
    END IF;
  END LOOP;

  RETURN granted;
END;
$fn$;

COMMENT ON FUNCTION tg_apply_plane_grants(text, text, text[]) IS
  'TG-164: (re)derive a credential-plane DB role''s privileges from tg_runtime, withholding writes on the named off-plane tables. Idempotent and convergent; a no-op returning -1 when the role does not exist. Called from the composition root after every migration run (db.ApplyPlaneGrants).';
