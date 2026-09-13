#!/usr/bin/env python3
"""judge-calibrate.py — candidate-judge AGREEMENT MEASUREMENT against the frozen primary judge.

WHAT THIS IS (recovery item (d), the measurement prerequisite): before the LLM-judge could ever
migrate to a local model, there must be a number that says how closely a candidate model agrees
with the judge that anchors every score in the campaign. This tool produces that number and
nothing else: it re-judges a deterministic sample of ALREADY-JUDGED scorecard pair records with a
--candidate-model served through the SAME LiteLLM gateway, over the SAME blind prompt path, and
reports per-dimension agreement between the candidate's scores and the recorded primary scores.

WHAT THIS IS NOT: a decision. The primary judge stays FIXED until the owner rules on the Phase-D
verdict — swapping the anchor mid-campaign would re-denominate every number in scorecard.jsonl.
This tool NEVER COMPUTES A MIGRATION VERDICT and never concludes anything about TG vs the
predecessor: it measures candidate-vs-primary agreement, prints the measured statistics, and
prints PROPOSED acceptance thresholds that are explicitly UNRATIFIED — the owner ratifies or
rejects them; nothing here applies them. It never imports the frozen analyze.py (or _driver.py,
which can append to the ledger), and test_judge_calibrate.py asserts both structurally.

EXACT PROMPT PATH, BY IMPORT: judge.py is imported — the same seam _driver.py and
test_judge_symmetry.py already use (judge.py is NOT hash-pinned; only analyze.py is) — and each
candidate re-judge goes through judge.build_verdict(): the identical normalize -> redact ->
blind A/B -> build_prompt -> gateway call -> defensive parse -> clamp -> de-blind pipeline the
primary went through. Nothing is copied, so the candidate's prompt cannot drift from the frozen
judge's by even a byte.

PRESENTATION-ORDER MATCHING: the blind protocol randomizes which system is shown as A vs B. The
original A/B order IS recorded (the verdict's `mapping`), so for each sampled record this tool
searches a small deterministic seed sequence for one that reproduces the recorded mapping (a
dry-run prompt build per try; no model call) and re-judges under it. The candidate then sees the
same trajectories in the same presentation order the primary saw — order variance is removed
from the agreement measurement instead of being smeared into it.

INPUTS: the scorecard stores verdicts, not trajectories, so the sampled records' original judge
inputs must be re-supplied from the read-only extractors: --pred-from (an
`extract_predecessor.py --json` document; repeatable) and --tg-from (an `extract_tg.sh`
jsononly array; repeatable). A sampled record whose inputs are not present in the supplied
files is REPORTED as inputs_unavailable — counted, never silently dropped.

SAMPLING (deterministic): eligible records are kind=pair, judged (not judge_unavailable) by the
primary alias, carrying at least one numeric dimension score. Each eligible record is ranked by
SHA-256(sample_seed | record key) and the lowest N ranks are taken, then processed in sorted-key
order — reproducible across runs, machines, and Python versions (no RNG-implementation
dependence). --sample N defaults to 30; --sample-seed defaults to a fixed constant.

STATISTICS: per dimension and pooled across dimensions — exact-agreement %, within-1 %, mean
signed delta (candidate - primary), and quadratic-weighted Cohen's kappa over the 1..5 scale;
N/A handling is explicit (primary-null-candidate-scored, candidate-null-primary-scored, both
null are counted separately and never enter kappa); plus verdict-level agreement on the pooled
winner (pred/tg/tie) where both verdicts carry one.

READ-ONLY against production data: it opens the scorecard and the input files for READING only
and never writes scorecard.jsonl. Its single write is the JSON report artifact under
tools/shadowbench/out/ (gitignored working artifacts): judge-calibration-<model>-<date>.json.
The artifact carries scores and agreement statistics only — no trajectories, no prompts.

CANDIDATE ALIAS: --candidate-model need not exist in deploy/litellm-config.yaml yet (e.g. an
ollama-backed `judge-local-candidate` entry may still be unconfigured). The gateway resolves the
alias at call time; an unknown or unreachable alias degrades per record to candidate_unavailable
via judge.py's honest-degrade path — never a fabricated number.

Usage (the one-liner):
    ./judge-calibrate.py --candidate-model judge-local-candidate \\
        --pred-from /tmp/pred-2026-07-26.json --tg-from /tmp/tg-2026-07-26.json

Flags:
    --candidate-model NAME  REQUIRED. Gateway alias to calibrate (same LiteLLM the primary uses).
    --pred-from FILE        extract_predecessor.py --json document (repeatable; incidents merged)
    --tg-from FILE          extract_tg.sh jsononly JSON array (repeatable; rows merged)
    --scorecard FILE        judged-verdict ledger (default: tools/shadowbench/scorecard.jsonl)
    --sample N              sample size (default 30)
    --sample-seed N         deterministic selection seed (default 20260730)
    --out-dir DIR           artifact directory (default: tools/shadowbench/out)
    --dry-run               list the deterministic sample + input availability; no model calls
    --ssh-key/--tg-host/--ssh-user/--env-path/--timeout   forwarded to judge.py's gateway call
"""
from __future__ import annotations

import argparse
import datetime as _dt
import hashlib
import json
import os
import re
import sys
from collections import Counter

_HERE = os.path.dirname(os.path.abspath(__file__))
if _HERE not in sys.path:
    sys.path.insert(0, _HERE)
import judge  # noqa: E402  — the frozen prompt path, reused by import, never copied

#: The five scored dimensions — the ONE source (core/judge/rubric.json via judge.py), never re-declared.
DIMENSIONS = list(judge.DIMENSIONS)
#: The ordinal category scale every dimension is scored on.
CATEGORIES = (1, 2, 3, 4, 5)
#: Fixed sampling seed: the calibration sample must be the SAME sample on every run over the same ledger.
DEFAULT_SAMPLE_SEED = 20260730
DEFAULT_SAMPLE = 30
DEFAULT_SCORECARD = os.path.join(_HERE, "scorecard.jsonl")
DEFAULT_OUT_DIR = os.path.join(_HERE, "out")
#: How many deterministic seeds to try when matching the recorded A/B presentation order.
_ORDER_SEED_TRIES = 32

#: PROPOSED acceptance thresholds — documented for the owner to ratify, NOT law. Nothing in this
#: file applies them to the measured numbers; they are printed beside the numbers, that is all.
PROPOSED_THRESHOLDS = {
    "pooled_weighted_kappa_min": 0.75,
    "min_dimension_weighted_kappa": 0.60,
    "mean_signed_delta_abs_max": 0.25,
    "status": "PROPOSED — owner ratification required; not law",
}
PROPOSED_GUIDANCE = (
    "a candidate judge is migration-eligible when pooled weighted kappa >= 0.75 "
    "AND no dimension kappa < 0.6 AND mean signed delta within ±0.25"
)


# ---------------------------------------------------------------------------
# Loading + eligibility + deterministic sampling
# ---------------------------------------------------------------------------

def load_jsonl(path):
    """Read a JSONL file. None when absent — absence (no ledger) must stay distinguishable from empty."""
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


def has_primary_scores(rec):
    """True when the record carries at least one numeric dimension score (either side)."""
    dims = rec.get("dims") or {}
    for side in dims.values():
        if isinstance(side, dict) and any(
            isinstance(side.get(d), (int, float)) and not isinstance(side.get(d), bool)
            for d in DIMENSIONS
        ):
            return True
    return False


def eligible_pairs(rows, primary_model=judge.DEFAULT_MODEL):
    """The calibration population: judged PAIR records scored by the frozen primary alias.

    A record judged under any OTHER requested model is excluded — agreement with a non-primary
    score would calibrate the candidate against the wrong anchor.
    """
    out = []
    for r in rows or []:
        if r.get("kind") != "pair":
            continue
        if r.get("judge_unavailable"):
            continue
        req = r.get("judge_model_requested")
        if req is not None and req != primary_model:
            continue
        if not has_primary_scores(r):
            continue
        out.append(r)
    return out


def _rank(sample_seed, key):
    return hashlib.sha256(f"{sample_seed}|{key}".encode("utf-8")).hexdigest()


def select_sample(eligible, n, sample_seed=DEFAULT_SAMPLE_SEED):
    """Deterministic sample: rank by SHA-256(seed|key), take the lowest N, process in key order.

    Hash-ranking (rather than random.sample) makes the selection a pure function of (seed, keys):
    independent of input row order, of Python version, and of RNG implementation details.
    """
    ranked = sorted(eligible, key=lambda r: (_rank(sample_seed, r.get("key") or ""), r.get("key") or ""))
    chosen = ranked[: max(0, int(n))]
    return sorted(chosen, key=lambda r: r.get("key") or "")


def parse_pair_key(rec):
    """Split a pair record's dedup key 'DATE|pair|PREDKEY|TGREF' -> (predkey, tgref) or (None, None)."""
    parts = (rec.get("key") or "").split("|")
    if len(parts) == 4 and parts[1] == "pair":
        return parts[2], parts[3]
    return None, None


def index_inputs(pred_docs, tg_docs):
    """Index re-supplied extractor outputs: predecessor incidents by incidentKey, TG rows by
    external_ref. First occurrence wins so re-supplying an overlapping file cannot silently
    replace a record."""
    pred_index, tg_index = {}, {}
    for doc in pred_docs or []:
        incs = doc.get("incidents", doc) if isinstance(doc, dict) else doc
        for inc in incs or []:
            k = (inc or {}).get("incidentKey")
            if k and k not in pred_index:
                pred_index[k] = inc
    for rows in tg_docs or []:
        for row in rows or []:
            k = (row or {}).get("external_ref")
            if k and k not in tg_index:
                tg_index[k] = row
    return pred_index, tg_index


# ---------------------------------------------------------------------------
# Candidate re-judge (the frozen path, by import) + presentation-order matching
# ---------------------------------------------------------------------------

def _derive_seed(sample_seed, key, k):
    """Deterministic per-record judge seed (k walks the order-matching sequence)."""
    return int(hashlib.sha256(f"{sample_seed}|{key}|{k}".encode("utf-8")).hexdigest()[:12], 16)


def _judge_args(args, incident_key, seed, dry_run=False):
    """The argparse surface judge.build_verdict()/call_litellm() expect, with the CANDIDATE model."""
    return argparse.Namespace(
        model=args.candidate_model,
        seed=seed,
        dry_run=dry_run,
        incident_key=incident_key,
        host=None,
        ssh_key=args.ssh_key,
        tg_host=args.tg_host,
        ssh_user=args.ssh_user,
        env_path=args.env_path,
        timeout=args.timeout,
    )


def order_matching_seed(pred_rec, tg_rec, rec, args, sample_seed):
    """Find a deterministic seed whose blind A/B assignment reproduces the RECORDED mapping.

    Each try is a dry-run prompt build (no model call). Returns (seed, matched). When no seed in
    the sequence reproduces the recorded order (or the record carries no mapping), the first
    derived seed is used and matched=False is reported — measured, not hidden.
    """
    key = rec.get("key") or rec.get("incident_key") or ""
    want = rec.get("mapping")
    incident_key = rec.get("incident_key") or key
    if isinstance(want, dict):
        for k in range(_ORDER_SEED_TRIES):
            seed = _derive_seed(sample_seed, key, k)
            v, _prompt = judge.build_verdict(pred_rec, tg_rec, _judge_args(args, incident_key, seed, dry_run=True))
            if v.get("mapping") == want:
                return seed, True
    return _derive_seed(sample_seed, key, 0), False


def rejudge_with_candidate(pred_rec, tg_rec, rec, args, sample_seed):
    """Re-run the EXACT judge pipeline for one sampled record with the candidate model.

    Returns (verdict, order_matched). Any exception degrades to a judge_unavailable-shaped
    verdict — the calibration must never fabricate a candidate score.
    """
    seed, matched = order_matching_seed(pred_rec, tg_rec, rec, args, sample_seed)
    incident_key = rec.get("incident_key") or (rec.get("key") or "")
    try:
        verdict, _prompt = judge.build_verdict(pred_rec, tg_rec, _judge_args(args, incident_key, seed))
    except Exception as e:  # defensive: a malformed re-supplied input must not abort the run
        verdict = {"judge_unavailable": True, "error": f"{type(e).__name__}: {e}"}
    return verdict, matched


# ---------------------------------------------------------------------------
# Agreement statistics
# ---------------------------------------------------------------------------

def side_scores(verdict, system):
    """{dim: int|None} for one system ('pred'/'tg'), de-blinded via the verdict's own mapping."""
    mapping = verdict.get("mapping") or {}
    letter = next((L for L, s in mapping.items() if s == system), None)
    if not letter:
        return None
    dims = (verdict.get("dims") or {}).get(letter)
    if not isinstance(dims, dict):
        return None
    out = {}
    for d in DIMENSIONS:
        v = dims.get(d)
        out[d] = int(v) if isinstance(v, (int, float)) and not isinstance(v, bool) else None
    return out


def compare_record(primary_rec, cand_verdict):
    """[(system, dim, primary, candidate)] for every dimension of every system both verdicts map."""
    out = []
    for system in ("pred", "tg"):
        p_side = side_scores(primary_rec, system)
        c_side = side_scores(cand_verdict, system)
        if p_side is None or c_side is None:
            continue
        for d in DIMENSIONS:
            out.append((system, d, p_side.get(d), c_side.get(d)))
    return out


def quadratic_weighted_kappa(pairs, categories=CATEGORIES):
    """Quadratic-weighted Cohen's kappa over ordinal categories.

    kappa_w = 1 - (observed weighted disagreement / chance-expected weighted disagreement), with
    weight (i-j)^2 / (max-min)^2 and chance from the two raters' marginal distributions over the
    FULL category set. Returns None for an empty input and for degenerate marginals (expected
    disagreement 0 — both raters constant — where kappa is mathematically undefined; the
    exact-agreement % carries the information there).
    """
    lo, hi = min(categories), max(categories)
    span2 = float((hi - lo) ** 2)
    scored = [(max(lo, min(hi, int(a))), max(lo, min(hi, int(b)))) for a, b in pairs]
    if not scored:
        return None
    n = len(scored)
    observed = sum((a - b) ** 2 for a, b in scored) / (n * span2)
    pa = Counter(a for a, _ in scored)
    pb = Counter(b for _, b in scored)
    expected = sum(
        (pa[i] / n) * (pb[j] / n) * ((i - j) ** 2) / span2
        for i in categories
        for j in categories
    )
    if expected == 0:
        return None
    return 1.0 - observed / expected


def agreement_stats(pc_pairs):
    """Agreement statistics over [(primary, candidate)] score pairs (either side may be None)."""
    numeric = [
        (p, c) for p, c in pc_pairs
        if isinstance(p, int) and not isinstance(p, bool) and isinstance(c, int) and not isinstance(c, bool)
    ]
    n = len(numeric)
    kappa = quadratic_weighted_kappa(numeric)
    return {
        "n": n,
        "exact_pct": round(100.0 * sum(1 for p, c in numeric if p == c) / n, 1) if n else None,
        "within1_pct": round(100.0 * sum(1 for p, c in numeric if abs(p - c) <= 1) / n, 1) if n else None,
        "mean_signed_delta": round(sum(c - p for p, c in numeric) / n, 3) if n else None,
        "weighted_kappa": round(kappa, 4) if kappa is not None else None,
        "primary_na_candidate_scored": sum(1 for p, c in pc_pairs if p is None and c is not None),
        "candidate_na_primary_scored": sum(1 for p, c in pc_pairs if p is not None and c is None),
        "both_na": sum(1 for p, c in pc_pairs if p is None and c is None),
    }


def verdict_agreement(winner_pairs):
    """Pooled-winner agreement over [(recorded, candidate)] where both carry pred/tg/tie."""
    valid = [(r, c) for r, c in winner_pairs
             if r in ("pred", "tg", "tie") and c in ("pred", "tg", "tie")]
    n = len(valid)
    agree = sum(1 for r, c in valid if r == c)
    mismatches = Counter(f"{r}->{c}" for r, c in valid if r != c)
    return {
        "n": n,
        "agree": agree,
        "agree_pct": round(100.0 * agree / n, 1) if n else None,
        "mismatches": dict(sorted(mismatches.items())),
    }


# ---------------------------------------------------------------------------
# The calibration run
# ---------------------------------------------------------------------------

def calibrate(scorecard_rows, pred_index, tg_index, args):
    """Measure candidate-vs-primary agreement over the deterministic sample. Returns the report
    dict (also the JSON artifact). Pure measurement — no migration decision, no campaign
    conclusion, and the scorecard is only ever read."""
    eligible = eligible_pairs(scorecard_rows)
    sample = select_sample(eligible, args.sample, args.sample_seed)

    records_out = []
    all_tuples = []          # (system, dim, primary, candidate)
    winner_pairs = []        # (recorded_winner, candidate_winner)
    served = set()
    compared = inputs_unavailable = candidate_unavailable = order_matched_n = 0

    for rec in sample:
        key = rec.get("key") or ""
        predkey, tgref = parse_pair_key(rec)
        pred_rec = pred_index.get(predkey) if predkey else None
        tg_rec = tg_index.get(tgref) if tgref else None
        if pred_rec is None or tg_rec is None:
            missing = [name for name, r in (("pred", pred_rec), ("tg", tg_rec)) if r is None]
            inputs_unavailable += 1
            records_out.append({"key": key, "status": "inputs_unavailable", "missing": missing})
            continue
        if args.dry_run:
            records_out.append({"key": key, "status": "inputs_available"})
            continue

        verdict, matched = rejudge_with_candidate(pred_rec, tg_rec, rec, args, args.sample_seed)
        if verdict.get("judge_unavailable"):
            candidate_unavailable += 1
            records_out.append({
                "key": key, "status": "candidate_unavailable",
                "error": str(verdict.get("error") or "")[:300],
            })
            continue

        compared += 1
        order_matched_n += 1 if matched else 0
        if verdict.get("judge_model_served"):
            served.add(str(verdict["judge_model_served"]))
        tuples = compare_record(rec, verdict)
        all_tuples.extend(tuples)
        winner_pairs.append((rec.get("winner"), verdict.get("winner")))
        scores = {}
        for system, d, p, c in tuples:
            scores.setdefault(system, {})[d] = {"primary": p, "candidate": c}
        records_out.append({
            "key": key,
            "status": "compared",
            "order_matched": matched,
            "recorded_winner": rec.get("winner"),
            "candidate_winner": verdict.get("winner"),
            "scores": scores,
        })

    per_dimension = {
        d: agreement_stats([(p, c) for system, dim, p, c in all_tuples if dim == d])
        for d in DIMENSIONS
    }
    pooled = agreement_stats([(p, c) for _system, _dim, p, c in all_tuples])

    return {
        "tool": "judge-calibrate",
        "generated_at": _dt.datetime.now(_dt.timezone.utc).isoformat(),
        "candidate_model": args.candidate_model,
        "candidate_model_served": sorted(served),
        "primary_model": judge.DEFAULT_MODEL,
        "dimensions": DIMENSIONS,
        "scorecard": args.scorecard,
        "scorecard_records": len(scorecard_rows or []),
        "eligible_pairs": len(eligible),
        "sample_requested": args.sample,
        "sample_seed": args.sample_seed,
        "sampled": len(sample),
        "compared": compared,
        "inputs_unavailable": inputs_unavailable,
        "candidate_unavailable": candidate_unavailable,
        "order_matched": order_matched_n,
        "dry_run": bool(args.dry_run),
        "per_dimension": per_dimension,
        "pooled": pooled,
        "verdict_agreement": verdict_agreement(winner_pairs),
        "proposed_thresholds": dict(PROPOSED_THRESHOLDS),
        "proposed_guidance": PROPOSED_GUIDANCE,
        "records": records_out,
        "notes": [
            "Measurement only: this artifact carries agreement statistics, never a migration decision.",
            "The primary judge anchors all scores and stays FIXED until the Phase-D verdict.",
            "Thresholds above are PROPOSED for owner ratification; nothing here applies them.",
            "Scores only — no trajectories or prompts are stored in this artifact.",
        ],
    }


# ---------------------------------------------------------------------------
# Rendering + artifact
# ---------------------------------------------------------------------------

def _fmt(v, none="n/a"):
    return none if v is None else str(v)


def render(report):
    out = []
    out.append("JUDGE CALIBRATION — candidate vs the frozen primary judge (measurement only; read-only)")
    out.append(f"  scorecard: {report['scorecard']}   records: {report['scorecard_records']}   "
               f"eligible pairs: {report['eligible_pairs']}")
    out.append(f"  sample: {report['sampled']} of {report['sample_requested']} requested "
               f"(deterministic; seed {report['sample_seed']})")
    out.append(f"  candidate model: {report['candidate_model']}   primary: {report['primary_model']}")
    if report.get("dry_run"):
        out.append("  DRY RUN — no model calls; sample + input availability only:")
        for r in report["records"]:
            extra = f"  (missing: {','.join(r['missing'])})" if r.get("missing") else ""
            out.append(f"    - {r['key']}  [{r['status']}]{extra}")
        out.append(f"  inputs unavailable: {report['inputs_unavailable']} of {report['sampled']}")
        return "\n".join(out)

    out.append(f"  re-judged: {report['compared']}   inputs unavailable: {report['inputs_unavailable']}   "
               f"candidate unavailable: {report['candidate_unavailable']}   "
               f"presentation-order matched: {report['order_matched']}/{report['compared']}")
    if report["candidate_model_served"]:
        out.append(f"  served as: {', '.join(report['candidate_model_served'])}")
    out.append("")
    hdr = (f"  {'dimension':<24}{'n':>5}{'exact%':>9}{'within1%':>10}{'mean_d':>9}{'wkappa':>9}"
           f"{'p-NA':>6}{'c-NA':>6}{'both-NA':>9}")
    out.append(hdr)
    out.append("  " + "-" * (len(hdr) - 2))
    for d in DIMENSIONS:
        s = report["per_dimension"][d]
        out.append(f"  {d:<24}{s['n']:>5}{_fmt(s['exact_pct']):>9}{_fmt(s['within1_pct']):>10}"
                   f"{_fmt(s['mean_signed_delta']):>9}{_fmt(s['weighted_kappa']):>9}"
                   f"{s['primary_na_candidate_scored']:>6}{s['candidate_na_primary_scored']:>6}"
                   f"{s['both_na']:>9}")
    out.append("  " + "-" * (len(hdr) - 2))
    p = report["pooled"]
    out.append(f"  {'POOLED':<24}{p['n']:>5}{_fmt(p['exact_pct']):>9}{_fmt(p['within1_pct']):>10}"
               f"{_fmt(p['mean_signed_delta']):>9}{_fmt(p['weighted_kappa']):>9}"
               f"{p['primary_na_candidate_scored']:>6}{p['candidate_na_primary_scored']:>6}"
               f"{p['both_na']:>9}")
    va = report["verdict_agreement"]
    mism = ("  mismatches: " + ", ".join(f"{k}={v}" for k, v in va["mismatches"].items())) if va["mismatches"] else ""
    out.append(f"  pooled-winner agreement: {va['agree']}/{va['n']}"
               + (f" = {va['agree_pct']}%" if va["agree_pct"] is not None else "") + mism)
    out.append("")
    out.append("  PROPOSED acceptance bar (NOT ratified — for the owner to ratify, not law):")
    out.append("    " + PROPOSED_GUIDANCE)
    out.append("  NOTE: this tool MEASURES agreement only. It renders no migration decision and no")
    out.append("        TG-vs-predecessor conclusion; the judge that anchors all scores stays FIXED")
    out.append("        until the owner rules on the Phase-D verdict.")
    if report["compared"] == 0:
        out.append("  NOTE: nothing was re-judged (missing inputs / unavailable candidate) — the table")
        out.append("        above is honestly empty; supply --pred-from/--tg-from extractor outputs.")
    return "\n".join(out)


def _sanitize_model(name):
    return re.sub(r"[^A-Za-z0-9._+-]+", "-", name or "").strip("-") or "model"


def write_artifact(report, out_dir):
    """The tool's ONLY write: the JSON report under out/ (gitignored). Never the scorecard."""
    os.makedirs(out_dir, exist_ok=True)
    date = _dt.datetime.now(_dt.timezone.utc).date().isoformat()
    path = os.path.join(out_dir, f"judge-calibration-{_sanitize_model(report['candidate_model'])}-{date}.json")
    with open(path, "w", encoding="utf-8") as fh:
        json.dump(report, fh, indent=2, ensure_ascii=False)
        fh.write("\n")
    return path


def main(argv=None):
    ap = argparse.ArgumentParser(
        description="Candidate-judge agreement measurement vs the frozen primary judge (read-only).",
        formatter_class=argparse.RawDescriptionHelpFormatter,
    )
    ap.add_argument("--candidate-model", required=True,
                    help="gateway alias to calibrate (need not exist in litellm-config yet)")
    ap.add_argument("--pred-from", action="append", default=[],
                    help="extract_predecessor.py --json document (repeatable)")
    ap.add_argument("--tg-from", action="append", default=[],
                    help="extract_tg.sh jsononly JSON array (repeatable)")
    ap.add_argument("--scorecard", default=DEFAULT_SCORECARD)
    ap.add_argument("--sample", type=int, default=DEFAULT_SAMPLE)
    ap.add_argument("--sample-seed", type=int, default=DEFAULT_SAMPLE_SEED)
    ap.add_argument("--out-dir", default=DEFAULT_OUT_DIR)
    ap.add_argument("--dry-run", action="store_true")
    ap.add_argument("--ssh-key", default=judge.DEFAULT_SSH_KEY)
    ap.add_argument("--tg-host", default=judge.DEFAULT_TG_HOST)
    ap.add_argument("--ssh-user", default=judge.DEFAULT_SSH_USER)
    ap.add_argument("--env-path", default=judge.DEFAULT_ENV_PATH)
    ap.add_argument("--timeout", type=int, default=150)
    args = ap.parse_args(argv)

    scorecard_rows = load_jsonl(args.scorecard)
    if scorecard_rows is None:
        print(f"scorecard ABSENT: {args.scorecard} — nothing to calibrate against (zero records)")
        scorecard_rows = []

    def _read(path):
        with open(os.path.expanduser(path), "r", encoding="utf-8") as fh:
            return json.load(fh)

    pred_index, tg_index = index_inputs(
        [_read(p) for p in args.pred_from],
        [_read(p) for p in args.tg_from],
    )

    report = calibrate(scorecard_rows, pred_index, tg_index, args)
    print(render(report))
    if not args.dry_run:
        path = write_artifact(report, args.out_dir)
        print(f"\n  artifact: {path}")
    return 0


if __name__ == "__main__":
    sys.exit(main())
