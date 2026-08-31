#!/usr/bin/env python3
"""accrual.py — pair-supply progress against the pre-registered section-3 population minimums. READ-ONLY.

Answers ONE question, as numbers: how far is the confirmatory campaign from the pre-registered supply bar
(>= 30 fault-class-matched pairs, >= 15 hosts, <= 3 per host) — per host, per fault class, and in total.
"Are we done accruing?" and nothing else.

THIS TOOL NEVER COMPUTES A VERDICT, AN ENDPOINT, OR ANY TEST STATISTIC — and must never grow the ability
to. The campaign's decision rule is frozen inside analyze.py (pre-registered, hash-pinned by
test_analyze.py); a second, UNFROZEN code path that could reach a conclusion would be exactly the gameable
back door the freeze exists to close. test_accrual.py asserts structurally that no such path exists here.
This file is deliberately NOT hash-pinned, so supply reporting can improve mid-campaign without touching
the frozen analysis.

Read-only by construction: it opens the manifests and the scorecard for READING only and writes
nothing anywhere (also asserted by test_accrual.py).

Supply comes from TWO manifests, merged: the LEGACY campaign manifest (out/campaign-manifest.jsonl,
written by the old campaign.sh orchestrator) AND the confirmatory manifest
(confirmatory/manifest.jsonl, appended by reconcile-supply.py, which joins the injector's durable
ground-truth ledger to the nightly harvest's scorecard pair records). Both are read-only here; both
record shapes carry the same counting fields (ts / status / host / fault_type / pred_issues / tg_refs).

Accrual boundary: only manifest records timestamped at/after the pre-registration freeze count toward
supply. The 2026-07-22 TG-84 shakedown records predate the plan (and its section-3a pair definition)
entirely — a pair cannot be accrued under a plan that did not yet exist — so they are listed as EXCLUDED
with that reason, never silently dropped.
"""

from __future__ import annotations

import argparse
import json
import os
import sys
from collections import Counter

# Section-3 minimums, MIRRORED from the frozen analyze.py rather than imported: importing the analysis
# module here would put a conclusion-capable surface one attribute-access away from an unfrozen file.
# test_accrual.py asserts these equal the frozen values, so they cannot drift silently.
# MIN_HOSTS amended 15 -> 12 (PRE-REGISTRATION.md section 6, 2026-07-31): the predecessor covers only 12 of
# the 15 pool hosts, a discovered, outcome-independent infeasibility. Kept equal to analyze.MIN_HOSTS.
MIN_PAIRS = 30
MIN_HOSTS = 12
MAX_PAIRS_PER_HOST = 3

#: Records before this instant predate the pre-registration freeze and can never be confirmatory supply.
PREREG_FREEZE_UTC = "2026-07-27T00:00:00Z"

HERE = os.path.dirname(os.path.abspath(__file__))
DEFAULT_MANIFEST = os.path.join(HERE, "out", "campaign-manifest.jsonl")
DEFAULT_CONFIRMATORY = os.path.join(HERE, "confirmatory", "manifest.jsonl")
DEFAULT_SCORECARD = os.path.join(HERE, "scorecard.jsonl")
#: The operator-declared diagnosis expectations — which injector classes carry a scoreable ground truth.
DEFAULT_EXPECTATIONS = os.path.normpath(os.path.join(HERE, "..", "..", "core", "diagcorpus", "expectations.json"))


def gt_scorable_classes(path: str = DEFAULT_EXPECTATIONS) -> set[str] | None:
    """Injector classes whose faults can produce a PRIMARY-endpoint item: the fault types core/diagcorpus
    scores (an `expectations` entry not marked `unhealable`). Returns None when the file cannot be read —
    the caller must treat that as UNKNOWN and refuse a done-verdict, never as "everything is scorable"
    (§6 2026-08-25: campaign #1 stopped at bar-met with only 18 of 24 counted pairs carrying ground truth,
    because the done-condition had no ground-truth term)."""
    try:
        with open(path, encoding="utf-8") as fh:
            doc = json.load(fh)
        entries = doc.get("expectations")
        if not isinstance(entries, list) or not entries:
            return None
        return {
            (e.get("fault_type") or "").strip()
            for e in entries
            if isinstance(e, dict) and e.get("fault_type") and not e.get("unhealable")
        }
    except (OSError, json.JSONDecodeError):
        return None


def load_jsonl(path: str) -> list[dict] | None:
    """Read a JSONL file. Returns None when the file does not exist — absence is evidence (of zero accrual),
    not an error, and the caller must be able to tell it apart from an empty file."""
    if not os.path.exists(path):
        return None
    rows = []
    with open(path, "r", encoding="utf-8") as fh:
        for ln in fh:
            ln = ln.strip()
            if not ln:
                continue
            try:
                rows.append(json.loads(ln))
            except json.JSONDecodeError:
                continue
    return rows


def merge_manifests(*row_sets: list[dict] | None) -> list[dict] | None:
    """Merge manifest sources into one row list. None everywhere stays None — absence is evidence (of zero
    accrual) and must remain distinguishable from "both present but empty"."""
    if all(rs is None for rs in row_sets):
        return None
    return [r for rs in row_sets if rs for r in rs]


def scorecard_pair_index(scorecard_rows: list[dict] | None) -> dict[tuple[str, str], bool]:
    """Map (predecessor incident key, TG ref) -> judge_unavailable for every judged PAIR in the scorecard.

    The scorecard keys pairs as "DATE|pair|PREDKEY|TGREF"; joining on (PREDKEY, TGREF) tells us which of the
    manifest's injected pairs already carry a judged record.
    """
    idx: dict[tuple[str, str], bool] = {}
    for r in scorecard_rows or []:
        parts = (r.get("key") or "").split("|")
        if len(parts) == 4 and parts[1] == "pair":
            idx[(parts[2], parts[3])] = bool(r.get("judge_unavailable"))
    return idx


def progress(
    manifest_rows: list[dict] | None,
    scorecard_rows: list[dict] | None = None,
    accrue_from: str = PREREG_FREEZE_UTC,
    scorable: set[str] | None = None,
) -> dict:
    """Compute supply progress against the section-3 minimums. Pure accounting — no statistics, no
    conclusion about either system, ever.

    `scorable` is the set of injector classes that can produce a PRIMARY-endpoint (ground-truth) item
    (gt_scorable_classes). The §3 bar is enforced on BOTH populations by the frozen analyze.py, so "done"
    here must also require MIN_PAIRS/MIN_HOSTS of ground-truth-capable supply — campaign #1 stopped at
    bar-met with an unpowered primary because this term was missing (§6 2026-08-25). None = the scorable
    set is UNKNOWN; done is then refused (a bar that cannot see its subject must not certify)."""
    rows = manifest_rows or []
    eligible = [r for r in rows if (r.get("ts") or "") >= accrue_from]
    excluded_prefreeze = [r for r in rows if (r.get("ts") or "") < accrue_from]
    paired = [r for r in eligible if r.get("status") == "PAIRED"]

    per_host_raw: Counter = Counter((r.get("host") or "?") for r in paired)
    per_class: Counter = Counter((r.get("fault_type") or "?") for r in paired)
    per_host = {
        h: {
            "paired_raw": c,
            "counted": min(c, MAX_PAIRS_PER_HOST),
            "over_cap": max(0, c - MAX_PAIRS_PER_HOST),
            "headroom": max(0, MAX_PAIRS_PER_HOST - c),
        }
        for h, c in sorted(per_host_raw.items())
    }
    supply = sum(v["counted"] for v in per_host.values())
    hosts = len(per_host)

    # Ground-truth-capable supply (the primary endpoint's population). Counted per host under the same cap;
    # a class-restricted rotation (only scorable classes injected) makes this exact — with mixed classes it
    # is an upper bound, because analyze.py's cap keeps the earliest 3 regardless of ground-truth carriage.
    gt_unknown_class = sum(1 for r in paired if not (r.get("injector_class") or "").strip())
    if scorable is not None:
        gt_per_host: Counter = Counter(
            (r.get("host") or "?")
            for r in paired
            if (r.get("injector_class") or "").strip() in scorable
        )
        gt_supply = sum(min(c, MAX_PAIRS_PER_HOST) for c in gt_per_host.values())
        gt_hosts = len(gt_per_host)
    else:
        gt_supply = gt_hosts = 0

    # Which accrued pairs already have a judged record (the scorecard is the judged-verdict ledger).
    idx = scorecard_pair_index(scorecard_rows)
    judged = unavailable = 0
    for r in paired:
        hit = None
        for pk in r.get("pred_issues") or []:
            for tr in r.get("tg_refs") or []:
                if (pk, tr) in idx:
                    hit = idx[(pk, tr)]
                    break
            if hit is not None:
                break
        if hit is not None:
            judged += 1
            if hit:
                unavailable += 1

    misses_by_status: Counter = Counter(
        (r.get("status") or "?") for r in eligible if r.get("status") != "PAIRED"
    )

    return {
        "accrue_from": accrue_from,
        "manifest_records": len(rows),
        "excluded_prefreeze": len(excluded_prefreeze),
        "eligible_records": len(eligible),
        "paired_raw": len(paired),
        "supply_after_cap": supply,
        "hosts": hosts,
        "per_host": per_host,
        "per_class": dict(sorted(per_class.items())),
        "non_paired_by_status": dict(sorted(misses_by_status.items())),
        "judged_pairs": judged,
        "judged_unavailable": unavailable,
        "pairs_needed": max(0, MIN_PAIRS - supply),
        "hosts_needed": max(0, MIN_HOSTS - hosts),
        "min_pairs": MIN_PAIRS,
        "min_hosts": MIN_HOSTS,
        "max_pairs_per_host": MAX_PAIRS_PER_HOST,
        "scorable_known": scorable is not None,
        "scorable_classes": sorted(scorable) if scorable is not None else None,
        "gt_supply": gt_supply,
        "gt_hosts": gt_hosts,
        "gt_pairs_needed": max(0, MIN_PAIRS - gt_supply),
        "gt_hosts_needed": max(0, MIN_HOSTS - gt_hosts),
        "gt_unknown_class": gt_unknown_class,
        "done": (
            scorable is not None
            and supply >= MIN_PAIRS
            and hosts >= MIN_HOSTS
            and gt_supply >= MIN_PAIRS
            and gt_hosts >= MIN_HOSTS
        ),
    }


def render(prog: dict, manifest_path: str, manifest_present: bool,
           scorecard_path: str, scorecard_present: bool,
           confirmatory_path: str | None = None, confirmatory_present: bool = False) -> str:
    out = ["CONFIRMATORY PAIR ACCRUAL — supply vs the pre-registered section-3 minimums (read-only)"]
    out.append(f"  manifest:  {manifest_path}" + ("" if manifest_present else "  (ABSENT — zero records)"))
    if confirmatory_path is not None:
        out.append(f"  confirmatory manifest: {confirmatory_path}"
                   + ("" if confirmatory_present else "  (ABSENT — zero records)"))
    out.append(f"  scorecard: {scorecard_path}" + ("" if scorecard_present else "  (ABSENT)"))
    out.append(f"  accrual counts records at/after the pre-registration freeze ({prog['accrue_from']})")
    if prog["excluded_prefreeze"]:
        out.append(
            f"    - EXCLUDED {prog['excluded_prefreeze']} pre-freeze record(s): they predate the plan and "
            "its section-3a pair definition, so they can never be confirmatory supply"
        )
    out.append(
        f"  supply: {prog['supply_after_cap']} / {prog['min_pairs']} pairs "
        f"(raw PAIRED={prog['paired_raw']}, counted <= {prog['max_pairs_per_host']}/host)   "
        f"hosts: {prog['hosts']} / {prog['min_hosts']}"
    )
    if prog.get("scorable_known"):
        out.append(
            f"  ground-truth-capable supply (classes {', '.join(prog['scorable_classes'] or []) or '—'}): "
            f"{prog['gt_supply']} / {prog['min_pairs']} pairs   gt hosts: {prog['gt_hosts']} / {prog['min_hosts']}"
            + (f"   ({prog['gt_unknown_class']} paired record(s) carry no injector_class — counted as NOT "
               "gt-capable)" if prog.get("gt_unknown_class") else "")
        )
        out.append(
            "    (exact when the rotation injects only scorable classes; with mixed classes this is an upper "
            "bound — analyze.py's per-host cap keeps the earliest 3 regardless of ground-truth carriage)"
        )
    else:
        out.append(
            "  ground-truth-capable supply: UNKNOWN — core/diagcorpus/expectations.json unreadable; the bar "
            "cannot certify (§6 2026-08-25)"
        )
    if prog["done"]:
        done = "YES — the section-3 supply bar is met on BOTH populations (judged and ground-truth)"
    elif not prog.get("scorable_known"):
        done = "NO — the ground-truth-capable term is UNKNOWN (expectations unreadable); refusing to certify"
    else:
        done = (
            f"NO — {prog['pairs_needed']} more counted pair(s), {prog['hosts_needed']} more host(s), "
            f"{prog['gt_pairs_needed']} more ground-truth pair(s), {prog['gt_hosts_needed']} more "
            "ground-truth host(s) needed"
        )
    out.append(f"  ARE WE DONE ACCRUING: {done}")
    if prog["per_host"]:
        out.append("  per host (counted <= cap; over-cap pairs accrue nothing):")
        for h, v in prog["per_host"].items():
            out.append(
                f"    - {h:24s} paired={v['paired_raw']:3d}  counted={v['counted']}  "
                f"headroom={v['headroom']}" + (f"  over-cap={v['over_cap']}" if v["over_cap"] else "")
            )
    else:
        out.append("  per host: (no accrued pairs yet)")
    if prog["per_class"]:
        out.append("  per fault class (section-3a: a pair is fault-class-matched by construction):")
        for c, n in prog["per_class"].items():
            out.append(f"    - {c:10s} {n}")
    else:
        out.append("  per fault class: (no accrued pairs yet)")
    if prog["non_paired_by_status"]:
        out.append(
            "  non-paired eligible records (counted and shown, per the no-silent-exclusion rule): "
            + ", ".join(f"{k}={v}" for k, v in prog["non_paired_by_status"].items())
        )
    out.append(
        f"  judged coverage of the accrued supply: {prog['judged_pairs']} pair(s) carry a scorecard record"
        + (f" ({prog['judged_unavailable']} judge_unavailable)" if prog["judged_unavailable"] else "")
    )
    out.append(
        "  NOTE: supply is not a result. The conclusion comes ONLY from the frozen analyze.py once the bar "
        "is met; this tool cannot and will not compute one."
    )
    return "\n".join(out)


def main() -> int:
    ap = argparse.ArgumentParser(
        description="Pair-supply progress against the pre-registered section-3 minimums (read-only)."
    )
    ap.add_argument("--manifest", default=DEFAULT_MANIFEST, help="legacy campaign manifest JSONL")
    ap.add_argument("--confirmatory", default=DEFAULT_CONFIRMATORY,
                    help="confirmatory manifest JSONL (appended by reconcile-supply.py)")
    ap.add_argument("--scorecard", default=DEFAULT_SCORECARD, help="judged-verdict scorecard JSONL")
    ap.add_argument("--accrue-from", default=PREREG_FREEZE_UTC,
                    help="ISO-8601 UTC boundary; records before it never count (default: the freeze)")
    ap.add_argument("--expectations", default=DEFAULT_EXPECTATIONS,
                    help="core/diagcorpus expectations JSON — declares which injector classes carry a "
                         "scoreable ground truth (unreadable → the done-verdict is refused)")
    ap.add_argument("--json", action="store_true", help="emit the raw progress record as JSON")
    args = ap.parse_args()

    legacy_rows = load_jsonl(args.manifest)
    confirmatory_rows = load_jsonl(args.confirmatory)
    manifest_rows = merge_manifests(legacy_rows, confirmatory_rows)
    scorecard_rows = load_jsonl(args.scorecard)
    prog = progress(manifest_rows, scorecard_rows, accrue_from=args.accrue_from,
                    scorable=gt_scorable_classes(args.expectations))
    if args.json:
        print(json.dumps(prog, indent=2))
    else:
        print(render(prog, args.manifest, legacy_rows is not None,
                     args.scorecard, scorecard_rows is not None,
                     confirmatory_path=args.confirmatory,
                     confirmatory_present=confirmatory_rows is not None))
    return 0


if __name__ == "__main__":
    sys.exit(main())
