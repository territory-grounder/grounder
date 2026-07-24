#!/usr/bin/env bash
# Forbidden-pattern gate (P0-8): make the predecessor's injection/leak class uncompilable by policy.
# Fails the build on: shell exec (sh -c), string-built SQL, a migration missing its down file, or a
# contiguous PEM private-key marker in source (which would trip the public-mirror leak guard).
# [O] INV-02 (no shell), INV-03 (parameterized SQL only), P0-8, P3-2.
set -uo pipefail
cd "$(dirname "$0")/.."
fail=0

# Ignore comments and test files for the shell/SQL code checks.
code() { grep -rnE "$1" --include='*.go' core adapters cmd temporal 2>/dev/null | grep -vE '_test\.go|:[0-9]+:[[:space:]]*//'; }

echo "== 1/6 no shell exec (actuation is a fixed argv vector) =="
if code 'exec\.Command(Context)?\([^)]*("(/bin/)?(ba)?sh"|"-c")'; then
  echo "  FORBIDDEN: shell exec — use exec.Command(bin, args...) with a fixed vector (INV-02)"; fail=1
else echo "  ok"; fi

echo "== 2/6 no string-built SQL (parameterized/sqlc only) =="
if code '(fmt\.Sprintf\("[^"]*(SELECT|INSERT|UPDATE|DELETE|FROM)|"[[:space:]]*\+[[:space:]]*[a-zA-Z].*(SELECT|INSERT|UPDATE|DELETE))'; then
  echo "  FORBIDDEN: SQL assembled from strings — use bound parameters (\$1) only (INV-03)"; fail=1
else echo "  ok"; fi

echo "== 3/6 every up-migration has a down-migration =="
for up in core/db/migrations/*.up.sql; do
  [ -e "$up" ] || continue
  down="${up%.up.sql}.down.sql"
  [ -f "$down" ] || { echo "  MISSING down-migration for $(basename "$up")"; fail=1; }
done
[ "$fail" = 0 ] && echo "  ok"

# A real private key must never reach the public GitHub mirror; the mirror's abort-on-survivor guard
# (github-sync/denylist.txt) refuses to publish on a contiguous PEM marker. Test fixtures that legitimately
# need a PEM shape assemble the marker from split literals ("-----BEGIN OPENSSH "+"PRIVATE KEY-----") so the
# runtime value is unchanged but the contiguous string never appears in source. Catch the class HERE, at
# branch time, instead of at mirror time. Exclude the mirror tooling + this script (they name the pattern).
echo "== 4/6 no contiguous PEM private-key marker in source (mirror-safety) =="
pk=$(grep -rnE 'BEGIN [A-Z ]*PRIVATE KEY' \
      --include='*.go' --include='*.md' --include='*.sh' --include='*.yml' --include='*.yaml' \
      --include='*.json' --include='*.txt' . 2>/dev/null \
      | grep -vE '/vendor/|\.claude/|github-sync/|scripts/lint-forbidden\.sh')
if [ -n "$pk" ]; then
  echo "$pk"
  echo "  FORBIDDEN: a contiguous PEM private-key marker in source — split the literal so a real key can"
  echo "  never reach the public mirror (runtime value unchanged). See core/screen/screen_test.go."; fail=1
else echo "  ok"; fi

# A compose init command embedded as ["sh","-c","…"] parses fine as YAML but can still be a shell syntax
# error (unquoted parens/globs) that only surfaces at deploy time — and because core services depend_on the
# init with service_completed_successfully, a non-zero exit wedges the WHOLE stack in Created (a full outage
# this gate now prevents). Run `sh -n` (parse-only) on each such command. Skip gracefully if python3 is
# absent (tooling, not a policy violation) — CI has it, so the gate still runs there.
echo "== 5/6 compose sh -c init commands parse under sh -n (deploy-safety) =="
if command -v python3 >/dev/null 2>&1; then
  if ! python3 scripts/check-compose-shellsafe.py deploy/docker-compose.yml; then fail=1; fi
else
  echo "  skipped (python3 not available)"
fi

# STONITH: no environment-specific literal (estate hostname dc1*/dc2*, realm SEC.NUCLEARLIGHTERS.NET,
# base DN dc=sec,dc=example,dc=net, or the estate domain sec.example.net) may be baked into a
# SHIPPED artifact — compiled Go (the deployed worker/grounder binaries) or the compose defaults. A fresh
# install must carry NO trace of this estate; site specifics belong in a deploy-time override, never the image
# (the same rule that split the attribution ruleset into a generic embedded default + a mounted override).
# Scope: shipped Go (core adapters cmd temporal modules, excluding _test.go + comment-only lines) + the
# compose file. Excluded here (separate, tracked concerns): tools/ (dev tools, not the runtime image),
# deploy/knowledge/*.seed.json + deploy/console/** + frontend/** (estate DATA needing seed-externalization),
# and *.example.* + docs. The go module path gitlab.example.net and registry-gitlab.example.net
# are the repo/registry IDENTITY, not estate config, and do not match the patterns below. Pre-existing
# violations are grandfathered in scripts/stonith-baseline.txt (a shrink-only ratchet); this check fails on
# any NEW estate literal. [O] owner STONITH directive; [F] prose-loadable-not-hardcoded extended to config.
echo "== 6/6 no environment-specific literals in shipped artifacts (STONITH) =="
stonith_pat='(dc1|dc2)[a-z0-9]|[Ss][Ee][Cc]\.[Nn][Uu][Cc][Ll][Ee][Aa][Rr][Ll][Ii][Gg][Hh][Tt][Ee][Rr][Ss]\.[Nn][Ee][Tt]|dc=sec,dc=example,dc=net'
# Gather candidate hits in the shipped set, dropping comment-only lines.
stonith_hits=$( { grep -rnE "$stonith_pat" --include='*.go' core adapters cmd temporal modules 2>/dev/null \
                    | grep -vE '_test\.go|:[0-9]+:[[:space:]]*//' ;
                  grep -nE "$stonith_pat" deploy/docker-compose.yml 2>/dev/null \
                    | sed 's#^#deploy/docker-compose.yml:#' | grep -vE ':[0-9]+:[[:space:]]*#' ; } )
# Suppress the grandfathered baseline: a hit is allowed iff (file, literal-substring) is a baseline row.
stonith_remaining=""
if [ -n "$stonith_hits" ]; then
  while IFS= read -r hit; do
    [ -z "$hit" ] && continue
    hfile="${hit%%:*}"
    # Drop a match that lives only in a TRAILING comment: strip from the first " //" (a comment is
    # conventionally " // text"; a URL value like "ldaps://…" has no space before "//", so its match
    # survives). If the pattern no longer matches the code portion, it was a comment example — skip it.
    hcontent="${hit#*:*:}"
    hcode="${hcontent%% //*}"
    printf '%s' "$hcode" | grep -qE "$stonith_pat" || continue
    allowed=0
    while IFS=$'\t' read -r bfile blit; do
      case "$bfile" in ''|'#'*) continue ;; esac
      if [ "$hfile" = "$bfile" ] && printf '%s' "$hit" | grep -qF "$blit"; then allowed=1; break; fi
    done < scripts/stonith-baseline.txt
    [ "$allowed" = 0 ] && stonith_remaining="${stonith_remaining}${hit}"$'\n'
  done <<< "$stonith_hits"
fi
if [ -n "${stonith_remaining//[$'\n']/}" ]; then
  echo "  FORBIDDEN: environment-specific literal(s) in a shipped artifact — move site config to a deploy-time"
  echo "  override (never the image). If genuinely unavoidable + already tracked, add to scripts/stonith-baseline.txt:"
  printf '%s' "$stonith_remaining" | sed 's/^/    /'
  fail=1
else
  echo "  ok (no new estate literals; $(grep -cvE '^#|^$' scripts/stonith-baseline.txt) grandfathered in baseline)"
fi

[ "$fail" = 0 ] && echo "forbidden-pattern gate: PASS" || echo "forbidden-pattern gate: FAIL"
exit $fail
