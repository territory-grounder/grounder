#!/usr/bin/env python3
"""Input-symmetry tests for the blind head-to-head judge (roadmap P1-1/P1-2).

A head-to-head in which one side is allowed more evidence than the other is not measuring the systems, it is
measuring the harness. These pin the properties that make the comparison fair. Run: python3 test_judge_symmetry.py
"""
import importlib.util
import os
import sys

_HERE = os.path.dirname(os.path.abspath(__file__))
sys.path.insert(0, _HERE)
_spec = importlib.util.spec_from_file_location("judge", os.path.join(_HERE, "judge.py"))
judge = importlib.util.module_from_spec(_spec)
sys.modules["judge"] = judge
_spec.loader.exec_module(judge)

FAILED = []


def check(name, cond, detail=""):
    print(("  ok: " if cond else "  FAIL: ") + name + (f" — {detail}" if detail and not cond else ""))
    if not cond:
        FAILED.append(name)


BIG = "OBSERVATION: something was observed\n" * 900


def test_symmetric_budget():
    """Both systems get the SAME trajectory budget. Before this fix the predecessor received up to ~3,200
    characters of reasoning while TG's card carried a 600-character conclusion."""
    tg = judge.normalize_tg({"host": "h", "reasoningExcerpt": BIG, "conclusionExcerpt": "tg concluded"})
    pr = judge.normalize_pred({"issue": "Device down on dc1foo01", "action": "POLL_PAUSE",
                               "reasoningExcerpt": BIG, "rationale": "pred concluded"})
    check("both sides share one trajectory budget",
          abs(len(tg["trajectory"]) - len(pr["trajectory"])) <= 2,
          f"tg={len(tg['trajectory'])} pred={len(pr['trajectory'])}")
    check("both cards are truncation-marked so the judge knows it saw a prefix",
          "…[truncated]" in tg["trajectory"] and "…[truncated]" in pr["trajectory"])


def test_conclusion_survives_truncation():
    """The budget truncates the TAIL, so the verdict must lead — otherwise the richest trajectories (the ones
    most likely to exceed the cap) lose the single most decision-relevant sentence."""
    for name, card in (
        ("tg", judge.normalize_tg({"host": "h", "reasoningExcerpt": BIG, "conclusionExcerpt": "TG VERDICT HERE"})),
        ("pred", judge.normalize_pred({"issue": "x on dc1foo01", "action": "AUTO",
                                       "reasoningExcerpt": BIG, "rationale": "PRED VERDICT HERE"})),
    ):
        check(f"{name}: conclusion survives truncation",
              card["trajectory"].startswith("CONCLUSION:") and "VERDICT HERE" in card["trajectory"])


def test_tg_gets_a_real_trajectory():
    """TG's card must carry its ReAct transcript, not its conclusion. The field is labelled 'trajectory'; it
    previously held a summary, so the judge compared a PROCESS against a SUMMARY on a rubric scoring
    correct_diagnosis and evidence_grounded."""
    traj = "CYCLE 1\nTHOUGHT: check the device\nTOOL: get-device-status\nOBSERVATION: observed dev-x\n\n" \
           "CYCLE 2\nTHOUGHT: check events\nTOOL: get-device-eventlog\nOBSERVATION: observed events-x"
    c = judge.normalize_tg({"host": "h", "reasoningExcerpt": traj, "conclusionExcerpt": "concluded"})
    check("TG trajectory carries the real cycles", "CYCLE 1" in c["trajectory"] and "CYCLE 2" in c["trajectory"])
    check("TG trajectory carries OBSERVATION markers (the judge's evidence proxy reads both sides alike)",
          c["trajectory"].count("OBSERVATION") >= 2)


def test_graceful_degrade_without_steps():
    """A session that genuinely recorded no steps must fall back to its conclusion, never fabricate one."""
    c = judge.normalize_tg({"host": "h", "reasoningExcerpt": "", "conclusionExcerpt": "only a conclusion"})
    check("no-steps session degrades to its conclusion", c["trajectory"] == "only a conclusion")
    e = judge.normalize_tg({"host": "h", "reasoningExcerpt": "", "conclusionExcerpt": ""})
    check("a wholly empty session says so rather than inventing reasoning",
          e["trajectory"] == "(no reasoning captured)")


if __name__ == "__main__":
    print("== judge input-symmetry tests ==")
    test_symmetric_budget()
    test_conclusion_survives_truncation()
    test_tg_gets_a_real_trajectory()
    test_graceful_degrade_without_steps()
    print("judge symmetry tests: " + ("PASS" if not FAILED else f"FAIL ({len(FAILED)})"))
    sys.exit(1 if FAILED else 0)
