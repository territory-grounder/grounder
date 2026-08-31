#!/usr/bin/env python3
"""Tests for accrual.py — the pair-supply progress meter (P6 / recovery Phase D2).

Two jobs, mirroring test_analyze.py's shape. First, check the supply accounting against hand-built
manifests, because a wrong "are we done" number either stops the campaign short of its own bar or runs
faults against the estate for pairs that can never count. Second — the load-bearing one — assert
STRUCTURALLY that accrual.py has NO VERDICT PATH: it is deliberately unfrozen, and an unfrozen file that
could reach a conclusion would be a second, gameable route around the pre-registration freeze. The freeze is
only as strong as the absence of alternatives to it.

stdlib unittest, runnable directly — CI executes every test_*.py by glob.
"""

import json
import os
import sys
import tempfile
import unittest

HERE = os.path.dirname(os.path.abspath(__file__))
sys.path.insert(0, HERE)

import accrual  # noqa: E402
import analyze  # noqa: E402  (imported by the TEST for cross-checks only; accrual itself must never import it)

ACCRUAL_PY = os.path.join(HERE, "accrual.py")


def _rec(host, fault, status="PAIRED", ts="2026-07-28T12:00:00Z", pred_issues=(), tg_refs=(),
         injector_class=""):
    return {
        "ts": ts,
        "incident_id": f"cmp-{ts}-{host}-{fault}",
        "fault_type": fault,
        "injector_class": injector_class,
        "host": host,
        "status": status,
        "pred_issues": list(pred_issues),
        "tg_refs": list(tg_refs),
    }


class TestNoVerdictPath(unittest.TestCase):
    """ACCRUAL MUST BE STRUCTURALLY UNABLE TO CONCLUDE ANYTHING.

    The decision rule is frozen inside analyze.py, hash-pinned. accrual.py is unfrozen by design (supply
    reporting must be able to improve mid-campaign) — which is exactly why it must be provable that nothing
    in it can compute a statistic or emit a verdict. If any of these assertions ever needs weakening, the
    change being made is the attack this test exists to stop.
    """

    def setUp(self):
        with open(ACCRUAL_PY, encoding="utf-8") as fh:
            self.src = fh.read()

    def test_no_statistical_machinery_in_the_source(self):
        low = self.src.lower()
        for token in (
            "mcnemar", "wilcoxon", "holm", "binom", "bootstrap", "p_value", "cliff",
            "significan", "exceeds", "non-inferior", "noninferior",
        ):
            self.assertNotIn(token, low, f"accrual.py must carry no statistical/verdict machinery: {token!r}")

    def test_it_never_imports_the_frozen_analysis(self):
        self.assertNotIn("import analyze", self.src)
        self.assertNotIn("from analyze", self.src)

    def test_no_module_attribute_names_a_verdict(self):
        offenders = [n for n in dir(accrual) if "verdict" in n.lower()]
        self.assertEqual(offenders, [], "no function, constant, or class in accrual may be a verdict path")

    def test_the_docstring_states_the_prohibition(self):
        self.assertIn("NEVER COMPUTES A VERDICT", accrual.__doc__)

    def test_it_opens_files_for_reading_only(self):
        for mode in ('"w"', "'w'", '"a"', "'a'", '"w+"', "'w+'", '"a+"', "'a+'", '"wb"', "'wb'"):
            self.assertNotIn(f", {mode}", self.src, "accrual is read-only; no write-mode open() allowed")

    def test_rendered_output_carries_no_verdict_label(self):
        prog = accrual.progress([_rec(f"h{i}", "disk") for i in range(40)], [])
        text = accrual.render(prog, "m", True, "s", True)
        for label in ("TG EXCEEDS", "TG MATCHES", "PREDECESSOR HOLDS"):
            self.assertNotIn(label, text)


class TestConstantsMirrorTheFrozenPlan(unittest.TestCase):
    def test_section_3_minimums_equal_the_frozen_values(self):
        # Mirrored, not imported (see accrual.py header); this is the anti-drift bolt.
        self.assertEqual(accrual.MIN_PAIRS, analyze.MIN_PAIRS)
        self.assertEqual(accrual.MIN_HOSTS, analyze.MIN_HOSTS)
        self.assertEqual(accrual.MAX_PAIRS_PER_HOST, analyze.MAX_PAIRS_PER_HOST)


class TestDualManifestRead(unittest.TestCase):
    """Supply comes from BOTH manifests: the legacy campaign.sh manifest AND confirmatory/manifest.jsonl
    (appended by reconcile-supply.py from the injector ledger × scorecard join). Reading them merged must
    stay pure accounting — the structural no-verdict bar above already covers the whole file."""

    def _conf_rec(self, host, fault, fault_id=1, ts="2026-07-28T12:00:00Z"):
        # The reconcile-supply record shape: same counting fields, plus the ground-truth join fields.
        return {
            "ts": ts, "source": "reconcile-supply", "status": "PAIRED", "fault_id": fault_id,
            "injector_class": {"disk": "disk-fill", "memory": "mem-pressure"}.get(fault, fault),
            "fault_type": fault, "host": host, "injected_at": ts,
            "pred_issues": [f"IFRX-{fault_id}"], "tg_refs": [f"tg-ref-{fault_id}"],
            "scorecard_keys": [f"2026-07-28|pair|IFRX-{fault_id}|tg-ref-{fault_id}"],
        }

    def test_merge_keeps_absence_distinguishable_from_empty(self):
        self.assertIsNone(accrual.merge_manifests(None, None))
        self.assertEqual(accrual.merge_manifests(None, []), [])
        self.assertEqual(accrual.merge_manifests([], None), [])

    def test_merge_counts_records_from_both_manifests(self):
        legacy = [_rec("h1", "disk")]
        confirmatory = [self._conf_rec("h2", "memory", fault_id=2)]
        prog = accrual.progress(accrual.merge_manifests(legacy, confirmatory), [])
        self.assertEqual(prog["paired_raw"], 2)
        self.assertEqual(prog["hosts"], 2)
        self.assertEqual(prog["per_class"], {"disk": 1, "memory": 1})

    def test_a_confirmatory_record_joins_judged_coverage_via_its_refs(self):
        confirmatory = [self._conf_rec("h1", "disk", fault_id=7)]
        scorecard = [{"key": "2026-07-28|pair|IFRX-7|tg-ref-7", "judge_unavailable": False}]
        prog = accrual.progress(confirmatory, scorecard)
        self.assertEqual(prog["judged_pairs"], 1)

    def test_the_default_confirmatory_path_is_the_committed_append_only_dir(self):
        self.assertTrue(accrual.DEFAULT_CONFIRMATORY.endswith(os.path.join("confirmatory", "manifest.jsonl")))

    def test_render_names_both_manifests(self):
        prog = accrual.progress([], [])
        text = accrual.render(prog, "legacy.jsonl", True, "s", True,
                              confirmatory_path="confirmatory/manifest.jsonl", confirmatory_present=False)
        self.assertIn("legacy.jsonl", text)
        self.assertIn("confirmatory/manifest.jsonl", text)
        self.assertIn("ABSENT", text)


class TestSupplyAccounting(unittest.TestCase):
    def test_per_host_cap_limits_counted_supply(self):
        rows = [_rec("h1", "disk") for _ in range(5)] + [_rec("h2", "memory")]
        prog = accrual.progress(rows, [])
        self.assertEqual(prog["paired_raw"], 6)
        self.assertEqual(prog["supply_after_cap"], accrual.MAX_PAIRS_PER_HOST + 1)
        self.assertEqual(prog["per_host"]["h1"]["counted"], accrual.MAX_PAIRS_PER_HOST)
        self.assertEqual(prog["per_host"]["h1"]["over_cap"], 2)
        self.assertEqual(prog["per_host"]["h2"]["headroom"], accrual.MAX_PAIRS_PER_HOST - 1)

    def test_the_done_number_counts_down_both_shortfalls(self):
        rows = [_rec("h1", "disk"), _rec("h2", "memory")]
        prog = accrual.progress(rows, [])
        self.assertFalse(prog["done"])
        self.assertEqual(prog["pairs_needed"], accrual.MIN_PAIRS - 2)
        self.assertEqual(prog["hosts_needed"], accrual.MIN_HOSTS - 2)

    def test_done_requires_both_minimums(self):
        scorable = {"device-down"}
        # 30 pairs on 10 hosts (3 each): pair bar met, host bar not (10 < MIN_HOSTS).
        rows = [_rec(f"h{i}", "device", injector_class="device-down") for i in range(10) for _ in range(3)]
        prog = accrual.progress(rows, [], scorable=scorable)
        self.assertEqual(prog["supply_after_cap"], 30)
        self.assertFalse(prog["done"])
        self.assertEqual(prog["hosts_needed"], accrual.MIN_HOSTS - 10)
        # 30 pairs across 15 hosts (2 each): both bars met (and every pair is ground-truth-capable).
        rows = [_rec(f"h{i}", "device", injector_class="device-down") for i in range(15) for _ in range(2)]
        self.assertTrue(accrual.progress(rows, [], scorable=scorable)["done"])


    def test_non_paired_records_are_counted_and_shown_not_silently_dropped(self):
        rows = [_rec("h1", "disk"), _rec("h1", "disk", status="MISS"), _rec("h2", "memory", status="ONE-SIDED")]
        prog = accrual.progress(rows, [])
        self.assertEqual(prog["non_paired_by_status"], {"MISS": 1, "ONE-SIDED": 1})
        text = accrual.render(prog, "m", True, "s", True)
        self.assertIn("MISS=1", text)

    def test_per_fault_class_supply_is_reported(self):
        rows = [_rec("h1", "disk"), _rec("h2", "disk"), _rec("h3", "memory")]
        self.assertEqual(accrual.progress(rows, [])["per_class"], {"disk": 2, "memory": 1})

    def test_prefreeze_records_are_excluded_with_the_count_reported(self):
        # The real 2026-07-22 shakedown shape: PAIRED, but before the plan existed.
        rows = [_rec("h1", "memory", ts="2026-07-22T20:45:54Z")] * 7 + [_rec("h2", "disk")]
        prog = accrual.progress(rows, [])
        self.assertEqual(prog["excluded_prefreeze"], 7)
        self.assertEqual(prog["supply_after_cap"], 1)
        self.assertIn("EXCLUDED 7 pre-freeze record(s)", accrual.render(prog, "m", True, "s", True))

    def test_an_absent_manifest_is_provably_zero_supply(self):
        missing = os.path.join(tempfile.mkdtemp(), "nope.jsonl")
        self.assertIsNone(accrual.load_jsonl(missing))
        prog = accrual.progress(accrual.load_jsonl(missing), None)
        self.assertEqual((prog["supply_after_cap"], prog["hosts"], prog["paired_raw"]), (0, 0, 0))
        self.assertFalse(prog["done"])
        text = accrual.render(prog, missing, False, "s", False)
        self.assertIn("ABSENT", text)
        self.assertIn(f"0 / {accrual.MIN_PAIRS} pairs", text)

    def test_judged_coverage_joins_manifest_pairs_to_scorecard_records(self):
        rows = [
            _rec("h1", "disk", pred_issues=["IFRX-1"], tg_refs=["tg-ref-1"]),
            _rec("h2", "memory", pred_issues=["IFRX-2"], tg_refs=["tg-ref-2"]),
        ]
        scorecard = [
            {"key": "2026-07-28|pair|IFRX-1|tg-ref-1", "judge_unavailable": False},
            {"key": "2026-07-28|pair|OTHER|tg-ref-9", "judge_unavailable": False},
            {"key": "2026-07-28|tg|tg-ref-2"},  # not a pair record — must not count
        ]
        prog = accrual.progress(rows, scorecard)
        self.assertEqual(prog["judged_pairs"], 1)
        self.assertEqual(prog["judged_unavailable"], 0)

    def test_judge_unavailable_pairs_are_visible_in_the_coverage(self):
        rows = [_rec("h1", "disk", pred_issues=["IFRX-1"], tg_refs=["tg-ref-1"])]
        scorecard = [{"key": "2026-07-28|pair|IFRX-1|tg-ref-1", "judge_unavailable": True}]
        prog = accrual.progress(rows, scorecard)
        self.assertEqual((prog["judged_pairs"], prog["judged_unavailable"]), (1, 1))

    def test_load_jsonl_reads_a_real_file_and_skips_junk_lines(self):
        path = os.path.join(tempfile.mkdtemp(), "m.jsonl")
        with open(path, "w", encoding="utf-8") as fh:
            fh.write(json.dumps(_rec("h1", "disk")) + "\n\nnot json\n")
        rows = accrual.load_jsonl(path)
        self.assertEqual(len(rows), 1)


class TestGroundTruthAccrualTerm(unittest.TestCase):
    """§6 2026-08-25: 'done' must hold on BOTH populations. Campaign #1 stopped at bar-met with only 18 of
    24 counted pairs carrying ground truth — the done-condition had no ground-truth term, the exact
    stop-on-an-unpowered-primary failure the 2026-07-27 amendment named. Each test here is the regression
    guard: revert the gt term in accrual.py and one of these goes red."""

    def test_supply_of_unscorable_classes_is_never_done(self):
        # 30 log-fill pairs on 15 hosts: the judged bar is met, the primary population is EMPTY.
        rows = [_rec(f"h{i}", "disk", injector_class="log-fill") for i in range(15) for _ in range(2)]
        prog = accrual.progress(rows, [], scorable={"device-down", "disk-fill"})
        self.assertEqual(prog["supply_after_cap"], 30)
        self.assertEqual(prog["gt_supply"], 0)
        self.assertFalse(prog["done"], "a bar met only by ground-truth-less pairs must not read DONE")
        text = accrual.render(prog, "m", True, "s", True)
        self.assertIn("ground-truth", text)
        self.assertIn("ARE WE DONE ACCRUING: NO", text)

    def test_unknown_scorable_set_refuses_done_loudly(self):
        # A measurement that cannot see its subject must not certify (the accrual mirror of that rule).
        rows = [_rec(f"h{i}", "device", injector_class="device-down") for i in range(15) for _ in range(2)]
        prog = accrual.progress(rows, [], scorable=None)
        self.assertFalse(prog["done"])
        self.assertFalse(prog["scorable_known"])
        self.assertIn("UNKNOWN", accrual.render(prog, "m", True, "s", True))

    def test_records_without_injector_class_are_not_gt_capable_and_are_counted(self):
        rows = [_rec(f"h{i}", "device") for i in range(15) for _ in range(2)]  # no injector_class
        prog = accrual.progress(rows, [], scorable={"device-down"})
        self.assertEqual(prog["gt_supply"], 0)
        self.assertEqual(prog["gt_unknown_class"], 30)
        self.assertFalse(prog["done"])

    def test_gt_scorable_classes_derives_from_the_real_expectations_file(self):
        # Derived, not hand-listed (a guard cannot catch what its list never names): the real file must
        # yield the healable classes and exclude the unhealable one, and log-fill must be absent entirely.
        got = accrual.gt_scorable_classes()
        self.assertIsNotNone(got, "the committed expectations file must be readable from the repo")
        self.assertEqual(got, {"container-down", "device-down", "disk-fill", "service-down"})
        self.assertNotIn("mem-pressure", got)
        self.assertNotIn("log-fill", got)

    def test_unreadable_expectations_is_none_not_everything(self):
        self.assertIsNone(accrual.gt_scorable_classes("/nonexistent/expectations.json"))


if __name__ == "__main__":
    unittest.main(verbosity=2)
