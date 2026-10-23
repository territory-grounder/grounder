package suppression

import (
	"strings"
	"testing"
	"time"

	"github.com/territory-grounder/grounder/core/ingest"
)

// TG-459 — when a re-fire matches a tracked parent (IssueRef set) but the parent's open/resolved state is
// UNCONFIRMABLE (no OpenIssue checker wired — the deployed dark-tracker case — or the checker cannot read the
// incident), TG-354 escalated EVERY such re-fire. That gave up recency-dedup entirely in the unconfirmable
// path and risks an alert storm on a rapid, genuine duplicate of a STILL-open incident in multi/dark-tracker
// configs. TG-459 restores a SHORT-RECENCY fallback: a re-fire while the prior anchor is still fresher than
// recencySubWindow is deduped on pure recency; only once the anchor is STALE (past the sub-window) does "did
// it resolve by now?" become a live question and the re-fire escalates as TG-354 intends.
//
// KILLING MUTATION: replace the short-recency fallback body with a bare `continue` (the pre-TG-459 code).
// Sub-case (1) then escalates instead of suppressing → RED.
func TestDedupShortRecencyFallbackWhenOpennessUnconfirmable(t *testing.T) {
	now := sunday3am
	// The alert's ExternalRef is TG's own incident ref; the prior's IssueRef is the estate ticket namespace —
	// DIFFERENT namespaces (illustrative id shapes, not live estate data), so no lookup can resolve openness.
	alert := Alert{ExternalRef: "librenms-dc1-184121", Host: "h", AlertRule: "R", Severity: ingest.SeverityWarning}
	parent := func(loggedAt time.Time) []TriageEntry {
		return []TriageEntry{{Host: "h", AlertRule: "R", LoggedAt: loggedAt, IssueRef: "IFRNLLEI01PRD-2247"}}
	}

	// (1) FRESH unconfirmable re-fire — prior logged INSIDE the recency sub-window, OpenIssue nil (the
	// deployed dark-tracker case). A rapid duplicate of a still-open incident: suppress on recency.
	// RED before the fix: the pre-TG-459 bare `continue` escalates this.
	fresh := &DedupStage{Recent: parent(now.Add(-(recencySubWindow / 2))), Window: time.Hour} // OpenIssue nil
	d, err := fresh.Evaluate(ctx(), alert, now)
	if err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	if d.Outcome != OutcomeSuppressed {
		t.Fatalf("a FRESH re-fire (prior within recencySubWindow) whose parent openness is unconfirmable must "+
			"be deduped on recency, got %v (%q) — TG-459 restores short-recency suppression to avoid a storm "+
			"on a rapid duplicate of a still-open incident", d.Outcome, d.Reason)
	}
	if !strings.Contains(d.Reason, "recency") {
		t.Fatalf("the suppression reason must name the short-recency window, got %q", d.Reason)
	}

	// (2) STALE unconfirmable re-fire — prior logged PAST the sub-window, OpenIssue nil. Now "did it resolve
	// by now?" is a live question: escalate (unchanged TG-354 behavior). The prior is still inside Window, so
	// it is a valid dedup candidate — only the sub-window freshness check gates suppression here.
	stale := &DedupStage{Recent: parent(now.Add(-(recencySubWindow + time.Minute))), Window: time.Hour} // OpenIssue nil
	if d2, _ := stale.Evaluate(ctx(), alert, now); d2.Outcome != OutcomeEscalate {
		t.Fatalf("a STALE re-fire (prior past recencySubWindow) whose parent openness is unconfirmable must "+
			"escalate, got %v (%q) — the short-recency fallback must not silence a stale re-fire (TG-354)",
			d2.Outcome, d2.Reason)
	}

	// (3) CONFIRMED-OPEN — even a fresh prior with OpenIssue==true takes the confirmed-open path, unchanged:
	// the sub-window fallback never runs when openness IS confirmable.
	confirmed := &DedupStage{Recent: parent(now.Add(-(recencySubWindow / 2))), Window: time.Hour, OpenIssue: func(string) bool { return true }}
	if d3, _ := confirmed.Evaluate(ctx(), alert, now); d3.Outcome != OutcomeSuppressed || !strings.Contains(d3.Reason, "confirmed-open") {
		t.Fatalf("a re-fire against a CONFIRMED-open parent must dedup with the confirmed-open reason, got %v (%q)",
			d3.Outcome, d3.Reason)
	}

	// (4) IssueRef=="" pure recency — no parent incident tracked at all; a fresh in-window prior still dedups
	// on window recency, unchanged. Distinct from the unconfirmable-parent path: there is no incident identity
	// whose openness to confirm, so the sub-window branch is not reached.
	noParent := &DedupStage{Recent: []TriageEntry{{Host: "h", AlertRule: "R", LoggedAt: now.Add(-(recencySubWindow / 2))}}, Window: time.Hour}
	if d4, _ := noParent.Evaluate(ctx(), alert, now); d4.Outcome != OutcomeSuppressed || !strings.Contains(d4.Reason, "no parent incident tracked") {
		t.Fatalf("a re-fire with NO parent incident tracked must dedup on window recency, got %v (%q)",
			d4.Outcome, d4.Reason)
	}

	// (5) CHECKER PRESENT and answers NOT-open, FRESH prior — the recency fallback must NOT rescue this. A
	// checker IS wired and did not confirm open (the incident RESOLVED, or the checker cannot resolve TG's ref —
	// both surface as !OpenIssue); a re-fire is then a genuine new incident and escalates (TG-354), recency
	// irrelevant. This is the case that separates "no tracker exists" (case 1, recency applies) from "a tracker
	// answered non-open" (here, escalate). SECOND KILLING MUTATION: gating the fallback on `OpenIssue==nil ||
	// !OpenIssue(ref)` (firing it on a confirmed-resolved anchor) makes THIS suppress → RED; the parity is
	// pinned live by temporal/runner TestLiveSuppressGateDedupOpenIncident.
	checkerNotOpen := &DedupStage{Recent: parent(now.Add(-(recencySubWindow / 2))), Window: time.Hour, OpenIssue: func(string) bool { return false }}
	if d5, _ := checkerNotOpen.Evaluate(ctx(), alert, now); d5.Outcome != OutcomeEscalate {
		t.Fatalf("a FRESH re-fire whose checker answers NOT-open must ESCALATE (a checker spoke and did not "+
			"confirm open — TG-354); the short-recency fallback fires ONLY when OpenIssue is nil, got %v (%q)",
			d5.Outcome, d5.Reason)
	}
}
