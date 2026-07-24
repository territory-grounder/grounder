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
  ALTER DEFAULT PRIVILEGES FOR ROLE tg_migration IN SCHEMA public
    GRANT SELECT, INSERT, UPDATE, DELETE ON TABLES TO tg_runtime;
  ALTER DEFAULT PRIVILEGES FOR ROLE tg_migration IN SCHEMA public
    GRANT USAGE, SELECT ON SEQUENCES TO tg_runtime;
SQL
