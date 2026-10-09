#!/usr/bin/env bash
# tier-ab.sh — the ON-BOX half of TG-204's three-arm model-tier A/B: does the expensive reasoning tier
# actually buy diagnosis quality over the fast tier?
#
# It runs the SAME corpus through three arms in the SAME window (so model/estate drift cancels), harvests the
# tg-claude-proxy's served-model telemetry for that window, and hands both to the deterministic comparator
# (tools/tierab), which EXITS NON-ZERO unless the arms provably ran different models.
#
#   ARM-CONTROL : investigate=fast     decide=primary   (production's current routing)
#   ARM-STRONG  : investigate=primary  decide=primary   (the reasoner throughout)
#   ARM-CHEAP   : investigate=fast     decide=fast      (the fast tier throughout)
#
# ★ READ THIS BEFORE RUNNING IT WITH THE DEFAULT TIERS. As of 2026-08-04 the DEPLOYED litellm config on
# dc1tg01 resolves `fast`, `primary` AND `opus-cc` all to `openai/opus-cc` — one upstream, one brain
# (claude-opus-5). With the defaults below, all three arms are ONE ARM MEASURED THREE TIMES, and the run will
# correctly terminate with TIER-AB: COLLAPSED and exit 1. That is not a harness failure; it is the finding.
# The 53-second reasoning tier TG-204 was written to interrogate no longer exists: the 2026-07-31 single-brain
# decision pointed every agent-facing alias at the same Claude Opus 5 proxy, so there is no tier gap to buy.
#
# To run an experiment whose arms CAN differ, override the tiers with aliases that resolve to different
# upstreams — `arm-haiku` and `arm-opus` exist for exactly this and are additive (they do NOT touch the
# production `fast`/`primary` routing):
#
#   TG_TIER_CHEAP=arm-haiku TG_TIER_STRONG=arm-opus TG_TIER_CONTROL_INV=arm-haiku eval/tier-ab.sh
#
# That answers a REAL question (does a cheaper brain lose diagnosis quality on TG's loop) but it is NOT
# TG-204's literal question, because TG does not route production through those aliases. Say which one you
# ran; the archived verdict records the tiers so the distinction survives.
#
# Env (all optional):
#   TG_SSH_KEY (~/.ssh/one_key)  TG_BOX (root@dc1tg01)  TG_EVAL_PORT (4010)  TG_TIERAB_OUT (eval/out/tierab)
#   TG_PROXY_HOST (root@dc1claude01)  TG_PROXY_CONTAINER (tg-claude-proxy)
#   TG_EVAL_LIMIT (corpus subset; default 8)  TG_EVAL_CONCURRENCY (default 4 — lower than the gate's 6
#     because three sequential arms on a subscription-metered proxy is a lot of burst)
#   TG_TIER_CONTROL_INV / TG_TIER_CONTROL_DEC / TG_TIER_STRONG / TG_TIER_CHEAP (the arm tier aliases)
set -euo pipefail

KEY="${TG_SSH_KEY:-$HOME/.ssh/one_key}"
BOX="${TG_BOX:-root@dc1tg01}"
LPORT="${TG_EVAL_PORT:-4010}"
# The proxy host is reached by SSH for `docker logs`, NOT through the model channel — so it is deliberately
# independent of whatever api_base litellm currently points at. TG-287 moved that channel to TLS on a new
# port; this fetch is unaffected, and hard-coding the api_base here would have broken on that merge for no
# reason. Named rather than numeric so it follows DNS if the host moves.
PROXY_HOST="${TG_PROXY_HOST:-root@dc1claude01}"
PROXY_CONTAINER="${TG_PROXY_CONTAINER:-tg-claude-proxy}"
HERE="$(cd "$(dirname "$0")/.." && pwd)"
OUT="${TG_TIERAB_OUT:-$HERE/eval/out/tierab}"
TG_EVAL_LIMIT="${TG_EVAL_LIMIT:-8}"; export TG_EVAL_LIMIT
export TG_EVAL_CONCURRENCY="${TG_EVAL_CONCURRENCY:-4}"

# The arm tier aliases. Defaults are TG-204's LITERAL arms, which collapse today — deliberately, so the
# harness's own default run reproduces the finding instead of quietly measuring something else.
CTRL_INV="${TG_TIER_CONTROL_INV:-fast}"
CTRL_DEC="${TG_TIER_CONTROL_DEC:-primary}"
STRONG="${TG_TIER_STRONG:-primary}"
CHEAP="${TG_TIER_CHEAP:-fast}"

mkdir -p "$OUT"

# ── THE GATEWAY LOCK ────────────────────────────────────────────────────────────────────────────────────
# Same advisory lock, same path, as eval/eval-gate.sh and tools/shadowbench/run-campaign.sh. Three sequential
# arms measured against one box gateway is the longest-running thing in this repo, and a contended arm here
# is worse than a contended gate arm: a 429-degraded arm does not just add noise, it changes an ARM's
# measured latency and spend, which are two of the axes being compared. Blocking beats failing (see the long
# rationale in eval-gate.sh); the timeout is loud and distinct (75 = EX_TEMPFAIL), never a silent skip.
#
# ★ "THE A/B SAID NO" AND "THE A/B NEVER RAN" MUST NOT SHARE AN EXIT CODE, and here they collide by default.
# flock returns 1 on TIMEOUT, and tools/tierab returns 1 on COLLAPSED — which is this harness's single most
# expected verdict (TG-204's literal arms collapse today). eval-gate.sh disambiguates by guessing from the
# code alone; that guess is wrong every single time this script reaches its normal conclusion, and it did:
# a live run on 2026-08-04 printed the full COLLAPSED verdict and then claimed the lock had timed out.
# A SENTINEL removes the guess — the inner run creates it the moment it holds the lock, so its absence is
# proof the command never executed, which is exactly what flock's timeout means and nothing else does.
GATE_LOCK="${TG_EVAL_LOCK:-/tmp/tg-gateway.lock}"
GATE_LOCK_WAIT="${TG_EVAL_LOCK_WAIT:-3600}"
# INHERIT the path, never recompute it: the inner run is a NEW process with a different $$, so a plain
# `...ran.$$` gives parent and child two different paths and the sentinel can never be observed. (It did
# exactly that on the first attempt, and the wrapper went on reporting a lock timeout after a completed run.)
RAN_SENTINEL="${TG_TIERAB_RAN_SENTINEL:-${TMPDIR:-/tmp}/tg-tierab-ran.$$}"
export TG_TIERAB_RAN_SENTINEL="$RAN_SENTINEL"
if [ "${TG_EVAL_LOCKED:-}" != "1" ]; then
  if ! command -v flock >/dev/null 2>&1; then
    echo "tier-ab: flock(1) not available — refusing to measure through a possibly-contended gateway." >&2
    exit 1
  fi
  echo "== taking the gateway lock ($GATE_LOCK, up to ${GATE_LOCK_WAIT}s) =="
  rm -f "$RAN_SENTINEL"
  set +e
  env TG_EVAL_LOCKED=1 flock --wait "$GATE_LOCK_WAIT" "$GATE_LOCK" "$0" "$@"
  rc=$?
  set -e
  if [ -e "$RAN_SENTINEL" ]; then
    rm -f "$RAN_SENTINEL"
    exit "$rc"   # the inner run executed — its verdict is the answer, whatever the code
  fi
  echo "" >&2
  echo "tier-ab: COULD NOT ACQUIRE THE GATEWAY LOCK within ${GATE_LOCK_WAIT}s ($GATE_LOCK)." >&2
  echo "  Something else is measuring through the box gateway (the nightly trend-watch, a shadowbench" >&2
  echo "  campaign, or another gate run). THE A/B DID NOT RUN; this is NOT a verdict about the arms." >&2
  echo "  Wait and re-run, raise TG_EVAL_LOCK_WAIT, or check for a stale holder: fuser -v $GATE_LOCK" >&2
  exit 75  # EX_TEMPFAIL — retryable, deliberately distinct from any verdict this harness can reach
fi
: > "$RAN_SENTINEL"   # we hold the lock: from here on, every exit is a real answer, not a lock timeout

TUN_PID=""; REUSE_TUN=0; SECDIR=""
cleanup() {
  if [ "${REUSE_TUN:-0}" -eq 0 ] && [ -n "${TUN_PID:-}" ]; then kill "$TUN_PID" 2>/dev/null || true; fi
  # The probe config carries the gateway master key; shred it rather than leaving it under eval/out/.
  if [ -n "${PROBE_CFG:-}" ] && [ -f "$PROBE_CFG" ]; then
    shred -u "$PROBE_CFG" 2>/dev/null || rm -f "$PROBE_CFG"
  fi
  if [ -n "${SECDIR:-}" ] && [ -d "$SECDIR" ]; then
    find "$SECDIR" -type f -exec shred -u {} \; 2>/dev/null || true
    rm -rf "$SECDIR" 2>/dev/null || true
  fi
}
trap cleanup EXIT

# Gateway master key — read the way a SHELL expands it (the OpenBao drop is single-quoted; a naive sed keeps
# the quotes and yields a key 2 chars too long that litellm rejects while /models still 200s, masking it).
# Verbatim discipline from eval/eval-gate.sh, which learned this the expensive way.
MK=$(ssh -i "$KEY" -o StrictHostKeyChecking=no "$BOX" '
  cid=$(docker ps -q -f name=litellm | head -1)
  v=$([ -n "$cid" ] && docker exec "$cid" sh -c "tr \"\0\" \"\n\" < /proc/1/environ" 2>/dev/null | sed -n "s/^LITELLM_MASTER_KEY=//p" | head -1)
  drop="${TG_LITELLM_SECRET_DROP:-/dev/shm/tg-litellm-secrets}/env"
  [ -n "$v" ] || v=$([ -f "$drop" ] && sh -c ". \"$drop\" >/dev/null 2>&1; printf %s \"\${LITELLM_MASTER_KEY:-}\"")
  printf %s "$v"')
# ★ THIS READ `^LIBRENMS_TOKEN=` FROM THE BOX .env — a variable the secret-policy migration removed. It has
# exported an EMPTY LibreNMS token into every arm measured since, so those tier comparisons scored an agent
# whose LibreNMS grounding was dead, in the very file that insists on toolset parity twelve lines below.
# eval-gate.sh was repaired for this on 2026-08-07; this copy was not, which is why there is now one copy.
# shellcheck source=eval/lib-librenms-token.sh
. "$HERE/eval/lib-librenms-token.sh"
LT=$(tg_resolve_librenms_token "$KEY" "$BOX")
[ -n "$MK" ] || { echo "could not read LITELLM_MASTER_KEY from $BOX" >&2; exit 1; }

# Toolset PARITY provisioning — identical to eval-gate.sh's. Without it the arms measure an agent production
# does not ship (no hostdiag => it cannot name a failed unit), and the whole tier comparison is about a
# toolless agent. The harness itself fails closed on expected-propose incidents with no deployments.
box_env() { ssh -i "$KEY" -o StrictHostKeyChecking=no "$BOX" "grep '^$1=' /srv/tg/deploy/.env | cut -d= -f2-" 2>/dev/null || true; }
HD_DEP=$(box_env TG_HOSTDIAG_DEPLOYMENTS)
SG_DEP=$(box_env TG_SYSLOGNG_DEPLOYMENTS)
NAT_RULES=$(box_env TG_CREDENTIAL_NATIVE_RULES)
HD_KH=$(box_env TG_HOSTDIAG_KNOWN_HOSTS)
if [ -n "$HD_DEP" ] || [ -n "$SG_DEP" ]; then
  SECDIR=$(mktemp -d); chmod 700 "$SECDIR"
  for ref in $(printf '%s\n%s\n%s\n' "$HD_DEP" "$SG_DEP" "$NAT_RULES" | grep -oE 'file:[^|,; "]+' | sort -u); do
    p=${ref#file:}; loc="$SECDIR/$(basename "$p")"
    if ssh -i "$KEY" -o StrictHostKeyChecking=no "$BOX" "cat '$p' 2>/dev/null || cat '/srv/tg/deploy$p' 2>/dev/null || cat '/srv/tg$p'" > "$loc" 2>/dev/null && [ -s "$loc" ]; then
      chmod 600 "$loc"
      HD_DEP=${HD_DEP//"file:$p"/"file:$loc"}
      SG_DEP=${SG_DEP//"file:$p"/"file:$loc"}
      NAT_RULES=${NAT_RULES//"file:$p"/"file:$loc"}
    else
      echo "warn: could not fetch $p from $BOX — tools referencing it will fail closed per call" >&2
      rm -f "$loc"
    fi
  done
  if [ -n "$HD_KH" ] && ssh -i "$KEY" -o StrictHostKeyChecking=no "$BOX" "cat '$HD_KH'" > "$SECDIR/known_hosts" 2>/dev/null && [ -s "$SECDIR/known_hosts" ]; then
    export TG_HOSTDIAG_KNOWN_HOSTS="$SECDIR/known_hosts"
  fi
  [ -n "$HD_DEP" ] && export TG_HOSTDIAG_DEPLOYMENTS="$HD_DEP" && echo "parity: hostdiag deployments provisioned from the box"
  [ -n "$SG_DEP" ] && export TG_SYSLOGNG_DEPLOYMENTS="$SG_DEP" && echo "parity: syslog-ng deployments provisioned from the box"
  [ -n "$NAT_RULES" ] && export TG_CREDENTIAL_NATIVE_RULES="$NAT_RULES"
fi

# Tunnel — both ends pinned to 127.0.0.1, never "localhost" (the box resolves localhost->::1 first and
# litellm publishes on 127.0.0.1:4000 only; forwarding to the dead [::1]:4000 wedges the forwarder silently).
probe_tunnel() {
  local code
  code=$(curl -sS -m 5 -o /dev/null -w '%{http_code}' "http://127.0.0.1:${LPORT}/health/liveliness" 2>/dev/null || echo 000)
  [ "$code" != "000" ]
}
EXISTING_TUN=$(pgrep -f "127.0.0.1:${LPORT}:127.0.0.1:4000" | head -1 || true)
if [ -n "$EXISTING_TUN" ] && probe_tunnel; then
  echo "== reusing healthy existing tunnel on 127.0.0.1:${LPORT} (pid ${EXISTING_TUN}) =="
  TUN_PID="$EXISTING_TUN"; REUSE_TUN=1
else
  [ -n "$EXISTING_TUN" ] && { kill "$EXISTING_TUN" 2>/dev/null || true; sleep 1; }
  ssh -i "$KEY" -o StrictHostKeyChecking=no -o ServerAliveInterval=30 -o ServerAliveCountMax=6 \
      -f -N -L "127.0.0.1:${LPORT}:127.0.0.1:4000" "$BOX" || true
  sleep 2
  TUN_PID=$(pgrep -f "127.0.0.1:${LPORT}:127.0.0.1:4000" | head -1 || true)
  REUSE_TUN=0
  probe_tunnel || { echo "FATAL: no working gateway tunnel on 127.0.0.1:${LPORT} -> ${BOX}:4000" >&2; exit 1; }
fi

cd "$HERE"
export TG_EVAL_GATEWAY="http://127.0.0.1:${LPORT}"
export LITELLM_MASTER_KEY="$MK"
export LIBRENMS_TOKEN="$LT"
export TG_LIBRENMS_URL="${TG_LIBRENMS_URL:-https://dc1nms01.example.net}"
export TG_LIBRENMS_INSECURE="${TG_LIBRENMS_INSECURE:-true}"

RUN_START=$(date -u +%Y-%m-%dT%H:%M:%SZ)
MANIFEST="$OUT/run.json"
PROXY_LOG="$OUT/proxy.log"
: > "$OUT/arms.ndjson"

# fetch_proxy_log SINCE — pull the tg-claude-proxy's structured log. This is the ONLY admissible evidence of
# which brain served an arm: litellm echoes the requested ALIAS back in the response's `model` field (probed
# live 2026-08-04 — alias "fast" answers {"model":"fast"}), so the gateway cannot supply it. A failure here
# is FATAL, never a degrade to "no calls observed": a run that cannot prove its arms differed is not an A/B.
fetch_proxy_log() {
  ssh -i "$KEY" -o StrictHostKeyChecking=no "$PROXY_HOST" \
    "docker logs '$PROXY_CONTAINER' --since '$1' 2>&1" > "$PROXY_LOG" || {
      echo "FATAL: could not read ${PROXY_CONTAINER} logs from ${PROXY_HOST}." >&2
      echo "  Without served-model evidence this run cannot prove its arms differed, and an A/B that cannot" >&2
      echo "  prove its arms differed is not an A/B. Fix the access and re-run rather than reporting a Δ." >&2
      exit 1
    }
}

# write_manifest — assemble the run manifest from the per-arm NDJSON entries.
write_manifest() {
  {
    printf '{\n  "measured_at": "%s",\n  "git_sha": "%s",\n  "gateway": "%s",\n' \
      "$(date -u +%Y-%m-%d)" "$(git -C "$HERE" rev-parse HEAD)" "$TG_EVAL_GATEWAY"
    printf '  "note": "%s",\n' "$1"
    printf '  "arms": [\n'
    paste -sd, - < "$OUT/arms.ndjson" | sed 's/},{/},\n    {/g; s/^/    /'
    printf '\n  ]\n}\n'
  } > "$MANIFEST"
}

# ── PREFLIGHT: PROVE THE ARMS CAN DIFFER, BEFORE SPENDING THREE CORPUS PASSES ON THEM ────────────────────
#
# One trivial completion per distinct tier alias, then the distinctness check. It costs ~4 completions and
# ~15 seconds; the corpus run it guards costs three full passes against a SUBSCRIPTION-METERED proxy. As of
# 2026-08-04 the default arms fail this preflight, which is the whole point — the finding arrives for the
# price of four calls instead of a night of the owner's rate-limit window.
#
# TG_TIERAB_SKIP_PREFLIGHT=1 exists for a re-run when the answer is already known, and is deliberately not
# the default: an experiment that skips the check of its own independent variable is how an A/B measures one
# arm three times.
if [ "${TG_TIERAB_SKIP_PREFLIGHT:-0}" != "1" ]; then
  echo ""
  echo "== PREFLIGHT: proving the arms run different models (4 completions, ~15s) =="
  PRE_START=$(date -u +%Y-%m-%dT%H:%M:%SZ)
  : > "$OUT/arms.ndjson"
  # The whole request — the gateway master key included — goes through a 0600 curl CONFIG FILE, never the
  # command line. `-H "Authorization: Bearer $MK"` puts the key in argv, where any local `ps` reads it; the
  # rest of this repo passes it by environment reference for that reason (eval-gate.sh exports it and lets
  # the Go client resolve `env:LITELLM_MASTER_KEY`), and a shell probe should not be the one place that
  # regresses the convention. --config keeps it in a file the run owns and shreds on exit.
  PROBE_CFG="$OUT/.probe.curlrc"
  probe_tier() {  # probe_tier ALIAS — one one-token completion, tagged so it attributes to this arm only
    : > "$PROBE_CFG"; chmod 600 "$PROBE_CFG"
    {
      printf 'url = "http://127.0.0.1:%s/v1/chat/completions"\n' "$LPORT"
      printf 'header = "Authorization: Bearer %s"\n' "$MK"
      printf 'header = "Content-Type: application/json"\n'
      printf 'data = "{\\"model\\":\\"%s\\",\\"user\\":\\"runner:tierab-preflight\\",\\"messages\\":[{\\"role\\":\\"user\\",\\"content\\":\\"Reply with the single word: ok\\"}]}"\n' "$1"
    } >> "$PROBE_CFG"
    curl -sS -m 300 -o /dev/null --config "$PROBE_CFG" \
      || { echo "FATAL: preflight probe of alias '$1' failed — the tier cannot be measured." >&2; exit 1; }
  }
  pre_arm() {  # pre_arm NAME INVESTIGATE DECIDE — probe both tiers, record the window
    local name="$1" inv="$2" dec="$3" s e
    # ★ MILLISECOND precision on both bounds (%3N), never `+%…%SZ`. Whole-second truncation NARROWS the END,
    # and an arm's LAST call lands closest to it — which is its DECIDE-tier call, the tier TG-204 is asking
    # about. Measured 2026-08-04 with whole-second bounds: ARM-STRONG's only completion logged at
    # 22:00:37.111 against an end of 22:00:37.000 and was dropped; ARM-CHEAP's at 22:00:39.744 against
    # 22:00:39.000, dropped. Two of three arms reported UNKNOWN with their calls sitting in the log.
    s=$(date -u +%Y-%m-%dT%H:%M:%S.%3NZ)
    probe_tier "$inv"
    [ "$dec" != "$inv" ] && probe_tier "$dec"
    e=$(date -u +%Y-%m-%dT%H:%M:%S.%3NZ)
    # caller_prefix "" — the preflight runs no judge, so every in-window call IS this arm's probe; and the
    # safe default would starve anyway (LiteLLM drops `user`, so the proxy logs caller="").
    printf '{"name":"%s","investigate_tier":"%s","decide_tier":"%s","scorecard":"","start":"%s","end":"%s","caller_prefix":""}\n' \
      "$name" "$inv" "$dec" "$s" "$e" >> "$OUT/arms.ndjson"
    sleep 1   # keep the arm windows disjoint at 1s resolution, so no probe lands in two arms at once
  }
  pre_arm ARM-CONTROL "$CTRL_INV" "$CTRL_DEC"
  pre_arm ARM-STRONG  "$STRONG"   "$STRONG"
  pre_arm ARM-CHEAP   "$CHEAP"    "$CHEAP"
  fetch_proxy_log "$PRE_START"
  write_manifest "TG-204 arm-distinctness PREFLIGHT — one completion per tier alias, no corpus run"
  set +e
  go run ./tools/tierab --preflight --manifest "$MANIFEST" --proxy-log "$PROXY_LOG" \
    --archive-dir "$HERE/eval/history" --git-sha "$(git -C "$HERE" rev-parse HEAD)"
  pre_rc=$?
  set -e
  if [ "$pre_rc" != 0 ]; then
    echo "" >&2
    echo "ABORTING BEFORE THE CORPUS RUN: the arms did not pass the distinctness preflight, so three full" >&2
    echo "corpus passes would measure the same brain repeatedly and produce a Δ table of judge noise." >&2
    echo "Point the arms at aliases that resolve to different upstreams (see the header of this script)." >&2
    exit "$pre_rc"
  fi
  # ★ THE POSITIVE CONTROL (house rule 3, applied to the harness itself). A distinctness check that can only
  # ever say COLLAPSED is worth nothing — it would "pass" this estate forever by never being satisfiable. So
  # the preflight is runnable on its own, and the answer to "can this experiment be run at all?" costs four
  # completions rather than three corpus passes:
  #
  #   TG_TIERAB_PREFLIGHT_ONLY=1 TG_TIER_CONTROL_INV=arm-haiku TG_TIER_CONTROL_DEC=arm-opus \
  #     TG_TIER_STRONG=arm-opus TG_TIER_CHEAP=arm-haiku eval/tier-ab.sh
  #
  # Measured 2026-08-04: the default arms COLLAPSE (exit 1) and the arm-haiku/arm-opus arms are DISTINCT
  # (exit 0), so the check discriminates rather than always refusing.
  if [ "${TG_TIERAB_PREFLIGHT_ONLY:-0}" = "1" ]; then
    echo ""
    echo "TG_TIERAB_PREFLIGHT_ONLY=1 — stopping after the distinctness check; no corpus was run."
    exit 0
  fi
  RUN_START=$(date -u +%Y-%m-%dT%H:%M:%SZ)
  : > "$OUT/arms.ndjson"
fi

# run_arm NAME INVESTIGATE_TIER DECIDE_TIER — measure one arm and append its manifest entry.
#
# TG_EVAL_ARM is the DOUBLE GATE from temporal/runner/activities.go: the tier overrides are inert unless a
# process declares itself an experiment, so a production worker cannot lower the MECH-402 investigation floor
# by setting one environment variable. This script is the only thing in the repo that sets it.
run_arm() {
  local name="$1" inv="$2" dec="$3"
  local start end sc
  sc="$OUT/scorecard.${name}.json"
  echo ""
  echo "== [$name] investigate=${inv} decide=${dec} — measuring ${TG_EVAL_LIMIT} incident(s) =="
  rm -f "$HERE/eval/phase.json"
  TG_EVAL_ARM="$name" TG_EVAL_ARM_INVESTIGATE="$inv" TG_EVAL_ARM_DECIDE="$dec" \
    go test ./eval/ -run 'TestEvalCorpusOnBox' -count=1 -timeout 60m
  # ★ THE ARM WINDOW IS THE SESSION PHASE, NOT THE WHOLE `go test`. The harness writes eval/phase.json
  # bracketing the AGENT phase and stops it before judging begins — and the judge runs on `primary` in EVERY
  # arm (core/judge/rubric.json pins params.model), so a shell-level start/end would stamp the judge's brain
  # onto each arm's served-model signature and load every arm with an identical cost/latency constant.
  # Filtering on the `user` field instead does NOT work: LiteLLM drops `user` before an openai/-provider
  # upstream, so the proxy logs caller="" (measured 2026-08-04, four probes, all caller=""). Fail LOUD if the
  # file is missing rather than falling back to the whole-test window — a silently-wider window is precisely
  # the confound this A/B cannot survive.
  [ -f "$HERE/eval/phase.json" ] || {
    echo "FATAL: [$name] produced no eval/phase.json — the arm's session-phase window is unknown, and the" >&2
    echo "  only alternative (the whole go-test window) silently includes the judge's own primary-tier calls." >&2
    exit 1
  }
  start=$(python3 -c 'import json,sys; print(json.load(open(sys.argv[1]))["sessions_start"])' "$HERE/eval/phase.json")
  end=$(python3 -c 'import json,sys; print(json.load(open(sys.argv[1]))["sessions_end"])' "$HERE/eval/phase.json")
  cp -f "$HERE/eval/scorecard.json" "$sc"
  # Refuse a degraded arm before it can enter the comparison — the same integrity property eval-gate.sh has
  # (TG-64). A short/errored arm here would show up as a TIER effect, which is the worst possible confound.
  go run ./tools/evalgate --verify-integrity "$sc" --expect-n "$TG_EVAL_LIMIT"
  # caller_prefix "" = attribute every call in the window. Correct ONLY because the window is now the agent
  # phase (above); the safe default ("runner:") starves to zero through today's gateway, which drops `user`.
  printf '{"name":"%s","investigate_tier":"%s","decide_tier":"%s","scorecard":"%s","start":"%s","end":"%s","caller_prefix":""}\n' \
    "$name" "$inv" "$dec" "$sc" "$start" "$end" >> "$OUT/arms.ndjson"
}

run_arm ARM-CONTROL "$CTRL_INV" "$CTRL_DEC"
run_arm ARM-STRONG  "$STRONG"   "$STRONG"
run_arm ARM-CHEAP   "$CHEAP"    "$CHEAP"

# ── HARVEST THE GROUND TRUTH ────────────────────────────────────────────────────────────────────────────
# The model gateway CANNOT say which brain served an arm: litellm echoes the requested ALIAS back in the
# response's `model` field (probed live 2026-08-04 — alias "fast" answers {"model":"fast"}). The
# tg-claude-proxy logs the model that ACTUALLY served (`served_model`), so the proxy's own telemetry is the
# only admissible evidence that the arms differed at all. Fetched with a small margin before RUN_START so a
# call logged in the same second as the first arm's start is not lost.
echo ""
echo "== harvesting served-model telemetry from ${PROXY_CONTAINER} on ${PROXY_HOST} =="
fetch_proxy_log "$RUN_START"
write_manifest "TG-204 three-arm model-tier A/B; corpus subset n=${TG_EVAL_LIMIT} per arm, arms sequential in one window"

echo ""
echo "== deterministic three-arm comparison (arm distinctness FIRST, then the deltas) =="
go run ./tools/tierab --manifest "$MANIFEST" --proxy-log "$PROXY_LOG" \
  --archive-dir "$HERE/eval/history" --git-sha "$(git -C "$HERE" rev-parse HEAD)"
