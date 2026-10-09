DO $$
BEGIN
  IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'tg_migration') THEN
    EXECUTE 'ALTER DEFAULT PRIVILEGES FOR ROLE tg_migration IN SCHEMA public GRANT UPDATE, DELETE ON TABLES TO tg_runtime';
  END IF;
END $$;
DROP TABLE IF EXISTS observation_coverage;
