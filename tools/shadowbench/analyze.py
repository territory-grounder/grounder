#!/usr/bin/env python3
"""Frozen confirmatory analysis for the TG-vs-predecessor exceed-proof (P6-2).

THIS FILE IS PRE-REGISTERED. Its SHA-256 is recorded in PRE-REGISTRATION.md and asserted by
test_analyze.py, which CI runs. Changing the analysis therefore cannot be done silently: the hash goes red,
and updating the recorded hash is a governed change carrying the Law-Change-Approved-By trailer. That is the
entire point — an analysis that can be adjusted after seeing the data is not a confirmatory analysis, and the
freeze is worth nothing if it is only a promise in prose.

Stdlib only, deliberately. The exit criterion is that a third party reproduces every published number with
ONE command from a clean clone; a numpy/scipy dependency puts an install step and a version skew between them
and that guarantee. Every statistic here is implemented directly and tested against hand-computed values.

WHAT IS PRIMARY, AND WHY IT IS NOT THE JUDGE
--------------------------------------------
PRIMARY endpoint is JUDGE-FREE: whether each system proposed the op-class that actually addresses the
INJECTED fault, scored against the injector's own record (core/diagcorpus). Ground truth, not opinion.
Paired binary outcome per fault -> McNemar's exact test on the discordant pairs.

SECONDARY endpoints are the five judged rubric dimensions, ordinal 1-5, paired per incident -> one-sided
Wilcoxon signed-rank with Cliff's delta, Holm-corrected across the family.

The ordering is deliberate. TG's INTERNAL session judge was measured uncalibrated (TPR 0.883, TNR 0.000,
kappa -0.141, n=94; and 58.3% of sessions receive one identical score across all five dimensions). The
SHADOWBENCH judge is a different instrument and measurably healthier on the same check (23.3% collapse,
80.3% of scores >=4) -- so the judged pairs are not void, but they carry a positivity skew and a
dimension-correlation that a primary endpoint should not depend on. A ground-truth endpoint has neither
problem, and P5's corpus made it available.

CLUSTERING. Pairs are NOT independent: in the 21-pair pilot, 7 of 15 shared one host. Confidence intervals
are therefore cluster-bootstrapped BY HOST (resample hosts, not pairs). A naive per-pair interval would be
too narrow, which is the direction that manufactures significance.

THE VERDICT IS PART OF THE FREEZE
---------------------------------
The campaign concludes in exactly ONE of three verdicts -- TG EXCEEDS / TG MATCHES (non-inferior) /
PREDECESSOR HOLDS -- computed by composite_verdict() IN THIS FILE (PRE-REGISTRATION.md section 7, frozen
2026-07-30 at ZERO accrued confirmatory pairs). A decision rule applied to the frozen numbers from OUTSIDE
the freeze is exactly as adjustable-after-the-fact as an unfrozen analysis, because the verdict is the thing
the whole campaign exists to produce. composite_verdict() takes a CLOSED keyword-only input set:
falsifiable_prediction and the unilateral axes (A3/A4/A5/A7/A8, docs/BENCHMARK-AXES.md) have no parameter to
arrive through -- the 2026-07-26 rejudge showed both of TG's apparent pooled wins were manufactured by
exactly such a one-sided dimension, and the fix is a function that cannot receive the data, not vigilance.
"""

from __future__ import annotations

import argparse
import datetime as _dt
import hashlib
import json
import math
import os
import random
import sys
from collections import defaultdict

# ---------------------------------------------------------------------------
# Pre-registered constants. Changing any of these changes the analysis and so
# changes this file's hash, which reds the freeze test.
# ---------------------------------------------------------------------------

#: The rubric dimensions, in a FIXED order so Holm's step-down is deterministic.
DIMENSIONS = (
    "correct_diagnosis",
    "evidence_grounded",
    "sensible_proposal",
    "appropriate_band",
    "falsifiable_prediction",
)

#: Family-wise error rate for the secondary family.
ALPHA = 0.05

#: Bootstrap resamples for the cluster-robust interval.
BOOTSTRAP_N = 10000

#: FIXED seed. A bootstrap whose seed is chosen after seeing the data is a garden of forking paths; pinning it
#: makes the interval reproducible to the digit, which the exit criterion requires.
BOOTSTRAP_SEED = 20260727

#: Minimum evidence for a confirmatory claim, from the evidence regime.
#: MIN_HOSTS amended 15 -> 12 (PRE-REGISTRATION.md section 6, 2026-07-31) — DISCOVERED INFEASIBILITY,
#: outcome-independent: the predecessor's alert intake structurally covers only 12 of the 15 pool hosts
#: (3 hosts got 0 predecessor sessions ever, despite real injected faults TG triaged 3/131/3x), so 15
#: distinct predecessor-paired hosts is arithmetically impossible while measuring the predecessor as-is.
#: MIN_PAIRS stays 30 — the pair floor governs statistical power and is untouched by the coverage ceiling.
MIN_PAIRS = 30
MIN_HOSTS = 12
MAX_PAIRS_PER_HOST = 3

#: Campaign accrual boundary (§6): the confirmatory population is pairs INJECTED at/after this instant.
#: Mirrors run-campaign.sh's ACCRUE_FROM and accrual.py's supply-side gate; the value is governed per
#: campaign under §6 (like MIN_HOSTS), declared before any pair accrues. A pair's injection time is its
#: incident time (tg_created_at or pred_first_ts, per reconcile-supply.py). Enforcing it in the analysis
#: stops the "earliest per host" cap from admitting a disowned pre-freeze pilot and evicting a confirmatory
#: pair (TG-249 item 10).
#: CAMPAIGN #3 BOUNDARY (§6 2026-08-25): declared at ZERO accrued campaign-#3 pairs — the injector has been
#: disabled since 2026-08-15, and every judged pair on record (including the 11 contaminated post-campaign-2
#: verdicts of 2026-07-31T22:29–23:02Z, §6 2026-08-19) predates this instant, so nothing sits inside it.
ACCRUE_FROM = "2026-08-26T00:00:00Z"

#: The like-for-like counter-leg of the EXCEEDS verdict (section 7): the two judged dimensions the
#: 2026-07-26 rejudge showed the real capability gap concentrates in. A primary (ground-truth) win cannot
#: stand while either is significantly in the predecessor's favour -- otherwise the campaign could publish
#: "exceeds" on ground its own like-for-like judged evidence contradicts. This tuple is CLOSED: it can never
#: contain falsifiable_prediction or any dimension only one system competes on.
COUNTER_LEG_DIMENSIONS = ("correct_diagnosis", "evidence_grounded")

#: Non-inferiority margin on the PRIMARY correct-rate difference (TG minus predecessor), in rate units:
#: 0.10 = ten percentage points. TG MATCHES only when the host-clustered bootstrap CI excludes a predecessor
#: advantage GREATER than this margin, i.e. the CI lower bound is >= -NONINFERIORITY_MARGIN. An advantage of
#: exactly the margin is not an advantage greater than it, so the boundary itself still matches.
NONINFERIORITY_MARGIN = 0.10

#: The three verdicts. Exactly one is emitted, by composite_verdict() below, never applied by hand.
VERDICT_EXCEEDS = "TG EXCEEDS"
VERDICT_MATCHES = "TG MATCHES (non-inferior)"
VERDICT_HOLDS = "PREDECESSOR HOLDS"

#: Axes with NO comparator (docs/BENCHMARK-AXES.md): the incumbent runs in shadow and never actuates, so on
#: A3/A4/A5 it was never playing and on A7/A8 it can only "win" by never acting. These are PRINTED as
#: labelled TG-only properties and are structurally unable to enter the verdict -- composite_verdict() has no
#: parameter that could carry them.
UNILATERAL_AXES = (
    ("A3", "heal success rate"),
    ("A4", "autonomy rate"),
    ("A5", "fault-class breadth"),
    ("A7", "false-actuation rate"),
    ("A8", "safety-violation count"),
)


# ---------------------------------------------------------------------------
# Statistics. Implemented directly; test_analyze.py checks each against values
# computed by hand or from a published worked example.
# ---------------------------------------------------------------------------


def binom_coeff(n: int, k: int) -> int:
    return math.comb(n, k)


def binom_test_two_sided(k: int, n: int, p: float = 0.5) -> float:
    """Exact two-sided binomial test.

    Used for McNemar's exact test on discordant pairs, which is the correct test for a PAIRED BINARY outcome.
    The common chi-square approximation is unreliable at the discordant counts a 30-pair campaign produces, and
    it errs toward significance -- so the exact form is pre-registered instead.
    """
    if n == 0:
        return 1.0
    probs = [binom_coeff(n, i) * (p**i) * ((1 - p) ** (n - i)) for i in range(n + 1)]
    obs = probs[k]
    # Sum every outcome AT LEAST AS EXTREME as observed, with a tolerance so float error cannot drop the
    # symmetric partner of the observed cell and halve the p-value.
    return min(1.0, sum(pr for pr in probs if pr <= obs * (1 + 1e-9)))


def mcnemar_exact(b: int, c: int) -> tuple[float, int]:
    """McNemar's exact test. b = TG right / other wrong, c = TG wrong / other right.

    Concordant pairs carry no information about a DIFFERENCE and are excluded by construction -- that is the
    test's design, not a filter applied after inspection.
    """
    return binom_test_two_sided(b, b + c), b + c


def wilcoxon_signed_rank(diffs: list[float]) -> tuple[float, float, int]:
    """One-sided Wilcoxon signed-rank test (H1: median difference > 0).

    Returns (W, p, n_nonzero). Zero differences are dropped (Wilcoxon's own convention); ties share averaged
    ranks. Exact for n <= 20 by full enumeration of sign assignments; normal approximation with a continuity
    correction and a tie correction above that. The exact branch matters: a 30-pair campaign with dropped
    zeros routinely lands under 20 informative pairs.
    """
    nz = [d for d in diffs if d != 0]
    n = len(nz)
    if n == 0:
        return 0.0, 1.0, 0
    order = sorted(range(n), key=lambda i: abs(nz[i]))
    ranks = [0.0] * n
    i = 0
    while i < n:
        j = i
        while j + 1 < n and abs(nz[order[j + 1]]) == abs(nz[order[i]]):
            j += 1
        avg = (i + j) / 2.0 + 1.0
        for k in range(i, j + 1):
            ranks[order[k]] = avg
        i = j + 1
    w_plus = sum(ranks[i] for i in range(n) if nz[i] > 0)

    if n <= 20:
        # Exact: enumerate every sign assignment and count those with W+ at least as large.
        count = 0
        total = 1 << n
        for mask in range(total):
            s = sum(ranks[i] for i in range(n) if mask & (1 << i))
            if s >= w_plus - 1e-9:
                count += 1
        return w_plus, count / total, n

    mean = n * (n + 1) / 4.0
    tie_groups: dict[float, int] = defaultdict(int)
    for i in range(n):
        tie_groups[abs(nz[i])] += 1
    tie_term = sum(t**3 - t for t in tie_groups.values())
    var = (n * (n + 1) * (2 * n + 1) - tie_term / 2.0) / 24.0
    if var <= 0:
        return w_plus, 1.0, n
    z = (w_plus - mean - 0.5) / math.sqrt(var)
    return w_plus, 1.0 - normal_cdf(z), n


def normal_cdf(z: float) -> float:
    return 0.5 * (1.0 + math.erf(z / math.sqrt(2.0)))


def wilcoxon_two_sided(diffs: list[float]) -> tuple[float, float, int, str]:
    """Two-sided Wilcoxon signed-rank test (H1: median difference != 0), by the doubling rule.

    Returns (W+, p_two_sided, n_nonzero, direction): direction is "positive" when the evidence leans toward
    a positive median difference, "negative" when it leans negative, "none" when there is no lean (or no
    informative pair). Doubling the smaller one-sided tail is the standard exact-test convention; it never
    understates the two-sided p, and it is capped at 1.

    This exists for the verdict's counter-leg, which is TWO-SIDED by pre-registration: the question asked is
    symmetric ("is either system significantly ahead on this dimension?"), and only its ANSWER is then read
    for direction. A one-sided counter-leg pointed at the predecessor would be the same test with half the
    burden, chosen by the party being measured.
    """
    w_plus, p_greater, n = wilcoxon_signed_rank(diffs)
    _, p_less, _ = wilcoxon_signed_rank([-d for d in diffs])
    p_two = min(1.0, 2.0 * min(p_greater, p_less))
    if n == 0 or p_greater == p_less:
        direction = "none"
    elif p_greater < p_less:
        direction = "positive"
    else:
        direction = "negative"
    return w_plus, p_two, n, direction


def cliffs_delta(a: list[float], b: list[float]) -> float:
    """Cliff's delta -- a non-parametric effect size on ordinal data.

    Reported alongside every p-value because significance without an effect size says only that n was large
    enough, never that the difference matters.
    """
    if not a or not b:
        return 0.0
    gt = sum(1 for x in a for y in b if x > y)
    lt = sum(1 for x in a for y in b if x < y)
    return (gt - lt) / float(len(a) * len(b))


def holm(pvalues: list[tuple[str, float]]) -> list[tuple[str, float, float, bool]]:
    """Holm-Bonferroni step-down. Returns (name, raw, adjusted, reject) in the INPUT order.

    Holm rather than Bonferroni: uniformly more powerful at the same family-wise error rate, with no extra
    assumption. Adjusted values are made monotone non-decreasing, which is part of the procedure and not a
    presentational nicety.
    """
    idx = sorted(range(len(pvalues)), key=lambda i: pvalues[i][1])
    m = len(pvalues)
    adj = [0.0] * m
    running = 0.0
    for rank, i in enumerate(idx):
        val = (m - rank) * pvalues[i][1]
        running = max(running, val)
        adj[i] = min(1.0, running)
    return [(pvalues[i][0], pvalues[i][1], adj[i], adj[i] < ALPHA) for i in range(m)]


def cluster_bootstrap_ci(
    clusters: dict[str, list[float]], statistic=lambda xs: sum(xs) / len(xs), conf: float = 0.95
) -> tuple[float, float, float]:
    """Cluster-robust bootstrap CI: resample HOSTS with replacement, not pairs.

    Pairs from one host are correlated -- in the pilot, 7 of 15 shared a host, so the effective independent n
    was nearer 6-8 than 15. Resampling pairs would treat those as independent draws and return an interval too
    narrow, which is the direction that manufactures a finding.
    """
    keys = list(clusters.keys())
    flat = [v for k in keys for v in clusters[k]]
    if not flat:
        return 0.0, 0.0, 0.0
    point = statistic(flat)
    if len(keys) < 2:
        return point, float("nan"), float("nan")
    rng = random.Random(BOOTSTRAP_SEED)
    stats = []
    for _ in range(BOOTSTRAP_N):
        drawn = [rng.choice(keys) for _ in keys]
        vals = [v for k in drawn for v in clusters[k]]
        if vals:
            stats.append(statistic(vals))
    stats.sort()
    lo = stats[int((1 - conf) / 2 * len(stats))]
    hi = stats[min(len(stats) - 1, int((1 + conf) / 2 * len(stats)))]
    return point, lo, hi


# ---------------------------------------------------------------------------
# The composite verdict (PRE-REGISTRATION.md section 7) -- frozen 2026-07-30 at
# ZERO accrued confirmatory pairs. The CLOSED input set is the point.
# ---------------------------------------------------------------------------


def composite_verdict(
    *,
    powered: bool,
    mcnemar_p: float | None,
    tg_only_correct: int,
    pred_only_correct: int,
    counter_leg: dict[str, dict],
    primary_diff_ci_low: float | None,
) -> dict:
    """Map the frozen statistics to exactly one of the three campaign verdicts.

    The inputs are a CLOSED keyword-only set, and that closure is the structural guarantee this function
    exists to give: there is no parameter through which falsifiable_prediction, a unilateral axis
    (A3/A4/A5/A7/A8), or any other TG-only number can reach the decision. The 2026-07-26 rejudge showed both
    of TG's apparent pooled wins were manufactured by exactly such a one-sided dimension; the remedy is a
    function that cannot receive the data, not a convention that promises not to look at it.

    counter_leg carries ONLY the like-for-like dimensions (COUNTER_LEG_DIMENSIONS), each entry shaped
    {"p_holm": two-sided Holm-adjusted p, "favors": "tg"|"pred"|"none"}. Any other dimension key is an
    ERROR, never filtered on a shrug -- silently dropping it would let a caller believe the dimension was
    considered.

    The rule (section 7, verbatim in intent):
      TG EXCEEDS  iff powered (both section-3 populations clear the minimums) AND the primary judge-free
                  McNemar exact two-sided p < ALPHA with the discordant count favouring TG (b > c) AND no
                  counter-leg dimension is significantly in the predecessor's favour (two-sided, Holm at
                  family ALPHA).
      TG MATCHES  iff not EXCEEDS, powered, and the host-clustered bootstrap CI on the primary correct-rate
                  difference excludes a predecessor advantage greater than NONINFERIORITY_MARGIN
                  (ci_low >= -NONINFERIORITY_MARGIN). An uncomputable interval (None/NaN -- no ground truth,
                  or a single host cluster) FAILS CLOSED: it certifies nothing.
      PREDECESSOR HOLDS otherwise -- including every unpowered run. The burden of proof is TG's, and an
                  unpowered campaign cannot discharge it in either direction; the render additionally marks
                  such a run NOT A CONFIRMATORY RESULT.
    """
    unexpected = sorted(set(counter_leg) - set(COUNTER_LEG_DIMENSIONS))
    if unexpected:
        raise ValueError(
            f"counter_leg may only carry {COUNTER_LEG_DIMENSIONS}; got extra {unexpected}. A one-sided "
            "dimension (falsifiable_prediction) or a unilateral axis can NEVER enter the verdict -- that is "
            "the exact gaming the 2026-07-26 rejudge documented, and it is refused loudly here rather than "
            "filtered quietly."
        )

    counter_leg_blocking = sorted(
        dim
        for dim, entry in counter_leg.items()
        if entry.get("p_holm", 1.0) < ALPHA and entry.get("favors") == "pred"
    )
    primary_significant = mcnemar_p is not None and mcnemar_p < ALPHA
    discordant_favor_tg = tg_only_correct > pred_only_correct
    # None never compares; NaN fails every comparison -- both directions fail closed.
    non_inferior = primary_diff_ci_low is not None and primary_diff_ci_low >= -NONINFERIORITY_MARGIN

    reasons: list[str] = []
    if powered and primary_significant and discordant_favor_tg and not counter_leg_blocking:
        verdict = VERDICT_EXCEEDS
        reasons.append(
            f"powered; primary McNemar exact two-sided p={mcnemar_p:.4g} < {ALPHA} with "
            f"b={tg_only_correct} > c={pred_only_correct}; no like-for-like dimension is significantly in "
            "the predecessor's favour (two-sided, Holm)"
        )
    else:
        if not powered:
            reasons.append(
                "not powered: the section-3 population minimums are not met, so neither EXCEEDS nor "
                "MATCHES can be claimed -- the burden of proof is TG's and this run cannot carry it"
            )
        if mcnemar_p is None:
            reasons.append("primary endpoint not computed (no ground truth) -- EXCEEDS is unreachable")
        elif not primary_significant:
            reasons.append(f"primary McNemar exact two-sided p={mcnemar_p:.4g} >= {ALPHA} -- not EXCEEDS")
        elif not discordant_favor_tg:
            reasons.append(
                f"discordant count does not favour TG (b={tg_only_correct} <= c={pred_only_correct}) -- "
                "not EXCEEDS"
            )
        if counter_leg_blocking:
            reasons.append(
                "like-for-like counter-leg significantly favours the predecessor on: "
                + ", ".join(counter_leg_blocking)
                + " -- not EXCEEDS"
            )
        if powered and non_inferior:
            verdict = VERDICT_MATCHES
            reasons.append(
                f"host-clustered bootstrap CI lower bound on the primary correct-rate difference "
                f"({primary_diff_ci_low:+.4f}) excludes a predecessor advantage greater than "
                f"{NONINFERIORITY_MARGIN:.0%} -- non-inferior"
            )
        else:
            verdict = VERDICT_HOLDS
            if powered:
                shown = "not computable" if primary_diff_ci_low is None else f"{primary_diff_ci_low:+.4f}"
                reasons.append(
                    f"non-inferiority not shown: CI lower bound {shown} does not exclude a predecessor "
                    f"advantage greater than {NONINFERIORITY_MARGIN:.0%}"
                )

    return {
        "verdict": verdict,
        "confirmatory": bool(powered),
        "rule": "PRE-REGISTRATION.md section 7, frozen inside analyze.py",
        "powered": bool(powered),
        "primary_p": mcnemar_p,
        "tg_only_correct": tg_only_correct,
        "pred_only_correct": pred_only_correct,
        "counter_leg": counter_leg,
        "counter_leg_blocking": counter_leg_blocking,
        "noninferiority_ci_low": primary_diff_ci_low,
        "noninferiority_margin": NONINFERIORITY_MARGIN,
        "reasons": reasons,
    }


# ---------------------------------------------------------------------------
# Population eligibility -- applied BEFORE any statistic is computed.
# ---------------------------------------------------------------------------


def _incident_ts(p: dict) -> "_dt.datetime | None":
    """A pair's injection/incident time as an aware datetime: the TG triage row's createdAt, else the
    predecessor's first ts (mirrors reconcile-supply.py's derivation). None when neither field is present or
    parseable. Handles both `...Z` and `+00:00` offsets; a naive stamp is read as UTC."""
    raw = p.get("tg_created_at") or p.get("pred_first_ts")
    if not raw:
        return None
    # Normalise EXACTLY as reconcile-supply.py's _driver._parse_ts does, so the analysis classifies a pair on
    # the same instant reconcile did: strip, `Z`->+00:00, and a space date/time separator -> `T`. (We cannot
    # import _driver — it reads os.environ["WORK"] at module load — so the normalisation is replicated here.)
    s = str(raw).strip().replace("Z", "+00:00").replace(" ", "T", 1)
    try:
        dt = _dt.datetime.fromisoformat(s)
    except ValueError:
        return None
    return dt if dt.tzinfo else dt.replace(tzinfo=_dt.timezone.utc)


_ACCRUE_FROM_DT = _dt.datetime.fromisoformat(ACCRUE_FROM.replace("Z", "+00:00"))


def enforce_population(pairs: list[dict], manifest_keys: set[str] | None = None) -> tuple[list[dict], list[str]]:
    """Apply the pre-registered inclusion rules and REPORT every exclusion.

    Capping pairs per host is the load-bearing rule: without it one host can dominate the sample and a
    "30-pair" result rests on a handful of independent observations. The cap keeps the EARLIEST pairs per host
    by judged_at, a rule fixed in advance so it cannot be steered by outcome.

    The ACCRUAL BOUNDARY runs BEFORE the cap (TG-249 item 10): the confirmatory population is pairs INJECTED
    at/after ACCRUE_FROM. Without it a disowned pre-freeze PILOT pair — injected earlier, hence sorting first
    by judged_at — is admitted and evicts a real confirmatory pair out of a host's cap. A pair whose injection
    time cannot be established is excluded (fail-closed: an unprovable pair is not counted as confirmatory).

    MANIFEST MEMBERSHIP defines the confirmatory population (TG-526, §6 2026-08-25): the boundary is FAULT
    time and the pair record carries no fault-time field, so a timestamp proxy still admits ORGANIC
    post-boundary pairs — which occupy per-host cap slots and evict ground-truth-carrying pairs before the
    downstream _gt_for join drops them. `manifest_keys` is the set of scorecard keys reconcile-supply.py
    matched to post-boundary INJECTED faults (confirmatory/manifest.jsonl); only members are confirmatory.
    None means the caller supplied no manifest — the cap then runs un-joined and analyze() refuses to call
    the run powered (a population that cannot prove its membership rule is not a confirmatory population).
    """
    notes: list[str] = []
    usable = [p for p in pairs if not p.get("judge_unavailable")]
    if len(usable) != len(pairs):
        notes.append(f"excluded {len(pairs) - len(usable)} pair(s): judge unavailable")

    both = [p for p in usable if sorted(p.get("present_systems", [])) == ["pred", "tg"]]
    if len(both) != len(usable):
        notes.append(f"excluded {len(usable) - len(both)} pair(s): not two-sided (one system absent)")

    accrued = []
    for p in both:
        ts = _incident_ts(p)
        if ts is not None and ts >= _ACCRUE_FROM_DT:
            accrued.append(p)
    if len(accrued) != len(both):
        notes.append(
            f"excluded {len(both) - len(accrued)} pair(s): injected before the accrual boundary {ACCRUE_FROM}"
        )

    if manifest_keys is not None:
        member = [p for p in accrued if (p.get("key") or "") in manifest_keys]
        if len(member) != len(accrued):
            notes.append(
                f"excluded {len(accrued) - len(member)} pair(s): not manifest-joined to a post-boundary "
                "injected fault (organic — §5 contamination-control arm, no injector ground truth; TG-526)"
            )
        accrued = member
    else:
        notes.append(
            "population NOT manifest-joined (no confirmatory manifest supplied) — organic pairs may occupy "
            "cap slots; this run cannot be confirmatory (TG-526, §6 2026-08-25)"
        )

    by_host: dict[str, list[dict]] = defaultdict(list)
    for p in sorted(accrued, key=lambda r: r.get("judged_at", "")):
        by_host[p.get("subject_host", "?")].append(p)
    kept, dropped = [], 0
    for host, rows in by_host.items():
        kept.extend(rows[:MAX_PAIRS_PER_HOST])
        dropped += max(0, len(rows) - MAX_PAIRS_PER_HOST)
    if dropped:
        notes.append(f"excluded {dropped} pair(s): more than {MAX_PAIRS_PER_HOST} per host (earliest kept)")
    return kept, notes


def unblind(pair: dict, dim: str) -> tuple[float | None, float | None]:
    """Recover (tg, pred) scores from the blinded A/B mapping. Returns (None, None) if either is missing."""
    mapping = pair.get("mapping") or {}
    dims = pair.get("dims") or {}
    out: dict[str, float | None] = {"tg": None, "pred": None}
    for letter, system in mapping.items():
        v = (dims.get(letter) or {}).get(dim)
        if isinstance(v, (int, float)):
            out[system] = float(v)
    return out.get("tg"), out.get("pred")


# ---------------------------------------------------------------------------
# Reporting
# ---------------------------------------------------------------------------


def self_hash(path: str) -> str:
    with open(path, "rb") as fh:
        return hashlib.sha256(fh.read()).hexdigest()


def analyze(
    pairs: list[dict],
    ground_truth: dict[str, dict] | None = None,
    manifest_keys: set[str] | None = None,
) -> dict:
    kept, notes = enforce_population(pairs, manifest_keys)
    hosts = sorted({p.get("subject_host", "?") for p in kept})

    # THE PRIMARY ENDPOINT HAS ITS OWN POPULATION, and power must be judged on it.
    #
    # `kept` is the JUDGED set. The primary endpoint is scored from ground truth, so its usable population is
    # the SUBSET of kept that carries a ground-truth entry — potentially far smaller. Checking power only on
    # the judged set let a run announce itself adequately powered while the endpoint the whole campaign rests
    # on had a handful of items behind it. The secondary family would be well-powered and the primary, which
    # is the one that decides the claim, would not be, and nothing said so.
    #
    # With no ground-truth file the primary is not computed at all, so the run is NOT powered by definition —
    # the exit criterion asks for the judge-free endpoint, and a secondary-only run does not satisfy it.
    # GROUND-TRUTH KEYING (§6 2026-07-31, error-correction): §1 declares the primary's unit as ONE INJECTED
    # FAULT, but joining on the bare incident_key (the predecessor ISSUE id) cannot express it — recurrence
    # dedup makes one issue back up to 5 distinct faults with different outcomes, so most faults were
    # UNREPRESENTABLE and which ones survived depended on alert-collision patterns (informative missingness).
    # The pair record's `key` (DATE|pair|ISSUE|TGREF) is bijective with the manifest's fault_id, so a
    # ground-truth file keyed by `key` IS per-fault. Bare-incident_key files still join (legacy fallback).
    def _gt_for(p: dict) -> dict | None:
        if not ground_truth:
            return None
        return ground_truth.get(p.get("key") or "") or ground_truth.get(p.get("incident_key") or "")

    gt_items = [p for p in kept if _gt_for(p)]
    gt_hosts = sorted({p.get("subject_host", "?") for p in gt_items})

    report: dict = {
        "analysis_sha256": self_hash(__file__),
        "n_pairs_submitted": len(pairs),
        "n_pairs_analyzed": len(kept),
        "n_hosts": len(hosts),
        "n_primary_items": len(gt_items),
        "n_primary_hosts": len(gt_hosts),
        "exclusions": notes,
        # The population's membership rule is part of power (TG-526, §6 2026-08-25): a run whose cap was not
        # manifest-joined may have evicted ground-truth pairs behind organic ones, so it cannot be powered.
        "manifest_joined": manifest_keys is not None,
        "powered": (
            manifest_keys is not None
            and len(kept) >= MIN_PAIRS
            and len(hosts) >= MIN_HOSTS
            and len(gt_items) >= MIN_PAIRS
            and len(gt_hosts) >= MIN_HOSTS
        ),
        "power_shortfall": [],
        "primary": None,
        "secondary": [],
    }
    if manifest_keys is None:
        report["power_shortfall"].append(
            "population not manifest-joined (no confirmatory manifest supplied) — the membership rule of the "
            "confirmatory population could not be applied, so this run cannot be confirmatory (TG-526)"
        )
    if not ground_truth:
        report["power_shortfall"].append(
            "no ground truth supplied — the PRIMARY (judge-free) endpoint is not computed, so this run cannot "
            "be a confirmatory result however many judged pairs it carries"
        )
    else:
        if len(gt_items) < MIN_PAIRS:
            report["power_shortfall"].append(
                f"PRIMARY endpoint: {len(gt_items)} ground-truth item(s) < the minimum {MIN_PAIRS} "
                f"(the judged set has {len(kept)} — power on the secondary family does not transfer)"
            )
        if len(gt_hosts) < MIN_HOSTS:
            report["power_shortfall"].append(
                f"PRIMARY endpoint: {len(gt_hosts)} host(s) < the minimum {MIN_HOSTS}"
            )
    if len(kept) < MIN_PAIRS:
        report["power_shortfall"].append(f"{len(kept)} pairs < the pre-registered minimum {MIN_PAIRS}")
    if len(hosts) < MIN_HOSTS:
        report["power_shortfall"].append(f"{len(hosts)} hosts < the pre-registered minimum {MIN_HOSTS}")

    # PRIMARY -- judge-free, ground truth from the injector's record.
    if ground_truth:
        b = c = concordant = 0
        for p in kept:
            gt = _gt_for(p)
            if not gt or "tg_correct" not in gt or "pred_correct" not in gt:
                continue
            if gt["tg_correct"] and not gt["pred_correct"]:
                b += 1
            elif gt["pred_correct"] and not gt["tg_correct"]:
                c += 1
            else:
                concordant += 1
        p_value, n_disc = mcnemar_exact(b, c)
        report["primary"] = {
            "endpoint": "correct op-class vs the INJECTED fault (injector ground truth; no judge)",
            "test": "McNemar exact",
            "tg_only_correct": b,
            "pred_only_correct": c,
            "concordant": concordant,
            "n_discordant": n_disc,
            "p_value": p_value,
            "reject_null": p_value < ALPHA,
        }
    else:
        report["primary"] = {
            "endpoint": "correct op-class vs the INJECTED fault (injector ground truth; no judge)",
            "status": "NOT COMPUTED — no ground-truth file supplied. The primary endpoint is judge-free by "
            "design; without it this run reports SECONDARY endpoints only and is not a confirmatory result.",
        }

    # SECONDARY -- the judged rubric dimensions.
    raw: list[tuple[str, float]] = []
    per_dim: dict[str, dict] = {}
    for dim in DIMENSIONS:
        diffs, tg_scores, pred_scores = [], [], []
        clusters: dict[str, list[float]] = defaultdict(list)
        for p in kept:
            tg, pred = unblind(p, dim)
            if tg is None or pred is None:
                continue
            diffs.append(tg - pred)
            tg_scores.append(tg)
            pred_scores.append(pred)
            clusters[p.get("subject_host", "?")].append(tg - pred)
        if not diffs:
            per_dim[dim] = {"n": 0, "status": "no pair scored this dimension on both sides"}
            continue
        w, p_one_sided, n_nz = wilcoxon_signed_rank(diffs)
        point, lo, hi = cluster_bootstrap_ci(clusters)
        per_dim[dim] = {
            "n": len(diffs),
            "n_informative": n_nz,
            "W": w,
            "mean_diff_tg_minus_pred": point,
            "ci95_cluster_bootstrap": [lo, hi],
            "cliffs_delta": cliffs_delta(tg_scores, pred_scores),
            "p_raw_one_sided": p_one_sided,
        }
        raw.append((dim, p_one_sided))

    for name, praw, padj, rej in holm(raw):
        per_dim[name]["p_holm_adjusted"] = padj
        per_dim[name]["reject_null"] = rej
    report["secondary"] = per_dim

    # COMPOSITE VERDICT (section 7) -- computed HERE, inside the frozen file, never applied by hand to the
    # numbers afterwards. The counter-leg is the like-for-like guard: a primary win may not stand on ground
    # the judged like-for-like evidence significantly contradicts.
    counter_leg: dict[str, dict] = {}
    counter_family: list[tuple[str, float]] = []
    for dim in COUNTER_LEG_DIMENSIONS:
        diffs = []
        for p in kept:
            tg, pred = unblind(p, dim)
            if tg is None or pred is None:
                continue
            diffs.append(tg - pred)
        _w, p_two, n_inf, direction = wilcoxon_two_sided(diffs)
        counter_leg[dim] = {
            "n": len(diffs),
            "n_informative": n_inf,
            "p_two_sided": p_two,
            "favors": {"positive": "tg", "negative": "pred"}.get(direction, "none"),
        }
        counter_family.append((dim, p_two))
    for name, _praw, padj, rej in holm(counter_family):
        counter_leg[name]["p_holm"] = padj
        counter_leg[name]["significant"] = rej

    # Non-inferiority input: the host-clustered bootstrap CI on the primary correct-rate difference
    # (TG minus predecessor, rate units), over the SAME ground-truth items as the McNemar test. Guarded so an
    # empty population yields NO interval rather than a degenerate [0, 0] that would certify non-inferiority
    # out of nothing.
    diff_ci = None
    if ground_truth:
        gt_clusters: dict[str, list[float]] = defaultdict(list)
        for p in kept:
            gt = _gt_for(p)  # per-fault key (TG-249 item 11), the same join as the primary McNemar leg
            if not gt or "tg_correct" not in gt or "pred_correct" not in gt:
                continue
            gt_clusters[p.get("subject_host", "?")].append(
                (1.0 if gt["tg_correct"] else 0.0) - (1.0 if gt["pred_correct"] else 0.0)
            )
        if gt_clusters:
            diff_ci = cluster_bootstrap_ci(gt_clusters)
    if diff_ci is not None:
        report["primary_rate_difference"] = {
            "endpoint": "primary correct-rate difference, TG minus predecessor (rate units)",
            "point": diff_ci[0],
            "ci95_cluster_bootstrap": [diff_ci[1], diff_ci[2]],
        }

    pri = report["primary"]
    report["verdict"] = composite_verdict(
        powered=report["powered"],
        mcnemar_p=pri.get("p_value"),
        tg_only_correct=pri.get("tg_only_correct", 0),
        pred_only_correct=pri.get("pred_only_correct", 0),
        counter_leg=counter_leg,
        primary_diff_ci_low=(diff_ci[1] if diff_ci is not None else None),
    )

    # UNILATERAL TG-ONLY PROPERTIES -- printed and labelled, never compared, never in the verdict.
    fp_scores = []
    for p in kept:
        tg, _pred = unblind(p, "falsifiable_prediction")
        if tg is not None:
            fp_scores.append(tg)
    report["unilateral_tg_properties"] = {
        "falsifiable_prediction": {
            "n": len(fp_scores),
            "tg_mean": round(sum(fp_scores) / len(fp_scores), 3) if fp_scores else None,
        },
        "axes_without_comparator": [f"{code} {name}" for code, name in UNILATERAL_AXES],
        "note": "TG-only properties (docs/BENCHMARK-AXES.md): published labelled as such and STRUCTURALLY "
        "excluded from the verdict -- composite_verdict() has no input that can carry them.",
    }
    return report


def render(report: dict) -> str:
    out = ["EXCEED-PROOF CONFIRMATORY ANALYSIS (pre-registered, frozen)"]
    out.append(f"  analysis sha256: {report['analysis_sha256']}")
    out.append(
        f"  pairs: {report['n_pairs_analyzed']} analyzed of {report['n_pairs_submitted']} submitted, "
        f"across {report['n_hosts']} host(s)"
    )
    out.append(
        f"  PRIMARY endpoint population: {report.get('n_primary_items', 0)} item(s) across "
        f"{report.get('n_primary_hosts', 0)} host(s) — a SUBSET of the judged set, and the one the claim rests on"
    )
    for n in report["exclusions"]:
        out.append(f"    - {n}")
    if not report["powered"]:
        out.append("  ⚠ NOT A CONFIRMATORY RESULT — the pre-registered population minimum is not met:")
        for s in report["power_shortfall"]:
            out.append(f"      {s}")
        out.append("    Numbers below are DESCRIPTIVE only and must not be published as an exceed-proof.")

    pri = report["primary"]
    out.append("  PRIMARY (judge-free) — " + pri["endpoint"])
    if "status" in pri:
        out.append(f"    {pri['status']}")
    else:
        out.append(
            f"    TG-only-correct={pri['tg_only_correct']}  pred-only-correct={pri['pred_only_correct']}  "
            f"concordant={pri['concordant']}"
        )
        out.append(
            f"    McNemar exact on {pri['n_discordant']} discordant pair(s): p={pri['p_value']:.4f}  "
            f"{'REJECT null' if pri['reject_null'] else 'no significant difference'}"
        )

    out.append("  SECONDARY (judged rubric, Holm-corrected across the family)")
    for dim in DIMENSIONS:
        d = report["secondary"].get(dim, {})
        if not d or d.get("n", 0) == 0:
            out.append(f"    - {dim:24s} n=0 — {d.get('status', 'absent')}")
            continue
        lo, hi = d["ci95_cluster_bootstrap"]
        out.append(
            f"    - {dim:24s} n={d['n']:3d}  mean Δ(TG−pred)={d['mean_diff_tg_minus_pred']:+.3f} "
            f"(CI {lo:+.3f}..{hi:+.3f})  δ={d['cliffs_delta']:+.3f}  "
            f"p={d['p_raw_one_sided']:.4f} → Holm {d['p_holm_adjusted']:.4f} "
            f"{'REJECT' if d.get('reject_null') else 'ns'}"
        )
    out.append(
        "  NOTE: CIs are cluster-bootstrapped BY HOST, not by pair — pairs from one host are not independent."
    )

    prd = report.get("primary_rate_difference")
    if prd:
        lo, hi = prd["ci95_cluster_bootstrap"]
        out.append(
            f"  PRIMARY correct-rate difference (TG−pred): {prd['point']:+.3f} "
            f"(CI {lo:+.3f}..{hi:+.3f}, host-clustered) — non-inferiority margin {-NONINFERIORITY_MARGIN:+.2f}"
        )

    v = report["verdict"]
    out.append(f"  COMPOSITE VERDICT (frozen decision rule, PRE-REGISTRATION §7): {v['verdict']}")
    for dim in COUNTER_LEG_DIMENSIONS:
        e = v["counter_leg"][dim]
        out.append(
            f"    counter-leg {dim:20s} n={e['n']:3d}  two-sided p={e['p_two_sided']:.4f} → "
            f"Holm {e['p_holm']:.4f}  favors={e['favors']}"
            + ("  ← significantly favours the predecessor" if dim in v["counter_leg_blocking"] else "")
        )
    for r in v["reasons"]:
        out.append(f"    - {r}")

    uni = report["unilateral_tg_properties"]
    fp = uni["falsifiable_prediction"]
    out.append("  UNILATERAL TG-ONLY PROPERTIES — labelled, printed, and STRUCTURALLY OUTSIDE the verdict:")
    out.append(
        f"    - falsifiable_prediction: TG mean {fp['tg_mean']} over n={fp['n']} (the predecessor "
        "structurally commits no predictions — a property, never a win)"
    )
    out.append(
        "    - axes with no comparator (docs/BENCHMARK-AXES.md): "
        + "; ".join(f"{c} {n}" for c, n in UNILATERAL_AXES)
        + " — published as TG properties only, never as a comparison in either direction"
    )
    return "\n".join(out)


def load_manifest_keys(path: str) -> set[str] | None:
    """The scorecard keys of PAIRED confirmatory-manifest records at/after the boundary (TG-526).

    Returns None when the manifest file does not exist — the caller must be able to tell "no manifest"
    (population rule inapplicable → never powered) from "manifest present, zero members" (an honest empty
    population). The fault time is the record's `ts` (reconcile-supply.py writes injected_at there).
    """
    if not os.path.exists(path):
        return None
    keys: set[str] = set()
    with open(path, encoding="utf-8") as fh:
        for line in fh:
            line = line.strip()
            if not line:
                continue
            try:
                r = json.loads(line)
            except json.JSONDecodeError:
                continue
            if r.get("status") != "PAIRED":
                continue
            # Parsed comparison, not string order: the sole producer (reconcile-supply's _iso_z) emits Z-form,
            # but a +00:00-form ts from any future writer would string-sort before "Z" and wrongly exclude a
            # boundary row. An unparseable ts is excluded (fail-closed, like an unprovable pair).
            try:
                ts = _dt.datetime.fromisoformat((r.get("ts") or "").replace("Z", "+00:00"))
            except ValueError:
                continue
            if ts.tzinfo is None:
                ts = ts.replace(tzinfo=_dt.timezone.utc)
            if ts < _ACCRUE_FROM_DT:
                continue
            keys.update(k for k in (r.get("scorecard_keys") or []) if k)
    return keys


def main() -> int:
    ap = argparse.ArgumentParser(description="Frozen confirmatory analysis for the exceed-proof.")
    ap.add_argument("pairs", nargs="?", help="JSONL of judged pairs")
    ap.add_argument("--ground-truth", help="JSON map incident_key -> {tg_correct, pred_correct}")
    ap.add_argument("--manifest", default=None,
                    help="confirmatory manifest JSONL (reconcile-supply.py's fault→pair join); default: "
                         "confirmatory/manifest.jsonl beside this file. The population keeps only pairs the "
                         "manifest joins to a post-boundary injected fault (TG-526); without a readable "
                         "manifest the run can never be powered.")
    ap.add_argument("--json", action="store_true", help="emit the raw report as JSON")
    ap.add_argument("--print-hash", action="store_true", help="print this file's SHA-256 and exit")
    args = ap.parse_args()

    if args.print_hash:
        print(self_hash(__file__))
        return 0
    if not args.pairs:
        ap.error("a pairs file is required unless --print-hash is given")

    with open(args.pairs) as fh:
        pairs = [json.loads(line) for line in fh if line.strip()]
    gt = None
    if args.ground_truth:
        with open(args.ground_truth) as fh:
            gt = json.load(fh)

    manifest_path = args.manifest or os.path.join(os.path.dirname(os.path.abspath(__file__)),
                                                  "confirmatory", "manifest.jsonl")
    manifest_keys = load_manifest_keys(manifest_path)

    report = analyze(pairs, gt, manifest_keys)
    print(json.dumps(report, indent=2) if args.json else render(report))
    return 0


if __name__ == "__main__":
    sys.exit(main())
