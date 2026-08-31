"""Oracle for arm_health.assess — the both-arms cycle health check added after TG-545.

unittest (not bare pytest funcs) with a __main__ runner, because the shadowbench-analysis CI job runs
each file as `python3 test_*.py`, not under pytest — a bare-function file would import, define, and exit 0
having run NOTHING (the "green that ran nothing" scar this directory already carries for test_rubric.py /
test_judge_symmetry.py). Every sibling test uses this shape for that reason.
"""
import unittest

from arm_health import assess


class ArmHealthTest(unittest.TestCase):
    def test_healthy_worker_with_activity_is_ok(self):
        ok, _ = assess("healthy", 6, 9)
        self.assertTrue(ok)

    def test_unhealthy_worker_is_degraded(self):
        ok, reason = assess("unhealthy", 6, 9)
        self.assertFalse(ok)
        self.assertIn("not healthy", reason)

    def test_unknown_worker_is_degraded(self):
        # ssh/docker failed to report — treat an unknown arm as degraded, never healthy-by-default.
        ok, _ = assess("unknown", 6, 9)
        self.assertFalse(ok)

    def test_stalled_arm_while_injecting_is_degraded(self):
        # The TG-545 shape: injects flowing, worker claims healthy, but ZERO triage banked.
        ok, reason = assess("healthy", 0, 9)
        self.assertFalse(ok)
        self.assertIn("stalled", reason)

    def test_quiet_cycle_is_not_a_false_alarm(self):
        # No injects this window -> nothing to triage -> a zero triage count is expected, not a stall.
        ok, _ = assess("healthy", 0, 0)
        self.assertTrue(ok)

    def test_below_inject_floor_is_not_a_false_alarm(self):
        # A couple of injects is too little for a zero triage to be conclusive (throttle/detection lag).
        ok, _ = assess("healthy", 0, 2)
        self.assertTrue(ok)

    def test_stall_needs_strictly_more_than_floor(self):
        # Exactly one over the floor with zero triage does trip it.
        ok, _ = assess("healthy", 0, 3)
        self.assertFalse(ok)


if __name__ == "__main__":
    unittest.main(verbosity=2)
