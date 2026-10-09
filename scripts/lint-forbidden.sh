#!/usr/bin/env bash
# Forbidden-pattern gate (P0-8): make the predecessor's injection/leak class uncompilable by policy.
# Fails the build on: shell exec (sh -c), string-built SQL, a migration missing its down file, or a
# contiguous PEM private-key marker in source (which would trip the public-mirror leak guard).
# [O] INV-02 (no shell), INV-03 (parameterized SQL only), P0-8, P3-2.
set -uo pipefail
cd "$(dirname "$0")/.."
fail=0

# Ignore comments and test files for the shell/SQL code checks.
# tools/ IS in scope (spec/025 REQ-2507): the harness runs real commands against production hosts and builds
# judge prompts, so a shell-built command or an embedded credential is exactly as dangerous there as in the
# runtime — and until this was widened, tools/ was excluded and the proof plane was linted by nothing.
code() { grep -rnE "$1" --include='*.go' core adapters cmd temporal tools 2>/dev/null | grep -vE '_test\.go|:[0-9]+:[[:space:]]*//'; }

echo "== 1/7 no shell exec (actuation is a fixed argv vector) =="
if code 'exec\.Command(Context)?\([^)]*("(/bin/)?(ba)?sh"|"-c")'; then
  echo "  FORBIDDEN: shell exec — use exec.Command(bin, args...) with a fixed vector (INV-02)"; fail=1
else echo "  ok"; fi

echo "== 2/7 no string-built SQL (parameterized/sqlc only) =="
if code '(fmt\.Sprintf\("[^"]*(SELECT|INSERT|UPDATE|DELETE|FROM)|"[[:space:]]*\+[[:space:]]*[a-zA-Z].*(SELECT|INSERT|UPDATE|DELETE))'; then
  echo "  FORBIDDEN: SQL assembled from strings — use bound parameters (\$1) only (INV-03)"; fail=1
else echo "  ok"; fi

echo "== 3/7 every up-migration has a down-migration =="
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
echo "== 4/7 no contiguous PEM private-key marker in source (mirror-safety) =="
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
echo "== 5/7 compose sh -c init commands parse under sh -n (deploy-safety) =="
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
# compose file, PLUS tools/ (the benchmark harness — spec/025 REQ-2507: it executes against production and
# builds prompts, so the same bans apply). Excluded here (separate, tracked concerns):
# deploy/knowledge/*.seed.json + deploy/console/** + frontend/** (estate DATA needing seed-externalization),
# and *.example.* + docs. The go module path gitlab.example.net and registry-gitlab.example.net
# are the repo/registry IDENTITY, not estate config, and do not match the patterns below. Pre-existing
# violations are grandfathered in scripts/stonith-baseline.txt (a shrink-only ratchet); this check fails on
# any NEW estate literal. [O] owner STONITH directive; [F] prose-loadable-not-hardcoded extended to config.
echo "== 6/7 no environment-specific literals in shipped artifacts (STONITH) =="
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

echo "== 7/7 no provider-credential literal in source (mirror-safety) =="
# WHY THIS EXISTS. The public GitHub mirror stopped syncing for TWO DAYS and nothing here noticed. The
# blocker was cmd/worker/boot_config_test.go carrying a SYNTHETIC Twilio account SID as a one-piece
# literal. There is no secret: the body is a repeated hex sequence and no account exists behind it. But
# GitHub push protection matches the SHAPE in file TEXT, refuses the push, and does not care.
#
# Our own scanners were not wrong either — the sync's 10-pass gitleaks came back clean and the
# abort-on-survivor denylist found nothing, because that denylist guards OUR estate identifiers, not
# other providers' credential shapes. The two scanners disagreed, and the only thing that surfaced it
# was the mirror failing days later on a job nobody was watching.
#
# This runs on every merge request, so a blocking literal is caught BEFORE it reaches main. THE FIX IS
# NEVER TO DELETE THE FIXTURE: assemble it at runtime so the value stays byte-identical and the
# validator is still exercised, while no matching string exists in the source.
#
# A SECOND SHAPE, FOR THE OPPOSITE REASON (added 2026-08-05). deploy/claude-proxy/src/oauth_rotate.rs
# carried a full-length `sk-ant-oat01-` OAuth token — its own comment called it the operator's real
# paste — and GitHub push protection did NOT refuse it. It published. That is the worse failure: the
# Twilio shape at least announced itself by breaking the sync, and this one announced nothing.
#
# It is caught now at publish time (the mirror's denylist gained the shape, and its scan was widened
# from nine extensions to every tracked file — it had never opened a .rs). But a publish-time catch
# ABORTS THE MIRROR, which is the same two-day silent-desync outage in a different costume. Catching
# it here, on the merge request, is what keeps the mirror running.
#
# NOTE THE INCLUDE LIST BELOW. It gained .rs .sql .py .toml .conf for the same reason: this gate could
# not see the sidecar crate either.
#
# SCOPED TO TWO SHAPES ON PURPOSE. For Twilio the evidence is the failed push. That push also
# carried a GitLab-PAT-shaped fixture (core/lessons/write_screen_test.go) and an AWS-key-shaped one (temporal/runner/diagnosis_scrub_test.go) —
# both synthetic screen-test fixtures — and GitHub blocked NEITHER: it listed exactly one violation.
# The AWS one is that provider's own documented example value and is allowlisted upstream; the GitLab
# body is not a valid PAT. Widening this gate to every provider prefix would fail merges on literals GitHub
# demonstrably accepts, and a gate that cries wolf is a gate someone deletes. If a NEW shape is ever
# observed blocking a real push, add it here WITH the failing job as the evidence.
#
# Those two fixtures are DESCRIBED rather than quoted, deliberately: writing them out here would put the
# very shapes this gate exists to keep out of the tree INTO the tree, and gitleaks flagged exactly that
# on the first attempt at this comment.
#
# Matching is done by grepping BARE PREFIXES and validating the shape in awk, deliberately. A regex with
# bounded quantifiers over a character class ({32}, {36,}) compiles to a state machine that has ballooned
# past 15 GB RSS on this box and wedged the whole machine — a lint that can take the host down is worse
# than the defect it looks for.
pp_hits=$(grep -rIn -e '"AC' -e '"SK' -e '"sk-ant-' \
  --include='*.go' --include='*.json' --include='*.yaml' --include='*.yml' --include='*.md' --include='*.txt' \
  --include='*.rs' --include='*.sql' --include='*.py' --include='*.toml' --include='*.conf' \
  . 2>/dev/null | grep -v '/vendor/' | grep -v '\.claude/worktrees/' \
  | awk '
      # Pull each double-quoted run out of the line and test it against the provider shapes.
      {
        line = $0
        while (match(line, /"[A-Za-z0-9_-]*"/)) {
          tok = substr(line, RSTART + 1, RLENGTH - 2)
          line = substr(line, RSTART + RLENGTH)
          n = length(tok)
          bad = 0
          if ((substr(tok,1,2) == "AC" || substr(tok,1,2) == "SK") && n == 34 && tok ~ /^..[0-9a-fA-F]*$/) bad = 1
          # Anthropic keys/tokens: `sk-ant-<kind><ver>-<long body>`. Length is the discriminator — the
          # bare 13-char prefix is a legitimate constant (the sidecar compares against it), a 40+ char
          # run after it is a credential. No bounded quantifier: awk counts, the regex does not.
          if (substr(tok,1,7) == "sk-ant-" && n >= 40) bad = 1
          if (bad) { print; next }
        }
      }' || true)
if [ -n "$pp_hits" ]; then
  echo "  FORBIDDEN: provider-credential literal(s) in source. Either GitHub push protection REFUSES THE"
  echo "  PUSH and the mirror desyncs silently (Twilio), or GitHub accepts it and the credential goes"
  echo "  PUBLIC until our own denylist aborts the next sync (Anthropic). Both stop the mirror."
  echo "  If this is a synthetic fixture (it usually is), assemble it at"
  echo "  runtime so the value is unchanged and no matching literal remains in the source, e.g."
  echo '    "AC" + strings.Repeat("0123456789abcdef", 2)        // Go'
  echo '    format!("sk-ant-api03-{}", "A".repeat(51))            // Rust'
  printf '%s\n' "$pp_hits" | sed 's/^/    /'
  fail=1
else
  echo "  ok (no source literal carries a provider credential shape)"
fi

[ "$fail" = 0 ] && echo "forbidden-pattern gate: PASS" || echo "forbidden-pattern gate: FAIL"
exit $fail
