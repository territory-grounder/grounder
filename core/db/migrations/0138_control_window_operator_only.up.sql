REVOKE INSERT, UPDATE, DELETE, TRUNCATE, REFERENCES, TRIGGER ON control_window FROM tg_runtime;
GRANT SELECT ON control_window TO tg_runtime;
DO $$
DECLARE
  r text;
BEGIN
  FOREACH r IN ARRAY ARRAY['tg_triage', 'tg_actuate'] LOOP
    IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = r) THEN
      EXECUTE format('REVOKE INSERT, UPDATE, DELETE, TRUNCATE, REFERENCES, TRIGGER ON control_window FROM %I', r);
    END IF;
  END LOOP;
END
$$;
COMMENT ON TABLE control_window IS 'plane: operator';
