#!/usr/bin/env python3
"""reconcile-supply.py — joins the fault injector's durable ledger to harvested scorecard PAIR records,
appending confirmatory PAIRED supply to confirmatory/manifest.jsonl. SUPPLY plumbing only — no statistics,
no conclusion, ever (the decision rule is frozen inside analyze.py; this file must stay as conclusion-free
as accrual.py, and test_reconcile_supply.py asserts that structurally).

THE GAP THIS CLOSES. The continuous path (the injector engine soaks 24/7 → both systems triage → the
nightly run.sh judges) produces scorecard pair records, but nothing joined them to the injector's ground
truth: only the OLD campaign.sh orchestrator ever wrote campaign-manifest PAIRED entries, so accrual.py
reported 0/30 forever while the estate looked busy. The §3a pair definition (PRE-REGISTRATION.md) requires
fault-class-matched pairs WITH injector ground truth; this tool performs that join and records the result.

THE JOIN RULE, exactly:
  a post-freeze injected fault (injected_at >= the §6 freeze) MATCHES a scorecard PAIR record iff
    1. HOST      — the fault's host and the record's subject host match (same loose containment rule the
                   aligner itself uses: _driver._host_match);
    2. CLASS     — the record's §3a coarse fault class equals the class the injector's fault type provokes
                   (see the mapping note below);
    3. WINDOW    — the record's incident time (the TG triage row's created_at, recorded by _driver.py at
                   harvest) falls inside [injected_at, restore_end + slack], where restore_end is
                   restored_at, else restore_due_at, else injected_at, and slack (default 15 min) absorbs
                   detection + triage latency;
  among qualifying candidates the NEAREST in time to injected_at wins, and one scorecard record serves at
  most one fault (§3a: one TG session serves at most one pairing).

THE CLASS MAPPING IS DECLARED ELSEWHERE, NOT INVENTED HERE. Injector fault vocabulary → monitoring rule
family is declared in core/db/axis_read.go (detectRuleMatch — the same mapping the A1 detection scorer
joins on); rule family → §3a coarse class is declared in _driver._CLASS_PATTERNS (fault_class — the same
classifier that formed the pair in the first place). _INJECTOR_RULE_FAMILY below quotes axis_read.go's
families verbatim and pushes them through _driver.fault_class, so this file composes the two existing
declarations and declares nothing new. test_reconcile_supply.py pins the composition per class.

DISCIPLINE (the §5/§6 posture):
  * READ-ONLY INPUTS: the ledger is fetched with a SELECT (over SSH, extract_tg.sh's conventions); the
    scorecard and existing manifest are only read.
  * APPEND-ONLY OUTPUT: confirmatory/manifest.jsonl only ever grows. IDEMPOTENT: a (fault id, scorecard
    key) pair already present is never re-appended, and neither side of it is ever re-matched elsewhere.
  * NO SILENT DROPS: every unmatched post-freeze fault and every unmatched pair record is PRINTED as a
    one-line exclusion with its reason.

USAGE
  ./reconcile-supply.py                       # fetch the ledger from the box, reconcile, append
  ./reconcile-supply.py --ledger rows.json    # offline: ledger from a local JSON array (tests use this)
  ./reconcile-supply.py --dry-run             # full report, appends nothing

The nightly harvest flow is: run.sh → reconcile-supply.py → accrual.py (see README "Cron recipe").
"""

from __future__ import annotations

import argparse
import datetime as _dt
import json
import os
import re
import subprocess
import sys
import tempfile

HERE = os.path.dirname(os.path.abspath(__file__))
if HERE not in sys.path:
    sys.path.insert(0, HERE)

# _driver reads its run.sh environment at import; give it the same harmless defaults test_align.py does.
os.environ.setdefault("WORK", tempfile.gettempdir())
os.environ.setdefault("SB_DIR", HERE)
os.environ.setdefault("SCORECARD", os.path.join(tempfile.gettempdir(), "_reconcile_unused_scorecard.jsonl"))
import _driver  # noqa: E402  (fault_class + _host_match + _parse_ts — the aligner's own primitives)
from accrual import PREREG_FREEZE_UTC, load_jsonl  # noqa: E402  (ONE freeze constant, one JSONL reader)

DEFAULT_SCORECARD = os.path.join(HERE, "scorecard.jsonl")
DEFAULT_MANIFEST = os.path.join(HERE, "confirmatory", "manifest.jsonl")
DEFAULT_SLACK_MINUTES = 15.0

# extract_tg.sh's env/ssh conventions, reused verbatim.
TG_HOST = os.environ.get("TG_HOST", "dc1tg01")
SSH_KEY = os.environ.get("SSH_KEY", os.path.expanduser("~/.ssh/one_key"))
SSH_USER = os.environ.get("SSH_USER", "root")
ENV_PATH = os.environ.get("ENV_PATH", "/srv/tg/deploy/.env")
PG_CONTAINER = os.environ.get("PG_CONTAINER", "territory-grounder-postgres-1")

#: Injector fault class -> the monitoring rule family it provokes, QUOTED from core/db/axis_read.go
#: (detectRuleMatch). Each family string is then classified by _driver.fault_class, so the coarse §3a class
#: comes from the aligner's own classifier — the exact one that classified the pair record's alert rule.
_INJECTOR_RULE_FAMILY = {
    "device-down": "Device Down / ICMP / SNMP",
    "disk-fill": "Space / disk",
    "log-fill": "Space / disk",  # axis_read.go: presents to the monitoring system exactly as disk-fill does
    "mem-pressure": "Memory / mem",
    "service-down": "Service / nginx",
    "container-down": "Service / http",  # axis_read.go: a dead container fires the Service up/down rule
}


def injector_coarse_class(fault_type: str) -> str:
    """Injector fault_type -> §3a coarse class ('' when the class has no declared rule family).

    '' must EXCLUDE the fault loudly, never join it loosely — same posture as fault_class itself.
    """
    family = _INJECTOR_RULE_FAMILY.get((fault_type or "").strip())
    return _driver.fault_class(family) if family else ""


def scorecard_pairs(scorecard_rows: list[dict] | None) -> list[dict]:
    """Extract the joinable view of every PAIR record in the scorecard.

    The incident time and fault class are the supply-join fields _driver.py records at harvest time
    (tg_created_at / pred_first_ts / fault_class). Records harvested before those fields existed carry
    None/'' here and are excluded downstream WITH a printed reason, never silently.
    """
    out = []
    for r in scorecard_rows or []:
        parts = (r.get("key") or "").split("|")
        if len(parts) != 4 or parts[1] != "pair":
            continue
        out.append({
            "key": r["key"],
            "pred_key": parts[2],
            "tg_ref": parts[3],
            "subject_host": r.get("subject_host") or "",
            "fault_class": r.get("fault_class") or "",
            "incident_ts": _driver._parse_ts(r.get("tg_created_at") or r.get("pred_first_ts")),
            "judge_unavailable": bool(r.get("judge_unavailable")),
        })
    return out


def manifest_index(manifest_rows: list[dict] | None) -> tuple[set, set, set]:
    """(fault ids, scorecard keys, (fault id, scorecard key) pairs) already reconciled — the idempotency set."""
    fault_ids, keys, pairs = set(), set(), set()
    for r in manifest_rows or []:
        fid = r.get("fault_id")
        if fid is not None:
            fault_ids.add(fid)
        for k in r.get("scorecard_keys") or []:
            keys.add(k)
            if fid is not None:
                pairs.add((fid, k))
    return fault_ids, keys, pairs


def _iso_z(ts: _dt.datetime) -> str:
    return ts.astimezone(_dt.timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ")


def reconcile(
    ledger_rows: list[dict] | None,
    scorecard_rows: list[dict] | None,
    manifest_rows: list[dict] | None,
    accrue_from: str = PREREG_FREEZE_UTC,
    slack_minutes: float = DEFAULT_SLACK_MINUTES,
    now: _dt.datetime | None = None,
) -> tuple[list[dict], list[str], dict]:
    """The pure join. Returns (new manifest records, one-line exclusions, counters). Touches no file."""
    from_ts = _driver._parse_ts(accrue_from)
    slack = _dt.timedelta(minutes=slack_minutes)
    now = now or _dt.datetime.now(_dt.timezone.utc)

    pairs = scorecard_pairs(scorecard_rows)
    done_faults, done_keys, done_pairs = manifest_index(manifest_rows)
    used_keys = set(done_keys)

    new_records: list[dict] = []
    exclusions: list[str] = []
    already = 0

    def fault_label(f, inj):
        when = _iso_z(inj) if inj else repr(f.get("injected_at"))
        return f"fault id={f.get('id')} {f.get('fault_type')} on {f.get('host')} @ {when}"

    ledger = sorted(ledger_rows or [], key=lambda f: (str(f.get("injected_at") or ""), str(f.get("id"))))
    for f in ledger:
        inj = _driver._parse_ts(f.get("injected_at"))
        ftype = (f.get("fault_type") or "").strip()
        if ftype == "__killswitch__":
            exclusions.append(f"EXCLUDED {fault_label(f, inj)}: kill-switch control row, not an injected fault")
            continue
        if inj is None:
            exclusions.append(f"EXCLUDED {fault_label(f, inj)}: unparseable injected_at")
            continue
        if from_ts and inj < from_ts:
            exclusions.append(
                f"EXCLUDED {fault_label(f, inj)}: pre-freeze — a pair cannot be accrued under a plan that "
                f"did not yet exist (freeze {accrue_from})"
            )
            continue
        coarse = injector_coarse_class(ftype)
        if not coarse:
            exclusions.append(
                f"EXCLUDED {fault_label(f, inj)}: class {ftype!r} has no declared alert-rule family "
                "(core/db/axis_read.go detectRuleMatch) — cannot class-match, and a guessed class is worse "
                "than a missing pair"
            )
            continue
        if f.get("id") in done_faults:
            already += 1
            continue

        restored_at = _driver._parse_ts(f.get("restored_at"))
        restore_end = restored_at or _driver._parse_ts(f.get("restore_due_at")) or inj
        window_end = restore_end + slack

        # The reason funnel: count survivors of each clause so the exclusion line says WHICH clause failed.
        host_ok = [p for p in pairs if _driver._host_match(f.get("host"), p["subject_host"])]
        class_ok = [p for p in host_ok if p["fault_class"] == coarse]
        ts_ok = [p for p in class_ok if p["incident_ts"] is not None]
        win_ok = [p for p in ts_ok if inj <= p["incident_ts"] <= window_end]
        free = [p for p in win_ok if p["key"] not in used_keys and (f.get("id"), p["key"]) not in done_pairs]

        if not free:
            exclusions.append(
                f"EXCLUDED {fault_label(f, inj)}: no scorecard pair record joins "
                f"(host-matched={len(host_ok)}, class[{coarse}]-matched={len(class_ok)}, "
                f"with-incident-time={len(ts_ok)}, in-window(<={_iso_z(window_end)})={len(win_ok)}, "
                f"unclaimed={len(free)})"
            )
            continue

        best = min(free, key=lambda p: (abs(p["incident_ts"] - inj), p["key"]))
        used_keys.add(best["key"])
        new_records.append({
            # ts = the FAULT time: accrual's freeze boundary must apply to when the fault happened,
            # not to when this tool ran.
            "ts": _iso_z(inj),
            "reconciled_at": _iso_z(now),
            "source": "reconcile-supply",
            "status": "PAIRED",
            "fault_id": f.get("id"),
            "injector_class": ftype,
            "fault_type": coarse,  # the §3a coarse class — the field accrual.py counts per class
            "host": f.get("host"),
            "injected_at": _iso_z(inj),
            "restored_at": _iso_z(restored_at) if restored_at else None,
            "restore_state": f.get("restore_state"),
            "subject_host": best["subject_host"],
            "incident_ts": _iso_z(best["incident_ts"]),
            "tg_refs": [best["tg_ref"]],
            "pred_issues": [best["pred_key"]],
            "scorecard_keys": [best["key"]],
            "judge_unavailable": best["judge_unavailable"],
        })

    # The other direction: every pair record that joined nothing, with its reason. The organic stream is the
    # §5 contamination-control arm — outside the primary endpoint BY PLAN, but stated, never silently dropped.
    matched_keys = {rec["scorecard_keys"][0] for rec in new_records}
    for p in pairs:
        if p["key"] in matched_keys or p["key"] in done_keys:
            continue
        if not p["fault_class"] and p["incident_ts"] is None:
            reason = ("carries no fault_class/incident-time supply fields (harvested before _driver.py "
                      "recorded them) — cannot join ground truth")
        elif not p["fault_class"]:
            reason = "carries no fault_class supply field — cannot class-match"
        elif p["incident_ts"] is None:
            reason = "carries no incident timestamp — cannot window-match"
        elif from_ts and p["incident_ts"] < from_ts:
            reason = f"pre-freeze incident (before {accrue_from})"
        else:
            reason = ("no post-freeze injected fault matches — organic incident "
                      "(§5 contamination-control arm; carries no injector ground truth)")
        exclusions.append(f"EXCLUDED pair record {p['key']}: {reason}")

    counters = {
        "ledger_rows": len(ledger),
        "pair_records": len(pairs),
        "matched": len(new_records),
        "already_reconciled": already,
        "exclusions": len(exclusions),
    }
    return new_records, exclusions, counters


# ---------------------------------------------------------------------------
# Ledger fetch — extract_tg.sh's file + scp + docker cp + psql -f path, verbatim conventions.
# ---------------------------------------------------------------------------

_TS_LITERAL_RX = re.compile(r"^[0-9][0-9T:+\-. Z]*$")


def _redact(text: str) -> str:
    return re.sub(r"(?i)(password|PGPASSWORD)=\S*", r"\1=<redacted>", text or "")


def fetch_ledger(accrue_from: str = PREREG_FREEZE_UTC) -> list[dict]:
    """SELECT the post-freeze injected_fault rows from the box (read-only), as a list of dicts.

    Same shape as extract_tg.sh: the SQL goes over as a FILE (scp + docker cp + psql -f) so the query text
    is never mangled by three shells; PGPASSWORD is resolved from the on-box .env INSIDE the remote shell
    and never touches this host's argv, env, or logs. accrue_from is operator-supplied, validated to a bare
    timestamp literal before interpolation (extract_tg.sh's SHADOW_FROM rule).
    """
    if not _TS_LITERAL_RX.match(accrue_from):
        raise SystemExit(f"--accrue-from must be a bare timestamp literal, got {accrue_from!r}")
    sql = (
        "SELECT COALESCE(json_agg(row_to_json(x)), '[]'::json) FROM (\n"
        "  SELECT id, host, fault_type, injected_at, restored_at, restore_due_at, restore_state\n"
        "    FROM injected_fault\n"
        f"   WHERE injected_at >= TIMESTAMPTZ '{accrue_from}'\n"
        "   ORDER BY injected_at, id\n"
        ") x;\n"
    )
    with tempfile.NamedTemporaryFile("w", suffix=".sql", delete=False) as fh:
        fh.write(sql)
        tmp = fh.name
    try:
        ssh_opts = ["-i", SSH_KEY, "-o", "StrictHostKeyChecking=no", "-o", "ConnectTimeout=15",
                    "-o", "BatchMode=yes"]
        subprocess.run(["scp", *ssh_opts, tmp, f"{SSH_USER}@{TG_HOST}:/tmp/reconcile_supply.sql"],
                       check=True, capture_output=True)
        remote = (
            "docker cp /tmp/reconcile_supply.sql " + PG_CONTAINER + ":/tmp/reconcile_supply.sql >/dev/null\n"
            "docker exec "
            "-e PGPASSWORD=\"$(grep -E \"^PG_SUPERUSER_PASSWORD=\" " + ENV_PATH + " | cut -d= -f2-)\" "
            + PG_CONTAINER + " psql -U postgres -d grounder -tAq -f /tmp/reconcile_supply.sql\n"
            "rm -f /tmp/reconcile_supply.sql\n"
        )
        proc = subprocess.run(["ssh", *ssh_opts, f"{SSH_USER}@{TG_HOST}", remote],
                              capture_output=True, text=True)
        if proc.returncode != 0:
            raise SystemExit(f"ledger fetch failed (ssh exit {proc.returncode}): "
                             f"{_redact(proc.stderr.strip())[:300]}")
        body = proc.stdout.strip()
        rows = json.loads(body) if body else []
        if not isinstance(rows, list):
            raise SystemExit("ledger fetch returned non-array JSON")
        return rows
    finally:
        os.unlink(tmp)


# ---------------------------------------------------------------------------
# CLI
# ---------------------------------------------------------------------------

def main(argv=None) -> int:
    ap = argparse.ArgumentParser(
        description="Join the injector ledger to scorecard pair records; append confirmatory PAIRED supply."
    )
    ap.add_argument("--ledger", help="local JSON array of injected_fault rows (skips the SSH fetch; tests)")
    ap.add_argument("--scorecard", default=DEFAULT_SCORECARD, help="judged-verdict scorecard JSONL")
    ap.add_argument("--manifest", default=DEFAULT_MANIFEST,
                    help="append-only confirmatory manifest JSONL (the output)")
    ap.add_argument("--accrue-from", default=PREREG_FREEZE_UTC,
                    help="ISO-8601 UTC boundary; faults before it never count (default: the freeze)")
    ap.add_argument("--slack-minutes", type=float, default=DEFAULT_SLACK_MINUTES,
                    help="detection-latency slack appended to each fault's restore end (default 15)")
    ap.add_argument("--dry-run", action="store_true", help="report only; append nothing")
    args = ap.parse_args(argv)

    if args.ledger:
        with open(args.ledger, "r", encoding="utf-8") as fh:
            ledger_rows = json.load(fh)
    else:
        ledger_rows = fetch_ledger(args.accrue_from)

    scorecard_rows = load_jsonl(args.scorecard)
    manifest_rows = load_jsonl(args.manifest)

    new_records, exclusions, counters = reconcile(
        ledger_rows, scorecard_rows, manifest_rows,
        accrue_from=args.accrue_from, slack_minutes=args.slack_minutes,
    )

    print("CONFIRMATORY SUPPLY RECONCILE — injector ledger × scorecard pair records (append-only output)")
    print(f"  ledger faults (post-freeze fetch): {counters['ledger_rows']}   "
          f"scorecard pair records: {counters['pair_records']}")
    print(f"  matched now: {counters['matched']}   already in manifest (idempotent no-op): "
          f"{counters['already_reconciled']}")
    for line in exclusions:
        print("  " + line)
    for rec in new_records:
        print(f"  PAIRED fault id={rec['fault_id']} {rec['injector_class']} [{rec['fault_type']}] on "
              f"{rec['host']} @ {rec['injected_at']} -> {rec['scorecard_keys'][0]}")

    if args.dry_run:
        print(f"  dry-run: {len(new_records)} record(s) NOT appended to {args.manifest}")
        return 0
    if new_records:
        os.makedirs(os.path.dirname(args.manifest) or ".", exist_ok=True)
        with open(args.manifest, "a", encoding="utf-8") as fh:
            for rec in new_records:
                fh.write(json.dumps(rec, ensure_ascii=False) + "\n")
    print(f"  appended {len(new_records)} record(s) to {args.manifest}")
    print("  NOTE: supply is not a result — accrual.py reports progress; the conclusion comes ONLY from the "
          "frozen analyze.py once the §3 bar is met.")
    return 0


if __name__ == "__main__":
    sys.exit(main())
