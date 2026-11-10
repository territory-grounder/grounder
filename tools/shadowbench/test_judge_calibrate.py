#!/usr/bin/env python3
"""Tests for judge-calibrate.py — the candidate-judge agreement harness (recovery item (d) prerequisite).

Three jobs, mirroring test_accrual.py's shape. First, the structural bar: the harness measures
candidate-vs-primary agreement and must be STRUCTURALLY UNABLE to render a migration decision or a
TG-vs-predecessor conclusion, and unable to write the scorecard — the primary judge anchors every
score and stays fixed until the Phase-D verdict, so an unfrozen file that could conclude or mutate
the ledger would be a back door around that. Second, the statistics against hand-computed values —
a kappa bug yields a confident, plausible, WRONG calibration number with nothing downstream to
catch it. Third, the judge seam: the candidate re-judge must go through judge.build_verdict (the
imported frozen prompt path) with the candidate model, proven offline by mocking the one gateway
function (judge.call_litellm) — no network, no gateway, no SSH in any test.

stdlib unittest, runnable directly — CI executes every test_*.py by glob (python3, no pip installs).
"""

import contextlib
import importlib.util
import io
import json
import os
import sys
import tempfile
import unittest

HERE = os.path.dirname(os.path.abspath(__file__))
sys.path.insert(0, HERE)

# The filename is hyphenated (a CLI tool, not a package module) — load it the way
# test_judge_symmetry.py loads judge.py.
_spec = importlib.util.spec_from_file_location("judge_calibrate", os.path.join(HERE, "judge-calibrate.py"))
jc = importlib.util.module_from_spec(_spec)
sys.modules["judge_calibrate"] = jc
_spec.loader.exec_module(jc)

CALIBRATE_PY = os.path.join(HERE, "judge-calibrate.py")
DIMS = jc.DIMENSIONS


def make_pair(key, pred_scores=None, tg_scores=None, winner=None, mapping=None,
              requested="primary", unavailable=False, kind="pair"):
    """A scorecard record in the production shape (per-system scores placed via the mapping)."""
    mapping = mapping or {"A": "pred", "B": "tg"}
    dims = {}
    for letter, system in mapping.items():
        base = {d: None for d in DIMS}
        base.update((pred_scores if system == "pred" else tg_scores) or {})
        base["comment"] = "fixture"
        dims[letter] = base
    parts = key.split("|")
    return {
        "key": key,
        "kind": kind,
        "incident_key": parts[2] if len(parts) == 4 else key,
        "mapping": mapping,
        "dims": dims,
        "winner": winner,
        "winner_letter": None,
        "single_sided": False,
        "present_systems": ["pred", "tg"],
        "judge_unavailable": unavailable,
        "judge_model_requested": requested,
        "judge_model_served": requested,
    }


def scores(cd, eg, sp, ab, fp):
    return {"correct_diagnosis": cd, "evidence_grounded": eg, "sensible_proposal": sp,
            "appropriate_band": ab, "falsifiable_prediction": fp}


def pred_incident(key, host="dc1foo01"):
    return {"incidentKey": key, "agentic": True, "host": "runnerbox",
            "issue": f"Infrastructure alert: {host} - Space on / is >= 90%",
            "action": "POLL_PAUSE", "rationale": "disk transient; no action",
            "reasoningExcerpt": "OBSERVATION: df shows 62% used", "outcome": "closed"}


def tg_row(ref, host="dc1foo01"):
    return {"external_ref": ref, "host": host, "alertRule": "Space-on-/-is-90-and-95-in-use",
            "band": "OBSERVE", "op": "", "conclusionExcerpt": "disk ok",
            "reasoningExcerpt": "OBSERVATION: checked mounts", "evidenceCount": 1,
            "hasPrediction": False, "severity": "warning", "createdAt": "2026-07-26T12:00:00Z"}


def canned_content(score=3, winner="tie"):
    side = {d: score for d in DIMS}
    side["comment"] = "canned"
    return json.dumps({"A": side, "B": dict(side), "winner": winner, "reason": "canned"})


class TestNoVerdictOrLedgerWritePath(unittest.TestCase):
    """THE HARNESS MUST BE STRUCTURALLY UNABLE TO CONCLUDE OR TO TOUCH THE LEDGER.

    The migration decision belongs to the owner after the Phase-D verdict; the campaign's decision
    rule is frozen in analyze.py. This file is unfrozen by design (measurement can improve), which
    is exactly why it must be provable that nothing in it can decide anything or append to
    scorecard.jsonl. If any assertion here ever needs weakening, the change being made is the
    attack this test exists to stop.
    """

    def setUp(self):
        with open(CALIBRATE_PY, encoding="utf-8") as fh:
            self.src = fh.read()

    def test_no_campaign_statistical_machinery_in_the_source(self):
        low = self.src.lower()
        for token in ("mcnemar", "wilcoxon", "holm", "binom", "bootstrap", "p_value", "cliff",
                      "significan", "exceeds", "non-inferior", "noninferior"):
            self.assertNotIn(token, low, f"judge-calibrate must carry no campaign verdict machinery: {token!r}")

    def test_it_never_imports_the_frozen_analysis_or_the_ledger_appender(self):
        for banned in ("import analyze", "from analyze", "import _driver", "from _driver",
                       "append_verdict"):
            self.assertNotIn(banned, self.src)

    def test_the_docstring_states_both_prohibitions(self):
        self.assertIn("NEVER COMPUTES A MIGRATION VERDICT", jc.__doc__)
        self.assertIn("never writes scorecard.jsonl", jc.__doc__)

    def test_no_append_mode_open_anywhere(self):
        # The scorecard is an append-only ledger; the one way this tool could corrupt it is an
        # append-mode open. There must be none, in any spelling.
        for mode in ('"a"', "'a'", '"a+"', "'a+'", '"ab"', "'ab'"):
            self.assertNotIn(f", {mode}", self.src, "no append-mode open() allowed")

    def test_the_single_write_path_is_the_artifact(self):
        # Exactly one write-mode open, and the artifact writer exists to own it.
        self.assertEqual(self.src.count('"w"'), 1, "exactly one write-mode open (the out/ artifact)")
        self.assertIn("def write_artifact", self.src)

    def test_rendered_output_carries_no_decision_label(self):
        args = _args(candidate="cand-x", scorecard="(none)")
        report = jc.calibrate([], {}, {}, args)
        text = jc.render(report)
        for label in ("TG EXCEEDS", "TG MATCHES", "PREDECESSOR HOLDS", "MIGRATION APPROVED",
                      "ELIGIBLE: YES", "ELIGIBLE: NO"):
            self.assertNotIn(label, text)
        # The PROPOSED guidance is printed verbatim, framed as unratified.
        self.assertIn("pooled weighted kappa >= 0.75", text)
        self.assertIn("NOT ratified", text)
        self.assertIn("MEASURES agreement only", text)

    def test_the_thresholds_are_marked_proposed_in_the_artifact_shape(self):
        self.assertIn("PROPOSED", jc.PROPOSED_THRESHOLDS["status"])
        self.assertEqual(jc.PROPOSED_THRESHOLDS["pooled_weighted_kappa_min"], 0.75)
        self.assertEqual(jc.PROPOSED_THRESHOLDS["min_dimension_weighted_kappa"], 0.60)
        self.assertEqual(jc.PROPOSED_THRESHOLDS["mean_signed_delta_abs_max"], 0.25)


class TestEligibility(unittest.TestCase):
    def test_only_primary_judged_scored_pairs_are_eligible(self):
        rows = [
            make_pair("2026-07-01|pair|P1|T1", scores(4, 4, 4, 4, None), scores(3, 3, 3, 3, 3)),
            make_pair("2026-07-01|pair|P2|T2", scores(2, 2, 2, 2, None), scores(3, 3, 3, 3, 3),
                      unavailable=True),                       # judge never scored it
            make_pair("2026-07-01|pair|P3|T3", scores(4, 4, 4, 4, None), scores(3, 3, 3, 3, 3),
                      requested="some-other-model"),           # scored by the wrong anchor
            make_pair("2026-07-01|tg|T4", None, scores(3, 3, 3, 3, 3), kind="tg_only"),  # not a pair
            make_pair("2026-07-01|pair|P5|T5", None, None),    # no numeric score at all
        ]
        got = [r["key"] for r in jc.eligible_pairs(rows)]
        self.assertEqual(got, ["2026-07-01|pair|P1|T1"])

    def test_a_record_without_the_requested_model_field_still_counts(self):
        # Legacy rows may predate the field; absence is not evidence of a foreign anchor.
        r = make_pair("2026-07-01|pair|P1|T1", scores(4, 4, 4, 4, None), scores(3, 3, 3, 3, 3))
        del r["judge_model_requested"]
        self.assertEqual(len(jc.eligible_pairs([r])), 1)


class TestSamplingDeterminism(unittest.TestCase):
    def _rows(self):
        return [make_pair(f"2026-07-0{1 + i % 2}|pair|P{i:02d}|T{i:02d}",
                          scores(3, 3, 3, 3, None), scores(3, 3, 3, 3, 3))
                for i in range(12)]

    def test_same_seed_same_sample_regardless_of_input_order(self):
        rows = self._rows()
        a = [r["key"] for r in jc.select_sample(rows, 5)]
        b = [r["key"] for r in jc.select_sample(list(reversed(rows)), 5)]
        self.assertEqual(a, b)
        self.assertEqual(a, [r["key"] for r in jc.select_sample(rows, 5)])
        self.assertEqual(len(a), 5)
        self.assertEqual(a, sorted(a), "the sample is processed in sorted-key order")

    def test_the_selection_is_a_pure_function_of_seed_and_keys(self):
        # Hash-rank (sha256) selection is stable across machines and Python versions, so the
        # expected sample is pinned LITERALLY (hand-derived from sha256("20260730|<key>"), not
        # from the code under test). If this red-lines, the sampling function changed — which
        # silently changes WHICH records every future calibration measures.
        got = [r["key"] for r in jc.select_sample(self._rows(), 5)]
        self.assertEqual(got, [
            "2026-07-01|pair|P00|T00",
            "2026-07-01|pair|P04|T04",
            "2026-07-01|pair|P08|T08",
            "2026-07-02|pair|P05|T05",
            "2026-07-02|pair|P11|T11",
        ])

    def test_a_different_seed_selects_a_different_sample(self):
        rows = self._rows()
        a = {r["key"] for r in jc.select_sample(rows, 5, sample_seed=1)}
        b = {r["key"] for r in jc.select_sample(rows, 5, sample_seed=2)}
        self.assertNotEqual(a, b)

    def test_n_larger_than_population_takes_everything_once(self):
        rows = self._rows()
        got = [r["key"] for r in jc.select_sample(rows, 99)]
        self.assertEqual(got, sorted(r["key"] for r in rows))


class TestWeightedKappa(unittest.TestCase):
    """Every value here is hand-computed from the definition — never from the code under test."""

    def test_hand_computed_mixed_case(self):
        # pairs (1,1),(2,3),(3,2),(4,4) on the 1..5 scale (span^2 = 16):
        #   observed = (0 + 1 + 1 + 0) / (4 * 16)          = 0.03125
        #   marginals: both raters 1/4 each on {1,2,3,4}
        #   expected = (1/16)*(1/16) * sum_{i,j in 1..4}(i-j)^2 = (1/256)*40 = 0.15625
        #   kappa    = 1 - 0.03125/0.15625                 = 0.8
        k = jc.quadratic_weighted_kappa([(1, 1), (2, 3), (3, 2), (4, 4)])
        self.assertAlmostEqual(k, 0.8, places=10)

    def test_perfect_agreement_with_varied_scores_is_one(self):
        self.assertAlmostEqual(jc.quadratic_weighted_kappa([(1, 1), (3, 3), (5, 5)]), 1.0, places=10)

    def test_total_systematic_disagreement_is_minus_one(self):
        # (1,5),(5,1): observed = (16+16)/(2*16) = 1; expected = 0.25*(16+0+0+16)/16 = 0.5
        self.assertAlmostEqual(jc.quadratic_weighted_kappa([(1, 5), (5, 1)]), -1.0, places=10)

    def test_degenerate_constant_raters_are_undefined_not_faked(self):
        # Both raters constant -> expected disagreement 0 -> kappa mathematically undefined.
        # Exact-agreement % carries the information; returning 1.0 here would flatter a candidate
        # that only ever emits one number.
        self.assertIsNone(jc.quadratic_weighted_kappa([(3, 3), (3, 3)]))

    def test_empty_input_is_none(self):
        self.assertIsNone(jc.quadratic_weighted_kappa([]))


class TestAgreementStats(unittest.TestCase):
    def test_stats_on_a_hand_computed_mixture(self):
        pairs = [(4, 3), (4, 4), (2, 4), (None, 3), (3, None), (None, None)]
        st = jc.agreement_stats(pairs)
        self.assertEqual(st["n"], 3)
        self.assertEqual(st["exact_pct"], 33.3)          # 1/3
        self.assertEqual(st["within1_pct"], 66.7)        # (4,3),(4,4)
        self.assertEqual(st["mean_signed_delta"], 0.333)  # (-1 + 0 + 2)/3, candidate - primary
        self.assertEqual(st["primary_na_candidate_scored"], 1)
        self.assertEqual(st["candidate_na_primary_scored"], 1)
        self.assertEqual(st["both_na"], 1)

    def test_empty_input_yields_honest_nones_not_zeros(self):
        st = jc.agreement_stats([])
        self.assertEqual(st["n"], 0)
        self.assertIsNone(st["exact_pct"])
        self.assertIsNone(st["mean_signed_delta"])
        self.assertIsNone(st["weighted_kappa"])


class TestSideScoresAndKeys(unittest.TestCase):
    def test_side_scores_deblind_through_the_mapping_both_orders(self):
        for mapping in ({"A": "pred", "B": "tg"}, {"A": "tg", "B": "pred"}):
            rec = make_pair("2026-07-01|pair|P|T", scores(5, 4, 3, 2, None),
                            scores(1, 2, 3, 4, 5), mapping=mapping)
            self.assertEqual(jc.side_scores(rec, "pred")["correct_diagnosis"], 5)
            self.assertEqual(jc.side_scores(rec, "tg")["falsifiable_prediction"], 5)

    def test_side_scores_coerce_and_reject_junk(self):
        rec = make_pair("2026-07-01|pair|P|T", scores(4.0, True, None, 3, None),
                        scores(3, 3, 3, 3, 3))
        got = jc.side_scores(rec, "pred")
        self.assertEqual(got["correct_diagnosis"], 4)     # float -> int
        self.assertIsNone(got["evidence_grounded"])       # bool is not a score
        self.assertIsNone(got["sensible_proposal"])
        self.assertNotIn("comment", got)

    def test_missing_mapping_side_is_none_not_a_guess(self):
        rec = make_pair("2026-07-01|pair|P|T", scores(3, 3, 3, 3, 3), scores(3, 3, 3, 3, 3))
        rec["mapping"] = {"A": "pred", "B": None}
        self.assertIsNone(jc.side_scores(rec, "tg"))

    def test_pair_key_parsing(self):
        self.assertEqual(jc.parse_pair_key({"key": "2026-07-26|pair|IFRX-1|librenms-1"}),
                         ("IFRX-1", "librenms-1"))
        self.assertEqual(jc.parse_pair_key({"key": "2026-07-26|tg|librenms-1"}), (None, None))
        self.assertEqual(jc.parse_pair_key({}), (None, None))


class TestVerdictAgreement(unittest.TestCase):
    def test_only_records_where_both_verdicts_carry_a_winner_count(self):
        va = jc.verdict_agreement([("pred", "tie"), ("tie", "tie"), (None, "tie"), ("tg", "junk")])
        self.assertEqual(va["n"], 2)
        self.assertEqual(va["agree"], 1)
        self.assertEqual(va["agree_pct"], 50.0)
        self.assertEqual(va["mismatches"], {"pred->tie": 1})


def _args(candidate="cand-x", scorecard="scorecard.jsonl", sample=30,
          sample_seed=jc.DEFAULT_SAMPLE_SEED, dry_run=False):
    import argparse
    return argparse.Namespace(
        candidate_model=candidate, scorecard=scorecard, sample=sample, sample_seed=sample_seed,
        dry_run=dry_run, ssh_key="/nonexistent", tg_host="nohost", ssh_user="nobody",
        env_path="/nonexistent", timeout=5)


class TestCandidateRejudgeSeamOffline(unittest.TestCase):
    """End-to-end through main() with the ONE gateway function mocked: the candidate re-judge must
    traverse judge.build_verdict (the imported frozen prompt path), forward the candidate model,
    reproduce the recorded blind presentation order, leave the scorecard byte-identical, and land
    hand-computed statistics in the artifact. No network anywhere."""

    def setUp(self):
        self._orig = jc.judge.call_litellm
        self.calls = []
        self.tmp = tempfile.mkdtemp()
        self.out_dir = os.path.join(self.tmp, "out")

        recs = [
            make_pair("2026-07-26|pair|IFRX-1|librenms-1",
                      scores(4, 4, 4, 4, None), scores(2, 2, 2, 2, 2), winner="pred",
                      mapping={"A": "pred", "B": "tg"}),
            make_pair("2026-07-26|pair|IFRX-2|librenms-2",
                      scores(3, 3, 3, 3, None), scores(3, 3, 3, 3, 3), winner="tie",
                      mapping={"A": "tg", "B": "pred"}),
            # Sampled, but its inputs are NOT in the re-supplied extracts -> inputs_unavailable.
            make_pair("2026-07-26|pair|IFRX-9|librenms-9",
                      scores(5, 5, 5, 5, None), scores(5, 5, 5, 5, 5), winner="tg"),
        ]
        self.scorecard = os.path.join(self.tmp, "scorecard.jsonl")
        with open(self.scorecard, "w", encoding="utf-8") as fh:
            for r in recs:
                fh.write(json.dumps(r) + "\n")
        self.pred_path = os.path.join(self.tmp, "pred.json")
        with open(self.pred_path, "w", encoding="utf-8") as fh:
            json.dump({"incidents": [pred_incident("IFRX-1"), pred_incident("IFRX-2")]}, fh)
        self.tg_path = os.path.join(self.tmp, "tg.json")
        with open(self.tg_path, "w", encoding="utf-8") as fh:
            json.dump([tg_row("librenms-1"), tg_row("librenms-2")], fh)

    def tearDown(self):
        jc.judge.call_litellm = self._orig

    def _install_fake(self, content):
        def call(prompt, args):
            self.calls.append({"model": args.model, "prompt": prompt})
            return content, "fake-served", None
        jc.judge.call_litellm = call

    def _run_main(self, extra=()):
        buf = io.StringIO()
        with contextlib.redirect_stdout(buf):
            rc = jc.main(["--candidate-model", "cand-x",
                          "--scorecard", self.scorecard,
                          "--pred-from", self.pred_path, "--tg-from", self.tg_path,
                          "--out-dir", self.out_dir,
                          "--ssh-key", "/nonexistent", "--tg-host", "nohost",
                          "--ssh-user", "nobody", "--env-path", "/nonexistent",
                          "--timeout", "5", *extra])
        return rc, buf.getvalue()

    def _artifact(self):
        files = [f for f in os.listdir(self.out_dir) if f.startswith("judge-calibration-cand-x-")]
        self.assertEqual(len(files), 1, files)
        with open(os.path.join(self.out_dir, files[0]), encoding="utf-8") as fh:
            return json.load(fh)

    def test_full_offline_calibration_run(self):
        self._install_fake(canned_content(score=3, winner="tie"))
        with open(self.scorecard, "rb") as fh:
            before = fh.read()
        rc, out = self._run_main()
        self.assertEqual(rc, 0)

        # READ-ONLY: the production ledger is byte-identical after the run.
        with open(self.scorecard, "rb") as fh:
            self.assertEqual(fh.read(), before)

        report = self._artifact()
        self.assertEqual(report["candidate_model"], "cand-x")
        self.assertEqual(report["sampled"], 3)
        self.assertEqual(report["compared"], 2)
        self.assertEqual(report["inputs_unavailable"], 1)
        self.assertEqual(report["candidate_unavailable"], 0)

        # The seam: exactly one gateway call per compared record, carrying the CANDIDATE model,
        # over the real blind two-system prompt built by the frozen path.
        self.assertEqual(len(self.calls), 2)
        self.assertEqual({c["model"] for c in self.calls}, {"cand-x"})
        for c in self.calls:
            self.assertIn("=== SYSTEM A ===", c["prompt"])
            self.assertIn("=== SYSTEM B ===", c["prompt"])

        # Blind presentation order reproduced from each record's recorded mapping.
        self.assertEqual(report["order_matched"], 2)
        compared = [r for r in report["records"] if r["status"] == "compared"]
        self.assertTrue(all(r["order_matched"] for r in compared))

        # Hand-computed pooled stats. Numeric (primary, candidate) pairs with candidate all-3s:
        #   rec1 pred (4,3) x4; rec1 tg (2,3) x5; rec2 pred (3,3) x4; rec2 tg (3,3) x5 -> n=18
        #   exact = 9/18 = 50.0%; within-1 = 18/18 = 100.0%; mean delta = (-4 + 5 + 0)/18 = 0.056
        #   kappa: observed = 9/(18*16) = 0.03125; candidate marginal degenerate at 3 ->
        #          expected = (4/18*1 + 9/18*0 + 5/18*1)/16 = 0.03125; kappa = 0.0 exactly —
        #          the pinned proof that exact% can read 50 while chance-corrected agreement is nil.
        p = report["pooled"]
        self.assertEqual(p["n"], 18)
        self.assertEqual(p["exact_pct"], 50.0)
        self.assertEqual(p["within1_pct"], 100.0)
        self.assertEqual(p["mean_signed_delta"], 0.056)
        self.assertEqual(p["weighted_kappa"], 0.0)
        # falsifiable_prediction: numeric only on the TG side: (2,3) and (3,3); the predecessor's
        # structural N/A (primary null, candidate scored 3) is counted, not silently dropped.
        fp = report["per_dimension"]["falsifiable_prediction"]
        self.assertEqual(fp["n"], 2)
        self.assertEqual(fp["exact_pct"], 50.0)
        self.assertEqual(fp["mean_signed_delta"], 0.5)
        self.assertEqual(fp["primary_na_candidate_scored"], 2)

        # Verdict-level agreement on the pooled winner: rec1 pred->tie disagrees, rec2 tie->tie.
        va = report["verdict_agreement"]
        self.assertEqual((va["n"], va["agree"], va["agree_pct"]), (2, 1, 50.0))
        self.assertEqual(va["mismatches"], {"pred->tie": 1})

        # The printed report carries the PROPOSED bar and the measurement-only framing.
        self.assertIn("pooled weighted kappa >= 0.75", out)
        self.assertIn("mean signed delta within ±0.25", out)
        self.assertIn("NOT ratified", out)
        # The artifact stores scores and stats only — never trajectories or prompts.
        blob = json.dumps(report)
        self.assertNotIn("OBSERVATION", blob)
        self.assertNotIn("SYSTEM A", blob)

    def test_a_candidate_gateway_failure_degrades_honestly_per_record(self):
        def failing(prompt, args):
            self.calls.append({"model": args.model})
            return None, None, "connect refused"
        jc.judge.call_litellm = failing
        # judge.py's retry loop reads the module global; 1 attempt keeps the test free of its
        # inter-attempt backoff sleeps (the retries themselves are judge.py's tested behavior).
        saved = jc.judge.JUDGE_MAX_ATTEMPTS
        jc.judge.JUDGE_MAX_ATTEMPTS = 1
        self.addCleanup(setattr, jc.judge, "JUDGE_MAX_ATTEMPTS", saved)
        rc, _ = self._run_main()
        self.assertEqual(rc, 0)
        report = self._artifact()
        self.assertEqual(report["compared"], 0)
        self.assertEqual(report["candidate_unavailable"], 2)
        self.assertEqual(report["pooled"]["n"], 0)
        self.assertIsNone(report["pooled"]["weighted_kappa"])

    def test_dry_run_makes_no_gateway_call_and_writes_no_artifact(self):
        def forbidden(prompt, args):  # pragma: no cover - reaching this IS the failure
            raise AssertionError("dry-run must never call the gateway")
        jc.judge.call_litellm = forbidden
        rc, out = self._run_main(extra=("--dry-run",))
        self.assertEqual(rc, 0)
        self.assertIn("DRY RUN", out)
        self.assertIn("inputs_available", out)
        self.assertIn("inputs_unavailable", out)
        self.assertFalse(os.path.exists(self.out_dir), "dry-run writes nothing")


class TestAbsentScorecardAndNames(unittest.TestCase):
    def test_an_absent_scorecard_is_zero_eligible_not_a_crash(self):
        tmp = tempfile.mkdtemp()
        buf = io.StringIO()
        with contextlib.redirect_stdout(buf):
            rc = jc.main(["--candidate-model", "cand-x",
                          "--scorecard", os.path.join(tmp, "nope.jsonl"),
                          "--out-dir", os.path.join(tmp, "out")])
        self.assertEqual(rc, 0)
        self.assertIn("ABSENT", buf.getvalue())

    def test_artifact_name_sanitizes_gateway_alias_characters(self):
        self.assertEqual(jc._sanitize_model("ollama/qwen3:32b"), "ollama-qwen3-32b")
        self.assertEqual(jc._sanitize_model("judge-local-candidate"), "judge-local-candidate")
        self.assertEqual(jc._sanitize_model(""), "model")


if __name__ == "__main__":
    unittest.main(verbosity=2)
