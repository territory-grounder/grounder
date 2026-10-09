#!/usr/bin/env bash
# CI-SCRIPT SHELL SYNTAX GATE.
#
# Every `script:` block in .gitlab-ci.yml is shell that only ever gets parsed ON THE RUNNER. A block that
# cannot parse fails the whole job at line 1, after the runner has pulled an image and cloned the repo —
# and on this project a failed pipeline emails the owner. That is not a hypothetical: deploy-sidecar shipped
# with a `case` statement split across backslash line continuations, and bash rejected the entire script
# before executing a single command ("syntax error near unexpected token `('", pipeline 45727). GitLab's own
# CI lint had already passed it, because `valid: true` means the YAML is a well-formed pipeline — it says
# nothing about whether the shell inside it parses.
#
# This gate closes that gap the only way that actually works: hand each block to `bash -n`.
#
# WHAT IT DOES NOT DO, stated so nobody mistakes its silence for proof: `bash -n` checks SYNTAX only. It
# cannot know whether a variable is set, a binary exists on the runner, or an API call returns what the
# script expects. A green run here means every job can START, not that any job is correct.
set -uo pipefail
cd "$(dirname "$0")/.."

CI_FILE="${1:-.gitlab-ci.yml}"
[ -f "$CI_FILE" ] || { echo "lint-ci-shell: $CI_FILE not found"; exit 1; }

echo "== CI shell-syntax gate ($CI_FILE) =="

python3 - "$CI_FILE" <<'PY'
import sys, subprocess, tempfile, os
try:
    import yaml
except ImportError:
    print("  SKIPPED — PyYAML unavailable; install it or this gate proves nothing.")
    # Fail rather than pass silently: a gate that cannot run must not report success.
    sys.exit(1)

path = sys.argv[1]
doc = yaml.safe_load(open(path))
if not isinstance(doc, dict):
    print("  FORBIDDEN: pipeline did not parse as a mapping."); sys.exit(1)

checked = failed = 0
for name, job in doc.items():
    if not isinstance(job, dict):
        continue
    # before_script / script / after_script are all shell the runner executes.
    for key in ("before_script", "script", "after_script"):
        blocks = job.get(key)
        if not isinstance(blocks, list):
            continue
        for i, block in enumerate(blocks):
            if not isinstance(block, str):
                continue
            checked += 1
            with tempfile.NamedTemporaryFile("w", suffix=".sh", delete=False) as f:
                f.write(block); tmp = f.name
            r = subprocess.run(["bash", "-n", tmp], capture_output=True, text=True)
            os.unlink(tmp)
            if r.returncode:
                failed += 1
                first = (r.stderr.strip().splitlines() or ["(no message)"])[0]
                print(f"  FORBIDDEN: {name} {key}[{i}] is not valid shell:")
                print(f"    {first[:200]}")
                print(f"    This job would fail on the runner at line 1, after pulling an image and cloning.")

# VACUITY FLOOR. If the walk finds nothing, the matcher is broken and every future syntax error rides
# through behind a green tick — the exact shape this repo keeps finding.
if checked == 0:
    print("  FORBIDDEN: zero script blocks found — the matcher is broken, not the pipeline clean.")
    sys.exit(1)

print(f"  {checked} script block(s) checked, {failed} unparseable")
sys.exit(1 if failed else 0)
PY
rc=$?
if [ "$rc" -ne 0 ]; then
  echo "CI shell-syntax gate: FAIL"
  exit 1
fi
echo "CI shell-syntax gate: PASS"
