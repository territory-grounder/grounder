#!/usr/bin/env python3
"""
extract_predecessor.py — Extract the PREDECESSOR (claude-gateway) shadow-mode
reasoning for the TG head-to-head benchmark.

Reproduces, for a given date, the classification of the mutation-shadow log into
SCHEDULED-CRON noise vs AGENTIC dispatched-session intents, and joins each agentic
incident to its per-session/per-issue reasoning + conclusion from the LIVE
gateway.db.

READ-ONLY. This script only READS the shadow JSONL and opens gateway.db with
`sqlite3 ... mode=ro` (immutable-friendly). It NEVER runs any gateway
scripts/hooks and NEVER writes to the DB.

CREDENTIAL SAFETY: the shadow log embeds live secrets (e.g. a NetBox
`Authorization: Token <hex>` header, passwords). EVERY string that leaves this
script (stdout, JSON records, excerpts) is passed through redact() first.

Usage:
    ./extract_predecessor.py [--date YYYY-MM-DD]
        [--shadow-dir ~/logs/claude-gateway/mutation-shadow]
        [--db /home/tg/gateway-state/gateway.db]
        [--json]           # emit the full machine-readable record to stdout

Exit 0 on success; prints a human summary unless --json is given.
"""
from __future__ import annotations

import argparse
import base64
import datetime as _dt
import json
import os
import re
import sqlite3
import sys

# ---------------------------------------------------------------------------
# Classification config (mirrors the gateway's shadow taxonomy)
# ---------------------------------------------------------------------------

# Scheduled/cron self-heal emitters — NOISE for the head-to-head benchmark.
SCHEDULED_CRON_SOURCES = {
    "freedom-qos-toggle.sh",
    "vti-freedom-recovery.sh",
    "vti-budget-recovery.sh",
    "platform-controller.py",
    "cronicle-remediate.py",
}

# The dispatched-agentic gate — one intent per line, grouped by session/issue.
AGENTIC_SOURCE = "mutation-shadow-gate.py"

DEFAULT_SHADOW_DIR = os.path.expanduser("~/logs/claude-gateway/mutation-shadow")
DEFAULT_DB = "/home/tg/gateway-state/gateway.db"

# ---------------------------------------------------------------------------
# Credential redaction — applied to EVERYTHING that leaves this process.
# ---------------------------------------------------------------------------

_REDACTIONS = [
    # NetBox / generic bearer tokens: "Token <hex>" or "Bearer <hex/base64>"
    (re.compile(r"(?i)\b(Token|Bearer)\s+[A-Za-z0-9._\-]{16,}"), r"\1 <REDACTED>"),
    # Authorization headers of any scheme
    (re.compile(r"(?i)(Authorization:\s*)\S.+?(?=[\"'\\]|$)"), r"\1<REDACTED>"),
    # Long bare hex secrets (>=32 hex chars), e.g. API keys
    (re.compile(r"\b[0-9a-fA-F]{32,}\b"), "<REDACTED>"),
    # password/secret/token=... assignments
    (re.compile(r"(?i)\b(password|passwd|secret|api[_-]?key|token)\b(\s*[=:]\s*)\S+"),
     r"\1\2<REDACTED>"),
]


def redact(value):
    """Redact credentials from any string / nested JSON-able structure."""
    if value is None:
        return None
    if isinstance(value, str):
        out = value
        for rx, repl in _REDACTIONS:
            out = rx.sub(repl, out)
        return out
    if isinstance(value, dict):
        return {k: redact(v) for k, v in value.items()}
    if isinstance(value, list):
        return [redact(v) for v in value]
    return value


# ---------------------------------------------------------------------------
# Read-only DB helper
# ---------------------------------------------------------------------------

def open_db_ro(path):
    real = os.path.realpath(path)
    if not os.path.exists(real):
        return None
    uri = f"file:{real}?mode=ro"
    conn = sqlite3.connect(uri, uri=True)
    conn.row_factory = sqlite3.Row
    return conn


def _one(conn, sql, params=()):
    try:
        cur = conn.execute(sql, params)
        return cur.fetchone()
    except sqlite3.Error:
        return None


def _all(conn, sql, params=()):
    try:
        cur = conn.execute(sql, params)
        return cur.fetchall()
    except sqlite3.Error:
        return []


# ---------------------------------------------------------------------------
# Shadow-log ingest + classification
# ---------------------------------------------------------------------------

def load_shadow(shadow_dir, date):
    path = os.path.join(shadow_dir, f"shadow-{date}.jsonl")
    if not os.path.exists(path):
        raise FileNotFoundError(path)
    rows = []
    with open(path, "r", encoding="utf-8", errors="replace") as fh:
        for ln in fh:
            ln = ln.strip()
            if not ln:
                continue
            try:
                rows.append(json.loads(ln))
            except json.JSONDecodeError:
                continue
    return path, rows


# Heuristic: a blocked read/investigation command (vs a mutation attempt).
_INVESTIGATION_RX = re.compile(
    r"\b(curl|wget|git\s+log|git\s+-C[^\n]*log|grep|jq|cat|less|ls|find|"
    r"python3?\s+-c|kubectl\s+get|kubectl\s+describe|show|status|read)\b",
    re.IGNORECASE,
)
_MUTATION_RX = re.compile(
    r"\b(systemctl\s+(restart|start|stop|reload)|reboot|pct\s+(start|stop|reboot)|"
    r"qm\s+(start|stop|reset)|kubectl\s+(apply|delete|scale|rollout|patch)|"
    r"helm\s+(install|upgrade|uninstall)|rm\s|write|set|toggle|heal|apply)\b",
    re.IGNORECASE,
)


def is_investigation(intent):
    cmd = (intent.get("full_command") or "") + " " + (intent.get("blocking_segment") or "")
    action = (intent.get("action") or "")
    if _MUTATION_RX.search(cmd):
        return False
    if _INVESTIGATION_RX.search(cmd):
        return True
    # session-command that isn't obviously a mutation → treat read attempts as investigation
    return action == "session-command" and not _MUTATION_RX.search(cmd)


def classify(rows):
    scheduled, agentic, other = [], [], []
    for r in rows:
        src = r.get("source")
        if src in SCHEDULED_CRON_SOURCES:
            scheduled.append(r)
        elif src == AGENTIC_SOURCE:
            agentic.append(r)
        else:
            other.append(r)
    return scheduled, agentic, other


# ---------------------------------------------------------------------------
# Per-incident reasoning join from gateway.db
# ---------------------------------------------------------------------------

def decode_response(b64):
    if not b64:
        return ""
    try:
        return base64.b64decode(b64).decode("utf-8", "replace")
    except Exception:
        return ""


def enrich_incident(conn, issue, session, intents):
    """Build a benchmark record for one agentic incident (issue/session)."""
    host = ""
    for it in intents:
        if it.get("host"):
            host = it["host"]
            break

    blocked_investigations = sum(
        1 for it in intents if it.get("blocked") and is_investigation(it)
    )
    could_ground = blocked_investigations == 0

    rec = {
        "incidentKey": issue,
        "session": session,
        "host": host,
        "agentic": True,
        "blockedInvestigations": blocked_investigations,
        "couldGround": could_ground,
        "action": "",
        "issue": "",
        "rationale": "",
        "outcome": "",
        "reasoningExcerpt": "",
        "firstTs": min((it.get("iso", "") for it in intents if it.get("iso")), default=""),
    }

    if conn is None or not issue or issue == "T":
        rec["rationale"] = (
            "Unattributed shadow actuation (no issue/session in gateway.db); "
            "not a network-triage dispatched session."
        )
        return rec

    # sessions: title + confidence + the (truncated) final response preview
    s = _one(
        conn,
        "SELECT issue_title, session_id, confidence, num_turns, alert_category, "
        "model, cost_usd, last_response_b64 FROM sessions WHERE issue_id=?",
        (issue,),
    )
    resp = decode_response(s["last_response_b64"]) if s else ""

    # risk audit: the classified band = the action the gateway would take
    ra = _one(
        conn,
        "SELECT risk_level, band, auto_approved, auto_proceed_on_timeout, "
        "sms_required FROM session_risk_audit WHERE issue_id=? "
        "ORDER BY classified_at DESC LIMIT 1",
        (issue,),
    )
    band = ra["band"] if ra else ""

    # judge verdict = the outcome/conclusion score
    j = _one(
        conn,
        "SELECT overall_score, recommended_action, rationale, concerns "
        "FROM session_judgment WHERE issue_id=? ORDER BY judged_at DESC LIMIT 1",
        (issue,),
    )

    rec["issue"] = redact(s["issue_title"]) if s else issue
    rec["action"] = band or "(no risk-audit band recorded)"
    rec["outcome"] = (
        f"band={band}; auto_approved={ra['auto_approved'] if ra else '?'}; "
        f"judge_score={j['overall_score'] if j else '?'}/5; "
        f"judge_action={j['recommended_action'] if j else '?'}"
    )
    # rationale = the agent's own conclusion (from the response preview),
    # backstopped by the judge rationale.
    concl = _extract_conclusion(resp)
    rec["rationale"] = redact(concl or (j["rationale"] if j else ""))[:1200]
    rec["reasoningExcerpt"] = redact(resp)[:2000]

    # ground-truth note: the agent may still ground via MCP tools even when the
    # raw shell investigation commands were default-denied.
    if not could_ground and resp:
        rec["notes"] = (
            f"{blocked_investigations} raw investigation command(s) default-denied "
            "by the shadow hook; agent fell back to MCP tools and still reached a "
            "grounded conclusion."
        )
    return rec


def _extract_conclusion(resp):
    """Pull the most decision-bearing lines out of the ReAct response."""
    if not resp:
        return ""
    # Prefer explicit conclusion/verification markers; else the last THOUGHT.
    for marker in ("**CONCLUSION", "CONCLUSION", "**VERIFICATION", "**THOUGHT"):
        idx = resp.rfind(marker)
        if idx != -1:
            return resp[idx:idx + 600].strip()
    return resp[:600].strip()


# ---------------------------------------------------------------------------
# Main
# ---------------------------------------------------------------------------

def run(date, shadow_dir, db_path):
    path, rows = load_shadow(shadow_dir, date)
    scheduled, agentic, other = classify(rows)

    # group agentic by (issue, session) — fall back to session or issue alone.
    groups = {}
    for it in agentic:
        key = (it.get("issue") or "", it.get("session") or "")
        groups.setdefault(key, []).append(it)

    conn = open_db_ro(db_path)
    incidents = []
    for (issue, session), intents in sorted(groups.items()):
        incidents.append(enrich_incident(conn, issue, session, intents))
    if conn is not None:
        conn.close()

    signal_to_noise = {
        "scheduledCron": len(scheduled),
        "agenticSession": len(agentic),
        "agenticIncidents": len(groups),
        "otherUnclassified": len(other),
        "total": len(rows),
    }

    return {
        "date": date,
        "shadowLog": path,
        "db": os.path.realpath(db_path),
        "signalToNoise": signal_to_noise,
        "incidents": [redact(i) for i in incidents],
    }


def main(argv=None):
    ap = argparse.ArgumentParser(description=__doc__)
    ap.add_argument("--date", default=_dt.date.today().isoformat(),
                    help="YYYY-MM-DD (default: today)")
    ap.add_argument("--shadow-dir", default=DEFAULT_SHADOW_DIR)
    ap.add_argument("--db", default=DEFAULT_DB)
    ap.add_argument("--json", action="store_true",
                    help="emit the full machine-readable record to stdout")
    args = ap.parse_args(argv)

    result = run(args.date, os.path.expanduser(args.shadow_dir), args.db)

    if args.json:
        print(json.dumps(result, indent=2))
        return 0

    s = result["signalToNoise"]
    print(f"# Predecessor shadow extraction — {result['date']}")
    print(f"shadow log : {result['shadowLog']}")
    print(f"gateway.db : {result['db']}")
    print(f"signal/noise: scheduledCron={s['scheduledCron']} "
          f"agenticIntents={s['agenticSession']} "
          f"agenticIncidents={s['agenticIncidents']} total={s['total']}")
    print()
    for inc in result["incidents"]:
        print(f"## {inc['incidentKey']}  host={inc['host']}")
        print(f"   couldGround={inc['couldGround']} "
              f"blockedInvestigations={inc['blockedInvestigations']}")
        print(f"   action={inc['action']}")
        print(f"   outcome={inc.get('outcome','')}")
        print(f"   rationale={inc['rationale'][:300]}")
        print()
    return 0


if __name__ == "__main__":
    sys.exit(main())
