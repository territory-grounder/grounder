#!/usr/bin/env python3
"""Completion-parsing + reasoning-model truncation tests for the blind judge (TG-72 failover hardening).

The judge runs behind LiteLLM. When the primary reasoning model (kimi-k3) is down, litellm fails over to
deepseek-v4-pro, which emits its chain-of-thought IN the content field (kimi-k3 uses reasoning_content). On a
tight token budget that CoT exhausts max_tokens before the verdict JSON closes, so the reply truncates
(finish_reason=length) and the judge silently degrades to judge_unavailable for EVERY call during the outage.
The fix has two halves, both pinned here: raise max_tokens so CoT + JSON both fit (corrective), and DIAGNOSE a
finish_reason=length truncation AS truncation instead of an opaque "no JSON object" from parse_verdict
(diagnostic). Run: python3 test_judge_completion.py
"""
import importlib.util
import os
import re
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


VERDICT = ('{"A": {"correct_diagnosis": 4, "evidence_grounded": 3}, '
           '"B": {"correct_diagnosis": 2, "evidence_grounded": 2}, "winner": "A"}')
# Plain-prose chain-of-thought with NO braces, so brace-scanning sees only the verdict object's braces.
COT = ("Let me reason about this carefully. First I weigh what system A concluded against system B. "
       "System A cited the eventlog and named the device; system B stayed vague. ") * 6


def _resp(content, finish="stop", model="kimi-k3", reasoning=None):
    msg = {"content": content}
    if reasoning is not None:
        msg["reasoning_content"] = reasoning
    return {"model": model, "choices": [{"finish_reason": finish, "message": msg}]}


def test_kimi_normal_verdict():
    """kimi-k3 (primary): CoT lives in reasoning_content, the verdict JSON is the whole content, finish=stop."""
    content, served, err = judge._completion_content(_resp(VERDICT, finish="stop", model="kimi-k3", reasoning=COT))
    check("kimi normal: no error", err is None, str(err))
    check("kimi normal: content returned verbatim", content == VERDICT)
    check("kimi normal: served model surfaced", served == "kimi-k3")


def test_deepseek_cot_truncated_is_diagnosed():
    """THE TG-72 failure: the deepseek-v4-pro failover puts CoT in content and runs out of budget before the
    JSON closes (finish=length, an unclosed '{'). Must be reported as TRUNCATION, not empty/opaque-parse."""
    truncated = COT + '{"A": {"correct_diagnosis": 4'  # CoT then an unclosed verdict object
    content, served, err = judge._completion_content(_resp(truncated, finish="length", model="deepseek-v4-pro"))
    check("deepseek truncation: no content returned (the fragment is not passed off as a verdict)", content is None)
    check("deepseek truncation: served model named (the outage is attributable)", served == "deepseek-v4-pro")
    check("deepseek truncation: error names finish_reason=length",
          err is not None and "finish_reason=length" in err, str(err))
    check("deepseek truncation: error says 'truncated' (not 'no JSON object')",
          err is not None and "truncated" in err.lower(), str(err))


def test_empty_content_length_is_truncation():
    """Empty content under finish=length is pure truncation (budget spent entirely on unreturned reasoning)."""
    content, served, err = judge._completion_content(_resp("", finish="length", model="deepseek-v4-pro"))
    check("empty+length: no content", content is None)
    check("empty+length: diagnosed as truncation, not a bare 'empty completion'",
          err is not None and "truncated" in err.lower() and "finish_reason=length" in err, str(err))


def test_empty_content_stop_is_plain_empty():
    """Empty content WITHOUT length is a different fault (a genuinely empty completion) — keep that message,
    so the two failure modes stay distinguishable in the logs."""
    content, served, err = judge._completion_content(_resp("", finish="stop"))
    check("empty+stop: plain empty-completion error (not mislabelled truncation)",
          err is not None and "empty completion" in err and "truncated" not in err.lower(), str(err))


def test_reasoning_content_fallback_preserved():
    """A model that leaves content empty but puts the answer in reasoning_content must still be read — the
    pre-existing behaviour this refactor must not drop."""
    content, served, err = judge._completion_content(_resp("", finish="stop", reasoning=VERDICT))
    check("reasoning_content fallback: verdict recovered", content == VERDICT and err is None, str(err))


def test_complete_json_at_length_is_not_discarded():
    """A verdict that DID close before the cap (finish=length only because the model rambled AFTER) is fully
    usable — the truncation guard must NOT throw it away. Guards against over-eager rejection, INCLUDING the
    case a brace-position heuristic got wrong: trailing prose that carries an unmatched '{'. Under
    finish_reason=length the model was cut mid-sentence, so a stray '{' in trailing commentary about
    runbooks/JSON/config (exactly this judge's domain) is plausible, not exotic."""
    for label, tail in (
        ("plain ramble", "\n\nAnd in further thought I would add much more commentary that got cut"),
        ("stray unmatched brace", " By the way the runbook template uses { as an opening delimiter."),
        ("mentions a JSON payload", " A payload like {\"host\": ... would also cut off here, e.g. {"),
    ):
        content = VERDICT + tail
        c, served, err = judge._completion_content(_resp(content, finish="length", model="deepseek-v4-pro"))
        check(f"complete-JSON-at-length ({label}): content returned, not falsely truncation-rejected",
              c == content and err is None, str(err))
        check(f"complete-JSON-at-length ({label}): the returned content still parses to a verdict",
              err is None and judge.parse_verdict(content).get("winner") == "A")


def test_error_shape_has_no_choices():
    """A LiteLLM error object (no 'choices') is reported as an error, with no fabricated content."""
    content, served, err = judge._completion_content({"error": {"message": "budget exceeded", "type": "x"}})
    check("error-shape: no content", content is None)
    check("error-shape: reported as a LiteLLM error", err is not None and "LiteLLM error" in err, str(err))


def test_fixed_happy_path_cot_prefix_then_complete_json():
    """The corrective half's intent: with enough budget the deepseek failover emits CoT-in-content THEN a
    complete verdict (finish=stop). _completion_content returns it and parse_verdict recovers the object from
    behind the CoT prefix — i.e. once max_tokens is large enough, the failover path judges correctly."""
    full = COT + VERDICT
    content, served, err = judge._completion_content(_resp(full, finish="stop", model="deepseek-v4-pro"))
    check("failover happy path: content returned", content == full and err is None, str(err))
    v = judge.parse_verdict(content)
    check("failover happy path: verdict parsed from behind the CoT prefix", v.get("winner") == "A")


def test_max_tokens_budget_is_large_enough_for_cot_plus_verdict():
    """Corrective half (source-scan guard): the payload's max_tokens must stay >= 16000. At 4000 the deepseek
    CoT-in-content path had no headroom for CoT + the verdict. A silent revert re-opens TG-72, so pin the floor."""
    with open(os.path.join(_HERE, "judge.py"), encoding="utf-8") as fh:
        src = fh.read()
    nums = [int(n) for n in re.findall(r'"max_tokens"\s*:\s*(\d+)', src)]
    check("max_tokens payload literal is present", len(nums) >= 1, f"found {nums}")
    check("every max_tokens literal is >= 16000 (headroom for CoT + verdict on the failover model)",
          all(n >= 16000 for n in nums), f"found {nums}")


if __name__ == "__main__":
    print("== judge completion / truncation-diagnosis tests ==")
    test_kimi_normal_verdict()
    test_deepseek_cot_truncated_is_diagnosed()
    test_empty_content_length_is_truncation()
    test_empty_content_stop_is_plain_empty()
    test_reasoning_content_fallback_preserved()
    test_complete_json_at_length_is_not_discarded()
    test_error_shape_has_no_choices()
    test_fixed_happy_path_cot_prefix_then_complete_json()
    test_max_tokens_budget_is_large_enough_for_cot_plus_verdict()
    print("judge completion tests: " + ("PASS" if not FAILED else f"FAIL ({len(FAILED)})"))
    sys.exit(1 if FAILED else 0)
