#!/usr/bin/env bash
# spec/009 REQ-908 — CI gate for the grounder Helm chart.
#
# Runs `helm lint` and a `helm template` render, then scans the rendered manifests for literal
# credential values. Fails the pipeline on a lint error or any literal-secret hit (INV-13). Wire this
# into CI for every change that touches deploy/helm/**.
set -euo pipefail

CHART_DIR="${CHART_DIR:-deploy/helm/grounder}"
RENDER="$(mktemp)"
trap 'rm -f "$RENDER"' EXIT

echo "==> helm lint ${CHART_DIR}"
helm lint "${CHART_DIR}"

echo "==> helm template ${CHART_DIR} (default values)"
helm template grounder "${CHART_DIR}" >"${RENDER}"

echo "==> scanning rendered output for literal credentials (spec/009 REQ-904 / INV-13)"
# Every credential in this chart MUST reach a pod via valueFrom.secretKeyRef or an ExternalSecret.
# A literal password/DSN/key on a `value:` line is a hard fail. These patterns catch the common shapes.
fail=0

# 1. A DSN with an inline password: postgres://user:password@host
if grep -Eni 'value:.*postgres(ql)?://[^:@[:space:]]+:[^@[:space:]]+@' "${RENDER}"; then
  echo "[FAIL] a Postgres DSN with an inline password was rendered as a literal" >&2
  fail=1
fi

# 2. A known credential env var carrying a literal `value:` instead of valueFrom.
for key in TG_RUNTIME_DSN TG_MIGRATION_DSN LITELLM_MASTER_KEY PG_SUPERUSER_PASSWORD \
           TG_RUNTIME_PASSWORD TG_MIGRATION_PASSWORD TEMPORAL_PG_PASSWORD POSTGRES_PASSWORD \
           POSTGRES_PWD ZAI_API_KEY DEEPSEEK_API_KEY MISTRAL_API_KEY ANTHROPIC_API_KEY \
           OPENAI_API_KEY XAI_API_KEY; do
  # match:  - name: KEY \n <ws> value: something   (a literal value on the credential env)
  if grep -A1 -E "name:[[:space:]]*${key}([[:space:]]|$)" "${RENDER}" \
       | grep -Eq '^\s*value:[[:space:]]*\S'; then
    echo "[FAIL] credential ${key} was rendered with a literal 'value:' (must be secretKeyRef)" >&2
    fail=1
  fi
done

if [[ "${fail}" -ne 0 ]]; then
  echo "==> literal-credential scan FAILED" >&2
  exit 1
fi

echo "==> OK: chart lints clean and no literal credential was rendered"
