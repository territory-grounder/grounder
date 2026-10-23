#!/bin/bash
# Create the two CREDENTIAL-PLANE database roles (TG-164, follow-on to TG-153).
#
# WHY THESE EXIST. TG-153 split the worker into two processes under two OpenBao AppRoles; OpenBao refuses each
# plane the other's secrets. Both processes still connected to Postgres as tg_runtime, so the triage worker —
# the one that reads untrusted alert bodies, device syslog and host command output — held the actuation
# worker's full database authority. It could not fetch the actuation KEY; it could still write the RECORD of
# an actuation (action_verdict, action_execution, interceptor_gate_verdict, policy_decision) and poison the
# state the gates and the console read back. See core/db/plane_roles.go for how the table lists were traced.
#
#   tg_triage  — the worker whose plane is `triage`.    May NOT write what records or authorises an actuation.
#   tg_actuate — the worker whose plane is `actuation`. May NOT write the untrusted-content corpus a mutation
#                is grounded in.
#
# THIS SCRIPT CREATES ROLES AND NOTHING ELSE. The privileges are DERIVED from tg_runtime's by
# db.ApplyPlaneGrants, which the grounder runs after every migration pass — because a role's privileges must
# track a schema that keeps growing, and because these roles may be created long after the migration that
# taught the database how to derive them.
#
# ★ OPT-IN, AND SILENT WHEN NOT OPTED IN. With the two passwords unset this script does nothing at all: the
# deployment stays exactly as it was, both workers keep using tg_runtime, and the worker's boot log says so in
# words ("LIVE EXPOSURE ... the DATABASE split is NOT in force"). That matches TG-153's posture — a security
# fix that broke every existing single-worker deployment on upgrade would be reverted, and a reverted control
# protects nobody.
#
# ★ ON AN ALREADY-INITIALISED DATABASE THIS FILE NEVER RUNS. docker-entrypoint-initdb.d executes only at
# FIRST init. To split an existing deployment, run the two CREATE ROLE statements below by hand as the
# superuser and restart the grounder — ApplyPlaneGrants converges the privileges on that boot:
#
#     psql -U postgres -d grounder \
#       -c "CREATE ROLE tg_triage  LOGIN PASSWORD '...';" \
#       -c "CREATE ROLE tg_actuate LOGIN PASSWORD '...';"
#     docker compose restart grounder     # applies the derived grants
#     # then set TG_DB_DSN_TRIAGE / TG_DB_DSN_ACTUATE in .env and restart the workers
set -euo pipefail

if [ -z "${TG_TRIAGE_PASSWORD:-}" ] && [ -z "${TG_ACTUATE_PASSWORD:-}" ]; then
  echo "01-plane-roles: TG_TRIAGE_PASSWORD / TG_ACTUATE_PASSWORD unset — NOT creating the credential-plane" \
       "database roles. Both worker planes will connect as tg_runtime, exactly as before TG-164."
  exit 0
fi

# Refuse a HALF split. One role without the other means one worker authenticates as a plane role and the
# other silently keeps tg_runtime's full authority, while the operator's runbook says the planes are split —
# the same failure class as a co-holding worker that logs "plane split OK".
if [ -z "${TG_TRIAGE_PASSWORD:-}" ] || [ -z "${TG_ACTUATE_PASSWORD:-}" ]; then
  echo "01-plane-roles: REFUSING a half split — set BOTH TG_TRIAGE_PASSWORD and TG_ACTUATE_PASSWORD, or" \
       "neither. One plane role without the other leaves the other plane on tg_runtime's full authority" \
       "while the deployment reads as split." >&2
  exit 1
fi

psql -v ON_ERROR_STOP=1 --username "$POSTGRES_USER" --dbname grounder <<-SQL
  CREATE ROLE tg_triage  LOGIN PASSWORD '${TG_TRIAGE_PASSWORD}';
  CREATE ROLE tg_actuate LOGIN PASSWORD '${TG_ACTUATE_PASSWORD}';

  -- CONNECT + schema visibility only. Every table, sequence and function privilege is DERIVED from
  -- tg_runtime's by db.ApplyPlaneGrants on the grounder's next boot -- deliberately NOT enumerated here,
  -- because fourteen migrations have carved append-only posture into tg_runtime by REVOKE and a hand-written
  -- grant list would silently re-grant every one of them. A privilege ESCALATION shipped inside a hardening
  -- change is the worst way to get this wrong.
  GRANT USAGE ON SCHEMA public TO tg_triage, tg_actuate;
SQL

echo "01-plane-roles: created tg_triage + tg_actuate (privileges are derived on the grounder's next boot;" \
     "set TG_DB_DSN_TRIAGE / TG_DB_DSN_ACTUATE to make the workers use them)."
