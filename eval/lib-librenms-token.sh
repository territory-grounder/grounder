#!/usr/bin/env bash
# SHARED LibreNMS READ-TOKEN RESOLVER for the eval harnesses (TG-362).
#
# WHY THIS IS A FILE AND NOT A COPIED BLOCK. This resolution was written once in eval-gate.sh, and
# eval/tier-ab.sh carried a COPY of its first, broken form — `grep "^LIBRENMS_TOKEN=" .env` — long after
# eval-gate.sh had been repaired. tier-ab.sh therefore exported an EMPTY LibreNMS token into every tier
# arm it has measured since the secret-policy migration, so those comparisons scored an agent whose
# LibreNMS grounding was dead, in a file whose own comments insist on toolset parity twelve lines below.
# A second copy is how the first defect outlived its fix; there is now one copy.
#
# tg_resolve_librenms_token <ssh-key> <box>
#   Prints a VALIDATED LibreNMS read token on stdout, or nothing at all. Warnings go to stderr, so
#   `LT=$(tg_resolve_librenms_token ...)` is safe. Never exits: an absent token is a legitimate,
#   REPORTED outcome, and the caller decides what that means for its own bar.

# shellcheck shell=bash

tg_resolve_librenms_token() {
  local key="$1" box="$2"
  local lt lref lpath lfield probe url
  url="${TG_LIBRENMS_URL:-https://dc1nms01.example.net}"

  # 1. The RAW variable. It no longer exists on the box, and this branch is kept only so a site that has
  #    not migrated still works — not because it is expected to hit.
  #
  #    ★ THIS READER SPENT WEEKS READING A VARIABLE THAT NO LONGER EXISTS. The secret-policy migration
  #    moved the credential behind a reference and left no raw value. `|| true` swallowed the empty
  #    result, controlObservability took its no-token early return, and EVERY negative control was
  #    counted in full — including three on hosts measured UNMONITORED on 2026-08-06. ctl-01
  #    (dc1freeipa01) is one of them, and it blocked !1018 for two days.
  lt=$(ssh -i "$key" -o StrictHostKeyChecking=no "$box" \
    'grep "^LIBRENMS_TOKEN=" /srv/tg/deploy/.env 2>/dev/null | cut -d= -f2-' || true)

  # 2. The REFERENCE. Field 3 of the first TG_LIBRENMS_DEPLOYMENTS row:
  #      site|baseURL|TokenRef|timezone   e.g. dc1|https://…|bao:secret/data/tg/librenms-nl-triage#token|…
  #
  #    NOT TG_LIBRENMS_INGEST_TOKEN_REF, which the first attempt at this reached for. That one is the
  #    INGEST direction (LibreNMS pushing INTO TG); LibreNMS answers "Unauthenticated." to a read with it,
  #    which only the auth probe below catches. The deployments ref is TG-342's tg-triage-ro identity:
  #    13 read verbs, and a DELETE with it returns 403.
  #
  #    ★ THE FIRST FIX PUT THE `bao` CALL INSIDE THE SSH AND WAS THEREFORE JUST AS INERT AS WHAT IT
  #    REPLACED. `bao` is not installed on the box and there is no vault token there, so it returned
  #    "bao: command not found", `|| true` swallowed it, and the verification — a length check
  #    `[ ${#T} -gt 12 ]` — passed on the 36-character error string. A probe whose success condition an
  #    error message also satisfies is not a probe. The BOX holds the reference; THIS host dereferences it.
  if [ -z "$lt" ]; then
    lref=$(ssh -i "$key" -o StrictHostKeyChecking=no "$box" \
      'grep "^TG_LIBRENMS_DEPLOYMENTS=" /srv/tg/deploy/.env 2>/dev/null | cut -d= -f2- | cut -d";" -f1 | cut -d"|" -f3' || true)
    case "$lref" in
      bao:*)
        lpath=${lref#bao:}; lfield=${lpath##*#}; lpath=${lpath%%#*}
        lt=$(VAULT_TOKEN="${VAULT_TOKEN:-$(cat "$HOME/.vault-token" 2>/dev/null)}" \
             bao kv get -field="$lfield" "${lpath/data\//}" 2>/dev/null || true)
        ;;
    esac
  fi

  # 3. VALIDATE, do not merely obtain. An expired or wrong-scope token is a STRING, and a string is what
  #    the length check above mistook for success. Ask LibreNMS something only a working token can answer;
  #    if the answer is an auth error, treat the token as ABSENT, so the bar it feeds reports UNMEASURED
  #    rather than adjudicating on a lookup that fails for every host and marks every control unobservable.
  if [ -n "$lt" ]; then
    probe=$(curl -sk -m 20 -H "X-Auth-Token: $lt" "$url/api/v0/devices" 2>/dev/null | head -c 400)
    case "$probe" in
      *Unauthenticated*|*Unauthorized*|"")
        echo "warn: the LibreNMS token resolved but FAILED its own auth probe — treating it as absent." >&2
        lt="" ;;
    esac
  fi

  # 4. An absent token is REPORTED, never silent. A check that quietly no-ops when its credential is
  #    missing is how this defect survived: the operator has to be able to tell "the bar was clean" from
  #    "the bar could not be computed".
  if [ -z "$lt" ]; then
    echo "warn: no usable LibreNMS token (tried LIBRENMS_TOKEN on $box, then the TG_LIBRENMS_DEPLOYMENTS" >&2
    echo "      token ref dereferenced locally, then an auth probe). Control observability CANNOT be" >&2
    echo "      checked, so the negative-control bar will report UNMEASURED rather than applying blind." >&2
  fi

  printf %s "$lt"
}
