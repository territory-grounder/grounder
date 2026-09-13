#!/usr/bin/env python3
"""Assert that every score-based calibration alert carries its N>0 guard.

promtool validates that a rule PARSES. It cannot know that a rule which reads a calibration SCORE is
meaningless — and actively misleading — when no samples back it.

The failure this prevents is specific and asymmetric. At N=0 the calibrator deliberately WITHHOLDS
tg_confidence_brier / _ece / _mce rather than publishing zeros, precisely so an unmeasured system cannot
render as a flawless one (spec/020 REQ-2022). A rule that reads those gauges without `and
tg_confidence_samples > 0` therefore behaves differently depending on how the absence is handled by the
scrape — and in the case where a zero DOES reach it, a `> threshold` rule stays silent, which reads as "the
agent's confidence is fine" for a system that has never been measured. Silence is the wrong default here.

Asserted over a CLOSED SET of score metrics rather than a hand-listed set of rule names: the next alert
someone adds against a calibration score is exactly the one they will forget to guard.
"""
import os
import re
import sys

RULES = os.path.join(os.path.dirname(__file__), "..", "alert.rules.yml")

# The CLOSED SET of gauges that are withheld at N=0. A rule reading any of these owes the guard.
# tg_confidence_skill is withheld TWICE over: at N=0 like the rest, and again when the base rate is
# degenerate and the ratio is undefined. tg_confidence_base_rate is withheld at N=0.
SCORE_METRICS = ("tg_confidence_brier", "tg_confidence_ece", "tg_confidence_mce",
                 "tg_confidence_base_rate", "tg_confidence_skill")
GUARD = "tg_confidence_samples"


def main() -> int:
    try:
        with open(RULES, encoding="utf-8") as fh:
            body = fh.read()
    except OSError as exc:
        print(f"FAIL cannot read {RULES}: {exc}")
        return 1

    # One (alert-name, expr) per rule. Deliberately line-oriented: an expr spanning lines would not match, and
    # a missed rule must not silently pass, so the count is reported and compared below.
    alerts = re.findall(r"-\s*alert:\s*(\S+)\s*\n\s*expr:\s*(.+)", body)
    if not alerts:
        print("FAIL no alert rules parsed — this validator would pass vacuously")
        return 1

    errs = []
    checked = 0
    for name, expr in alerts:
        if not any(m in expr for m in SCORE_METRICS):
            continue
        checked += 1
        if GUARD not in expr:
            errs.append(
                f"{name}: reads a calibration SCORE but carries no `{GUARD}` guard. Those gauges are "
                f"WITHHELD at N=0 (REQ-2022), so without the guard this rule's silence reads as "
                f"'confidence is fine' for a system that has never been measured."
            )

    if checked == 0:
        print(f"FAIL no rule reads any of {SCORE_METRICS} — either the metrics were renamed or the alerts "
              f"were removed; this validator must not pass by finding nothing to check")
        return 1

    # Second, symmetric obligation: the GUARD gauge is what every score rule leans on, so its own absence is
    # a blind spot for the entire family. `== 0` cannot observe a metric that is not being published — a
    # calibrator whose goroutine never ran emits nothing, and every rule above silently evaluates to nothing.
    # Measured live: a worker recreated at 21:07 published no calibration gauge until the first tick 15
    # minutes later, and no rule could have said so. Require at least one rule to test absence explicitly.
    absent_re = re.compile(r"absent(?:_over_time)?\s*\(\s*" + re.escape(GUARD) + r"\b")
    if not any(absent_re.search(expr) for _, expr in alerts):
        print(f"FAIL no rule tests `absent({GUARD})`. Every calibration-score rule is guarded on that gauge, "
              f"so if it stops being published the whole family goes quiet and the quiet reads as health. "
              f"An absence must be alertable, not merely un-representable.")
        return 1

    for e in errs:
        print("FAIL", e)
    if errs:
        return 1
    print(f"OK  {os.path.normpath(RULES)}: {checked} calibration-score alert(s) all guarded on {GUARD}; "
          f"absence of {GUARD} is itself alerted")
    return 0


if __name__ == "__main__":
    sys.exit(main())
