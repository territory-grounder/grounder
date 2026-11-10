-- TG-368. tg_apply_plane_grants() aborts the ENTIRE derivation on any table the calling role cannot
-- REVOKE on, and one such table exists in production.
--
-- MEASURED on dc1tg01 2026-08-06, immediately after creating tg_triage/tg_actuate by the runbook in
-- deploy/postgres-init/01-plane-roles.sh and restarting the grounder:
--
--   credential-plane DB roles: DERIVATION FAILED (db: plane grants: derive tg_triage from tg_runtime:
--   ERROR: permission denied for table policy_ruleset_bak_handsoff (SQLSTATE 42501)) — any
--   tg_triage/tg_actuate worker is running on whatever privileges it already had, NOT the ones this
--   build declares (TG-164)
--
-- policy_ruleset_bak_handsoff is owned by `postgres`, is in NO migration in this repo, carries no plane
-- declaration, and is the only one of the 60 tables in `public` that tg_runtime cannot SELECT. It is a
-- hand-made backup whose name asks not to be touched, so the fix belongs here, not in the database.
--
-- THE DEFECT. The privilege loop is exhaustive over pg_class and has two arms:
--
--     IF mirrored AND NOT withheld THEN  GRANT  ...
--     ELSE                               REVOKE ...      <-- unconditional
--
-- For a table where tg_runtime holds nothing, `mirrored` is false for every privilege, so every iteration
-- takes the REVOKE arm. The function is SECURITY INVOKER and runs as tg_migration, which does not own that
-- table, so the REVOKE raises 42501 and the whole transaction rolls back. Nothing is derived for EITHER
-- role — one foreign table denies the plane split to the entire schema.
--
-- THE FIX. Only REVOKE when there is something to revoke. This preserves the convergence property the
-- ELSE arm exists for — a table moved ONTO the withheld list still loses the privilege it was granted,
-- because in that case the role demonstrably HAS it — while never issuing a REVOKE that could only ever
-- be a no-op. A privilege the role does not hold needs no statement to keep it that way.
--
-- WHY NOT the alternatives:
--   - GRANT tg_runtime SELECT on the stray table: widens the source role's authority inside a hardening
--     change. Deriving from a widened source is exactly the escalation 0059 was written to prevent.
--   - DROP the table: it is not ours, it is not in the repo, and its name says handsoff.
--   - SECURITY DEFINER on the function: makes it run as its owner and REVOKE succeeds — but that hands a
--     schema-wide privilege-mutating function a permanent elevation to satisfy one stray table. The
--     narrower fix is to not issue the statement.
--
-- SECOND DEFECT, found by the oracle written for the first. `p_withhold_write` arriving as SQL NULL
-- (a nil Go slice) made every write privilege evaluate to NULL and be skipped, yielding a SELECT-only
-- plane role that the caller's vacuity floor scores as success. Production is not exposed today --
-- ApplyPlaneGrants only calls withheldFor() for the two known roles, which return non-nil -- but the
-- function must not depend on its caller to avoid a NULL, because the failure is silent and the result
-- is a worker that boots green and fails inside an activity.
--
-- Everything else about 0059 is unchanged; this is a CREATE OR REPLACE of the one function.

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

  FOR t IN
    SELECT c.oid AS oid, c.relname AS relname
    FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
    WHERE n.nspname = 'public' AND c.relkind IN ('r', 'p')
    ORDER BY c.relname
  LOOP
    FOREACH priv IN ARRAY ARRAY['SELECT', 'INSERT', 'UPDATE', 'DELETE'] LOOP
      mirrored := has_table_privilege(p_source_role, t.oid, priv);
      -- COALESCE, because a NULL withhold list is not an empty one. `relname = ANY (NULL)` is NULL, so
      -- `(priv <> 'SELECT') AND NULL` is NULL for every write privilege, the IF treats NULL as false, and
      -- the plane role is granted SELECT and nothing else. Worse, ApplyPlaneGrants' `granted <= 0` floor
      -- cannot see it: the SELECTs are counted, so the function returns a healthy-looking positive number
      -- while producing a read-only role. Demonstrated in TestNullWithholdListIsNotAnEmptyOne.
      withheld := (priv <> 'SELECT') AND (t.relname = ANY (COALESCE(p_withhold_write, ARRAY[]::text[])));
      IF mirrored AND NOT withheld THEN
        EXECUTE format('GRANT %s ON public.%I TO %I', priv, t.relname, p_role);
        granted := granted + 1;
      ELSIF has_table_privilege(p_role, t.oid, priv) THEN
        -- Converge DOWN, but only where there is a privilege to remove. The guard is not an optimisation:
        -- without it this arm fires on every table the source role cannot touch, and on any one of those
        -- that the CALLER does not own the REVOKE raises 42501 and rolls back the whole derivation.
        EXECUTE format('REVOKE %s ON public.%I FROM %I', priv, t.relname, p_role);
      END IF;
    END LOOP;
  END LOOP;

  EXECUTE format('GRANT USAGE, SELECT ON ALL SEQUENCES IN SCHEMA public TO %I', p_role);

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
