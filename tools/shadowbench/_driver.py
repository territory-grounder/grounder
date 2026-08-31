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
  SB_TIER     benchmark-ladder tier tag stamped on appended records (default "1")
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
# CAMPAIGN THROUGHPUT KNOBS (2026-07-31, harness-side only — pairing rule + judge scoring untouched):
#   SB_PAIRS_ONLY=1     judge PAIRS only this run; singles (pred_only/tg_only) are deferred, NOT lost —
#                       the scorecard is append-only and idempotent, so the next unflagged run judges
#                       them. Rationale: singles can NEVER bank toward the §3 bar (single-sided records
#                       are excluded by the pre-registration), yet a serial judge burned most of each
#                       campaign cycle on them, starving the confirmatory-critical pairs.
#   SB_JUDGE_WORKERS=N  run up to N judge.py subprocesses concurrently (default 1 = legacy serial).
#                       Verdict CONTENT per pair is unchanged; only wall-clock ordering moves.
PAIRS_ONLY = os.environ.get("SB_PAIRS_ONLY", "0") == "1"
JUDGE_WORKERS = max(1, int(os.environ.get("SB_JUDGE_WORKERS", "1")))
# PER-TIER TAG (TG-72, additive): every appended record carries the benchmark-ladder tier that produced
# it, so tier-scoped selects can separate Tier-2 (ambiguous multi-signal) pairs from the Tier-1/campaign
# population. ABSENT means "1" — every record written before this field existed is a tier-1-era record,
# so old scorecards stay valid unmodified, and analyze.py (sha-frozen LAW, PRE-REGISTRATION.md) ignores
# unknown fields: the tag steers SELECTION only, never the frozen analysis.
TIER = os.environ.get("SB_TIER") or "1"


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


#: Coarse fault classes, derived from each side's own words. A pair is only a comparison if BOTH sides were
#: looking at the same KIND of incident; matching on host and time alone is not enough on an estate where one
#: host commonly has several concurrent alerts.
#: Patterns are written in POST-NORMALISATION form (lowercase, every run of non-alphanumerics collapsed to a
#: single space), because that is what fault_class matches against. Writing them in raw rule-name form is how
#: "Devices-up/down" and "Space-on-/-is-90-and-95-in-use" both silently classified as unknown.
_CLASS_PATTERNS = (
    ("disk", ("space on", "disk", "filesystem", "storage")),
    ("memory", ("memory", "oom", "swap")),
    ("service", ("service up down", "service", "http check", "container", "systemd", "unit")),
    ("device", ("device down", "devices up", "device up", "icmp", "snmp", "unreachable", "rebooted")),
    # The predecessor labels incidents with its OWN coarse vocabulary; "availability" is its word for the
    # device/service reachability family. Listed LAST so a specific rule name in the title always wins.
    ("device", ("availability",)),
)


def fault_class(text):
    """Classify an alert rule / category into a coarse fault class, or "" when it cannot be told.

    Returns "" rather than guessing. An unclassifiable side must BLOCK the pair, not join it on a shrug —
    a wrong pair is worse than a missing one, because it enters the analysis as evidence.
    """
    # Normalise separators FIRST. LibreNMS rule names are hyphenated ("Space-on-/-is-90-and-95-in-use") while
    # the predecessor's categories are spaced ("disk space"), so matching raw text silently classified every
    # disk alert as unknown -- which would have quietly dropped the entire disk stratum from the campaign
    # rather than failing loudly. Caught by the unit test, not by reading the code.
    t = re.sub(r"[^a-z0-9]+", " ", (text or "").lower()).strip()
    for name, pats in _CLASS_PATTERNS:
        if any(p in t for p in pats):
            return name
    return ""


def align(pred_incs, tg_rows):
    """Return (pairs, pred_only, tg_only).

    A pair requires ALL of: same subject host, the SAME COARSE FAULT CLASS on both sides, and |Δt| within
    ALIGN_HOURS -- and among the candidates that qualify it takes the NEAREST IN TIME.

    Every clause here fixes a real defect in the previous loose rule (host + <=12h, first match wins):

      * NO FAULT-CLASS CHECK meant a TG disk-fill triage could pair with a predecessor device-down triage on
        the same host. Those are different incidents, and comparing the two systems' handling of different
        incidents is not a comparison at all. On this estate a single stopped guest raises four separate
        LibreNMS rules, so concurrent unrelated alerts on one host are the normal case, not an edge case.
      * FIRST MATCH WINS made the pairing depend on the order rows came back from the database. Two runs over
        the same data could pair differently, which is disqualifying for a confirmatory campaign that must
        reproduce to the digit. Nearest-in-time is deterministic and is also the better match.

    An unclassifiable side yields no pair rather than a loose one.
    """
    used = set()
    pairs, pred_only = [], []
    for inc in pred_incs:
        sub = pred_subject_host(inc)
        t_p = _parse_ts(inc.get("firstTs"))
        # CLASSIFY OVER EVERY FIELD, not the first non-empty one.
        #
        # This previously read `alertCategory or issue or incidentKey` — first-non-empty wins. The predecessor
        # ALWAYS populates alertCategory, and its vocabulary is its own: availability / maintenance / general /
        # kubernetes / resource. None of those name a fault class, so cls_p was "" for every record and the
        # fault-class requirement refused 100% OF PAIRS — silently, because an unclassifiable side is designed
        # to yield no pair rather than error. The whole confirmatory campaign would have accrued nothing while
        # the harness reported "no aligned pairs yet", which is indistinguishable from a quiet estate.
        #
        # The signal was in `issue` the whole time: "Infrastructure alert: dc1openwebui01 - Space on / is
        # >= 90% ..." carries the LibreNMS rule verbatim. Joining the fields lets the specific title classify
        # while the coarse category adds its own hints, and a match on ANY of them is enough.
        cls_p = fault_class(" ".join(str(inc.get(k) or "") for k in ("issue", "incidentKey", "alertCategory")))
        candidates = []
        for i, row in enumerate(tg_rows):
            if i in used:
                continue
            if not _host_match(sub, row.get("host")):
                continue
            cls_t = fault_class(row.get("alertRule"))
            if not cls_p or not cls_t or cls_p != cls_t:
                continue
            t_t = _parse_ts(row.get("createdAt"))
            if not (t_p and t_t):
                continue
            dt = abs((t_p - t_t).total_seconds())
            if dt > ALIGN_HOURS * 3600:
                continue
            candidates.append((dt, i))
        if candidates:
            candidates.sort()
            best = candidates[0][1]
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
    # setdefault, not assignment: a caller that already stamped an explicit tier keeps it.
    verdict.setdefault("tier", TIER)
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
    # OVERALL over COMPARABLE dimensions only (roadmap P1-2).
    #
    # The previous version pooled every score on each side and noted that falsifiable_prediction was
    # "naturally excluded where N/A". That is true only for the PREDECESSOR, which structurally commits no
    # prediction and is therefore never scored on it. TG *is* scored — 17 times, at a mean of 5.0 — so the two
    # OVERALL figures were means over DIFFERENT dimension sets: 4 for the predecessor, 5 for TG. That is not a
    # head-to-head, and it flatters TG (measured: 3.447 over the four comparable dims becomes 3.758 once its
    # own strongest, uncontested dimension is pooled in).
    #
    # A dimension only enters OVERALL when BOTH systems have at least one score for it. One-sided dimensions
    # are real TG capabilities and are still reported — as UNILATERAL properties, which is what they are —
    # but they never inflate a comparative mean.
    comparable = [d for d in DIMENSIONS if sums["pred"][d] and sums["tg"][d]]
    unilateral = [d for d in DIMENSIONS
                  if (sums["pred"][d] or sums["tg"][d]) and d not in comparable]
    all_p = [x for d in comparable for x in sums["pred"][d]]
    all_t = [x for d in comparable for x in sums["tg"][d]]
    print("-" * len(hdr))
    op = f"{mean(all_p)} ({len(all_p)})" if all_p else "— (0)"
    ot = f"{mean(all_t)} ({len(all_t)})" if all_t else "— (0)"
    print(f"{'OVERALL (comparable)':<24}{op:>18}{ot:>18}")
    print("=" * 74)
    print(f"OVERALL covers the {len(comparable)} dimension(s) BOTH systems are scored on: "
          f"{', '.join(comparable) if comparable else '(none)'}")
    for d in unilateral:
        only = "TG" if sums["tg"][d] else "predecessor"
        vals = sums["tg"][d] or sums["pred"][d]
        print(f"UNILATERAL — {d}: only {only} is scored (mean {mean(vals)}, n={len(vals)}); the other system "
              f"structurally does not compete here, so this is EXCLUDED from OVERALL and must be published "
              f"as a one-sided property, never as a head-to-head win.")
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

    # NEWEST-FIRST: the freshest pairs are the ones that can still bank against a live fault window;
    # oldest-first put them behind the day's backlog and they missed reconcile by a full cycle.
    _EPOCH = _dt.datetime(1970, 1, 1, tzinfo=_dt.timezone.utc)
    pairs.sort(key=lambda pr: (_parse_ts(pr[1].get("createdAt")) or _parse_ts(pr[0].get("firstTs"))
                               or _EPOCH), reverse=True)

    def _judge_pair(inc, row):
        """Judge one pair and return its enriched verdict (thread-safe: no shared state touched)."""
        pk = inc.get("incidentKey")
        tref = row.get("external_ref")
        v = run_judge(inc, row, pk or tref or "(pair)")
        # SUPPLY-JOIN FIELDS (reconcile-supply.py). The §3a confirmatory join needs each pair record to
        # carry its coarse fault class and its incident time — the judge verdict alone has neither, and
        # without them the record can never be joined to the injector's ground-truth ledger (the exact gap
        # that left accrual at 0/30 while the estate looked busy). The class is recomputed from the TG
        # side's own alert rule; align() already required both sides to classify identically, so this IS
        # the pair's class, from the same classifier that formed the pair.
        v["fault_class"] = fault_class(row.get("alertRule"))
        v["tg_created_at"] = row.get("createdAt")
        v["pred_first_ts"] = inc.get("firstTs")
        return v

    todo = []
    for inc, row in pairs:
        key = f"{DATE}|pair|{inc.get('incidentKey')}|{row.get('external_ref')}"
        if key in keys and not DRY:
            continue
        if DRY:
            v = run_judge(inc, row, inc.get("incidentKey") or row.get("external_ref") or "(pair)")
            print(json.dumps({"kind": "pair", "key": key, "dry": v.get("dry_run", True)}))
            continue
        todo.append((key, inc, row))

    if todo:
        if JUDGE_WORKERS > 1:
            import concurrent.futures as _cf
            with _cf.ThreadPoolExecutor(max_workers=JUDGE_WORKERS) as pool:
                futs = {pool.submit(_judge_pair, inc, row): key for key, inc, row in todo}
                for fut in _cf.as_completed(futs):
                    # append serially in the main thread — the scorecard stays a clean line ledger
                    append_verdict(fut.result(), futs[fut], "pair")
                    keys.add(futs[fut])
                    judged += 1
        else:
            for key, inc, row in todo:
                append_verdict(_judge_pair(inc, row), key, "pair")
                keys.add(key)
                judged += 1

    if PAIRS_ONLY:
        print(f"judged this run: {judged} pair(s)   (SB_PAIRS_ONLY=1: {len(pred_only)} pred-only + "
              f"{len(tg_only)} tg-only singles DEFERRED to the next unflagged run — they cannot bank "
              f"toward the section-3 bar and were starving the pairs)")
        if not DRY:
            aggregate()
        return

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
