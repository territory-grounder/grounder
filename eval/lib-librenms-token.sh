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
  local lt lref lpath lfield probe url why baoerr baoenv baoaddr scheme
  why=""
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
        # ★ THE SECOND FIX RESOLVED THE TOKEN ONLY FROM AN INTERACTIVE SHELL (TG-596). `bao` finds its
        #    server through BAO_ADDR (or VAULT_ADDR), and on the runner that variable is exported from
        #    ~/.bashrc — which a cron shell never reads. So the nightly trend-watch dereferenced NOTHING on
        #    every run since this lib was written: `bao` dialled its built-in default address and failed,
        #    `2>/dev/null` hid why, and the same command typed in a terminal succeeded — every interactive
        #    change gate drilled a path the cron never took, and the nightly record read "every control
        #    EXCLUDED as unobservable" for weeks. A non-interactive run now takes the address from
        #    TG_BAO_ENV (default ~/.tg-bao.env — the ~/.tg-youtrack.env convention: a 0600 file holding
        #    `export BAO_ADDR=…`), and an unset address is NAMED in the warning, not folded into "no
        #    usable token". The address is not a secret; keeping it out of the repo is the mirror rule.
        #    The file is READ, not executed: only its `BAO_ADDR=` line is taken (quotes stripped), so a
        #    stray command in it can neither run here nor abort a `set -e` caller, and the lib's "never
        #    exits" promise holds. The export lands in the $(...) subshell the callers run us in.
        baoenv="${TG_BAO_ENV:-$HOME/.tg-bao.env}"
        if [ -z "${BAO_ADDR:-}${VAULT_ADDR:-}" ] && [ -r "$baoenv" ]; then
          baoaddr=$(grep -E '^(export )?BAO_ADDR=' "$baoenv" 2>/dev/null | tail -n1 | cut -d= -f2- | tr -d "'\"" || true)
          [ -n "$baoaddr" ] && export BAO_ADDR="$baoaddr"
        fi
        if [ -z "${BAO_ADDR:-}${VAULT_ADDR:-}" ]; then
          why="bao address unset: neither BAO_ADDR nor VAULT_ADDR is in this environment and $baoenv is absent or has no BAO_ADDR= line (a cron shell never reads ~/.bashrc; put 'export BAO_ADDR=…' in that file)"
        else
          baoerr=$(mktemp 2>/dev/null || true); [ -n "$baoerr" ] || baoerr=/dev/null
          lt=$(VAULT_TOKEN="${VAULT_TOKEN:-$(cat "$HOME/.vault-token" 2>/dev/null)}" \
               bao kv get -field="$lfield" "${lpath/data\//}" 2>"$baoerr" || true)
          [ -z "$lt" ] && why="bao dereference of $lref failed: $(head -n1 "$baoerr" 2>/dev/null)"
          [ "$baoerr" = /dev/null ] || rm -f "$baoerr"
        fi
        ;;
      "")
        why="no TG_LIBRENMS_DEPLOYMENTS token ref readable on $box"
        ;;
      *)
        # Only the SCHEME is named: a site that put a raw value in the ref field would otherwise see it
        # echoed into the nightly log by a warning about it.
        scheme="${lref%%:*}"
        case "$lref" in *:*) ;; *) scheme="" ;; esac
        case "$scheme" in *[!a-z]*|"") scheme="(not a lowercase scheme:ref — a bare value is never echoed)" ;; *) scheme="$scheme:" ;; esac
        why="unsupported token ref scheme in TG_LIBRENMS_DEPLOYMENTS: $scheme (only bao: is dereferenced here)"
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
        why="LibreNMS rejected the resolved token (auth probe: ${probe:-empty response})"
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
    [ -n "$why" ] && echo "      reason: $why" >&2
  fi

  printf %s "$lt"
}
