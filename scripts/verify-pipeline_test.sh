#!/usr/bin/env bash
# DRILL for scripts/verify-pipeline.sh (TG-434). Same convention as deployed-sha-witness_test.sh: every
# verdict arm proven against a deterministic PATH-shimmed `glab` — no GitLab, no network; the TG-594 arms read
# this repository's own HEAD (any clone, shallow included) and never fetch (VERIFY_FETCH=0).
#
# WHY THIS DRILL EXISTS: the original script grepped the sha out of `glab ci list` TEXT; a glab upgrade
# dropped the sha column and every invocation quietly became NO VERDICT — the anti-hit-and-run instrument
# was blind for days and nothing said so. These arms pin the API contract instead, and the empty-answer
# arm (TG-365) pins that "the API returned []" reads as NO PIPELINE (exit 4), never as a verdict.
#
# KILLING MUTATION (executed at authoring): make the script treat an empty pipelines answer as success —
# the empty-answer arm below goes RED (want rc=4, got rc=0). Restore ⇒ green.
set -uo pipefail
cd "$(dirname "$0")/.."
V=scripts/verify-pipeline.sh
fail=0
ran=0

shimdir="$(mktemp -d)"
trap 'rm -rf "$shimdir"' EXIT

# The shim speaks just enough `glab api`: FAKE_PIPELINES is the pipelines?sha= answer; FAKE_JOBS the
# pipelines/N/jobs answer. Everything else answers [].
cat >"$shimdir/glab" <<'SHIM'
#!/usr/bin/env bash
# Every query is recorded so a drill can assert WHICH sha the script asked the API about (TG-594).
[ -n "${GLAB_QUERY_LOG:-}" ] && printf '%s\n' "${2:-}" >>"$GLAB_QUERY_LOG"
case "${2:-}" in
  *"/pipelines?sha="*) printf '%s' "${FAKE_PIPELINES:-[]}" ;;
  *"/jobs"*)           printf '%s' "${FAKE_JOBS:-[]}" ;;
  *)                   printf '[]' ;;
esac
SHIM
chmod +x "$shimdir/glab"
export PATH="$shimdir:$PATH"

SHA_A="aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa" # full length: the script must not need git for it

check() { # name want-rc [expected-substring ...]
  local name="$1" want="$2"; shift 2
  local out rc m
  out="$(TIMEOUT_S=3 VERIFY_POLL_S=1 bash "$V" "$SHA_A" 2>&1)"; rc=$?
  ran=$((ran + 1))
  if [ "$rc" -ne "$want" ]; then
    echo "  FAIL: $name — want rc=$want got rc=$rc"
    printf '%s\n' "$out" | sed 's/^/      /'
    fail=1
    return
  fi
  for m in "$@"; do
    if ! printf '%s' "$out" | grep -qF -- "$m"; then
      echo "  FAIL: $name — output lacks: $m"
      printf '%s\n' "$out" | sed 's/^/      /'
      fail=1
      return
    fi
  done
  echo "  ok: $name (rc=$rc)"
}

# Every arm runs with TIMEOUT_S=3 VERIFY_POLL_S=1: the loop genuinely polls the shim (2-3 reads for
# non-terminal answers; first read breaks for terminal ones) and the whole drill stays under ~10s.

check_live() { # name want-rc pipelines-json [expected-substring ...]
  local name="$1" want="$2" pipes="$3"; shift 3
  local out rc m
  out="$(FAKE_PIPELINES="$pipes" TIMEOUT_S=3 VERIFY_POLL_S=1 bash "$V" "$SHA_A" 2>&1)"; rc=$?
  ran=$((ran + 1))
  if [ "$rc" -ne "$want" ]; then
    echo "  FAIL: $name — want rc=$want got rc=$rc"
    printf '%s\n' "$out" | sed 's/^/      /'
    fail=1
    return
  fi
  for m in "$@"; do
    if ! printf '%s' "$out" | grep -qF -- "$m"; then
      echo "  FAIL: $name — output lacks: $m"
      printf '%s\n' "$out" | sed 's/^/      /'
      fail=1
      return
    fi
  done
  echo "  ok: $name (rc=$rc)"
}

echo "== verify-pipeline drill =="

# 1. Green pipeline → PASS, names the pipeline id.
check_live "green pipeline is a PASS" 0 '[{"id": 4242, "status": "success"}]' "PASS" "4242"

# 2. Failed pipeline → rc 1, names the failing job from the jobs API.
FAKE_JOBS='[{"name":"image-worker","status":"failed"},{"name":"build-test","status":"success"}]' \
  FAKE_PIPELINES='[{"id": 4243, "status": "failed"}]' TIMEOUT_S=3 VERIFY_POLL_S=1 bash "$V" "$SHA_A" >"$shimdir/out2" 2>&1
rc=$?; ran=$((ran + 1))
if [ "$rc" -ne 1 ] || ! grep -qF "image-worker: failed" "$shimdir/out2"; then
  echo "  FAIL: red pipeline must exit 1 and name the failing job"; cat "$shimdir/out2" | sed 's/^/      /'; fail=1
else
  echo "  ok: red pipeline names its failing job (rc=1)"
fi

# 3. THE TG-365 ARM — an empty pipelines answer is NO PIPELINE (rc 4), never a verdict and never
#    "still running". This is the exact blindness TG-434 shipped with: absent must have its own state.
check "empty API answer is NO PIPELINE, its own state" 4 "NO PIPELINE" "NOT"

# 4. Still-running at deadline → rc 3, distinct from absent.
check_live "running-at-deadline is NO VERDICT (rc 3)" 3 '[{"id": 4244, "status": "running"}]' "NO VERDICT" "still running"

# 5. Canceled → red-class rc 1 (a canceled deploy did not deploy).
check_live "canceled pipeline is red-class" 1 '[{"id": 4245, "status": "canceled"}]' "canceled"

if [ "$ran" -lt 5 ]; then
  echo "verify-pipeline drill: VACUOUS — only $ran arm(s) executed"
  exit 3
fi
# ---- TG-594: an abbreviated sha is expanded to the FULL sha before the API is asked; one that cannot be
# resolved is REFUSED up front (exit 2) instead of polling for the whole timeout and ending in a false
# "NO PIPELINE exists" (observed twice 2026-10-23 on merge commits with green pipelines). VERIFY_FETCH=0
# keeps the drill off the network.
qlog="$shimdir/queries"; : >"$qlog"
headfull="$(git rev-parse HEAD)"; headshort="${headfull:0:8}"
out="$(GLAB_QUERY_LOG="$qlog" FAKE_PIPELINES="[{\"id\":7,\"status\":\"success\"}]" VERIFY_FETCH=0 TIMEOUT_S=3 VERIFY_POLL_S=1 bash "$V" "$headshort" 2>&1)"; rc=$?
ran=$((ran + 1))
if [ "$rc" -ne 0 ] || ! grep -qF -- "pipelines?sha=$headfull&" "$qlog"; then
  echo "  FAIL: a resolvable short sha must be expanded to the full sha before the API is asked (rc=$rc)"
  printf '%s\n' "$out" | sed 's/^/      /'; cat "$qlog" | sed 's/^/      q: /'
  fail=1
else
  echo "  ok: short sha $headshort expanded to the full sha for the API (rc=0)"
fi
: >"$qlog"
out="$(GLAB_QUERY_LOG="$qlog" VERIFY_FETCH=0 TIMEOUT_S=3 VERIFY_POLL_S=1 bash "$V" "0badc0de0badc0de" 2>&1)"; rc=$?
ran=$((ran + 1))
if [ "$rc" -ne 2 ] || ! printf '%s' "$out" | grep -qF "cannot resolve '0badc0de0badc0de'" || [ -s "$qlog" ]; then
  echo "  FAIL: an unresolvable short sha must be refused (rc=2) before any API query, got rc=$rc, queries=$(wc -l <"$qlog")"
  printf '%s\n' "$out" | sed 's/^/      /'
  fail=1
else
  echo "  ok: unresolvable short sha refused up front, no API query (rc=2)"
fi

if [ "$fail" -ne 0 ]; then
  echo "verify-pipeline drill: FAIL"
  exit 1
fi
echo "verify-pipeline drill: PASS ($ran arms)"
