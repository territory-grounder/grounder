-- Reverse 0059 (TG-164): drop the plane-grant deriver AND un-grant what it handed out.
--
-- Dropping the function alone would be a false reversal: the privileges it granted are catalog state that
-- outlives it, so tg_triage/tg_actuate would keep writing after a "rollback" while nothing in the schema
-- could re-derive or audit them. Revoke first, then drop.
--
-- The roles themselves are NOT dropped: they are created outside the migration lattice (they carry
-- passwords), a DROP ROLE fails while any session holds one, and an operator who rolls a migration back has
-- not asked to delete their deployment's identities.
DO $$
DECLARE r text;
BEGIN
  FOREACH r IN ARRAY ARRAY['tg_triage', 'tg_actuate'] LOOP
    IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = r) THEN
      EXECUTE format('REVOKE ALL ON ALL TABLES IN SCHEMA public FROM %I', r);
      EXECUTE format('REVOKE ALL ON ALL SEQUENCES IN SCHEMA public FROM %I', r);
      EXECUTE format('REVOKE ALL ON ALL FUNCTIONS IN SCHEMA public FROM %I', r);
      EXECUTE format('REVOKE USAGE ON SCHEMA public FROM %I', r);
    END IF;
  END LOOP;
END $$;

DROP FUNCTION IF EXISTS tg_apply_plane_grants(text, text, text[]);
