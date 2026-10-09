#!/usr/bin/env bash
# lint-ci-shell_test.sh — the CI-script shell-syntax gate's own drill.
#
# The gate hands every `script:` block in .gitlab-ci.yml to `bash -n`, because a block that cannot parse
# fails its job at line 1 on the runner — and on this project a failed pipeline emails the owner. It was the
# last gate in `make all` without an oracle proving it can still FAIL (TG-283: a gate can be broken while
# every pipeline stays green).
#
# Hermetic and seam-free: the gate already takes the CI file as its first argument, so each case is a small
# fixture YAML in $TMPDIR. The clean case is the anti-vacuity floor — without it, every refusal below could
# be "detecting" a gate that simply always fails.
set -uo pipefail
cd "$(dirname "$0")/.."
G="$PWD/scripts/lint-ci-shell.sh"

fails=0
tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT

check() { # check <name> <want-rc> <file>
  local name="$1" want="$2" file="$3" rc
  bash "$G" "$file" >/dev/null 2>&1
  rc=$?
  if [ "$rc" = "$want" ]; then
    echo "  ok: $name (rc=$rc)"
  else
    echo "  FAIL: $name — want rc=$want got rc=$rc" >&2
    fails=$((fails + 1))
  fi
}

echo "== CI shell-syntax gate drill =="

# A pipeline whose blocks all parse must PASS — the floor that makes every refusal below meaningful.
printf 'stages: [test]\njob:\n  script:\n    - echo hello\n    - |\n      for f in a b; do\n        echo "$f"\n      done\n' > "$tmp/good.yml"
check "a pipeline whose script blocks parse PASSES (anti-vacuity floor)" 0 "$tmp/good.yml"

# THE DEFECT THE GATE EXISTS FOR: a block that bash cannot parse. deploy-sidecar shipped a `case` split
# across backslash continuations and bash rejected the whole script before running a command (pipeline
# 45727) — GitLab's own CI lint passed it, because `valid: true` is a statement about the YAML.
printf 'stages: [test]\njob:\n  script:\n    - |\n      case $x in\n        a) echo 1;;\n' > "$tmp/unparseable.yml"
check "a block bash cannot parse is REFUSED" 1 "$tmp/unparseable.yml"

# An unbalanced quote is the other everyday shape.
printf 'stages: [test]\njob:\n  script:\n    - echo "unterminated\n' > "$tmp/quote.yml"
check "an unbalanced quote is REFUSED" 1 "$tmp/quote.yml"

# VACUITY FLOOR, the gate's own: a file with no script blocks means the MATCHER is broken, not that the
# pipeline is clean. It must refuse rather than report a green scan of nothing.
printf 'stages: [test]\nvariables:\n  FOO: bar\n' > "$tmp/noscripts.yml"
check "zero script blocks is a BROKEN MATCHER, not a pass" 1 "$tmp/noscripts.yml"

# A missing file is a named failure, never a silent pass.
check "a missing CI file fails loudly" 1 "$tmp/absent.yml"

if [ "$fails" -gt 0 ]; then
  echo "CI shell-syntax gate drill: FAIL ($fails)" >&2
  exit 1
fi
echo "CI shell-syntax gate drill: PASS"
