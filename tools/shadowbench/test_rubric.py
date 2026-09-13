#!/usr/bin/env python3
"""
test_rubric.py — proves the Python shadowbench judge reads the SAME single-source rubric that core/judge
embeds for the Go surfaces. Runnable directly (`python3 test_rubric.py` → exit 0/1) and as a pytest module.

The Go side (core/judge/rubric_test.go: TestRubricIsSingleSource) asserts the embedded rubric bytes equal
the on-disk core/judge/rubric.json. This side asserts the Python judge loads that SAME file and builds its
prompt from it. Transitively: Go embed == on-disk rubric.json == Python read — one rubric, not two copies.
"""
import hashlib
import json
import os
import sys

_HERE = os.path.dirname(os.path.abspath(__file__))
if _HERE not in sys.path:
    sys.path.insert(0, _HERE)

import judge  # noqa: E402


def _canonical_rubric_path():
    # Independently resolve the canonical file (do not trust judge's constant blindly).
    return os.path.normpath(os.path.join(_HERE, "..", "..", "core", "judge", "rubric.json"))


def test_python_reads_the_canonical_file():
    """judge.RUBRIC_PATH resolves to core/judge/rubric.json and loads its exact bytes."""
    canonical = _canonical_rubric_path()
    assert os.path.abspath(judge.RUBRIC_PATH) == os.path.abspath(canonical), (
        f"judge points at {judge.RUBRIC_PATH!r}, not the canonical {canonical!r}"
    )
    with open(canonical, "rb") as fh:
        raw = fh.read()
    disk = json.loads(raw)
    assert judge.load_rubric() == disk, "judge.load_rubric() diverged from the on-disk rubric.json"
    # sha256 of the one source — a stable fingerprint the Go embed shares by construction (embed == disk).
    print("rubric.json sha256:", hashlib.sha256(raw).hexdigest())


def test_dimensions_and_params_are_sourced():
    r = judge.load_rubric()
    assert judge.DIMENSIONS == list(r["dimensions"]), "DIMENSIONS must be the rubric's dimensions"
    assert judge.DEFAULT_MODEL == r["params"]["model"], "DEFAULT_MODEL must be the rubric's params.model"
    # Deterministic-judging temperature is single-sourced too.
    assert r["params"]["temperature"] == 0, "canonical judge temperature must be 0"


def test_build_prompt_uses_the_shared_calibration_text():
    """The A/B prompt's guidance + hollow-proposal rule are the shared rubric text verbatim (no local copy)."""
    r = judge.load_rubric()
    card = {
        "subject_host": "web01", "alert": "HostDown", "severity": "critical",
        "decision_band": "POLL_PAUSE", "proposed_action": True, "proposed_op": "restart-service",
        "committed_prediction": "clears in 10m", "evidence_summary": "2 cited", "investigation_notes": "(none)",
        "outcome": "proposed", "trajectory": "looked at device",
    }
    prompt = judge.build_prompt(card, None)
    # guidance body (leading "Guidance: " stripped in the A/B header) and the hollow rule must appear verbatim.
    guidance_body = r["guidance"].strip()
    if guidance_body.startswith("Guidance: "):
        guidance_body = guidance_body[len("Guidance: "):]
    assert guidance_body in prompt, "A/B prompt must carry the shared rubric guidance verbatim"
    assert r["hollow_proposal_rule"].strip() in prompt, "A/B prompt must carry the shared hollow-proposal rule"
    # And it must NOT hard-code the old, drifted wording.
    assert "scores WELL on sensible_proposal, evidence_grounded and correct_diagnosis" not in prompt, (
        "the old hand-copied guidance wording must be gone — the rubric is now single-sourced"
    )


def _run():
    fns = [test_python_reads_the_canonical_file, test_dimensions_and_params_are_sourced,
           test_build_prompt_uses_the_shared_calibration_text]
    for fn in fns:
        fn()
        print("PASS", fn.__name__)
    print("OK: Python shadowbench judge reads the one-source rubric.json")


if __name__ == "__main__":
    try:
        _run()
    except AssertionError as e:
        print("FAIL:", e, file=sys.stderr)
        sys.exit(1)
