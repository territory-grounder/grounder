#!/usr/bin/env python3
"""Tests for reconcile-supply.py — the injector-ledger × scorecard join that accrues confirmatory supply.

Two jobs, mirroring test_accrual.py's shape. First, pin the JOIN itself against fixture ledgers and
scorecards (never the live box): the §3a pair definition demands host + coarse-class + window, nearest in
time, one record per fault — and a join that is loose in any clause manufactures pairs that enter the
analysis as evidence. Second, assert STRUCTURALLY that reconcile-supply.py has NO VERDICT PATH: like
accrual.py it is deliberately unfrozen supply plumbing, and an unfrozen file that could reach a conclusion
would be a second, gameable route around the pre-registration freeze.

stdlib unittest, runnable directly — CI executes every test_*.py by glob.
"""

import contextlib
import datetime as dt
import importlib.util
import io
import json
import os
import sys
import tempfile
import unittest

HERE = os.path.dirname(os.path.abspath(__file__))
sys.path.insert(0, HERE)

# reconcile-supply.py is hyphenated (a CLI, not a library); load it by path.
_spec = importlib.util.spec_from_file_location("reconcile_supply", os.path.join(HERE, "reconcile-supply.py"))
rs = importlib.util.module_from_spec(_spec)
_spec.loader.exec_module(rs)

import accrual  # noqa: E402

RS_PATH = os.path.join(HERE, "reconcile-supply.py")
UTC = dt.timezone.utc


def _fault(fid=1, host="dc1wallos01", fault_type="disk-fill",
           injected_at="2026-07-28T12:00:00+00:00", restored_at="2026-07-28T12:40:00+00:00",
           restore_due_at=None, restore_state="restored"):
    return {"id": fid, "host": host, "fault_type": fault_type, "injected_at": injected_at,
            "restored_at": restored_at, "restore_due_at": restore_due_at, "restore_state": restore_state}


def _pair(key="2026-07-28|pair|IFRX-1|librenms-dc1-1", subject_host="dc1wallos01",
          fault_class="disk", tg_created_at="2026-07-28T12:05:00+00:00", judge_unavailable=False):
    return {"key": key, "kind": "pair", "subject_host": subject_host, "fault_class": fault_class,
            "tg_created_at": tg_created_at, "judge_unavailable": judge_unavailable}


class TestNoVerdictPath(unittest.TestCase):
    """SUPPLY PLUMBING MUST BE STRUCTURALLY UNABLE TO CONCLUDE ANYTHING (same bar as accrual.py)."""

    def setUp(self):
        with open(RS_PATH, encoding="utf-8") as fh:
            self.src = fh.read()

    def test_no_statistical_machinery_in_the_source(self):
        low = self.src.lower()
        for token in (
            "mcnemar", "wilcoxon", "holm", "binom", "bootstrap", "p_value", "cliff",
            "significan", "exceeds", "non-inferior", "noninferior",
        ):
            self.assertNotIn(token, low, f"reconcile-supply must carry no statistical/verdict machinery: {token!r}")

    def test_it_never_imports_the_frozen_analysis(self):
        self.assertNotIn("import analyze", self.src)
        self.assertNotIn("from analyze", self.src)

    def test_no_module_attribute_names_a_verdict(self):
        offenders = [n for n in dir(rs) if "verdict" in n.lower()]
        self.assertEqual(offenders, [], "no function, constant, or class here may be a verdict path")

    def test_output_carries_no_verdict_label(self):
        new, excl, _ = rs.reconcile([_fault()], [_pair()], [])
        text = "\n".join(excl) + json.dumps(new)
        for label in ("TG EXCEEDS", "TG MATCHES", "PREDECESSOR HOLDS"):
            self.assertNotIn(label, text)

    def test_the_freeze_constant_is_accruals_not_a_second_copy(self):
        # ONE freeze boundary. A drifting local copy would let this tool accrue what accrual then refuses.
        self.assertIs(rs.PREREG_FREEZE_UTC, accrual.PREREG_FREEZE_UTC)


class TestClassMapping(unittest.TestCase):
    """The injector class -> §3a coarse class mapping is a COMPOSITION of two existing declarations
    (axis_read.go detectRuleMatch × _driver.fault_class), and each class must land where they say."""

    def test_every_injector_class_maps_to_its_declared_coarse_class(self):
        want = {
            "device-down": "device",
            "disk-fill": "disk",
            "log-fill": "disk",       # axis_read.go: presents exactly as disk-fill
            "mem-pressure": "memory",
            "service-down": "service",
            "container-down": "service",  # axis_read.go: a dead container fires the Service up/down rule
        }
        for cls, coarse in want.items():
            self.assertEqual(rs.injector_coarse_class(cls), coarse, cls)

    def test_unknown_and_control_classes_map_to_nothing_not_a_guess(self):
        self.assertEqual(rs.injector_coarse_class("__killswitch__"), "")
        self.assertEqual(rs.injector_coarse_class("novel-fault"), "")
        self.assertEqual(rs.injector_coarse_class(""), "")
        self.assertEqual(rs.injector_coarse_class(None), "")


class TestJoinHappyPath(unittest.TestCase):
    def test_a_post_freeze_fault_joins_its_pair_record(self):
        new, excl, counters = rs.reconcile([_fault()], [_pair()], [])
        self.assertEqual(counters["matched"], 1)
        rec = new[0]
        self.assertEqual(rec["status"], "PAIRED")
        self.assertEqual(rec["fault_id"], 1)
        self.assertEqual(rec["injector_class"], "disk-fill")
        self.assertEqual(rec["fault_type"], "disk")  # the coarse §3a class accrual counts per class
        self.assertEqual(rec["host"], "dc1wallos01")
        self.assertEqual(rec["ts"], "2026-07-28T12:00:00Z")  # the FAULT time, freeze-comparable in accrual
        self.assertEqual(rec["injected_at"], "2026-07-28T12:00:00Z")
        self.assertEqual(rec["restored_at"], "2026-07-28T12:40:00Z")
        self.assertEqual(rec["tg_refs"], ["librenms-dc1-1"])
        self.assertEqual(rec["pred_issues"], ["IFRX-1"])
        self.assertEqual(rec["scorecard_keys"], ["2026-07-28|pair|IFRX-1|librenms-dc1-1"])
        # no exclusion lines: everything matched
        self.assertEqual(excl, [])

    def test_the_record_feeds_accruals_counting_and_judged_coverage(self):
        # The whole point: a reconciled record must be countable supply AND joinable to its scorecard row.
        new, _, _ = rs.reconcile([_fault()], [_pair()], [])
        scorecard = [{"key": "2026-07-28|pair|IFRX-1|librenms-dc1-1", "judge_unavailable": False}]
        prog = accrual.progress(new, scorecard)
        self.assertEqual(prog["supply_after_cap"], 1)
        self.assertEqual(prog["hosts"], 1)
        self.assertEqual(prog["per_class"], {"disk": 1})
        self.assertEqual(prog["judged_pairs"], 1)

    def test_nearest_in_time_wins_and_one_record_serves_one_fault(self):
        near = _pair(key="d|pair|P1|t1", tg_created_at="2026-07-28T12:03:00+00:00")
        far = _pair(key="d|pair|P2|t2", tg_created_at="2026-07-28T12:30:00+00:00")
        # order-independence too: same winner whichever way the rows come back
        a, _, _ = rs.reconcile([_fault()], [far, near], [])
        b, _, _ = rs.reconcile([_fault()], [near, far], [])
        self.assertEqual(a[0]["scorecard_keys"], ["d|pair|P1|t1"])
        self.assertEqual(a[0]["scorecard_keys"], b[0]["scorecard_keys"])
        # two faults, one record: the second fault must NOT reuse the claimed record
        f2 = _fault(fid=2, injected_at="2026-07-28T12:01:00+00:00", restored_at="2026-07-28T12:41:00+00:00")
        new, excl, _ = rs.reconcile([_fault(), f2], [near], [])
        self.assertEqual(len(new), 1)
        self.assertTrue(any("id=2" in e and "unclaimed=0" in e for e in excl),
                        f"the losing fault must be excluded with the claim reason: {excl}")


class TestWindowBoundaries(unittest.TestCase):
    def _one(self, tg_created_at, **fkw):
        new, _, _ = rs.reconcile([_fault(**fkw)], [_pair(tg_created_at=tg_created_at)], [])
        return len(new)

    def test_incident_at_injection_instant_is_inside(self):
        self.assertEqual(self._one("2026-07-28T12:00:00+00:00"), 1)

    def test_incident_before_injection_is_outside(self):
        self.assertEqual(self._one("2026-07-28T11:59:59+00:00"), 0)

    def test_incident_at_restore_plus_slack_is_inside(self):
        # restored 12:40 + 15 min slack = 12:55:00 inclusive
        self.assertEqual(self._one("2026-07-28T12:55:00+00:00"), 1)

    def test_incident_past_restore_plus_slack_is_outside(self):
        self.assertEqual(self._one("2026-07-28T12:55:01+00:00"), 0)

    def test_restore_due_at_bounds_a_still_pending_fault(self):
        kw = dict(restored_at=None, restore_due_at="2026-07-28T13:00:00+00:00", restore_state="pending")
        self.assertEqual(self._one("2026-07-28T13:15:00+00:00", **kw), 1)
        self.assertEqual(self._one("2026-07-28T13:15:01+00:00", **kw), 0)

    def test_a_restoreless_fault_gets_injection_plus_slack_only(self):
        kw = dict(restored_at=None, restore_due_at=None, restore_state="none")  # e.g. mem-pressure
        self.assertEqual(self._one("2026-07-28T12:15:00+00:00", **kw), 1)
        self.assertEqual(self._one("2026-07-28T12:15:01+00:00", **kw), 0)


class TestExclusionsAreLoudAndReasoned(unittest.TestCase):
    def test_class_mismatch_is_excluded_on_both_sides(self):
        mem = _pair(fault_class="memory")
        new, excl, _ = rs.reconcile([_fault()], [mem], [])  # disk-fill fault vs memory pair, same host+window
        self.assertEqual(new, [])
        self.assertTrue(any("fault id=1" in e and "class[disk]-matched=0" in e for e in excl), excl)
        self.assertTrue(any(mem["key"] in e for e in excl), excl)

    def test_host_mismatch_is_named_in_the_funnel(self):
        other = _pair(subject_host="dc1mealie01")
        _, excl, _ = rs.reconcile([_fault()], [other], [])
        self.assertTrue(any("host-matched=0" in e for e in excl), excl)

    def test_pre_freeze_faults_are_excluded_with_the_freeze_named(self):
        old = _fault(injected_at="2026-07-22T20:00:00+00:00", restored_at="2026-07-22T20:30:00+00:00")
        new, excl, _ = rs.reconcile([old], [], [])
        self.assertEqual(new, [])
        self.assertTrue(any("pre-freeze" in e and rs.PREREG_FREEZE_UTC in e for e in excl), excl)

    def test_killswitch_rows_are_control_rows_not_faults(self):
        ks = _fault(fault_type="__killswitch__")
        new, excl, _ = rs.reconcile([ks], [], [])
        self.assertEqual(new, [])
        self.assertTrue(any("kill-switch control row" in e for e in excl), excl)

    def test_an_unmappable_class_is_excluded_never_guessed(self):
        odd = _fault(fault_type="novel-fault")
        new, excl, _ = rs.reconcile([odd], [_pair(fault_class="")], [])
        self.assertEqual(new, [])
        self.assertTrue(any("no declared alert-rule family" in e for e in excl), excl)

    def test_pre_supply_fields_records_are_excluded_with_that_reason(self):
        # The rolling scorecard's records from before _driver recorded the join fields: no class, no time.
        legacy = {"key": "2026-07-25|pair|IFRX-9|librenms-9", "subject_host": "dc1wallos01"}
        new, excl, _ = rs.reconcile([], [legacy], [])
        self.assertEqual(new, [])
        self.assertTrue(any("harvested before _driver.py recorded them" in e for e in excl), excl)

    def test_organic_pairs_are_labelled_as_the_contamination_arm(self):
        organic = _pair(key="d|pair|ORG-1|tg-org-1")
        new, excl, _ = rs.reconcile([], [organic], [])
        self.assertEqual(new, [])
        self.assertTrue(any("organic incident" in e for e in excl), excl)


class TestIdempotency(unittest.TestCase):
    def test_an_already_manifested_fault_is_never_reappended(self):
        first, _, _ = rs.reconcile([_fault()], [_pair()], [])
        again, _, counters = rs.reconcile([_fault()], [_pair()], first)
        self.assertEqual(again, [])
        self.assertEqual(counters["already_reconciled"], 1)

    def test_a_claimed_scorecard_record_cannot_serve_a_second_fault(self):
        first, _, _ = rs.reconcile([_fault()], [_pair()], [])
        f2 = _fault(fid=2, injected_at="2026-07-28T12:01:00+00:00")
        new, excl, _ = rs.reconcile([f2], [_pair()], first)
        self.assertEqual(new, [])
        self.assertTrue(any("id=2" in e and "unclaimed=0" in e for e in excl), excl)

    def test_end_to_end_second_run_appends_nothing(self):
        work = tempfile.mkdtemp()
        ledger = os.path.join(work, "ledger.json")
        scorecard = os.path.join(work, "scorecard.jsonl")
        manifest = os.path.join(work, "confirmatory", "manifest.jsonl")
        with open(ledger, "w", encoding="utf-8") as fh:
            json.dump([_fault()], fh)
        with open(scorecard, "w", encoding="utf-8") as fh:
            fh.write(json.dumps(_pair()) + "\n")
        argv = ["--ledger", ledger, "--scorecard", scorecard, "--manifest", manifest]
        with contextlib.redirect_stdout(io.StringIO()):
            rs.main(argv)
            rs.main(argv)
        with open(manifest, encoding="utf-8") as fh:
            lines = [ln for ln in fh if ln.strip()]
        self.assertEqual(len(lines), 1, "the second run must be a no-op append")
        rec = json.loads(lines[0])
        self.assertEqual((rec["fault_id"], rec["status"]), (1, "PAIRED"))

    def test_dry_run_appends_nothing(self):
        work = tempfile.mkdtemp()
        ledger = os.path.join(work, "ledger.json")
        manifest = os.path.join(work, "manifest.jsonl")
        with open(ledger, "w", encoding="utf-8") as fh:
            json.dump([_fault()], fh)
        scorecard = os.path.join(work, "scorecard.jsonl")
        with open(scorecard, "w", encoding="utf-8") as fh:
            fh.write(json.dumps(_pair()) + "\n")
        with contextlib.redirect_stdout(io.StringIO()):
            rs.main(["--ledger", ledger, "--scorecard", scorecard, "--manifest", manifest, "--dry-run"])
        self.assertFalse(os.path.exists(manifest))


class TestNoVerdictInAccrualEitherWay(unittest.TestCase):
    """The dual-manifest accrual read must still end at supply numbers, not a conclusion."""

    def test_reconciled_supply_renders_without_a_verdict_label(self):
        new, _, _ = rs.reconcile([_fault()], [_pair()], [])
        prog = accrual.progress(new, [])
        text = accrual.render(prog, "m", True, "s", True, confirmatory_path="c", confirmatory_present=True)
        for label in ("TG EXCEEDS", "TG MATCHES", "PREDECESSOR HOLDS"):
            self.assertNotIn(label, text)
        self.assertIn("supply: 1 /", text)


if __name__ == "__main__":
    unittest.main(verbosity=2)
