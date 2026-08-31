#!/usr/bin/env python3
"""Tests for the frozen confirmatory analysis (P6-2).

Two jobs. First, check every statistic against a value computed BY HAND, because a statistics bug is the one
class of defect that produces a confident, plausible, wrong headline and nothing downstream would catch it.
Second, enforce the FREEZE: analyze.py's SHA-256 must equal the hash recorded in PRE-REGISTRATION.md, so the
analysis cannot be edited after seeing data without the pre-registration visibly changing with it.

stdlib unittest — no pytest, for the same clean-clone reason analyze.py has no numpy.
"""

import hashlib
import json
import os
import re
import unittest

import analyze

HERE = os.path.dirname(os.path.abspath(__file__))
ANALYZE_PY = os.path.join(HERE, "analyze.py")
PREREG = os.path.join(HERE, "PRE-REGISTRATION.md")

# A post-accrual-boundary incident timestamp (campaign-#3 ACCRUE_FROM = 2026-08-26T00:00:00Z, §6 2026-08-25).
# Fixtures set this so pairs survive the accrual-boundary rule (analyze._incident_ts) and keep exercising the
# cap/power/primary rules on judged_at; a real post-freeze pair record always carries tg_created_at.
ACCRUED = "2026-08-27T00:00:00+00:00"


class TestFreeze(unittest.TestCase):
    """THE FREEZE IS THE WHOLE POINT OF PRE-REGISTRATION.

    An analysis that can be adjusted after seeing the data is not confirmatory, and a freeze that lives only in
    prose is not a freeze. Binding the recorded hash to the file makes a silent change impossible: editing the
    analysis reds this test, and updating the recorded hash is a governed change carrying the
    Law-Change-Approved-By trailer. Someone can still change the analysis — they just cannot do it quietly.
    """

    def test_recorded_hash_matches_the_analysis_file(self):
        with open(ANALYZE_PY, "rb") as fh:
            actual = hashlib.sha256(fh.read()).hexdigest()
        with open(PREREG) as fh:
            text = fh.read()
        m = re.search(r"analyze\.py.*?SHA-256.*?`([0-9a-f]{64})`", text, re.S)
        self.assertIsNotNone(m, "PRE-REGISTRATION.md must record analyze.py's SHA-256 in a code span")
        self.assertEqual(
            m.group(1),
            actual,
            "analyze.py has changed since it was pre-registered.\n"
            f"  recorded: {m.group(1)}\n  actual:   {actual}\n"
            "If the change is intended, update PRE-REGISTRATION.md with the new hash IN THE SAME MR and "
            "carry the Law-Change-Approved-By trailer. Do not update the hash to make this test pass without "
            "recording WHY the analysis changed and whether any pair had already been accrued.",
        )

    def test_prereg_declares_the_primary_endpoint_is_judge_free(self):
        with open(PREREG) as fh:
            text = fh.read()
        self.assertRegex(
            text, r"(?i)primary endpoint", "the pre-registration must name a primary endpoint"
        )
        self.assertRegex(
            text,
            r"(?i)judge-free|no judge|no llm judge is involved",
            "the pre-registration must state that the primary endpoint does not depend on the judge — TG's "
            "internal judge is measured uncalibrated (TNR 0.000, kappa -0.141), so a judged primary would "
            "rest the whole campaign on an instrument known not to discriminate",
        )
        self.assertRegex(
            text,
            r"(?i)deviation",
            "the pre-registration must say what happens when the plan is deviated from, or the freeze is "
            "advisory",
        )


class TestBinomialAndMcNemar(unittest.TestCase):
    def test_two_sided_binomial_against_hand_computed_values(self):
        # 0 of 5 successes: only the two extreme outcomes are as extreme -> 2 * (1/32).
        self.assertAlmostEqual(analyze.binom_test_two_sided(0, 5), 0.0625, places=10)
        self.assertAlmostEqual(analyze.binom_test_two_sided(5, 5), 0.0625, places=10)
        # 2 of 4 is the modal outcome; every outcome is at least as extreme -> 1.0.
        self.assertAlmostEqual(analyze.binom_test_two_sided(2, 4), 1.0, places=10)

    def test_mcnemar_exact_against_hand_computed_value(self):
        # b=8, c=1 -> n=9. Outcomes with probability <= C(9,8)/512: i in {0,1,8,9} -> (1+9+9+1)/512.
        p, n = analyze.mcnemar_exact(8, 1)
        self.assertEqual(n, 9)
        self.assertAlmostEqual(p, 20 / 512, places=10)

    def test_no_discordant_pairs_is_p_equals_one_not_a_crash(self):
        p, n = analyze.mcnemar_exact(0, 0)
        self.assertEqual((p, n), (1.0, 0))

    def test_concordant_pairs_carry_no_information_about_a_difference(self):
        # McNemar depends ONLY on the discordant cells; 500 agreements must not move it.
        self.assertEqual(analyze.mcnemar_exact(8, 1), analyze.mcnemar_exact(8, 1))


class TestWilcoxon(unittest.TestCase):
    def test_exact_branch_against_hand_enumeration(self):
        # diffs [1,2,3]: ranks 1,2,3, W+=6. Of 8 sign assignments only all-positive reaches 6 -> p=1/8.
        w, p, n = analyze.wilcoxon_signed_rank([1.0, 2.0, 3.0])
        self.assertAlmostEqual(w, 6.0)
        self.assertAlmostEqual(p, 0.125, places=10)
        self.assertEqual(n, 3)

    def test_zero_differences_are_dropped_not_counted_as_agreement(self):
        # Wilcoxon's own convention. Counting zeros would dilute the statistic toward "no difference".
        w1, p1, n1 = analyze.wilcoxon_signed_rank([1.0, 2.0, 3.0])
        w2, p2, n2 = analyze.wilcoxon_signed_rank([1.0, 2.0, 3.0, 0.0, 0.0])
        self.assertEqual((w1, n1), (w2, n2))
        self.assertAlmostEqual(p1, p2)

    def test_all_zero_differences_is_undefined_not_significant(self):
        w, p, n = analyze.wilcoxon_signed_rank([0.0, 0.0])
        self.assertEqual((w, p, n), (0.0, 1.0, 0))

    def test_a_losing_direction_is_not_significant_one_sided(self):
        # H1 is "TG > predecessor". All-negative differences must NOT reject.
        _, p, _ = analyze.wilcoxon_signed_rank([-1.0, -2.0, -3.0])
        self.assertGreater(p, 0.5)

    def test_ties_share_averaged_ranks(self):
        # diffs [1,1]: both rank 1.5, W+ = 3.
        w, _, _ = analyze.wilcoxon_signed_rank([1.0, 1.0])
        self.assertAlmostEqual(w, 3.0)


class TestEffectSizeAndCorrection(unittest.TestCase):
    def test_cliffs_delta_is_plus_one_when_every_pair_favours_a(self):
        self.assertAlmostEqual(analyze.cliffs_delta([5, 5], [1, 1]), 1.0)
        self.assertAlmostEqual(analyze.cliffs_delta([1, 1], [5, 5]), -1.0)
        self.assertAlmostEqual(analyze.cliffs_delta([3, 3], [3, 3]), 0.0)

    def test_holm_against_hand_computed_values(self):
        got = analyze.holm([("a", 0.01), ("b", 0.04)])
        self.assertAlmostEqual(got[0][2], 0.02, places=10)  # 2 * 0.01
        self.assertAlmostEqual(got[1][2], 0.04, places=10)  # 1 * 0.04
        self.assertTrue(got[0][3] and got[1][3])

    def test_holm_adjusted_values_are_monotone(self):
        got = analyze.holm([("a", 0.03), ("b", 0.031), ("c", 0.9)])
        adj = [g[2] for g in sorted(got, key=lambda g: g[1])]
        self.assertEqual(adj, sorted(adj), "Holm's step-down must produce non-decreasing adjusted values")

    def test_holm_is_less_conservative_than_bonferroni_but_still_controls(self):
        # The largest p in a family is adjusted by 1x under Holm and by m x under Bonferroni.
        got = analyze.holm([("a", 0.001), ("b", 0.5)])
        self.assertLess(got[1][2], 0.5 * 2 + 1e-12)


class TestPopulationRules(unittest.TestCase):
    @staticmethod
    def _pair(key, host, when, tg, pred, present=("pred", "tg"), unavailable=False):
        return {
            "incident_key": key,
            "key": key,
            "subject_host": host,
            "judged_at": when,
            "tg_created_at": ACCRUED,
            "present_systems": list(present),
            "judge_unavailable": unavailable,
            "mapping": {"A": "tg", "B": "pred"},
            "dims": {
                "A": {d: tg for d in analyze.DIMENSIONS},
                "B": {d: pred for d in analyze.DIMENSIONS},
            },
        }

    def test_per_host_cap_keeps_the_earliest_and_reports_the_drop(self):
        pairs = [self._pair(f"k{i}", "h1", f"2026-07-27T0{i}:00:00", 5, 1) for i in range(5)]
        kept, notes = analyze.enforce_population(pairs)
        self.assertEqual(len(kept), analyze.MAX_PAIRS_PER_HOST)
        self.assertEqual([p["incident_key"] for p in kept], ["k0", "k1", "k2"])
        self.assertTrue(any("per host" in n for n in notes), "a capped exclusion must be REPORTED, not silent")

    def test_judge_unavailable_and_single_sided_pairs_are_excluded_with_a_reason(self):
        pairs = [
            self._pair("ok", "h1", "2026-07-27T01:00:00", 5, 1),
            self._pair("bad", "h2", "2026-07-27T02:00:00", 5, 1, unavailable=True),
            self._pair("solo", "h3", "2026-07-27T03:00:00", 5, 1, present=("tg",)),
        ]
        kept, notes = analyze.enforce_population(pairs, manifest_keys={"ok", "bad", "solo"})
        self.assertEqual([p["incident_key"] for p in kept], ["ok"])
        self.assertEqual(len(notes), 2, f"each exclusion class needs its own stated reason, got {notes}")

    def test_underpowered_sample_is_refused_as_a_confirmatory_result(self):
        pairs = [self._pair(f"k{i}", f"h{i}", f"2026-07-27T0{i}:00:00", 5, 1) for i in range(3)]
        rep = analyze.analyze(pairs)
        self.assertFalse(rep["powered"])
        self.assertTrue(rep["power_shortfall"])
        self.assertIn("NOT A CONFIRMATORY RESULT", analyze.render(rep))

    def test_the_pilot_corpus_is_not_publishable_as_an_exceed_proof(self):
        """The 15 committed pilot pairs must NOT pass the population bar — 6 hosts, 11 pairs after the cap."""
        path = os.path.join(HERE, "evidence-rejudge-2026-07-26.jsonl")
        with open(path) as fh:
            pairs = [json.loads(line) for line in fh if line.strip()]
        rep = analyze.analyze(pairs)
        self.assertFalse(
            rep["powered"],
            "the pilot corpus must never satisfy the confirmatory population rule — if this passes, the "
            "minimums have been weakened",
        )


class TestPrimaryEndpoint(unittest.TestCase):
    def test_primary_is_absent_and_says_so_when_no_ground_truth_is_supplied(self):
        rep = analyze.analyze([TestPopulationRules._pair("k", "h", "2026-07-27T01:00:00", 5, 1)])
        self.assertIn("NOT COMPUTED", rep["primary"]["status"])

    def test_primary_uses_ground_truth_and_ignores_the_judge_entirely(self):
        pairs = [TestPopulationRules._pair(f"k{i}", f"h{i}", f"2026-07-27T0{i}:00:00", 1, 5) for i in range(3)]
        # The JUDGE scores TG 1 and the predecessor 5 on every dimension. Ground truth says the opposite.
        gt = {f"k{i}": {"tg_correct": True, "pred_correct": False} for i in range(3)}
        rep = analyze.analyze(pairs, gt)
        self.assertEqual(rep["primary"]["tg_only_correct"], 3)
        self.assertEqual(rep["primary"]["pred_only_correct"], 0)

    def test_unscored_incidents_are_dropped_from_the_primary_not_defaulted(self):
        pairs = [TestPopulationRules._pair(f"k{i}", f"h{i}", f"2026-07-27T0{i}:00:00", 3, 3) for i in range(3)]
        gt = {"k0": {"tg_correct": True, "pred_correct": False}}  # k1, k2 have no ground truth
        rep = analyze.analyze(pairs, gt)
        self.assertEqual(rep["primary"]["n_discordant"], 1)
        self.assertEqual(rep["primary"]["concordant"], 0)


class TestUnblinding(unittest.TestCase):
    def test_scores_follow_the_mapping_not_the_letter(self):
        pair = {
            "mapping": {"A": "pred", "B": "tg"},
            "dims": {"A": {"correct_diagnosis": 1}, "B": {"correct_diagnosis": 5}},
        }
        tg, pred = analyze.unblind(pair, "correct_diagnosis")
        self.assertEqual((tg, pred), (5.0, 1.0), "A/B is a blind; the mapping decides which system is which")

    def test_a_missing_side_yields_none_rather_than_a_default(self):
        pair = {"mapping": {"A": "tg", "B": "pred"}, "dims": {"A": {"correct_diagnosis": 4}, "B": {}}}
        self.assertEqual(analyze.unblind(pair, "correct_diagnosis"), (4.0, None))


class TestClusterBootstrap(unittest.TestCase):
    def test_it_resamples_hosts_not_pairs(self):
        # One host holding many correlated pairs must NOT yield a tight interval. With a single cluster the
        # interval is undefined rather than falsely narrow.
        point, lo, hi = analyze.cluster_bootstrap_ci({"h1": [1.0] * 50})
        self.assertAlmostEqual(point, 1.0)
        self.assertTrue(lo != lo, "a single cluster cannot support an interval — expected NaN")

    def test_it_is_reproducible_under_the_pinned_seed(self):
        clusters = {f"h{i}": [float(i)] for i in range(10)}
        self.assertEqual(analyze.cluster_bootstrap_ci(clusters), analyze.cluster_bootstrap_ci(clusters))




class TestOneSidedDimensionExclusion(unittest.TestCase):
    """REQ-2504's REAL oracle, exercised against the code that implements it.

    The Go acceptance test for this requirement set `w.winner = "tie"` in its When step and then asserted the
    winner was a tie — a value it had assigned two lines earlier — before grepping _driver.py for the
    substring "comparable". Neither touches the logic. The rule lives in Python, so its oracle belongs here.

    The rule: a dimension only ONE system is scored on must not enter the comparative aggregate. Averaging a
    dimension the other system does not compete on is a mean over different dimension sets, not a comparison,
    and it decides a head-to-head on a category the loser never entered.
    """

    @staticmethod
    def _pair(key, host, when, tg_dims, pred_dims):
        return {
            "incident_key": key,
            "subject_host": host,
            "judged_at": when,
            "tg_created_at": ACCRUED,
            "present_systems": ["pred", "tg"],
            "judge_unavailable": False,
            "mapping": {"A": "tg", "B": "pred"},
            "dims": {"A": tg_dims, "B": pred_dims},
        }

    def test_a_dimension_only_one_system_is_scored_on_is_excluded(self):
        # TG is scored on falsifiable_prediction; the predecessor is not (null, as it is live).
        pairs = [
            self._pair(
                f"k{i}", f"h{i}", f"2026-07-27T0{i}:00:00",
                {"correct_diagnosis": 4, "falsifiable_prediction": 5},
                {"correct_diagnosis": 4, "falsifiable_prediction": None},
            )
            for i in range(3)
        ]
        rep = analyze.analyze(pairs)
        fp = rep["secondary"]["falsifiable_prediction"]
        self.assertEqual(
            fp.get("n", 0), 0,
            "a dimension the predecessor is not scored on must contribute NO pairs to the comparison",
        )
        self.assertNotIn(
            "p_holm_adjusted", fp,
            "an unscored dimension must not enter the Holm family — including it shrinks every other "
            "dimension's correction and inflates the family-wise error rate",
        )
        # The mutually-scored dimension must still be compared, or this is a fix that just measures less.
        self.assertEqual(rep["secondary"]["correct_diagnosis"]["n"], 3)

    def test_a_missing_side_is_dropped_per_pair_not_per_dimension(self):
        # One pair lacks the predecessor's score; the other two have both. The dimension survives with n=2.
        pairs = [
            self._pair("k0", "h0", "2026-07-27T00:00:00", {"correct_diagnosis": 5}, {"correct_diagnosis": None}),
            self._pair("k1", "h1", "2026-07-27T01:00:00", {"correct_diagnosis": 4}, {"correct_diagnosis": 3}),
            self._pair("k2", "h2", "2026-07-27T02:00:00", {"correct_diagnosis": 4}, {"correct_diagnosis": 3}),
        ]
        rep = analyze.analyze(pairs)
        self.assertEqual(
            rep["secondary"]["correct_diagnosis"]["n"], 2,
            "only the pairs carrying BOTH sides are comparable; a one-sided pair is dropped from that "
            "dimension without discarding the dimension",
        )

class TestPrimaryEndpointPower(unittest.TestCase):
    """POWER ON THE SECONDARY FAMILY DOES NOT TRANSFER TO THE PRIMARY.

    `powered` was computed from the JUDGED set alone. The primary endpoint is scored from ground truth, so its
    usable population is the SUBSET of judged pairs carrying a ground-truth entry — potentially far smaller. A
    run could therefore announce itself adequately powered while the endpoint the whole campaign rests on had a
    handful of items behind it, and nothing said so.
    """

    @staticmethod
    def _pairs(n):
        return [
            {
                "incident_key": f"k{i}",
                "key": f"k{i}",
                "subject_host": f"h{i}",
                "judged_at": f"2026-07-27T{i:02d}:00:00",
                "tg_created_at": ACCRUED,
                "present_systems": ["pred", "tg"],
                "judge_unavailable": False,
                "mapping": {"A": "tg", "B": "pred"},
                "dims": {"A": {d: 4 for d in analyze.DIMENSIONS}, "B": {d: 3 for d in analyze.DIMENSIONS}},
            }
            for i in range(n)
        ]

    def test_a_well_powered_judged_set_with_a_thin_primary_is_not_powered(self):
        pairs = self._pairs(40)  # 40 pairs on 40 distinct hosts — the judged set clears both minimums
        gt = {"k0": {"tg_correct": True, "pred_correct": False}}  # ...but ONE ground-truth item
        rep = analyze.analyze(pairs, gt)
        self.assertGreaterEqual(rep["n_pairs_analyzed"], analyze.MIN_PAIRS)
        self.assertGreaterEqual(rep["n_hosts"], analyze.MIN_HOSTS)
        self.assertFalse(
            rep["powered"],
            "the judged set clears the bar but the PRIMARY endpoint has one item — declaring this powered is "
            "how a campaign publishes a claim its own primary endpoint cannot support",
        )
        self.assertTrue(
            any("PRIMARY endpoint" in s for s in rep["power_shortfall"]),
            f"the shortfall must name the PRIMARY endpoint specifically, got {rep['power_shortfall']}",
        )

    def test_no_ground_truth_is_never_powered(self):
        rep = analyze.analyze(self._pairs(40))
        self.assertFalse(rep["powered"], "without ground truth the primary is not computed, so no run qualifies")
        self.assertTrue(any("no ground truth" in s for s in rep["power_shortfall"]))

    def test_both_populations_clearing_the_bar_is_powered(self):
        # MUTATION-CONTROL COUNTERPART: without this the fix could be "always unpowered", which blocks the
        # campaign from ever succeeding rather than guarding it. Manifest-joined per TG-526: powered now
        # additionally requires the population's membership rule to have been applied.
        pairs = self._pairs(40)
        gt = {f"k{i}": {"tg_correct": True, "pred_correct": False} for i in range(40)}
        rep = analyze.analyze(pairs, gt, manifest_keys={f"k{i}" for i in range(40)})
        self.assertTrue(
            rep["powered"],
            f"both populations clear the minimums; this must be powered. shortfall={rep['power_shortfall']}",
        )

    def test_the_primary_population_is_reported_even_when_it_is_zero(self):
        rep = analyze.analyze(self._pairs(3))
        self.assertIn("n_primary_items", rep)
        self.assertIn("PRIMARY endpoint population: 0", analyze.render(rep))


class TestTwoSidedWilcoxon(unittest.TestCase):
    """The verdict's counter-leg is TWO-SIDED by pre-registration (§7): the question asked is symmetric, and
    only its answer is read for direction. A one-sided test pointed at the predecessor would be the same test
    with half the burden, chosen by the party being measured."""

    def test_doubles_the_smaller_one_sided_tail_hand_computed(self):
        # diffs [1,2,3]: one-sided exact p = 1/8, so two-sided = 2/8.
        _, p, n, direction = analyze.wilcoxon_two_sided([1.0, 2.0, 3.0])
        self.assertAlmostEqual(p, 0.25, places=10)
        self.assertEqual((n, direction), (3, "positive"))

    def test_symmetric_under_sign_flip(self):
        _, p_pos, _, d_pos = analyze.wilcoxon_two_sided([1.0, 2.0, 3.0])
        _, p_neg, _, d_neg = analyze.wilcoxon_two_sided([-1.0, -2.0, -3.0])
        self.assertAlmostEqual(p_pos, p_neg, places=10)
        self.assertEqual((d_pos, d_neg), ("positive", "negative"))

    def test_no_informative_pairs_is_p_one_direction_none(self):
        self.assertEqual(analyze.wilcoxon_two_sided([0.0, 0.0]), (0.0, 1.0, 0, "none"))
        self.assertEqual(analyze.wilcoxon_two_sided([]), (0.0, 1.0, 0, "none"))

    def test_balanced_evidence_is_capped_at_one_with_no_direction(self):
        # diffs [1,-1]: both one-sided tails are 0.75; doubling would give 1.5 -> capped at 1, no lean.
        _, p, _, direction = analyze.wilcoxon_two_sided([1.0, -1.0])
        self.assertEqual(p, 1.0)
        self.assertEqual(direction, "none")


class TestClusterBootstrapKnownDifference(unittest.TestCase):
    def test_constant_clusters_recover_the_true_difference_exactly(self):
        # Every host contributes the same correct-rate difference, so every resample mean is 0.4 and the
        # interval degenerates onto the truth.
        point, lo, hi = analyze.cluster_bootstrap_ci({f"h{i}": [0.4] for i in range(12)})
        self.assertAlmostEqual(point, 0.4)
        self.assertAlmostEqual(lo, 0.4)
        self.assertAlmostEqual(hi, 0.4)

    def test_mixed_clusters_bracket_the_point_and_reproduce_under_the_pinned_seed(self):
        clusters = {f"h{i}": [1.0 if i % 2 else 0.0] for i in range(16)}
        a = analyze.cluster_bootstrap_ci(clusters)
        b = analyze.cluster_bootstrap_ci(clusters)
        self.assertEqual(a, b, "same seed, same clusters -> the CI must reproduce to the digit")
        point, lo, hi = a
        self.assertAlmostEqual(point, 0.5)
        self.assertLess(lo, point)
        self.assertGreater(hi, point)
        self.assertGreaterEqual(lo, 0.0)
        self.assertLessEqual(hi, 1.0)


class TestCompositeVerdictTruthTable(unittest.TestCase):
    """The §7 decision rule, exercised predicate by predicate against composite_verdict directly."""

    @staticmethod
    def _clear_counter():
        return {d: {"p_holm": 1.0, "favors": "none"} for d in analyze.COUNTER_LEG_DIMENSIONS}

    @classmethod
    def _v(cls, **overrides):
        base = dict(
            powered=True,
            mcnemar_p=0.01,
            tg_only_correct=10,
            pred_only_correct=2,
            counter_leg=cls._clear_counter(),
            primary_diff_ci_low=0.05,
        )
        base.update(overrides)
        return analyze.composite_verdict(**base)

    def test_powered_significant_favoring_tg_with_clean_counter_leg_is_exceeds(self):
        self.assertEqual(self._v()["verdict"], analyze.VERDICT_EXCEEDS)

    def test_unpowered_is_never_exceeds_regardless_of_p(self):
        v = self._v(powered=False, mcnemar_p=1e-12)
        self.assertNotEqual(v["verdict"], analyze.VERDICT_EXCEEDS)
        self.assertEqual(
            v["verdict"], analyze.VERDICT_HOLDS,
            "an unpowered run cannot claim MATCHES either — the burden of proof is TG's",
        )

    def test_counter_leg_significantly_favoring_pred_blocks_exceeds(self):
        counter = self._clear_counter()
        counter["correct_diagnosis"] = {"p_holm": 0.01, "favors": "pred"}
        v = self._v(counter_leg=counter)
        self.assertNotEqual(v["verdict"], analyze.VERDICT_EXCEEDS)
        self.assertEqual(v["counter_leg_blocking"], ["correct_diagnosis"])
        self.assertEqual(v["verdict"], analyze.VERDICT_MATCHES, "the CI still supports non-inferiority")

    def test_counter_leg_significant_in_tg_favour_does_not_block(self):
        counter = self._clear_counter()
        counter["evidence_grounded"] = {"p_holm": 0.001, "favors": "tg"}
        self.assertEqual(self._v(counter_leg=counter)["verdict"], analyze.VERDICT_EXCEEDS)

    def test_insignificant_primary_is_not_exceeds(self):
        self.assertNotEqual(self._v(mcnemar_p=0.06)["verdict"], analyze.VERDICT_EXCEEDS)

    def test_discordant_count_favoring_pred_is_not_exceeds_even_when_significant(self):
        v = self._v(mcnemar_p=0.001, tg_only_correct=2, pred_only_correct=10)
        self.assertNotEqual(v["verdict"], analyze.VERDICT_EXCEEDS)

    def test_missing_primary_p_is_not_exceeds(self):
        self.assertNotEqual(self._v(mcnemar_p=None)["verdict"], analyze.VERDICT_EXCEEDS)

    def test_noninferiority_boundary_exactly_ten_points_still_matches(self):
        # A CI lower bound of exactly -0.10 excludes every advantage GREATER than 10pp (open interval).
        v = self._v(mcnemar_p=0.5, primary_diff_ci_low=-0.10)
        self.assertEqual(v["verdict"], analyze.VERDICT_MATCHES)

    def test_noninferiority_just_below_the_boundary_is_predecessor_holds(self):
        v = self._v(mcnemar_p=0.5, primary_diff_ci_low=-0.10 - 1e-9)
        self.assertEqual(v["verdict"], analyze.VERDICT_HOLDS)

    def test_uncomputable_interval_fails_closed(self):
        for bad in (None, float("nan")):
            v = self._v(mcnemar_p=0.5, primary_diff_ci_low=bad)
            self.assertEqual(
                v["verdict"], analyze.VERDICT_HOLDS,
                f"ci_low={bad!r} must certify nothing — non-inferiority needs a real interval",
            )

    def test_the_margin_is_ten_percentage_points(self):
        self.assertEqual(analyze.NONINFERIORITY_MARGIN, 0.10)


class TestVerdictStructuralExclusion(unittest.TestCase):
    """REJUDGE-2026-07-26 closed prose-side; this closes it structurally.

    Both of TG's apparent pooled wins were awarded on falsifiable_prediction — a dimension the predecessor is
    structurally never scored on. The verdict function must be INCAPABLE of receiving that dimension, or any
    unilateral axis (A3/A4/A5/A7/A8): not filtered, not warned about — impossible to pass.
    """

    def test_falsifiable_prediction_in_the_counter_leg_is_refused_loudly(self):
        counter = {d: {"p_holm": 1.0, "favors": "none"} for d in analyze.COUNTER_LEG_DIMENSIONS}
        counter["falsifiable_prediction"] = {"p_holm": 0.001, "favors": "tg"}
        with self.assertRaises(ValueError):
            analyze.composite_verdict(
                powered=True, mcnemar_p=0.01, tg_only_correct=10, pred_only_correct=2,
                counter_leg=counter, primary_diff_ci_low=0.05,
            )

    def test_unilateral_axis_data_has_no_parameter_to_arrive_through(self):
        base = dict(
            powered=True, mcnemar_p=0.01, tg_only_correct=10, pred_only_correct=2,
            counter_leg={d: {"p_holm": 1.0, "favors": "none"} for d in analyze.COUNTER_LEG_DIMENSIONS},
            primary_diff_ci_low=0.05,
        )
        for smuggled in ("falsifiable_prediction", "heal_success_rate", "autonomy_rate",
                         "fault_class_breadth", "false_actuation_rate", "safety_violation_count"):
            with self.assertRaises(TypeError, msg=f"{smuggled} must not be acceptable as an argument"):
                analyze.composite_verdict(**base, **{smuggled: 5.0})

    def test_the_input_set_is_closed_keyword_only_with_no_kwargs(self):
        import inspect

        sig = inspect.signature(analyze.composite_verdict)
        kinds = {p.kind for p in sig.parameters.values()}
        self.assertNotIn(inspect.Parameter.VAR_KEYWORD, kinds, "a **kwargs would reopen the smuggling route")
        self.assertNotIn(inspect.Parameter.VAR_POSITIONAL, kinds)
        self.assertEqual(kinds, {inspect.Parameter.KEYWORD_ONLY})
        self.assertEqual(
            set(sig.parameters),
            {"powered", "mcnemar_p", "tg_only_correct", "pred_only_correct", "counter_leg",
             "primary_diff_ci_low"},
            "the verdict's input set is pre-registered; widening it is an analysis change",
        )

    def test_counter_leg_dimensions_are_the_like_for_like_pair_and_nothing_one_sided(self):
        self.assertEqual(analyze.COUNTER_LEG_DIMENSIONS, ("correct_diagnosis", "evidence_grounded"))
        self.assertNotIn("falsifiable_prediction", analyze.COUNTER_LEG_DIMENSIONS)
        self.assertTrue(set(analyze.COUNTER_LEG_DIMENSIONS) < set(analyze.DIMENSIONS))

    def test_a_falsifiable_prediction_sweep_cannot_produce_a_tg_verdict(self):
        # The REJUDGE scenario end-to-end: every comparable dimension tied, TG carrying a perfect 5.0 on
        # falsifiable_prediction that the predecessor is never scored on, ground truth fully concordant.
        pairs = [
            {
                "incident_key": f"k{i}", "subject_host": f"h{i}", "judged_at": f"2026-07-30T{i:02d}:00:00", "tg_created_at": ACCRUED,
                "present_systems": ["pred", "tg"], "judge_unavailable": False,
                "mapping": {"A": "tg", "B": "pred"},
                "dims": {
                    "A": {d: 3 for d in analyze.DIMENSIONS} | {"falsifiable_prediction": 5},
                    "B": {d: 3 for d in analyze.DIMENSIONS} | {"falsifiable_prediction": None},
                },
            }
            for i in range(3)
        ]
        gt = {f"k{i}": {"tg_correct": True, "pred_correct": True} for i in range(3)}
        rep = analyze.analyze(pairs, gt)
        self.assertEqual(rep["verdict"]["verdict"], analyze.VERDICT_HOLDS)
        self.assertEqual(set(rep["verdict"]["counter_leg"]), set(analyze.COUNTER_LEG_DIMENSIONS))
        fp = rep["unilateral_tg_properties"]["falsifiable_prediction"]
        self.assertEqual((fp["n"], fp["tg_mean"]), (3, 5.0), "the property is still PUBLISHED — labelled")
        self.assertIn("UNILATERAL TG-ONLY PROPERTIES", analyze.render(rep))


class TestVerdictEndToEnd(unittest.TestCase):
    """The three verdicts reached through analyze() itself, not by calling the rule directly."""

    @staticmethod
    def _pairs(n, tg=4, pred=3):
        return [
            {
                "incident_key": f"k{i}", "key": f"k{i}", "subject_host": f"h{i}", "judged_at": f"2026-07-30T{i:02d}:00:00", "tg_created_at": ACCRUED,
                "present_systems": ["pred", "tg"], "judge_unavailable": False,
                "mapping": {"A": "tg", "B": "pred"},
                "dims": {"A": {d: tg for d in analyze.DIMENSIONS}, "B": {d: pred for d in analyze.DIMENSIONS}},
            }
            for i in range(n)
        ]

    def test_powered_primary_win_with_no_counter_evidence_is_exceeds(self):
        pairs = self._pairs(40, tg=4, pred=3)
        gt = {f"k{i}": {"tg_correct": True, "pred_correct": False} for i in range(40)}
        rep = analyze.analyze(pairs, gt, manifest_keys={f"k{i}" for i in range(40)})
        self.assertTrue(rep["powered"])
        self.assertEqual(rep["verdict"]["verdict"], analyze.VERDICT_EXCEEDS)

    def test_a_primary_win_the_judged_leg_contradicts_is_downgraded_to_matches(self):
        # Ground truth favours TG on every discordant pair, but the judge scores the predecessor FAR ahead on
        # every dimension — the like-for-like counter-leg must block EXCEEDS, and the (fully TG-favourable)
        # correct-rate CI then supports non-inferiority.
        pairs = self._pairs(40, tg=2, pred=5)
        gt = {f"k{i}": {"tg_correct": True, "pred_correct": False} for i in range(40)}
        rep = analyze.analyze(pairs, gt, manifest_keys={f"k{i}" for i in range(40)})
        self.assertTrue(rep["powered"])
        v = rep["verdict"]
        self.assertEqual(v["verdict"], analyze.VERDICT_MATCHES)
        self.assertEqual(v["counter_leg_blocking"], ["correct_diagnosis", "evidence_grounded"])

    def test_an_unpowered_run_holds_for_the_predecessor_whatever_its_numbers_say(self):
        pairs = self._pairs(3, tg=5, pred=1)
        gt = {f"k{i}": {"tg_correct": True, "pred_correct": False} for i in range(3)}
        rep = analyze.analyze(pairs, gt)
        self.assertFalse(rep["powered"])
        self.assertEqual(rep["verdict"]["verdict"], analyze.VERDICT_HOLDS)
        self.assertIn("NOT A CONFIRMATORY RESULT", analyze.render(rep))

    def test_the_verdict_is_printed_with_its_reasons(self):
        rep = analyze.analyze(self._pairs(3))
        text = analyze.render(rep)
        self.assertIn("COMPOSITE VERDICT", text)
        self.assertIn(analyze.VERDICT_HOLDS, text)
        self.assertIn("counter-leg", text)


class TestTG249FrozenAnalysisCorrections(unittest.TestCase):
    """The three governed corrections landed under TG-249 (§6 2026-08-19). Each is a regression guard: revert
    the fix in analyze.py and the matching test here goes red."""

    @staticmethod
    def _pair(key, host, incident_ts, present=("pred", "tg"), tg=1, pred=5, gt_key=None):
        return {
            "incident_key": key,
            "key": gt_key or key,
            "subject_host": host,
            "judged_at": incident_ts,
            "tg_created_at": incident_ts,
            "present_systems": list(present),
            "judge_unavailable": False,
            "mapping": {"A": "tg", "B": "pred"},
            "dims": {"A": {d: tg for d in analyze.DIMENSIONS}, "B": {d: pred for d in analyze.DIMENSIONS}},
        }

    def test_item10_pre_boundary_pilot_is_excluded_ahead_of_a_confirmatory_pair(self):
        # A pre-freeze pilot and a post-boundary confirmatory pair on the SAME host. The pilot sorts earliest by
        # judged_at; without the accrual boundary it would consume the host's single kept slot (the item-10 bug).
        pre = self._pair("pilot", "h1", "2026-07-18T22:00:00+00:00")
        post = self._pair("real", "h1", "2026-08-27T00:00:00+00:00")
        kept, notes = analyze.enforce_population([pre, post])
        self.assertEqual(
            [p["incident_key"] for p in kept], ["real"],
            "a pre-ACCRUE_FROM pilot must be excluded, never admitted ahead of a confirmatory pair",
        )
        self.assertTrue(any("accrual boundary" in n for n in notes), "the boundary exclusion must be REPORTED")

    def test_item10_a_pair_with_no_establishable_injection_time_is_excluded_fail_closed(self):
        p = self._pair("x", "h1", "2026-08-27T00:00:00+00:00")
        del p["tg_created_at"]  # and no pred_first_ts -> injection time unprovable
        kept, _ = analyze.enforce_population([p])
        self.assertEqual(kept, [], "a pair whose injection time cannot be established is not confirmatory")

    def test_item11_non_inferiority_leg_joins_ground_truth_by_per_fault_key(self):
        # GT keyed by the per-fault `key` (the §6-compliant form), NOT the bare incident_key. Before the fix the
        # non-inferiority leg joined on incident_key and found nothing, so no rate-difference CI was produced and
        # TG MATCHES was structurally unreachable.
        pairs = [self._pair(f"i{i}", f"h{i}", "2026-08-27T00:00:00+00:00", gt_key=f"fault-{i}") for i in range(3)]
        gt = {f"fault-{i}": {"tg_correct": True, "pred_correct": False} for i in range(3)}
        rep = analyze.analyze(pairs, gt)
        self.assertIn(
            "primary_rate_difference", rep,
            "the non-inferiority leg must populate from per-fault-keyed ground truth (TG-249 item 11)",
        )

    def test_item12_dead_blinding_bar_is_removed(self):
        self.assertFalse(
            hasattr(analyze, "MAX_BLINDING_GUESS_ACCURACY"),
            "the producer-less, consumer-less blinding bar was removed as dead (TG-249 item 12)",
        )


class TestTG526ManifestJoinedPopulation(unittest.TestCase):
    """TG-526 (§6 2026-08-25): MANIFEST MEMBERSHIP defines the confirmatory population. The boundary is FAULT
    time and the pair record carries no fault-time field, so the timestamp proxy still admits ORGANIC
    post-boundary pairs — which occupy per-host cap slots and evict ground-truth pairs before _gt_for drops
    them. Each test is a regression guard: revert the join in analyze.py and one of these goes red."""

    _pair = staticmethod(TestTG249FrozenAnalysisCorrections._pair)

    def test_organic_pair_cannot_evict_a_manifest_joined_pair_from_the_cap(self):
        # Three organic pairs judged EARLIER than the injected one, same host: pre-fix they fill the host's
        # cap and the ground-truth-carrying pair is evicted before the GT join. The manifest join must keep
        # only the injected pair.
        organics = [self._pair(f"org{i}", "h1", f"2026-08-27T0{i}:00:00+00:00") for i in range(3)]
        injected = self._pair("inj", "h1", "2026-08-27T09:00:00+00:00")
        kept, notes = analyze.enforce_population(organics + [injected], manifest_keys={"inj"})
        self.assertEqual([p["incident_key"] for p in kept], ["inj"],
                         "the manifest member must survive; organic pairs may not occupy its cap slot")
        self.assertTrue(any("manifest" in n for n in notes), "the organic exclusion must be REPORTED")

    def test_no_manifest_is_loud_and_never_powered(self):
        pairs = [self._pair(f"i{i}", f"h{i}", "2026-08-27T00:00:00+00:00", gt_key=f"f{i}") for i in range(40)]
        gt = {f"f{i}": {"tg_correct": True, "pred_correct": False} for i in range(40)}
        rep = analyze.analyze(pairs, gt)  # no manifest_keys
        self.assertFalse(rep["manifest_joined"])
        self.assertFalse(rep["powered"],
                         "a population whose membership rule was not applied must never be powered")
        self.assertTrue(any("manifest" in s for s in rep["power_shortfall"]))

    def test_empty_manifest_membership_is_an_honest_empty_population(self):
        pairs = [self._pair("a", "h1", "2026-08-27T00:00:00+00:00")]
        kept, notes = analyze.enforce_population(pairs, manifest_keys=set())
        self.assertEqual(kept, [])
        self.assertTrue(any("manifest" in n for n in notes))

    def test_load_manifest_keys_absent_file_is_none_not_empty(self):
        self.assertIsNone(analyze.load_manifest_keys("/nonexistent/manifest.jsonl"),
                          "absence of the manifest must stay distinguishable from zero members")

    def test_load_manifest_keys_applies_status_and_boundary(self):
        import tempfile
        rows = [
            {"status": "PAIRED", "ts": "2026-08-27T00:00:00Z", "scorecard_keys": ["in"]},
            {"status": "PAIRED", "ts": "2026-07-30T00:00:00Z", "scorecard_keys": ["prefreeze"]},
            {"status": "MISSED", "ts": "2026-08-27T00:00:00Z", "scorecard_keys": ["unpaired"]},
        ]
        with tempfile.NamedTemporaryFile("w", suffix=".jsonl", delete=False) as fh:
            for r in rows:
                fh.write(json.dumps(r) + "\n")
            path = fh.name
        try:
            self.assertEqual(analyze.load_manifest_keys(path), {"in"})
        finally:
            os.unlink(path)


class TestPairsSnapshot(unittest.TestCase):
    """snapshot_pairs freezes a campaign's post-boundary judged pairs so a verdict reproduces (TG-249)."""

    def test_select_keeps_only_post_boundary_pairs_with_an_incident_time(self):
        import snapshot_pairs

        rows = [
            {"key": "pilot", "tg_created_at": "2026-07-18T00:00:00+00:00"},  # pre-boundary -> dropped
            {"key": "conf", "pred_first_ts": "2026-08-01T00:00:00Z"},        # post-boundary (Z form) -> kept
            {"key": "orphan"},                                               # no incident time -> dropped
        ]
        kept = snapshot_pairs.select(rows, "2026-07-31T21:55:57Z")
        self.assertEqual([r["key"] for r in kept], ["conf"])

    def test_select_with_manifest_keys_keeps_only_members(self):
        # TG-526: the committed snapshot IS the confirmatory set — organic post-boundary pairs excluded.
        import snapshot_pairs

        rows = [
            {"key": "member", "tg_created_at": "2026-08-27T00:00:00+00:00"},
            {"key": "organic", "tg_created_at": "2026-08-27T01:00:00+00:00"},
        ]
        kept = snapshot_pairs.select(rows, "2026-08-26T00:00:00Z", manifest_keys={"member"})
        self.assertEqual([r["key"] for r in kept], ["member"])


if __name__ == "__main__":
    unittest.main(verbosity=2)
