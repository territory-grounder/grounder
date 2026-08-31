#!/usr/bin/env bash
# TG-420 slice 1 — the litellm egress-proxy OBSERVE drill. Run ON THE BOX, AFTER arming observe.
#
# Per OWNER RULING TG-488 B11 AS CORRECTED 2026-08-14 on TG-420, the fence is FIVE endpoints — sidecar +
# ollama (direct, HTTP) and api.z.ai + api.deepseek.com + api.mistral.ai (HTTPS, through the proxy).
# DeepSeek and Mistral are NOT strangers: the correction found DeepSeek is the judge and Mistral the live
# primary/fast brain, so blocking them would cut both (see provider-allowlist.txt). "Log first, then block
# strangers" still governs: in OBSERVE mode the proxy blocks nothing, and this drill's job is to (a) prove
# completions still go GREEN through the proxy, and (b) MEASURE whatever IS still a stranger — the provider
# hosts litellm dials that are OUTSIDE the corrected fence — so the owner can review them before slice-2
# enforcement blocks them.
#
# TWO-SIDED BY DESIGN — TG-381's analogous drill (deploy/host/tg-egress-lan-drill.sh) was retired by
# TG-160; this is its living descendant: a positive control (a real completion returns 200 —
# the proxy did not break the brain) beside the observation (the proxy is in-path and the strangers are
# surfaced). A drill with only one half would pass on a proxy that logs everything and serves nothing, or
# serves everything and observes nothing.
#
# ── ARMING OBSERVE (do THIS before running the drill) ───────────────────────────────────────────────────
#   1. In the box .env:  COMPOSE_PROFILES=split-planes,egress-proxy
#                        TG_LITELLM_HTTPS_PROXY=http://tg-egress-proxy:8888
#   2. cd /srv/tg/deploy && docker compose up -d tg-egress-proxy && docker compose up -d litellm
#   3. LITELLM_KEY=<master key>  bash deploy/host/tg-egress-proxy-observe-drill.sh
#
# ── SLICE 2 — ENFORCE (OWNER-GATED, a SEPARATE change; NOT armed by this MR) ─────────────────────────────
# Only after this drill is clean AND the owner has reviewed the STRANGER list it reports:
#   a. Build a host-anchored enforce filter from the fence's THREE HTTPS endpoints and turn on default-deny
#      (see the SLICE 2 block in deploy/egress-proxy/tinyproxy.conf: permit api.z.ai / api.deepseek.com /
#      api.mistral.ai, deny the rest), then `docker compose up -d --build tg-egress-proxy`. Only genuine
#      strangers (today: none — see TestProxyFenceStrangerReport) are BLOCKED; DeepSeek/Mistral are fenced,
#      not strangers, since the 2026-08-14 correction.
#   b. Re-run this drill: completions still green, and any stranger now REFUSES at the proxy (not merely
#      logged). The sidecar + ollama lanes (and the TG_LITELLM01_BASE lane, see provider-allowlist.txt) are
#      HTTP/direct and unaffected.
#   c. A host-side drop of litellm's DIRECT public egress (so the proxy is the ONLY path out) has NO
#      surviving template to mirror: TG-160 deleted deploy/host/tg-host-isolation.sh (Won't-fix, 2026-08-18)
#      for blocking litellm's LAN reach to dc1litellm01 — the same-day TG-293 change made that hop
#      LOAD-BEARING (primary/fast/opus-cc/arm-*/forensic-ir all dial it, litellm-config.yaml). This leg
#      needs a fresh design scoped to litellm's public-provider destinations only, not a revert.
#   d. Prove the drop from the litellm container: a direct socket to a NON-fence public host (e.g.
#      example.com:443) must TIME OUT while a completion on the fence (api.z.ai / api.deepseek.com /
#      api.mistral.ai) stays green.
# ─────────────────────────────────────────────────────────────────────────────────────────────────────────
#
# Exits non-zero if the brain is not green through the proxy or the proxy is not in the path. STRANGERS are
# REPORTED, never failed on — surfacing them is the whole point of observe mode.
set -euo pipefail

PROXY_CTR="${PROXY_CTR:-territory-grounder-tg-egress-proxy-1}"
LITELLM_URL="${LITELLM_URL:-http://127.0.0.1:4000}"
LITELLM_KEY="${LITELLM_KEY:-${LITELLM_MASTER_KEY:-}}"
FENCE="${FENCE:-/srv/tg/deploy/egress-proxy/provider-allowlist.txt}"
# Model aliases to drive, one per provider tier, so both the fence HTTPS endpoint AND the strangers get
# touched. Override for your ladder. Any may app-error (e.g. z.ai balance); the CONNECT is logged before any
# application-level failure, which is what the observation reads.
MODELS="${MODELS:-primary judge fallback-zai}"
LOG_SINCE="${LOG_SINCE:-15m}"

if [[ -z "$LITELLM_KEY" ]]; then
  echo "drill: REFUSING — set LITELLM_KEY (or LITELLM_MASTER_KEY) to the gateway master key" >&2
  exit 2
fi
if ! docker inspect "$PROXY_CTR" >/dev/null 2>&1; then
  echo "drill: REFUSING — proxy container '$PROXY_CTR' not found. Arm observe first (see this script's" >&2
  echo "       header): add egress-proxy to COMPOSE_PROFILES and 'docker compose up -d tg-egress-proxy'." >&2
  exit 2
fi

# The fence's PUBLIC domains (for classifying observed hosts). The fence's other two entries — the `sidecar`
# service name and the `${TG_OLLAMA_HOST}` env-ref — are DIRECT lanes that never traverse the proxy, so they
# are not domains an observed CONNECT could match; only dotted, non-env-ref tokens (i.e. api.z.ai,
# api.deepseek.com, api.mistral.ai) count.
fence_domains=()
while IFS= read -r line; do
  line="${line#"${line%%[![:space:]]*}"}" # ltrim
  [[ -z "$line" || "$line" == \#* ]] && continue
  if [[ "$line" == *.* && "$line" != *'$'* && "$line" != *'{'* ]]; then
    fence_domains+=("$line")
  fi
done <"$FENCE"

is_fence() { # host -> 0 if inside the fence's public domains, 1 otherwise
  local host="$1" d
  for d in "${fence_domains[@]}"; do
    [[ "$host" == "$d" || "$host" == *".$d" ]] && return 0
  done
  return 1
}

echo "== driving completions through the armed proxy (positive control + stranger measurement) =="
read -ra model_list <<<"$MODELS"
brain_green=0
for m in "${model_list[@]}"; do
  code="$(curl -sS -m 30 -o /dev/null -w '%{http_code}' \
    -H "Authorization: Bearer $LITELLM_KEY" -H 'Content-Type: application/json' \
    -d "{\"model\":\"$m\",\"max_tokens\":1,\"messages\":[{\"role\":\"user\",\"content\":\"ping\"}]}" \
    "$LITELLM_URL/v1/chat/completions" 2>/dev/null || echo 000)"
  echo "  drove $m -> HTTP $code"
  [[ "$code" == "200" ]] && brain_green=1
done

fail=0

# POSITIVE CONTROL: at least one completion served 200. If none did, the proxy path broke the brain (or the
# key/URL is wrong) and the observation below would be meaningless.
if [[ "$brain_green" -eq 1 ]]; then
  echo "PASS  positive-control  a completion returned HTTP 200 through the proxy"
else
  echo "FAIL  positive-control  NO completion returned 200 — the proxy path is not serving the brain"
  fail=1
fi

# The proxy must actually be IN the path: no CONNECT lines means litellm is not routed through it.
log="$(docker logs --since "$LOG_SINCE" "$PROXY_CTR" 2>&1 || true)"
observed="$(printf '%s\n' "$log" | grep -oE 'CONNECT [A-Za-z0-9._-]+:[0-9]+' | sed -E 's/^CONNECT //; s/:[0-9]+$//' | sort -u || true)"
if [[ -z "$observed" ]]; then
  echo "FAIL  proxy-in-path     no CONNECT destinations in the proxy log — is TG_LITELLM_HTTPS_PROXY set and litellm restarted?"
  fail=1
fi

# OBSERVATION (report only): classify every host the proxy saw as FENCE or STRANGER. Strangers are the
# evidence the owner asked for; they are what slice-2 enforcement will block, so they are surfaced, not failed.
echo "== observed proxy-borne HTTPS destinations (fence vs stranger) =="
strangers=0
while IFS= read -r host; do
  [[ -z "$host" ]] && continue
  if is_fence "$host"; then
    echo "  FENCE     $host  (permitted)"
  else
    echo "  STRANGER  $host  (logged now; slice-2 enforcement will BLOCK this)"
    strangers=$((strangers + 1))
  fi
done <<<"$observed"
echo "  -> $strangers stranger host(s) observed outside the owner-ruled fence"

if [[ "$fail" -eq 0 ]]; then
  echo "observe drill: PASS — brain green through the proxy; strangers surfaced above for owner review before enforce"
else
  echo "observe drill: FAIL — the proxy path is broken or not in-path; fix before reading the stranger list"
fi
exit "$fail"
