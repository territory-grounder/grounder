#!/usr/bin/env bash
# TG-675 (was TG-420 slice 1) — the litellm egress-proxy drill. Run ON THE BOX, AFTER arming the proxy.
#
# The proxy ENFORCES default-deny over the DEPLOYED provider set (deploy/egress-proxy/provider-allowlist.txt,
# read from the live box 2026-11-07: the Azure Foundry endpoint behind primary/fast + the deepseek/mistral/z.ai
# auto-heal rows whose keys are present; enforce-filter is its anchored form). The TG-488 B11 fence this drill
# used to cite named a model plane the owner's 2026-08-31 ruling retired. This drill is THREE-SIDED:
#   positive control — a real completion returns 200 through the proxy (the fence did not break the brain);
#   in-path          — the proxy log shows CONNECT destinations (litellm is actually routed through it);
#   negative control — a CONNECT to an UNLISTED public host, from inside the litellm container and through
#                      the proxy, is REFUSED (HTTP 403, "Proxying refused on filtered domain" in the log).
# A drill with only the first half would pass on a proxy that serves everything; one with only the last would
# pass on a proxy that serves nothing.
#
# ── ARMING (do THIS before running the drill; done on the production box 2026-11-07) ─────────────────────
#   1. In the box .env:  COMPOSE_PROFILES=split-planes,egress-proxy
#                        TG_LITELLM_HTTPS_PROXY=http://tg-egress-proxy:8888
#   2. cd /srv/tg/deploy && docker compose up -d --build tg-egress-proxy && docker compose up -d litellm
#   3. LITELLM_KEY=<master key>  bash deploy/host/tg-egress-proxy-observe-drill.sh
#   Rollback: restore the .env backup taken before step 1 and `docker compose up -d litellm`.
#
# ── WHAT THIS PROXY DOES NOT BOUND (docs/THREAT-MODEL.md §5.3) ───────────────────────────────────────────
#   The HTTP direct lanes — the operator's litellm01 (every Claude call), the Ollama embed lane, the sidecar
#   (TG_LITELLM01_BASE / TG_OLLAMA_HOST / `sidecar` in provider-allowlist.txt) — never traverse HTTPS_PROXY.
#   A host-side drop of litellm's DIRECT public egress (so the proxy is the ONLY path out) has NO surviving
#   template: TG-160 deleted deploy/host/tg-host-isolation.sh (Won't-fix, 2026-08-18) for blocking litellm's
#   LAN reach to litellm01, which TG-293 made LOAD-BEARING the same day. That leg needs a fresh design scoped
#   to litellm's public-provider destinations only, not a revert; until then the fence binds what litellm
#   routes through the proxy, and a compromised litellm could still dial out directly.
# ─────────────────────────────────────────────────────────────────────────────────────────────────────────
#
# Exits non-zero if the brain is not green through the proxy, the proxy is not in the path, or the unlisted
# host is NOT refused. A permitted host outside the fence is impossible under default-deny; one in the log
# would mean the filter is not loaded, and is failed on.
set -euo pipefail

PROXY_CTR="${PROXY_CTR:-territory-grounder-tg-egress-proxy-1}"
LITELLM_URL="${LITELLM_URL:-http://127.0.0.1:4000}"
LITELLM_KEY="${LITELLM_KEY:-${LITELLM_MASTER_KEY:-}}"
FENCE="${FENCE:-/srv/tg/deploy/egress-proxy/provider-allowlist.txt}"
# Model aliases to drive, one per provider tier, so both the fence HTTPS endpoint AND the strangers get
# touched. Override for your ladder. Any may app-error (e.g. z.ai balance); the CONNECT is logged before any
# application-level failure, which is what the observation reads.
# ONLY the lanes that traverse the proxy count as the positive control: primary/fast dial the Azure host over
# HTTPS_PROXY. judge and every other Claude lane go to litellm01 over plain HTTP and never touch the proxy, so a
# 200 from them proves nothing about the fence (fresh-eyes review, 2026-11-07).
MODELS="${MODELS:-primary fast}"
LOG_SINCE="${LOG_SINCE:-15m}"
# The NEGATIVE control's unlisted public host (never a fence host) and the litellm container that dials it.
NEG_HOST="${NEG_HOST:-example.com}"
LITELLM_CTR="${LITELLM_CTR:-territory-grounder-litellm-1}"
PROXY_URL="${PROXY_URL:-http://tg-egress-proxy:8888}"

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

echo "== driving the PROXIED completions (positive control: the fence did not break the Azure lanes) =="
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

# Every CONNECT the proxy SERVED must be a fence host: under default-deny a served stranger means the filter
# is not loaded (the config names a file the image lacks, or FilterDefaultDeny is off), which is a failure.
# Refusals are listed from the proxy's own refusal lines, so the operator sees what litellm tried.
echo "== proxy-borne HTTPS destinations (served vs refused) =="
strangers=0
while IFS= read -r host; do
  [[ -z "$host" ]] && continue
  if is_fence "$host"; then
    echo "  FENCE     $host  (permitted)"
  else
    echo "  STRANGER  $host  (SERVED — the filter is NOT enforcing)"
    strangers=$((strangers + 1))
  fi
done <<<"$observed"
refused="$(printf '%s\n' "$log" | grep -oE 'refused on filtered domain "[^"]+"' | sed -E 's/^refused on filtered domain "//; s/"$//' | sort | uniq -c | sort -rn || true)"
if [[ -n "$refused" ]]; then
  echo "  refused (count host):"
  printf '%s\n' "$refused" | sed 's/^/    /'
fi
if [[ "$strangers" -gt 0 ]]; then
  echo "FAIL  enforcing         $strangers host(s) outside the fence were SERVED — default-deny is not in effect"
  fail=1
else
  echo "PASS  enforcing         every served CONNECT was a fence host"
fi

# NEGATIVE CONTROL: from inside the litellm container, through the proxy, an UNLISTED public host must be
# refused with 403. Runs litellm's own Python (the image ships no curl) with an explicit proxy, so the result is
# about the proxy's filter and not about the container's env. A refused CONNECT does NOT surface as HTTPError:
# urllib raises URLError("Tunnel connection failed: 403 Filtered"), so the tunnel status is parsed out of the
# reason (fresh-eyes review, 2026-11-07 — the first draft caught HTTPError only and would have failed on a working
# fence). `docker exec -i` is what carries the heredoc to python's stdin. A 200 means the filter is not
# enforcing; 000 means no proxy answer at all (and the positive control would have failed too).
neg_code="$(docker exec -i -e "NEG_URL=https://$NEG_HOST/" -e "PROXY_URL=$PROXY_URL" "$LITELLM_CTR" python3 - <<'PYNEG' || echo 000
import os, re, urllib.request, urllib.error
h = urllib.request.ProxyHandler({"https": os.environ["PROXY_URL"], "http": os.environ["PROXY_URL"]})
try:
    r = urllib.request.build_opener(h).open(os.environ["NEG_URL"], timeout=20)
    print(r.status)
except urllib.error.HTTPError as e:
    print(e.code)
except urllib.error.URLError as e:
    m = re.search(r"Tunnel connection failed: (\d{3})", str(e.reason))
    print(m.group(1) if m else "000")
except Exception:
    print("000")
PYNEG
)"
neg_code="${neg_code//[^0-9]/}"
if [[ "$neg_code" == "403" ]]; then
  echo "PASS  negative-control  CONNECT $NEG_HOST through the proxy -> HTTP 403 (refused on filtered domain)"
else
  echo "FAIL  negative-control  CONNECT $NEG_HOST through the proxy -> ${neg_code:-no answer} (expected 403)"
  fail=1
fi

if [[ "$fail" -eq 0 ]]; then
  echo "egress-proxy drill: PASS — brain green through the proxy, in-path, every served host fenced, unlisted host refused"
else
  echo "egress-proxy drill: FAIL — see the FAIL line(s) above; rollback = restore the .env backup and 'docker compose up -d litellm'"
fi
exit "$fail"
