package main

// TG-559 (owner ruling 2026-09-13, "there should be no voting --> TG must act autonomously"): the switch that
// makes TG the approver graph's FALLBACK APPROVER (CONSTITUTION §4.3, spec/012 REQ-1127). When armed, a
// POLL_PAUSE is resolved the instant it is classified — act on what only lacked a human's first look, hold
// everything else — instead of posting a poll and parking the session for VoteWait.
//
// DARK BY DEFAULT (CONSTITUTION rule 4: every autonomy layer ships dark and is independently disableable):
// unset keeps the human vote path byte-identical. FAIL DIRECTION: an unrecognised value is read as the HUMAN
// path — a typo must never be the thing that removes the human, and it is said out loud so the typo is found.
//
// Read once at boot; the per-session decision is recorded by the classify activity, so a flip takes effect for
// NEW sessions only and an in-flight poll keeps the behaviour it opened under.

import (
	"log"
	"strings"
)

// approvalResolverFromEnv reports whether the autonomous fallback approver is armed (TG_APPROVAL_RESOLVER =
// "human" (default) | "autonomous"), logging the posture either way.
func approvalResolverFromEnv(get func(k, def string) string) bool {
	raw := strings.TrimSpace(get("TG_APPROVAL_RESOLVER", ""))
	switch strings.ToLower(raw) {
	case "autonomous":
		log.Printf("approval resolver: AUTONOMOUS (TG-559, spec/012 REQ-1127) — TG is the fallback approver: a POLL_PAUSE " +
			"whose only reason was a missing human first look (canary pin / un-graduated class / novel incident) ACTS; " +
			"anything else (irreversible, destructive, stateful, security, a person's deliberate change, doubtful " +
			"grounding) is HELD. No poll is posted and no vote is awaited; each decision is ledger-recorded as tg:fallback-approver")
		return true
	case "", "human":
		log.Printf("approval resolver: human — a POLL_PAUSE posts a poll and waits up to VoteWait for a vote " +
			"(TG_APPROVAL_RESOLVER=autonomous makes TG decide instead)")
		return false
	default:
		log.Printf("approval resolver: UNRECOGNISED TG_APPROVAL_RESOLVER=%q — using the HUMAN vote path (fail toward the "+
			"human); valid values are \"human\" and \"autonomous\"", raw)
		return false
	}
}
