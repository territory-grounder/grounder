#!/bin/bash
# Create the two least-privilege roles for Territory Grounder's role model (INV-16, P0-3):
#   tg_migration — owns DDL, used only by the startup migrator.
#   tg_runtime   — DML ONLY, not a table owner → FORCE ROW LEVEL SECURITY applies to it, so a
#                  cross-tenant SELECT returns zero rows and a runtime CREATE TABLE fails on privilege.
# Passwords come from the environment (never hard-coded here).
set -euo pipefail

psql -v ON_ERROR_STOP=1 --username "$POSTGRES_USER" --dbname grounder <<-SQL
  -- pgvector (semantic retrieval, migration 0013): created here as the superuser because CREATE EXTENSION
  -- needs privileges tg_migration deliberately lacks. The migration's own CREATE EXTENSION IF NOT EXISTS
  -- is then a no-op. On a database initialized BEFORE this line existed, run it once by hand as the
  -- superuser (init scripts only run at first init):  CREATE EXTENSION IF NOT EXISTS vector;
  CREATE EXTENSION IF NOT EXISTS vector;

  CREATE ROLE tg_migration LOGIN PASSWORD '${TG_MIGRATION_PASSWORD}';
  CREATE ROLE tg_runtime   LOGIN PASSWORD '${TG_RUNTIME_PASSWORD}';

  -- migration role owns the schema (DDL); it runs the versioned migrations at startup.
  GRANT ALL ON SCHEMA public TO tg_migration;

  -- runtime role: connect + read/write DATA only, no DDL, no ownership.
  GRANT USAGE ON SCHEMA public TO tg_runtime;
  -- APPEND-ONLY IS THE DEFAULT, MUTABILITY IS OPT-IN (TG-80 P1-3). Every new table is born
  -- SELECT+INSERT only; a table that genuinely needs UPDATE/DELETE (a latest-wins projection, an upsert
  -- cache) GRANTs them back explicitly in the migration that CREATEs it. Before this flip the default was
  -- GRANT ALL and ~24 migrations subtracted UPDATE/DELETE one audit table at a time — a privilege model
  -- assembled by wildcard-then-subtract, where a forgotten REVOKE silently shipped a mutable audit table.
  -- Init scripts run only at FIRST init: the retrofit for an already-initialised database is migration
  -- 0105, which REVOKEs across the existing tables and re-grants the enumerated mutable set.
  ALTER DEFAULT PRIVILEGES FOR ROLE tg_migration IN SCHEMA public
    GRANT SELECT, INSERT ON TABLES TO tg_runtime;
  ALTER DEFAULT PRIVILEGES FOR ROLE tg_migration IN SCHEMA public
    GRANT USAGE, SELECT ON SEQUENCES TO tg_runtime;
SQL
