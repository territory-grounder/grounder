#!/usr/bin/env python3
"""
judge.py — scripted, headless, BLIND unified judge for the shadow head-to-head
benchmark (shadowbench v2).

Given ONE predecessor (claude-gateway) incident trajectory and/or ONE Territory
Grounder (TG) incident trajectory for the SAME incident, this scores EACH system
1..5 on the ONE unified rubric read from the single source core/judge/rubric.json
(the same file core/judge embeds for the Go eval gate / rejudge / durable cron) =
TG's five eval dimensions: correct_diagnosis, evidence_grounded, sensible_proposal,
appropriate_band, falsifiable_prediction. Either side may be absent (single-sided).

It is BLIND: the two trajectories are normalized into an identical field template,
their NATIVE judge scores are stripped, system-identifying jargon is neutralized,
and they are presented to the judge model as "System A"/"System B" in RANDOMIZED
order. The verdict is de-blinded only AFTER the model replies, via a mapping the
model never sees.

Judge model: the box LiteLLM `primary` reasoning alias (kimi-k3, with LiteLLM's
own fallback chain → deepseek-v4-pro when kimi is overloaded) — the same model
TG's eval uses. LiteLLM is reached over SSH to the box; the master key is read
from /srv/tg/deploy/.env INSIDE the box and is never placed in this host's argv,
env, stdout, or logs.

HONEST DEGRADE: if LiteLLM is unreachable / errors / returns no parseable verdict,
the emitted record carries "judge_unavailable": true with an error string and NULL
scores — never a fabricated number.

CREDENTIAL SAFETY: every trajectory string is passed through redact() (reused from
extract_predecessor.py) before it enters the prompt, the payload, stdout, or any
log. The predecessor shadow log carries a live NetBox token; it must never leave.

READ-ONLY: this script only READS its input JSON and POSTs a chat completion. It
mutates nothing on either system.

Usage:
    # single-sided (predecessor only) — the required smoke test:
    ./extract_predecessor.py --date 2026-07-18 --json > pred.json
    ./judge.py --pred-from pred.json --incident-key IFRNLLEI01PRD-1824

    # single-sided (TG only): a TG row dict on stdin
    ./judge.py --tg - < tg_row.json --incident-key librenms-...

    # head-to-head: one predecessor incident record + one TG row
    ./judge.py --pred pred_incident.json --tg tg_row.json --incident-key ...

    # inspect the exact (redacted) prompt without calling the model:
    ./judge.py --pred-from pred.json --incident-key ... --dry-run

Flags:
    --pred FILE           predecessor incident record (one incidents[] element), '-' = stdin
    --pred-from FILE      full extract_predecessor.py --json doc; --incident-key selects the incident
    --tg FILE             TG row dict (one extract_tg.sh JSON row), '-' = stdin
    --tg-from FILE        a JSON array of TG rows; --incident-key / --host selects the row
    --incident-key KEY    incident label + selector
    --host HOST           optional subject-host hint (selects/annotates)
    --model ALIAS         judge model alias (default: primary)
    --seed N              deterministic A/B randomization (default: OS-random)
    --dry-run             build+redact the prompt, print it, do NOT call the model
    --out FILE            also write the verdict JSON here
    --ssh-key PATH        default ~/.ssh/one_key
    --tg-host HOST        default dc1tg01
    --ssh-user USER       default root
    --env-path PATH       default /srv/tg/deploy/.env (on the box)
    --timeout SECS        LiteLLM call timeout (default 150)
"""
from __future__ import annotations

import argparse
import datetime as _dt
import json
import os
import random
import re
import subprocess
import sys
import time

# Reuse the v1 redaction (single source of truth) + the credential-safe posture.
_HERE = os.path.dirname(os.path.abspath(__file__))
if _HERE not in sys.path:
    sys.path.insert(0, _HERE)
try:
    from extract_predecessor import redact  # type: ignore
except Exception:  # pragma: no cover - fallback keeps the judge safe if the import path breaks
    _FALLBACK_REDACTIONS = [
        (re.compile(r"(?i)\b(Token|Bearer)\s+[A-Za-z0-9._\-]{16,}"), r"\1 <REDACTED>"),
        (re.compile(r"(?i)(Authorization:\s*)\S.+?(?=[\"'\\]|$)"), r"\1<REDACTED>"),
        (re.compile(r"\b[0-9a-fA-F]{32,}\b"), "<REDACTED>"),
        (re.compile(r"(?i)\b(password|passwd|secret|api[_-]?key|token)\b(\s*[=:]\s*)\S+"),
         r"\1\2<REDACTED>"),
    ]

    def redact(value):
        if value is None:
            return None
        if isinstance(value, str):
            out = value
            for rx, repl in _FALLBACK_REDACTIONS:
                out = rx.sub(repl, out)
            return out
        if isinstance(value, dict):
            return {k: redact(v) for k, v in value.items()}
        if isinstance(value, list):
            return [redact(v) for v in value]
        return value


# ONE rubric source: the judge rubric text (guidance + hollow-proposal rule), the five scored dimensions,
# and the canonical JudgeParams (model + temperature) are READ from core/judge/rubric.json — the SAME file
# core/judge embeds for the Go surfaces (the eval gate, rejudge, the durable session-judge cron). There is
# no hand-copied second rubric here: if the calibration wording, the judged axes, or the judge model change,
# they change in one place and both languages follow (OpenAI Evals 3.4; the one-judge principle). The path
# is repo-relative to this file (tools/shadowbench → ../../core/judge/rubric.json).
RUBRIC_PATH = os.path.normpath(os.path.join(_HERE, "..", "..", "core", "judge", "rubric.json"))


def load_rubric(path=RUBRIC_PATH):
    """Load the single-source rubric.json. Raises if it is missing/malformed — the shadow judge must never
    silently fall back to a divergent local copy (that is the exact drift this unification removes)."""
    with open(path, "r", encoding="utf-8") as fh:
        r = json.load(fh)
    for k in ("dimensions", "guidance", "hollow_proposal_rule", "params"):
        if k not in r:
            raise ValueError(f"rubric.json missing required key {k!r}")
    return r


RUBRIC = load_rubric()
# The five unified dimensions — sourced from rubric.json (order preserved), never re-declared.
DIMENSIONS = list(RUBRIC["dimensions"])

DEFAULT_MODEL = RUBRIC["params"]["model"]
DEFAULT_SSH_KEY = os.path.expanduser("~/.ssh/one_key")
DEFAULT_TG_HOST = "dc1tg01"
DEFAULT_SSH_USER = "root"
DEFAULT_ENV_PATH = "/srv/tg/deploy/.env"
LITELLM_URL = "http://127.0.0.1:4000/v1/chat/completions"

# The judge call is the head-to-head bottleneck: kimi-k3 at reasoning_effort=max is slow, and LiteLLM
# intermittently RESETS a long connection under concurrent worker load (observed: curl exit 56 recv-reset /
# exit 7 connect-fail on ~60% of a run with only 1 retry). These are TRANSIENT — a fresh draw after a short
# backoff almost always succeeds. Retry up to JUDGE_MAX_ATTEMPTS so the head-to-head scores nearly every pair
# instead of dropping the majority (env-overridable for a slower/faster upstream).
JUDGE_MAX_ATTEMPTS = max(1, int(os.environ.get("JUDGE_MAX_ATTEMPTS", "5")))

# Hostname pattern for pulling the incident SUBJECT out of a predecessor issue title
# (the predecessor record's `host` is the RUNNER box, not the incident subject).
_HOST_RX = re.compile(r"\b([a-z]{2}[a-z0-9]{2,}\d{2}[a-z0-9]+\d{2})\b")
# Neutralize system-identifying jargon so the blind judge cannot fingerprint origin.
_JARGON = [
    (re.compile(r"(?i)\bshadow[- ]?(gate|hook|mode)\b"), "safety guard"),
    (re.compile(r"(?i)\bmutation-shadow[-\w.]*"), "safety guard"),
    (re.compile(r"(?i)\bclaude[- ]?gateway\b"), "the agent"),
    (re.compile(r"(?i)\bterritory[- ]?grounder\b"), "the agent"),
    (re.compile(r"(?i)\bopenclaw\b"), "the upstream triage layer"),
    (re.compile(r"(?i)\bgateway\.db\b"), "the record store"),
    # TG-specific evidence-id scheme (lnms-…, estate-ctx-…) → generic, so rendering real ids in the
    # card (below) improves fidelity WITHOUT letting the blind judge fingerprint the system by id shape.
    (re.compile(r"(?i)\b(?:lnms|estate-ctx|estate)-(?=[a-z0-9])"), "obs-"),
]

# ONE symmetric trajectory budget for BOTH systems (roadmap P1-1). Before this, the character budget was set
# independently per extractor — the predecessor received up to ~3,200 characters of reasoning while TG's card
# carried a 600-character conclusion. A head-to-head in which one side is allowed four times the evidence is
# not measuring the systems, it is measuring the harness. The cap lives HERE, applied identically to both
# cards, so the budget can never drift apart again.
_TRAJ_CAP = 3000


def _lead_with_conclusion(conclusion, reasoning):
    """Compose a trajectory as CONCLUSION-then-reasoning, so the shared budget truncates supporting detail
    rather than the verdict. Skips the header when the reasoning already opens with the same conclusion."""
    concl = (conclusion or "").strip()
    body = (reasoning or "").strip()
    if not body:
        return concl
    if not concl or concl[:60] in body:
        return body
    return "CONCLUSION: " + concl + "\n\n" + body


def _cap_trajectory(text):
    """Apply the shared trajectory budget, marking truncation so the judge knows it saw a prefix."""
    t = (text or "").strip()
    if len(t) <= _TRAJ_CAP:
        return t
    return t[:_TRAJ_CAP].rstrip() + " …[truncated]"


# Caps for the richer TG card fields (bounded so a long prediction can't blow up the prompt / make a
# reasoner ramble past its JSON): the committed-prediction text and the number of evidence ids rendered.
_PRED_CAP = 700
_EV_CAP = 12
# Strip any native judge score that leaked into an outcome/disposition string —
# these must NEVER reach the blind judge (they'd both leak identity and bias it).
_NATIVE_SCORE_RX = re.compile(
    r"(?i)\s*;?\s*(judge_score|judge_action|judgeoverall|overall_score|trajectory)\s*[=:]\s*\S+"
)


def _neutralize(text):
    if not text:
        return text
    out = text
    for rx, repl in _JARGON:
        out = rx.sub(repl, out)
    return out


def _strip_native_scores(text):
    if not text:
        return text
    return _NATIVE_SCORE_RX.sub("", text).strip().strip(";").strip()


# ---------------------------------------------------------------------------
# Normalization → a neutral, system-agnostic trajectory "card"
# ---------------------------------------------------------------------------

def _clean(s):
    """redact → neutralize jargon → strip native scores. Applied to every card string."""
    return _strip_native_scores(_neutralize(redact(s or "")))


def normalize_pred(rec):
    """Predecessor incidents[] element (extract_predecessor.py) → neutral card."""
    issue = rec.get("issue") or ""
    subject = ""
    m = _HOST_RX.search(issue)
    if m:
        subject = m.group(1)
    subject = subject or rec.get("host") or "(unknown)"

    band = (rec.get("action") or "").strip()
    # CONCLUSION FIRST — see _lead_with_conclusion. The predecessor's `rationale` is its conclusion
    # equivalent, so both systems are composed and budgeted identically.
    traj = _lead_with_conclusion(rec.get("rationale"), rec.get("reasoningExcerpt"))

    stood_down = bool(re.search(r"(?i)no remediation|no action|stand[- ]?down|do not (act|restart)|poll_pause", band + " " + traj))
    proposed = band.upper() in {"AUTO", "AUTO_NOTICE"} and not stood_down

    n_blocked = int(rec.get("blockedInvestigations") or 0)
    inv_notes = ""
    if n_blocked:
        inv_notes = (f"{n_blocked} investigation command(s) were blocked by a safety guard; "
                     "the agent fell back to alternative read-only tools.")
    # crude but honest evidence proxy: count grounded OBSERVATION cycles in the trajectory
    ev_obs = len(re.findall(r"(?i)OBSERVATION", traj))
    ev_summary = (f"{ev_obs} grounded observation cycle(s) cited inline in the trajectory"
                  if ev_obs else "(evidence, if any, is cited inline in the trajectory)")

    return {
        "system": "pred",
        "subject_host": subject,
        "alert": _clean(issue) or "(unspecified)",
        "severity": "(unspecified)",
        "decision_band": band or "(none)",
        "proposed_action": proposed,
        "proposed_op": "(none — grounded stand-down)" if not proposed else "(action proposed; op not recorded)",
        "committed_prediction": "(none)",
        "evidence_summary": ev_summary,
        "investigation_notes": _clean(inv_notes) or "(none)",
        "outcome": _clean(rec.get("outcome")) or "(unspecified)",
        "trajectory": _cap_trajectory(_clean(traj)) or "(no reasoning captured)",
    }


def normalize_tg(row):
    """TG row (extract_tg.sh JSON) → neutral card."""
    band = (row.get("band") or "").strip()
    op = (row.get("op") or "").strip()
    concl = row.get("conclusionExcerpt") or ""
    has_pred = bool(row.get("hasPrediction"))
    ev_count = row.get("evidenceCount")
    try:
        ev_count = int(ev_count)
    except (TypeError, ValueError):
        ev_count = 0

    # Prefer the explicit terminal flags when the extractor carries them; else infer from op/conclusion.
    if "proposed" in row:
        proposed = bool(row.get("proposed"))
    else:
        stood_down = ("stop" in op.lower()) or bool(re.search(r"(?i)no[- ]proposal|no action|stand[- ]?down", op + " " + concl))
        proposed = (not stood_down) and (op != "" or band.upper() in {"AUTO", "AUTO_NOTICE"})
    has_pred = has_pred or bool(row.get("predicted"))

    # Committed prediction — INPUT-FIDELITY FIX: TG's real eval judge (core/judge/judge.go) scores
    # falsifiable_prediction over the ACTUAL committed-prediction text; the old card collapsed it to a
    # bare boolean, so the shadow judge scored that dimension blind (and a vacuous prediction could read
    # as "committed" = fine). Prefer the real (redacted+neutralized+capped) text when the extractor
    # carries it; fall back to the boolean flag, then "(none)". Symmetric-safe: the predecessor card
    # renders its own prediction the same way (predecessors structurally commit none).
    pred_text = (row.get("prediction") or "").strip()
    if pred_text:
        committed = _clean(pred_text[:_PRED_CAP]) + (" …[truncated]" if len(pred_text) > _PRED_CAP else "")
    elif has_pred:
        committed = "(a machine prediction was committed)"
    else:
        committed = "(none)"

    # Cited evidence — INPUT-FIDELITY FIX: render the real evidence ids (redacted+neutralized+capped)
    # the way TG's judge sees them, not just a count; fall back to the count-only summary. The id scheme
    # is neutralized (see _JARGON) so this does not de-blind a head-to-head.
    ev_ids = row.get("evidenceIds") or row.get("evidence_ids") or []
    if isinstance(ev_ids, list) and ev_ids:
        shown = _clean(", ".join(str(e) for e in ev_ids[:_EV_CAP]))
        ev_summary = f"{ev_count} cited evidence id(s): {shown}"
    else:
        ev_summary = f"{ev_count} cited evidence id(s)"

    # REASONING TRAJECTORY — INPUT-FIDELITY FIX (roadmap P1-1). This field previously carried TG's 600-char
    # CONCLUSION while the predecessor card carried that system's full reasoning narrative, so the judge was
    # comparing a PROCESS against a SUMMARY on a rubric that scores correct_diagnosis and evidence_grounded.
    # TG's real ReAct trajectory (agent_step) is now extracted; fall back to the conclusion only when a
    # session genuinely recorded no steps, and say which it was rather than implying reasoning was absent.
    # CONCLUSION FIRST, then the supporting reasoning. The budget truncates the TAIL, so a card that appended
    # the conclusion last would drop the single most decision-relevant sentence on exactly the richest
    # trajectories — the ones most likely to exceed the cap. Leading with it means truncation costs supporting
    # detail, never the verdict. Applied the same way on the predecessor card.
    traj = _lead_with_conclusion(concl, row.get("reasoningExcerpt"))

    return {
        "system": "tg",
        "subject_host": row.get("host") or "(unknown)",
        "alert": _clean(row.get("alertRule")) or "(unspecified)",
        "severity": _clean(row.get("severity")) or "(unspecified)",
        "decision_band": band or "(none)",
        "proposed_action": proposed,
        "proposed_op": _clean(op) or ("(none — grounded stand-down)" if not proposed else "(action proposed)"),
        "committed_prediction": committed,
        "evidence_summary": ev_summary,
        "investigation_notes": "(none)",
        "outcome": _clean(row.get("outcome") or row.get("op")) or "(unspecified)",
        "trajectory": _cap_trajectory(_clean(traj)) or "(no reasoning captured)",
    }


# ---------------------------------------------------------------------------
# Prompt (unified rubric read from core/judge/rubric.json — the one source — adapted for two systems)
# ---------------------------------------------------------------------------

def _render_card(card):
    if card is None:
        return ("(NO TRAJECTORY — this system did not triage this incident. "
                "Leave every score for this system null.)")
    return (
        f"Subject host: {card['subject_host']}\n"
        f"Alert: {card['alert']}\n"
        f"Severity: {card['severity']}\n"
        f"Decision band: {card['decision_band']}\n"
        f"Proposed an action: {'yes' if card['proposed_action'] else 'no (grounded stand-down)'}\n"
        f"Proposed action / op: {card['proposed_op']}\n"
        f"Committed prediction: {card['committed_prediction']}\n"
        f"Cited evidence: {card['evidence_summary']}\n"
        f"Investigation notes: {card['investigation_notes']}\n"
        f"Outcome: {card['outcome']}\n"
        f"Reasoning trajectory:\n{card['trajectory']}\n"
    )


def build_prompt(card_a, card_b):
    b = []
    b.append(
        "You are a strict, IMPARTIAL SRE triage-quality judge. Two systems (System A and System B) "
        "each independently triaged an infrastructure alert in READ-ONLY shadow mode (no mutation was "
        "performed by either). You do NOT know which system is which; judge ONLY on the trajectory shown, "
        "never on style or verbosity. Score EACH system on EACH of the five dimensions from 1 (poor) to "
        "5 (excellent)."
    )
    b.append("")
    # Calibration text is the ONE source (rubric.json) — byte-identical wording to the eval gate's judge,
    # so both systems here are scored by the same rubric the production eval uses (no hand-copied drift).
    # The leading "Guidance:" is dropped so the A/B header reads naturally; the body is verbatim.
    _guidance = RUBRIC["guidance"].strip()
    if _guidance.startswith("Guidance: "):
        _guidance = _guidance[len("Guidance: "):]
    b.append("Guidance (identical rubric for both systems): " + _guidance)
    b.append(RUBRIC["hollow_proposal_rule"].strip())
    b.append(
        "FALSIFIABLE_PREDICTION APPLICABILITY: if a system made a grounded decision NOT to act (a "
        "stand-down / no remediation), it committed no action-consequence to predict — falsifiable_prediction "
        "is genuinely N/A for that system. Output null (not a low number) for that system's "
        "falsifiable_prediction; do not penalize a correct stand-down for having nothing to falsify."
    )
    b.append(
        # The WINNER must be decided on like-for-like ground. A dimension scored for one system and null for
        # the other is not a contest on that dimension — awarding the win there rewards a system for competing
        # in a category the other structurally cannot enter. Measured: in a re-judge of 15 archived pairs,
        # BOTH of the wins one system received were awarded on a dimension where the other side was null, even
        # though it did not lead on any dimension both were scored on. Scores stay per-system and honest; only
        # the WINNER is restricted to comparable ground.
        "CHOOSING THE WINNER — LIKE FOR LIKE: decide the winner ONLY on dimensions where BOTH systems have a "
        "numeric score. If a dimension is null for either system, it is NOT a contest on that dimension and it "
        "MUST NOT decide the winner — however strong the scored side is there. If the two systems are level on "
        "every mutually-scored dimension, answer \"tie\" even when one has extra non-comparable strengths, and "
        "say in the reason which dimensions you compared."
    )
    b.append("")
    b.append("=== SYSTEM A ===")
    b.append(_render_card(card_a))
    b.append("=== SYSTEM B ===")
    b.append(_render_card(card_b))
    b.append("")
    b.append(
        "Reply with ONLY this JSON object and nothing else:\n"
        '{"A":{"correct_diagnosis":N,"evidence_grounded":N,"sensible_proposal":N,'
        '"appropriate_band":N,"falsifiable_prediction":N_or_null,"comment":"one line"},'
        '"B":{"correct_diagnosis":N,"evidence_grounded":N,"sensible_proposal":N,'
        '"appropriate_band":N,"falsifiable_prediction":N_or_null,"comment":"one line"},'
        '"winner":"A"|"B"|"tie","reason":"one line"}\n'
        "For a system whose trajectory is absent, set all five of its scores to null and its comment to "
        '"absent". Every N is an integer 1..5.'
    )
    return "\n".join(b)


# ---------------------------------------------------------------------------
# LiteLLM call (over SSH; key stays on the box) + defensive parse
# ---------------------------------------------------------------------------

def call_litellm(prompt, args):
    """POST one chat completion to the box LiteLLM. Returns (content, served_model, error)."""
    payload = json.dumps({
        "model": args.model,
        "messages": [{"role": "user", "content": prompt}],
        # Deterministic judging — the canonical JudgeParams temperature, read from the one source
        # (rubric.json) so Go and Python agree. (For the kimi-k3 "primary" tier LiteLLM strips the sampling
        # params; the value is the shared intent, honored on any tier that accepts it — see core/judge.)
        "temperature": RUBRIC["params"]["temperature"],
        # Reasoning models spend tokens on chain-of-thought BEFORE the answer, in TWO shapes: kimi-k3 (the
        # "primary" tier) emits CoT in reasoning_content and the verdict JSON in content; deepseek-v4-pro
        # (the FAILOVER served when kimi-k3 is down — TG-72 re-run 2026-08-15) emits CoT IN content, so the
        # whole budget goes to CoT before the JSON and the reply truncates mid-reasoning → no closing JSON →
        # judge_unavailable, silently degrading EVERY judge call during a primary-model outage. 4000 had no
        # headroom for the content-CoT path; 16000 fits CoT + the verdict on any tier (a CEILING, not a
        # target — kimi-k3, whose content is just the JSON, never approaches it). Truncation is also detected
        # below (finish_reason=length), so this failure is diagnosed rather than an opaque parse error.
        "max_tokens": 16000,
    })
    # The master key is read on the box INSIDE the remote shell and never touches this host.
    # SOURCE the OpenBao-delivered env drop (spec/024) — it holds `export LITELLM_MASTER_KEY='…'`
    # SINGLE-QUOTED, so we must source it (as litellm's own entrypoint does), NOT grep the raw value:
    # a grepped/quoted key mismatches litellm's master key → litellm (stateless, no DB) falls through to a
    # virtual-key DB lookup and returns "No connected db". Fall back to a legacy literal in .env for
    # pre-bao/dev boxes. (See memory: litellm-no-connected-db-rootcause.) --data-binary @- reads the
    # payload from ssh stdin (no argv quoting).
    remote = (
        'K=$( set -a; . /dev/shm/tg-litellm-secrets/env 2>/dev/null; set +a; printf %s "$LITELLM_MASTER_KEY" ); '
        '[ -z "$K" ] && K=$(grep -E "^LITELLM_MASTER_KEY=" ' + args.env_path + ' 2>/dev/null | head -n1 | cut -d= -f2- | tr -d "\\r\\n"); '
        'if [ -z "$K" ]; then echo "__NO_KEY__" >&2; exit 3; fi; '
        'curl -s --max-time ' + str(int(args.timeout)) + ' ' + LITELLM_URL + ' '
        '-H "Authorization: Bearer $K" -H "Content-Type: application/json" --data-binary @-'
    )
    ssh_cmd = [
        "ssh", "-i", args.ssh_key,
        "-o", "StrictHostKeyChecking=no",
        "-o", "ConnectTimeout=15",
        "-o", "BatchMode=yes",
        f"{args.ssh_user}@{args.tg_host}",
        remote,
    ]
    try:
        proc = subprocess.run(
            ssh_cmd, input=payload.encode("utf-8"),
            capture_output=True, timeout=args.timeout + 30,
        )
    except subprocess.TimeoutExpired:
        return None, None, "ssh/litellm call timed out"
    except FileNotFoundError:
        return None, None, "ssh binary not found on this host"
    if proc.returncode != 0:
        err = redact(proc.stderr.decode("utf-8", "replace")).strip()[:300]
        return None, None, f"ssh/curl exit {proc.returncode}: {err or 'no stderr'}"
    body = proc.stdout.decode("utf-8", "replace")
    if not body.strip():
        return None, None, "empty response body from LiteLLM"
    try:
        resp = json.loads(body)
    except json.JSONDecodeError:
        return None, None, f"non-JSON LiteLLM body: {redact(body)[:200]}"
    return _completion_content(resp)


def _completion_content(resp):
    """Extract (content, served_model, error) from a parsed LiteLLM chat-completion response.

    Pure (no I/O) so the reasoning-model truncation paths are unit-testable without an ssh round-trip.
    Two model shapes must both work: kimi-k3 (primary) puts chain-of-thought in reasoning_content and the
    verdict JSON in content; deepseek-v4-pro (the failover served when kimi-k3 is down) puts CoT IN content,
    so a tight token budget is spent on CoT before the JSON and the reply truncates (finish_reason=length)
    before the object closes. That second case is diagnosed here as truncation rather than surfacing later
    as an opaque "no JSON object" from parse_verdict."""
    if "choices" not in resp:
        return None, None, f"LiteLLM error: {redact(json.dumps(resp))[:300]}"
    choice = (resp.get("choices") or [{}])[0] or {}
    msg = choice.get("message", {}) or {}
    content = msg.get("content") or ""
    if not content.strip():
        # reasoning models occasionally place the answer in reasoning_content
        content = msg.get("reasoning_content") or ""
    served = resp.get("model")
    finish = choice.get("finish_reason")
    if not content.strip():
        # Empty content under finish_reason=length = pure truncation: the served model spent the whole
        # budget on (unreturned) reasoning. Name it, so a primary->failover switch reads as truncation
        # rather than a bare "empty completion".
        if finish == "length":
            return None, served, (
                f"LiteLLM reply truncated (finish_reason=length): served model {served!r} exhausted "
                f"max_tokens before emitting any content - the reasoning/failover model needs more budget"
            )
        return None, served, "LiteLLM returned an empty completion"
    # A finish_reason=length reply may be truncated mid-verdict, OR carry a COMPLETE verdict followed by
    # trailing ramble that hit the cap. Don't guess from brace positions - a complete verdict trailed by
    # prose containing a stray '{' (e.g. "the runbook template uses { as a delimiter") would false-positive
    # and discard a recoverable answer. Ask the real parser (parse_verdict, incl. its repair + scrape layers)
    # whether a usable verdict survives: if it does, fall through and return it (identical to the
    # non-truncated success path, and the scrape fallback stays reachable); only when NOTHING parses is the
    # reply truly truncated - the SAME failure as before, but now with a clear cause instead of an opaque
    # "no JSON object", so a primary-model outage is diagnosable rather than looking like a code fault.
    if finish == "length":
        try:
            parse_verdict(content)
        except ValueError:
            return None, served, (
                f"LiteLLM reply truncated (finish_reason=length): served model {served!r} ran out of tokens "
                f"before a parseable verdict closed (chain-of-thought in content on the failover model) - raise max_tokens"
            )
    return content, served, None


def _try_json(s):
    try:
        return json.loads(s)
    except (ValueError, TypeError):
        return None


def _scrape_verdict(frag):
    """Last-resort recovery: regex-scrape the A/B dimension integers + winner out of a fragment whose
    JSON is malformed (typically an unescaped quote inside a comment string). The NUMBERS almost always
    survive; only the free-text comment breaks strict parsing — so we recover real scores rather than
    fabricating or discarding them."""
    out = {}
    posA = frag.find('"A"')
    posB = frag.find('"B"')
    spans = []
    if posA >= 0:
        spans.append(("A", posA, posB if posB > posA else len(frag)))
    if posB >= 0:
        spans.append(("B", posB, len(frag)))
    for letter, start, end in spans:
        sub = frag[start:end]
        side = {}
        for d in DIMENSIONS:
            dm = re.search(r'"' + d + r'"\s*:\s*"?(null|-?\d+)', sub)
            if dm:
                side[d] = None if dm.group(1) == "null" else int(dm.group(1))
        if side:
            out[letter] = side
    wm = re.search(r'"winner"\s*:\s*"?(A|B|tie|draw)"?', frag, re.IGNORECASE)
    if wm:
        out["winner"] = wm.group(1)
    return out or None


def parse_verdict(raw):
    """Defensively extract the judge verdict (model may wrap it in prose/fences, add comments, leave
    trailing commas, or emit an unescaped quote in a comment). Layered: strict → light-repair → scrape."""
    i, j = raw.find("{"), raw.rfind("}")
    if i < 0 or j <= i:
        raise ValueError("no JSON object in judge reply")
    frag = raw[i:j + 1]
    obj = _try_json(frag)
    if obj is None:
        s = re.sub(r"```(?:json)?", "", frag)
        s = re.sub(r"//[^\n]*", "", s)
        s = re.sub(r"/\*.*?\*/", "", s, flags=re.S)
        s = re.sub(r",\s*([}\]])", r"\1", s)  # trailing commas
        obj = _try_json(s)
    if obj is None:
        obj = _scrape_verdict(frag)
    if obj is None:
        raise ValueError("could not parse or scrape a verdict")
    return obj


def _clamp_side(side):
    """Coerce one side's dimension map to {dim: int|None}. Preserves N/A nulls."""
    out = {}
    for d in DIMENSIONS:
        v = side.get(d, None) if isinstance(side, dict) else None
        if v is None or (isinstance(v, str) and v.strip().lower() in {"null", "n/a", "na", ""}):
            out[d] = None
            continue
        n = None
        if isinstance(v, bool):
            n = None
        elif isinstance(v, (int, float)):
            n = int(v)
        elif isinstance(v, str):
            m = re.search(r"-?\d+", v)
            n = int(m.group()) if m else None
        if n is None:
            out[d] = None
        else:
            out[d] = max(1, min(5, n))
    out["comment"] = redact(side.get("comment")) if isinstance(side, dict) else None
    return out


# ---------------------------------------------------------------------------
# Orchestration
# ---------------------------------------------------------------------------

def _load_json_arg(path):
    if path == "-":
        return json.load(sys.stdin)
    with open(os.path.expanduser(path), "r", encoding="utf-8") as fh:
        return json.load(fh)


def _select_pred(args):
    if args.pred:
        return _load_json_arg(args.pred)
    if args.pred_from:
        doc = _load_json_arg(args.pred_from)
        incs = doc.get("incidents", doc) if isinstance(doc, dict) else doc
        for inc in incs:
            if args.incident_key and inc.get("incidentKey") == args.incident_key:
                return inc
        if args.incident_key:
            raise SystemExit(f"incident {args.incident_key!r} not in {args.pred_from}")
        # no key given → first real (agentic) incident
        return incs[0] if incs else None
    return None


def _select_tg(args):
    if args.tg:
        return _load_json_arg(args.tg)
    if args.tg_from:
        rows = _load_json_arg(args.tg_from) or []
        for r in rows:
            ref = (r.get("external_ref") or "")
            if args.incident_key and (args.incident_key in ref or ref in (args.incident_key or "")):
                return r
            if args.host and r.get("host") == args.host:
                return r
        return None
    return None


def build_verdict(pred_rec, tg_rec, args):
    card_pred = normalize_pred(pred_rec) if pred_rec else None
    card_tg = normalize_tg(tg_rec) if tg_rec else None
    present = [s for s, c in (("pred", card_pred), ("tg", card_tg)) if c is not None]
    single_sided = len(present) < 2

    # BLIND: randomize which real card is A vs B; the model never sees the mapping.
    rng = random.Random(args.seed) if args.seed is not None else random.SystemRandom()
    cards = [("pred", card_pred), ("tg", card_tg)]
    if rng.random() < 0.5:
        cards.reverse()
    (sys_a, card_a), (sys_b, card_b) = cards[0], cards[1]
    mapping = {"A": sys_a if card_a else None, "B": sys_b if card_b else None}

    incident_key = args.incident_key or (pred_rec or {}).get("incidentKey") or (tg_rec or {}).get("external_ref") or "(unknown)"
    subject = (card_pred or card_tg or {}).get("subject_host") if (card_pred or card_tg) else args.host

    prompt = build_prompt(card_a, card_b)

    verdict = {
        "incident_key": incident_key,
        "subject_host": subject,
        "judged_at": _dt.datetime.now(_dt.timezone.utc).isoformat(),
        "judge_model_requested": args.model,
        "judge_model_served": None,
        "present_systems": present,
        "single_sided": single_sided,
        "mapping": mapping,
        "dims": {"A": None, "B": None},
        "winner": None,
        "winner_letter": None,
        "reason": None,
        "judge_unavailable": False,
    }

    if args.dry_run:
        verdict["dry_run"] = True
        verdict["prompt"] = redact(prompt)
        return verdict, prompt

    # Retry with backoff (JUDGE_MAX_ATTEMPTS): a NETWORK error (LiteLLM reset/connect-fail on a long
    # kimi-max call — curl exit 56/7/28, empty body) recovers with breathing room for the proxy/upstream; a
    # PARSE error (a reasoner emitting an unescaped quote in a comment) recovers on a fresh draw. Both are
    # transient — without this, ~60% of a run drops to judge_unavailable and the head-to-head can't score
    # enough pairs. Network errors get a growing backoff; a parse miss retries promptly.
    obj = None
    last_err = None
    for attempt in range(JUDGE_MAX_ATTEMPTS):
        content, served, err = call_litellm(prompt, args)
        verdict["judge_model_served"] = served
        if err:
            last_err = err
            if attempt < JUDGE_MAX_ATTEMPTS - 1:
                time.sleep(min(2 + 3 * attempt, 15))  # network reset/connect-fail: let LiteLLM/upstream recover
            continue
        try:
            obj = parse_verdict(content)
            last_err = None
            break
        except (ValueError, json.JSONDecodeError) as e:
            last_err = f"unparseable judge verdict: {e}; raw={redact(content)[:300]}"
            if attempt < JUDGE_MAX_ATTEMPTS - 1:
                time.sleep(1)  # a malformed draw: a prompt re-draw almost always parses
            continue
    if obj is None:
        verdict["judge_unavailable"] = True
        verdict["error"] = last_err or "judge produced no verdict"
        return verdict, prompt

    dims_a = _clamp_side(obj.get("A", {})) if card_a else {d: None for d in DIMENSIONS}
    dims_b = _clamp_side(obj.get("B", {})) if card_b else {d: None for d in DIMENSIONS}
    verdict["dims"] = {"A": dims_a, "B": dims_b}

    winner_letter = obj.get("winner")
    if isinstance(winner_letter, str):
        winner_letter = winner_letter.strip().upper()
    if single_sided:
        # not a contest — the present side is the only one scored; report no head-to-head winner.
        verdict["winner_letter"] = None
        verdict["winner"] = None
    elif winner_letter in {"A", "B"}:
        verdict["winner_letter"] = winner_letter
        verdict["winner"] = mapping.get(winner_letter)
    elif winner_letter in {"TIE", "DRAW"}:
        verdict["winner_letter"] = "tie"
        verdict["winner"] = "tie"
    verdict["reason"] = redact(obj.get("reason"))
    return verdict, prompt


def main(argv=None):
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("--pred")
    ap.add_argument("--pred-from")
    ap.add_argument("--tg")
    ap.add_argument("--tg-from")
    ap.add_argument("--incident-key")
    ap.add_argument("--host")
    ap.add_argument("--model", default=DEFAULT_MODEL)
    ap.add_argument("--seed", type=int, default=None)
    ap.add_argument("--dry-run", action="store_true")
    ap.add_argument("--out")
    ap.add_argument("--ssh-key", default=DEFAULT_SSH_KEY)
    ap.add_argument("--tg-host", default=DEFAULT_TG_HOST)
    ap.add_argument("--ssh-user", default=DEFAULT_SSH_USER)
    ap.add_argument("--env-path", default=DEFAULT_ENV_PATH)
    ap.add_argument("--timeout", type=int, default=150)
    args = ap.parse_args(argv)

    pred_rec = _select_pred(args)
    tg_rec = _select_tg(args)
    if not pred_rec and not tg_rec:
        ap.error("nothing to judge: provide at least one of --pred/--pred-from/--tg/--tg-from")

    verdict, _prompt = build_verdict(pred_rec, tg_rec, args)

    out = json.dumps(verdict, indent=2, ensure_ascii=False)
    print(out)
    if args.out:
        with open(os.path.expanduser(args.out), "w", encoding="utf-8") as fh:
            fh.write(out + "\n")
    # exit 0 even when judge_unavailable: the record honestly says so, and run.sh keeps going.
    return 0


if __name__ == "__main__":
    sys.exit(main())
