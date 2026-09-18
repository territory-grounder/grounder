#!/usr/bin/env bash
# SELF-TEST for eval/lib-librenms-token.sh (TG-362).
#
# THIS DRILL IS THE POINT OF THE TICKET. The resolver it exercises was broken in two different ways for
# weeks, and both times the breakage was invisible because nothing ever ran its FAILURE paths:
#
#   1. It read `LIBRENMS_TOKEN` from the box .env after the secret-policy migration deleted that variable.
#      `|| true` swallowed the empty result and the negative-control bar silently applied blind.
#   2. The first repair put `bao` inside the ssh, where it is not installed. It returned
#      "bao: command not found", and the verification — a length check — PASSED on the error string.
#
# So the arms below are the ones nobody drilled: raw-absent-fallback-works, dereference-error-is-not-a-token,
# and auth-rejection-is-absence. `ssh`, `bao` and `curl` are shadowed on PATH, so this needs no estate, no
# OpenBao and no network, and it runs in CI.
set -euo pipefail

LIB="$(cd "$(dirname "$0")" && pwd)/lib-librenms-token.sh"
tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT
mkdir -p "$tmp/bin"
export PATH="$tmp/bin:$PATH"
export HOME="$tmp"                      # so the resolver's ~/.vault-token read is ours
echo "fake-vault-token" > "$tmp/.vault-token"

# The fakes read their scripted behaviour from files, so each arm rewrites the scenario, not the fakes.
cat > "$tmp/bin/ssh" <<'EOF'
#!/usr/bin/env bash
# The resolver's two ssh reads are distinguished by the remote command it sends.
case "$*" in
  *TG_LIBRENMS_DEPLOYMENTS*) cat "$TMPDIR_FAKE/deployments" 2>/dev/null ;;
  *LIBRENMS_TOKEN*)          cat "$TMPDIR_FAKE/rawtoken"    2>/dev/null ;;
esac
exit 0
EOF
cat > "$tmp/bin/bao" <<'EOF'
#!/usr/bin/env bash
[ -s "$TMPDIR_FAKE/baofail" ] && { echo "bao: permission denied" >&2; exit 1; }
cat "$TMPDIR_FAKE/baotoken" 2>/dev/null
EOF
cat > "$tmp/bin/curl" <<'EOF'
#!/usr/bin/env bash
cat "$TMPDIR_FAKE/probe" 2>/dev/null
EOF
chmod +x "$tmp/bin/ssh" "$tmp/bin/bao" "$tmp/bin/curl"
export TMPDIR_FAKE="$tmp"

# shellcheck source=eval/lib-librenms-token.sh
. "$LIB"

fails=0
scenario() {  # scenario <rawtoken> <deployments> <baotoken> <probe> [baofail]
  : > "$tmp/baofail"
  printf %s "$1" > "$tmp/rawtoken"
  printf %s "$2" > "$tmp/deployments"
  printf %s "$3" > "$tmp/baotoken"
  printf %s "$4" > "$tmp/probe"
  [ "${5:-}" = "baofail" ] && echo 1 > "$tmp/baofail"
  return 0
}
check() {  # check <name> <want> <got>
  if [ "$2" = "$3" ]; then
    echo "  ok   $1"
  else
    echo "  FAIL $1: want '$2', got '$3'"; fails=1
  fi
}
OK='{"status":"ok","devices":[]}'

echo "== 1/6 the raw variable still works where it still exists =="
scenario "raw-token-value" "" "" "$OK"
check "raw LIBRENMS_TOKEN is used as-is" "raw-token-value" "$(tg_resolve_librenms_token k b 2>/dev/null)"

echo "== 2/6 raw ABSENT falls back to the deployments ref (the migration case) =="
scenario "" "bao:secret/data/tg/librenms-nl-triage#token" "ref-token-value" "$OK"
check "dereferenced token returned" "ref-token-value" "$(tg_resolve_librenms_token k b 2>/dev/null)"

echo "== 3/6 a FAILED dereference is absence, not a token =="
# The exact defect: `bao` fails, its stderr is swallowed, and a length check passes on the error string.
scenario "" "bao:secret/data/tg/librenms-nl-triage#token" "" "$OK" baofail
got="$(tg_resolve_librenms_token k b 2>/dev/null)"
check "no token when bao fails" "" "$got"

echo "== 4/6 an AUTH REJECTION is absence, not a token =="
# The other defect: the wrong ref resolves to a real string that LibreNMS refuses. Obtaining is not validating.
scenario "" "bao:secret/data/tg/librenms-nl-triage#token" "wrong-scope-token" '{"status":"error","message":"Unauthenticated."}'
check "auth failure yields no token" "" "$(tg_resolve_librenms_token k b 2>/dev/null)"

echo "== 5/6 an EMPTY probe response is absence too =="
scenario "" "bao:secret/data/tg/librenms-nl-triage#token" "some-token" ""
check "empty probe yields no token" "" "$(tg_resolve_librenms_token k b 2>/dev/null)"

echo "== 6/6 every absent outcome WARNS on stderr (silence is the defect) =="
for arm in baofail unauth; do
  case "$arm" in
    baofail) scenario "" "bao:x#t" "" "$OK" baofail ;;
    unauth)  scenario "" "bao:x#t" "tok" '{"message":"Unauthenticated."}' ;;
  esac
  err="$(tg_resolve_librenms_token k b 2>&1 >/dev/null)"
  case "$err" in
    *"no usable LibreNMS token"*) echo "  ok   $arm warns" ;;
    *) echo "  FAIL $arm produced NO warning — a check that no-ops silently is what this ticket is about"
       fails=1 ;;
  esac
done

# ANTI-VACUITY. If the fakes are wired wrong, every arm above returns "" and arms 3-6 pass for the wrong
# reason. Arms 1 and 2 are the positive controls, and this restates them as a hard precondition.
echo "== anti-vacuity: the resolver must be able to SUCCEED under these fakes =="
scenario "" "bao:secret/data/tg/librenms-nl-triage#token" "positive-control" "$OK"
if [ "$(tg_resolve_librenms_token k b 2>/dev/null)" != "positive-control" ]; then
  echo "  FAIL the harness cannot produce a token at all — the negative arms prove nothing"; fails=1
else
  echo "  ok   a good path still yields a token"
fi

[ "$fails" = 0 ] && echo "lib-librenms-token: PASS" || echo "lib-librenms-token: FAIL"
exit "$fails"
