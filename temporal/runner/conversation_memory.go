package runner

// The cross-session conversation memory (TG-80 P2-8; clean-room from h-apache-stack's hot conversation
// tier, attribution: docs/SOURCE-BENCHMARK-CATALOG). A recurring incident LINEAGE — the canonical rule
// family on one host, the same stable subject novelty keys on (TG-124) — accumulates one terminal digest
// per session (written by the triage recorder, TTL-bounded, migration 0109); the next session on that
// lineage reads the recent digests and folds them into its seed as the <conversation_memory> UNTRUSTED
// block. The temporal half the <precedent> block does not carry: precedent answers "what worked on
// SIMILAR incidents anywhere", this answers "what did WE conclude the last times this exact thing
// happened here" — including the stops and the polls that never became precedent rows.

import (
	"fmt"
	"strings"
	"time"

	"github.com/territory-grounder/grounder/core/judge"
	"github.com/territory-grounder/grounder/core/knowledge"
)

// ConversationTurn is one prior session's terminal digest on a lineage — the runner's own shape, so this
// package needs no core/db import (the composition root adapts the pgx store's rows).
type ConversationTurn struct {
	ExternalRef string
	Content     string
	CreatedAt   time.Time
}

// conversationHotTurns bounds the hot tier folded into a seed — enough to show a pattern, never an
// archive (the archive is session_triage, reachable through <precedent> and the console).
const conversationHotTurns = 5

// conversationDigestRunes bounds ONE turn's digest at write time, so the read side's block budget
// (untrustedBlockBudgetRunes) trims layout, not substance.
const conversationDigestRunes = 400

// conversationKey derives the lineage key: the canonical rule FAMILY (the same authority the
// prior-verdict fold uses) plus the ingest-validated incident host. Degenerate halves yield "" — an
// unkeyed incident has no lineage and neither reads nor writes memory.
func conversationKey(alertRule, host string) string {
	fam := strings.TrimSpace(knowledge.CanonicalRule(alertRule))
	h := strings.TrimSpace(host)
	if fam == "" || h == "" {
		return ""
	}
	return fam + "|" + h
}

// conversationMemoryContext renders the hot tier as the seed block's inner text — one line per prior
// session, newest first, each carrying when, which session, the terminal, and the digest. Empty input
// renders "" (no block).
func conversationMemoryContext(turns []ConversationTurn, now time.Time) string {
	if len(turns) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("Prior sessions on this exact rule+host lineage (newest first — the temporal record; <precedent> holds the cross-estate one):\n")
	for _, t := range turns {
		age := now.Sub(t.CreatedAt).Round(time.Minute)
		fmt.Fprintf(&b, "- %s ago (%s): %s\n", age, t.ExternalRef, t.Content)
	}
	return strings.TrimRight(b.String(), "\n")
}

// conversationDigest renders THIS session's terminal as the one line the next session will read: the
// outcome plus the screened free-text the triage row already carries (REQ-2606 ran upstream — the
// conclusion on a stop, the committed prediction on a proposal). Bounded; never raw tool output.
func conversationDigest(row judge.TriageRow) string {
	claim := strings.TrimSpace(row.Conclusion)
	if claim == "" {
		claim = strings.TrimSpace(row.Prediction)
	}
	d := "outcome=" + row.Outcome
	if row.Op != "" {
		d += " op=" + row.Op
	}
	if row.StopReason != "" {
		d += " stop=" + row.StopReason
	}
	if claim != "" {
		d += " — " + claim
	}
	if r := []rune(d); len(r) > conversationDigestRunes {
		d = string(r[:conversationDigestRunes]) + "…"
	}
	return d
}
