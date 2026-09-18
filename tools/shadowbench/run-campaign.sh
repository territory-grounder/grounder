#!/usr/bin/env bash
# run-campaign.sh — the CONFIRMATORY CAMPAIGN as a bounded, self-terminating run.
#
# NOT a cron, NOT "nightly". The guinea pigs exist to close the window in HOURS, not days: the
# injector fires every few minutes and both systems triage in real time, so this drives to the
# pre-registered bar on a TIGHT loop and STOPS itself the moment accrual.py says the bar is met —
# then disarms the injector so the pool goes back to rest (BOARD working rule 5). A once-a-day
# cron would throw away all that supply and stretch a 30-pair target into a week; that is exactly
# the undefined-ETA behavior the guinea pigs were built to eliminate.
#
# Lifecycle:
#   1. ARM the injector (systemctl enable --now on the box) — a named consumer, so rule 5 permits it.
#   2. Loop every INTERVAL_MIN (default 30): harvest today's dual-triaged faults -> reconcile
#      against the injector ledger -> accrual.py. All under the gateway flock so it never contends
#      with an eval run.
#   3. When accrual.py reports the §3 bar met (>=30 counted pairs, >=12 hosts, <=3/host — the host
#      minimum was amended 15->12 per §6 and accrual.py is the authority, not this comment), STOP:
#      disarm+stop the injector, leave the pool healthy, print the final accrual, exit 0.
#   4. Hard safety cap MAX_HOURS (default 12): if the bar is not met by then, stop, disarm, and
#      exit non-zero for a human to look at — a campaign that will not converge must not soak
#      forever.
#
# Env: BOX (root@dc1tg01), KEY (~/.ssh/one_key), INTERVAL_MIN (30), MAX_HOURS (12),
#      LOCK (/tmp/tg-gateway.lock). Run under nohup; it logs its own progress.
set -uo pipefail
HERE="$(cd "$(dirname "$0")" && pwd)"
cd "$HERE"
BOX="${BOX:-root@dc1tg01}"
KEY="${KEY:-$HOME/.ssh/one_key}"
INTERVAL_MIN="${INTERVAL_MIN:-30}"
MAX_HOURS="${MAX_HOURS:-12}"
LOCK="${LOCK:-/tmp/tg-gateway.lock}"
# COMPARATOR-CHANGE BOUNDARY (PRE-REGISTRATION §6, 2026-07-30 21:31Z): the predecessor's triage prompt was
# fixed at this instant to diagnose recognized-synthetic alerts instead of skipping them. A pre-patch
# predecessor triage is not a valid head-to-head data point (it declined to play), so ONLY faults injected
# at/after this boundary — and the pairs they form — may count as confirmatory supply. This is threaded into
# BOTH reconcile-supply.py (which fetches faults WHERE injected_at >= boundary) and accrual.py (which counts
# manifest records WHERE fault-ts >= boundary), so the 1283 pre-patch ledger faults are excluded mechanically
# at both stages. Later than the plan's own 2026-07-27 freeze on purpose; never earlier.
# Boundary moved again 2026-07-31T01:30Z (§6): predecessor parallelism raised to cap-4; serial-era
# pairs excluded the same mechanical way.
# CAMPAIGN #2 BOUNDARY (PRE-REGISTRATION §6, 2026-07-31T21:55:57Z): both arms verified on
# claude-opus-5. Campaign #1 (unequal brains) is closed; none of its pairs may be pooled here.
# CAMPAIGN #3 BOUNDARY (PRE-REGISTRATION §6, 2026-08-25): 2026-08-26T00:00:00Z, declared at ZERO accrued
# pairs (injector disabled since 08-15; campaigns #1/#2 non-confirmatory and never pooled). Campaign #3 is
# an AS-DEPLOYED STACK comparison (owner-ruled 2026-08-25): TG on its production Azure rail vs the
# predecessor on claude-opus-5 — model parity is NOT asserted; the §6 entry carries the declaration.
ACCRUE_FROM="${ACCRUE_FROM:-2026-08-26T00:00:00Z}"
# The campaign whose reproducible pairs snapshot is materialised + committed at bar-met (TG-249). Set per
# campaign; campaigns #1/#2 are non-confirmatory (§6 2026-08-19), so the next confirmatory run is #3.
CAMPAIGN="${CAMPAIGN:-3}"
DEADLINE=$(( $(date +%s) + MAX_HOURS * 3600 ))
ssh() { command ssh -i "$KEY" -o BatchMode=yes -o ConnectTimeout=10 "$@"; }
log() { echo "[campaign $(date -u +%H:%M)] $*"; }

disarm() {
  log "disarming injector — the pool returns to rest (rule 5)"
  ssh "$BOX" 'systemctl disable --now faultinjector' 2>&1 | sed 's/^/  /' || true
  # graceful drain: the fail-closed engine restores every outstanding fault before it exits
  for _ in $(seq 1 20); do
    out="$(ssh "$BOX" 'systemctl is-active faultinjector 2>/dev/null; docker exec territory-grounder-postgres-1 psql -U postgres -d grounder -tAc "SELECT count(*) FROM injected_fault WHERE restore_state IN ('"'"'pending'"'"','"'"'failed'"'"');"' 2>/dev/null | tr "\n" " ")"
    [ "$out" = "inactive 0 " ] && { log "injector stopped, obligations drained to 0 — pool healthy"; return 0; }
    sleep 30
  done
  log "WARN: drain did not reach 0 in 10m — check the pool by hand"
}
trap 'disarm' EXIT

# The §3 bar met is decided by accrual.py's own output (it owns the definition, not this script).
bar_met() { grep -q "ARE WE DONE ACCRUING: YES" "$1"; }

log "ARMING injector for the confirmatory campaign (initiation gate satisfied 2026-07-31)"
ssh "$BOX" 'systemctl enable --now faultinjector && systemctl is-active faultinjector' 2>&1 | sed 's/^/  /'

cycle=0
while :; do
  cycle=$((cycle+1))
  if [ "$(date +%s)" -ge "$DEADLINE" ]; then
    log "MAX_HOURS ($MAX_HOURS) reached without meeting the bar — stopping for a human to review"
    flock "$LOCK" -c "cd '$HERE' && ./accrual.py --accrue-from '$ACCRUE_FROM'" || true
    exit 2
  fi
  log "cycle $cycle: harvest -> reconcile -> accrual"
  today="$(date -u +%F)"
  # SB_PAIRS_ONLY: campaign cycles judge PAIRS only (singles cannot bank toward the bar and were
  # starving the judge); SB_JUDGE_WORKERS: 3 concurrent judge subprocesses. Harness-side only.
  # MODEL=judge: the dedicated judge alias (deepseek-v4-pro pinned, no fallback) — the same model
  # that has served every judged pair so far, now by config instead of by the kimi-400 accident.
  flock "$LOCK" -c "cd '$HERE' && DATE='$today' SHADOW_FROM='$today 00:00:00+00' MODEL=judge SB_PAIRS_ONLY=1 SB_JUDGE_WORKERS=3 ./run.sh" >> /tmp/campaign-harvest.log 2>&1 || log "  harvest returned non-zero (continuing)"
  flock "$LOCK" -c "cd '$HERE' && ./reconcile-supply.py --accrue-from '$ACCRUE_FROM'" >> /tmp/campaign-harvest.log 2>&1 || log "  reconcile returned non-zero (continuing)"
  acc="$(mktemp)"; flock "$LOCK" -c "cd '$HERE' && ./accrual.py --accrue-from '$ACCRUE_FROM'" | tee "$acc"
  if bar_met "$acc"; then
    log "PRE-REGISTRATION §3 bar MET — campaign supply complete."
    # REPRODUCIBILITY (TG-249): freeze the exact judged pairs this campaign accrued into a COMMITTED snapshot,
    # so the verdict reproduces from a clean clone (scorecard.jsonl is mutable + git-ignored + spans campaigns).
    snapshot="confirmatory/pairs-campaign${CAMPAIGN}.jsonl"
    npairs="$(cd "$HERE" && python3 snapshot_pairs.py scorecard.jsonl --accrue-from "$ACCRUE_FROM" --out "$snapshot" 2>/dev/null || echo '?')"
    ( cd "$HERE" && git add "$snapshot" >/dev/null 2>&1 ) || true
    log "materialised + staged $snapshot ($npairs pairs). COMMIT it with the verdict so the result reproduces:"
    log "  ./analyze.py $snapshot --ground-truth confirmatory/ground-truth-campaign${CAMPAIGN}.json"
    log "  git commit -m 'shadowbench: campaign ${CAMPAIGN} pairs snapshot + verdict (reproducible)' -- $snapshot confirmatory/"
    log "the injector is being disarmed now."
    rm -f "$acc"
    exit 0
  fi
  rm -f "$acc"
  # BOTH-ARMS HEALTH (TG-545): a shadowbench pair needs BOTH arms, but this loop used to watch only the
  # injector — TG's triage arm once sat dead for ~3.5h behind a starved credential while the campaign kept
  # banking nothing and read the frozen count as slow accrual. Check TG's arm each cycle too and say so
  # LOUDLY when it is down (the same discipline as the injector's own CAMPAIGN BARREN self-report). It only
  # reports; a human/monitor acts, so a transient blip does not kill an otherwise-converging campaign.
  wh="$(ssh "$BOX" "docker inspect -f '{{.State.Health.Status}}' territory-grounder-worker-1" 2>/dev/null || echo unknown)"
  tr="$(ssh "$BOX" "docker exec territory-grounder-postgres-1 psql -U postgres -d grounder -tAc \"SELECT count(*) FROM session_triage WHERE created_at >= now() - interval '${INTERVAL_MIN} min';\"" 2>/dev/null || echo 0)"
  ij="$(ssh "$BOX" "docker exec territory-grounder-postgres-1 psql -U postgres -d grounder -tAc \"SELECT count(*) FROM injected_fault WHERE injected_at >= now() - interval '${INTERVAL_MIN} min';\"" 2>/dev/null || echo 0)"
  if ! ah="$(cd "$HERE" && python3 arm_health.py "$wh" "${tr:-0}" "${ij:-0}")"; then
    log "CAMPAIGN DEGRADED — ${ah#DEGRADED: } (worker=$wh triage/${INTERVAL_MIN}m=${tr:-0} injects/${INTERVAL_MIN}m=${ij:-0}); pairs banked this window may be one-armed — fix TG's arm before trusting further accrual."
  fi
  log "bar not yet met — next cycle in ${INTERVAL_MIN}m"
  sleep $(( INTERVAL_MIN * 60 ))
done
