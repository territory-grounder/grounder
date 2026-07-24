#!/usr/bin/env python3
"""
_driver.py — run.sh's alignment + judge-dispatch + aggregation core (shadowbench v2).

Not meant to be run directly; run.sh sets the environment and invokes it. Kept as a
real module (not a shell heredoc) so the alignment/aggregation is testable and never
subjected to shell interpolation.

Reads (env, set by run.sh):
  WORK        temp dir holding pred.json (extract_predecessor --json) + tg.json (extract_tg jsononly)
  SB_DIR      the shadowbench dir (locates judge.py)
  SCORECARD   append-only verdict ledger (one JSON line per judged incident)
  DATE        the predecessor date being processed
  ALIGN_HOURS loose-time alignment half-window (hours)
  DRY_JUDGE   "1" → pass --dry-run to judge.py and do NOT append (offline check)
  MODEL, SSH_KEY, TG_HOST, SSH_USER, ENV_PATH  → forwarded to judge.py

Output: appends verdicts to SCORECARD (dedup by date+incident) and prints a rolling aggregate.
"""
from __future__ import annotations

import datetime as _dt
import json
import os
import re
import subprocess
import sys

# The five scored dimensions are the ONE source (core/judge/rubric.json via judge.py) — imported, never
# re-declared here, so the aggregator and the judge can never drift onto different axes.
_HERE = os.path.dirname(os.path.abspath(__file__))
if _HERE not in sys.path:
    sys.path.insert(0, _HERE)
from judge import DIMENSIONS  # noqa: E402 (sys.path must be set first)

_HOST_RX = re.compile(r"\b([a-z]{2}[a-z0-9]{2,}\d{2}[a-z0-9]+\d{2})\b")

WORK = os.environ["WORK"]
SB_DIR = os.environ["SB_DIR"]
SCORECARD = os.environ["SCORECARD"]
DATE = os.environ.get("DATE", "")
ALIGN_HOURS = float(os.environ.get("ALIGN_HOURS", "12"))
DRY = os.environ.get("DRY_JUDGE", "0") == "1"


def _load(name, default):
    try:
        with open(os.path.join(WORK, name), "r", encoding="utf-8") as fh:
            return json.load(fh)
    except Exception:
        return default


def _parse_ts(s):
    if not s:
        return None
    s = str(s).strip().replace("Z", "+00:00").replace(" ", "T", 1)
    try:
        dt = _dt.datetime.fromisoformat(s)
        if dt.tzinfo is None:
            dt = dt.replace(tzinfo=_dt.timezone.utc)
        return dt
    except ValueError:
        return None


def pred_subject_host(inc):
    m = _HOST_RX.search(inc.get("issue") or "")
    if m:
        return m.group(1)
    return inc.get("host") or ""


def is_real_pred(inc):
    """Keep real dispatched agentic triage; drop the degenerate unattributed shadow-gate rows."""
    key = (inc.get("incidentKey") or "").strip()
    if key in ("", "T"):
        return False
    if not inc.get("agentic"):
        return False
    return bool(inc.get("issue") or inc.get("reasoningExcerpt"))


def _host_match(a, b):
    a, b = (a or "").lower(), (b or "").lower()
    if not a or not b:
        return False
    return a == b or a in b or b in a


def align(pred_incs, tg_rows):
    """Return (pairs, pred_only, tg_only). Loose: subject-host match + |Δt| ≤ ALIGN_HOURS."""
    used = set()
    pairs, pred_only = [], []
    for inc in pred_incs:
        sub = pred_subject_host(inc)
        t_p = _parse_ts(inc.get("firstTs"))
        best = None
        for i, row in enumerate(tg_rows):
            if i in used:
                continue
            if not _host_match(sub, row.get("host")):
                continue
            t_t = _parse_ts(row.get("createdAt"))
            if t_p and t_t:
                if abs((t_p - t_t).total_seconds()) > ALIGN_HOURS * 3600:
                    continue
            best = i
            break
        if best is not None:
            used.add(best)
            pairs.append((inc, tg_rows[best]))
        else:
            pred_only.append(inc)
    tg_only = [row for i, row in enumerate(tg_rows) if i not in used]
    return pairs, pred_only, tg_only


def load_keys():
    keys = set()
    if os.path.exists(SCORECARD):
        with open(SCORECARD, "r", encoding="utf-8") as fh:
            for ln in fh:
                ln = ln.strip()
                if not ln:
                    continue
                try:
                    keys.add(json.loads(ln).get("key"))
                except json.JSONDecodeError:
                    continue
    return keys


def run_judge(pred_rec, tg_rec, incident_key):
    argv = [sys.executable, os.path.join(SB_DIR, "judge.py"), "--incident-key", incident_key,
            "--model", os.environ.get("MODEL", "primary"),
            "--ssh-key", os.environ.get("SSH_KEY", os.path.expanduser("~/.ssh/one_key")),
            "--tg-host", os.environ.get("TG_HOST", "dc1tg01"),
            "--ssh-user", os.environ.get("SSH_USER", "root"),
            "--env-path", os.environ.get("ENV_PATH", "/srv/tg/deploy/.env")]
    if DRY:
        argv.append("--dry-run")
    if pred_rec is not None:
        p = os.path.join(WORK, "j_pred.json")
        json.dump(pred_rec, open(p, "w"))
        argv += ["--pred", p]
    if tg_rec is not None:
        p = os.path.join(WORK, "j_tg.json")
        json.dump(tg_rec, open(p, "w"))
        argv += ["--tg", p]
    proc = subprocess.run(argv, capture_output=True, text=True)
    if proc.returncode != 0:
        return {"incident_key": incident_key, "judge_unavailable": True,
                "error": f"judge.py exit {proc.returncode}: {proc.stderr.strip()[:300]}"}
    try:
        return json.loads(proc.stdout)
    except json.JSONDecodeError:
        return {"incident_key": incident_key, "judge_unavailable": True,
                "error": "judge.py produced non-JSON stdout"}


def append_verdict(verdict, key, kind):
    verdict["key"] = key
    verdict["date"] = DATE
    verdict["kind"] = kind
    with open(SCORECARD, "a", encoding="utf-8") as fh:
        fh.write(json.dumps(verdict, ensure_ascii=False) + "\n")


def side_scores(verdict, system):
    """Extract {dim: int|None} for one system ('pred'/'tg') from a verdict via its A/B mapping."""
    mapping = verdict.get("mapping") or {}
    letter = next((L for L, sysn in mapping.items() if sysn == system), None)
    if not letter:
        return None
    dims = (verdict.get("dims") or {}).get(letter)
    if not isinstance(dims, dict):
        return None
    return {d: dims.get(d) for d in DIMENSIONS}


def aggregate():
    """Rolling aggregate over the WHOLE scorecard (all dates)."""
    rows = []
    if os.path.exists(SCORECARD):
        with open(SCORECARD, "r", encoding="utf-8") as fh:
            for ln in fh:
                ln = ln.strip()
                if not ln:
                    continue
                try:
                    rows.append(json.loads(ln))
                except json.JSONDecodeError:
                    continue

    sums = {"pred": {d: [] for d in DIMENSIONS}, "tg": {d: [] for d in DIMENSIONS}}
    cov = {"pair": 0, "pred_only": 0, "tg_only": 0}
    unavailable = 0
    wins = {"pred": 0, "tg": 0, "tie": 0}
    scored_incidents = {"pred": set(), "tg": set()}

    for v in rows:
        cov[v.get("kind", "")] = cov.get(v.get("kind", ""), 0) + 1
        if v.get("judge_unavailable"):
            unavailable += 1
            continue
        w = v.get("winner")
        if w in wins:
            wins[w] += 1
        for system in ("pred", "tg"):
            sc = side_scores(v, system)
            if not sc:
                continue
            any_score = False
            for d in DIMENSIONS:
                val = sc.get(d)
                if isinstance(val, (int, float)):
                    sums[system][d].append(val)
                    any_score = True
            if any_score:
                scored_incidents[system].add(v.get("key"))

    def mean(lst):
        return round(sum(lst) / len(lst), 3) if lst else None

    print("\n" + "=" * 74)
    print("ROLLING AGGREGATE  (blind unified rubric — TG's 5 eval dims, scored 1..5)")
    print("=" * 74)
    print(f"scorecard: {SCORECARD}   verdicts: {len(rows)}   judge_unavailable: {unavailable}")
    print(f"coverage:  aligned_pairs={cov.get('pair',0)}  "
          f"pred_only={cov.get('pred_only',0)}  tg_only={cov.get('tg_only',0)}")
    print(f"incidents scored:  predecessor={len(scored_incidents['pred'])}  TG={len(scored_incidents['tg'])}")
    print(f"head-to-head wins (aligned pairs only):  pred={wins['pred']}  tg={wins['tg']}  tie={wins['tie']}")
    print()
    hdr = f"{'dimension':<24}{'pred mean (n)':>18}{'TG mean (n)':>18}"
    print(hdr)
    print("-" * len(hdr))
    for d in DIMENSIONS:
        pm, tm = mean(sums["pred"][d]), mean(sums["tg"][d])
        pcell = f"{pm} ({len(sums['pred'][d])})" if pm is not None else f"— (0)"
        tcell = f"{tm} ({len(sums['tg'][d])})" if tm is not None else f"— (0)"
        print(f"{d:<24}{pcell:>18}{tcell:>18}")
    # overall = mean across applicable dims (falsifiable_prediction naturally excluded where N/A)
    all_p = [x for d in DIMENSIONS for x in sums["pred"][d]]
    all_t = [x for d in DIMENSIONS for x in sums["tg"][d]]
    print("-" * len(hdr))
    op = f"{mean(all_p)} ({len(all_p)})" if all_p else "— (0)"
    ot = f"{mean(all_t)} ({len(all_t)})" if all_t else "— (0)"
    print(f"{'OVERALL (all dims)':<24}{op:>18}{ot:>18}")
    print("=" * 74)
    if cov.get("pair", 0) == 0:
        print("NOTE: 0 aligned pairs — no head-to-head winner yet. Single-sided means above are")
        print("      per-system quality on the incidents each actually triaged, NOT a contest.")
    if not all_p and not all_t:
        print("NOTE: no scored verdicts yet (sparse data / judge unavailable). Accumulate incidents.")


def main():
    pred_doc = _load("pred.json", {"incidents": []})
    tg_rows = _load("tg.json", [])
    if not isinstance(tg_rows, list):
        tg_rows = []
    pred_incs = [i for i in (pred_doc.get("incidents") or []) if is_real_pred(i)]

    pairs, pred_only, tg_only = align(pred_incs, tg_rows)
    print(f"aligned: pairs={len(pairs)}  pred_only={len(pred_only)}  tg_only={len(tg_only)}"
          f"   (real pred incidents={len(pred_incs)}, tg rows={len(tg_rows)})")

    keys = load_keys()
    judged = 0

    for inc, row in pairs:
        pk = inc.get("incidentKey")
        tref = row.get("external_ref")
        key = f"{DATE}|pair|{pk}|{tref}"
        if key in keys and not DRY:
            continue
        v = run_judge(inc, row, pk or tref or "(pair)")
        if DRY:
            print(json.dumps({"kind": "pair", "key": key, "dry": v.get("dry_run", True)}))
            continue
        append_verdict(v, key, "pair")
        keys.add(key)
        judged += 1

    for inc in pred_only:
        pk = inc.get("incidentKey")
        key = f"{DATE}|pred|{pk}"
        if key in keys and not DRY:
            continue
        v = run_judge(inc, None, pk or "(pred)")
        if DRY:
            print(json.dumps({"kind": "pred_only", "key": key}))
            continue
        append_verdict(v, key, "pred_only")
        keys.add(key)
        judged += 1

    for row in tg_only:
        tref = row.get("external_ref")
        key = f"{DATE}|tg|{tref}"
        if key in keys and not DRY:
            continue
        v = run_judge(None, row, tref or "(tg)")
        if DRY:
            print(json.dumps({"kind": "tg_only", "key": key}))
            continue
        append_verdict(v, key, "tg_only")
        keys.add(key)
        judged += 1

    print(f"judged this run: {judged}   (dedup skipped the rest)")
    if not DRY:
        aggregate()


if __name__ == "__main__":
    main()
