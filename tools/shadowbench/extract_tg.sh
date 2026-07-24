#!/usr/bin/env bash
# extract_tg.sh — Territory Grounder triage-record extractor for the shadow-window head-to-head benchmark.
#
# WHAT IT DOES
#   Pulls TERRITORY GROUNDER's own triage records (session_triage) for a time window, joined with the
#   mean judge score (session_judgment), so they can be compared head-to-head against the predecessor
#   (claude-gateway) over the same shadow window. Emits one CSV-ish psql table + a JSON blob per row.
#
# WHY A FILE + docker cp
#   The app DB runs inside the `territory-grounder-postgres-1` container on the box. Piping SQL through
#   `docker exec ... psql -c "..."` over SSH means the query text is mangled by THREE shells (local,
#   ssh, container). We instead write the SQL to a local file, scp + docker cp it in, and run with -f.
#   No shell-quoting of the query, no `sh -c`. Credentials are read from the on-box .env and passed via
#   PGPASSWORD *inside* the container only — never printed, never logged.
#
# WINDOW
#   The predecessor entered shadow mode at 2026-07-18 17:20 UTC. Default window = that boundary onward.
#   Override with $SHADOW_FROM (a timestamptz literal, e.g. '2026-07-18 17:20:00+00').
#
# FILTERS (match the benchmark spec)
#   external_ref LIKE '%librenms%'          -- real LibreNMS-sourced alerts only
#   external_ref NOT LIKE '%tz-%'           -- exclude synthetic tz-val / tz-accuracy probes
#   external_ref NOT LIKE '%probe%'         -- exclude qa-portstatus / probe rows
#   (also, structurally, e2e-/synth-val rows will be visible for triage but are real replays; keep them
#    unless you tighten the WHERE — see NOTE at bottom.)
#
# JUDGE SCORE
#   session_judgment has NO explicit "overall" dimension; it stores per-dimension scores
#   (appropriate_band, correct_diagnosis, evidence_grounded, sensible_proposal, falsifiable_prediction).
#   judgeOverall here = AVG(score) across whatever dimensions were judged for that external_ref.
#
# USAGE
#   ./extract_tg.sh                       # default window (2026-07-18 17:20:00+00 onward)
#   SHADOW_FROM='2026-07-18 00:00:00+00' ./extract_tg.sh
#   TG_HOST=dc1tg01 SSH_KEY=~/.ssh/one_key ./extract_tg.sh
#
# SAFETY
#   Read-only (SELECT only). No mutation. Credentials are redacted from all output.
set -euo pipefail

TG_HOST="${TG_HOST:-dc1tg01}"
SSH_KEY="${SSH_KEY:-$HOME/.ssh/one_key}"
SSH_USER="${SSH_USER:-root}"
ENV_PATH="${ENV_PATH:-/srv/tg/deploy/.env}"
PG_CONTAINER="${PG_CONTAINER:-territory-grounder-postgres-1}"
SHADOW_FROM="${SHADOW_FROM:-2026-07-18 17:20:00+00}"
# TG_FORMAT=full (default) → the human psql tables + framed JSON (v1 behaviour, unchanged).
# TG_FORMAT=jsononly      → emit ONLY a clean JSON array of the window's real triage rows on stdout
#                           (for run.sh to parse). Drops the librenms-only restriction so the driver
#                           can align/tag EVERY real TG triage in the window; still excludes the
#                           synthetic tz-/probe rows. Uses psql -tAq so nothing but the JSON prints.
TG_FORMAT="${TG_FORMAT:-full}"

SSH=(ssh -i "$SSH_KEY" -o StrictHostKeyChecking=no -o ConnectTimeout=15 "${SSH_USER}@${TG_HOST}")
SCP=(scp -i "$SSH_KEY" -o StrictHostKeyChecking=no)

TMP_SQL="$(mktemp)"
trap 'rm -f "$TMP_SQL"' EXIT

if [ "$TG_FORMAT" = "jsononly" ]; then
  PSQL_FLAGS="-tAq"
  cat > "$TMP_SQL" <<SQL
SELECT COALESCE(json_agg(row_to_json(x)), '[]'::json) FROM (
  SELECT
    t.host,
    t.alert_rule                                 AS "alertRule",
    t.band,
    t.op,
    t.outcome,
    t.proposed,
    t.predicted,
    left(t.conclusion, 600)                      AS "conclusionExcerpt",
    COALESCE(array_length(t.evidence_ids, 1), 0) AS "evidenceCount",
    t.evidence_ids                               AS "evidenceIds",
    (t.prediction <> '')                         AS "hasPrediction",
    left(t.prediction, 900)                      AS "prediction",
    t.created_at                                 AS "createdAt",
    t.external_ref                               AS "external_ref"
  FROM session_triage t
  WHERE t.created_at > TIMESTAMPTZ '${SHADOW_FROM}'
    AND t.external_ref NOT LIKE '%tz-%'
    AND t.external_ref NOT LIKE '%probe%'
  ORDER BY t.created_at
) x;
SQL
  "${SCP[@]}" "$TMP_SQL" "${SSH_USER}@${TG_HOST}:/tmp/extract_tg.sql" >/dev/null
  "${SSH[@]}" '
    docker cp /tmp/extract_tg.sql '"$PG_CONTAINER"':/tmp/extract_tg.sql >/dev/null
    docker exec \
      -e PGPASSWORD="$(grep -E "^PG_SUPERUSER_PASSWORD=" '"$ENV_PATH"' | cut -d= -f2-)" \
      '"$PG_CONTAINER"' psql -U postgres -d grounder '"$PSQL_FLAGS"' -f /tmp/extract_tg.sql
    rm -f /tmp/extract_tg.sql
  ' | sed -E 's/(password|PGPASSWORD)=[^ ]*/\1=<redacted>/gi'
  exit 0
fi

# NOTE: $SHADOW_FROM is interpolated into the SQL as a literal. It is operator-supplied, not untrusted
# input; keep it a bare timestamptz literal. The DB session TZ is Etc/UTC, so a bare literal without an
# offset is already UTC — we still append nothing and rely on the +00 in the default for clarity.
cat > "$TMP_SQL" <<SQL
\pset pager off
\echo '=== TG triage records — shadow window from ${SHADOW_FROM} ==='
SELECT
  t.host,
  t.alert_rule                                   AS "alertRule",
  t.band,
  t.op,
  left(t.conclusion, 300)                        AS "conclusionExcerpt",
  COALESCE(array_length(t.evidence_ids, 1), 0)   AS "evidenceCount",
  (t.prediction <> '')                           AS "hasPrediction",
  t.created_at                                   AS "createdAt",
  t.external_ref,
  j.overall                                      AS "judgeOverall"
FROM session_triage t
LEFT JOIN (
  SELECT external_ref, round(avg(score)::numeric, 3) AS overall
  FROM session_judgment
  GROUP BY external_ref
) j ON j.external_ref = t.external_ref
WHERE t.created_at > TIMESTAMPTZ '${SHADOW_FROM}'
  AND t.external_ref LIKE '%librenms%'
  AND t.external_ref NOT LIKE '%tz-%'
  AND t.external_ref NOT LIKE '%probe%'
ORDER BY t.created_at;

\echo ''
\echo '=== same window, as JSON (one object per row) ==='
SELECT json_agg(row_to_json(x)) FROM (
  SELECT
    t.host,
    t.alert_rule                                 AS "alertRule",
    t.band,
    t.op,
    left(t.conclusion, 300)                      AS "conclusionExcerpt",
    COALESCE(array_length(t.evidence_ids, 1), 0) AS "evidenceCount",
    (t.prediction <> '')                         AS "hasPrediction",
    t.created_at                                 AS "createdAt",
    t.external_ref,
    j.overall                                    AS "judgeOverall"
  FROM session_triage t
  LEFT JOIN (
    SELECT external_ref, round(avg(score)::numeric, 3) AS overall
    FROM session_judgment GROUP BY external_ref
  ) j ON j.external_ref = t.external_ref
  WHERE t.created_at > TIMESTAMPTZ '${SHADOW_FROM}'
    AND t.external_ref LIKE '%librenms%'
    AND t.external_ref NOT LIKE '%tz-%'
    AND t.external_ref NOT LIKE '%probe%'
  ORDER BY t.created_at
) x;

\echo ''
\echo '=== predecessor-incident cross-check: any librespeed / dc1librespeed01 record (ANY time) ==='
SELECT external_ref, host, alert_rule, created_at
FROM session_triage
WHERE host ILIKE '%librespeed%' OR external_ref ILIKE '%librespeed%'
ORDER BY created_at;

\echo ''
\echo '=== window sanity: ALL triage rows in window (unfiltered), to see what TG actually processed ==='
SELECT external_ref, host, alert_rule, created_at
FROM session_triage
WHERE created_at > TIMESTAMPTZ '${SHADOW_FROM}'
ORDER BY created_at;
SQL

# Ship the SQL in and run it. PGPASSWORD is resolved from the on-box .env inside the container command;
# it never touches this host's process table or stdout.
"${SCP[@]}" "$TMP_SQL" "${SSH_USER}@${TG_HOST}:/tmp/extract_tg.sql" >/dev/null
"${SSH[@]}" '
  docker cp /tmp/extract_tg.sql '"$PG_CONTAINER"':/tmp/extract_tg.sql >/dev/null
  docker exec \
    -e PGPASSWORD="$(grep -E "^PG_SUPERUSER_PASSWORD=" '"$ENV_PATH"' | cut -d= -f2-)" \
    '"$PG_CONTAINER"' psql -U postgres -d grounder -f /tmp/extract_tg.sql
  rm -f /tmp/extract_tg.sql
' | sed -E 's/(password|PGPASSWORD)=[^ ]*/\1=<redacted>/gi'
