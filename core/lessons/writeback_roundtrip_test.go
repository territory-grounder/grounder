package lessons

import (
	"testing"

	"github.com/territory-grounder/grounder/core/knowledge"
	"github.com/territory-grounder/grounder/core/safety"
)

// TestNoveltyWritebackRoundTrip is THE regression-trap oracle (TG-124): the WRITER key must equal the READER
// key. It drives the REAL persistence path (lessons.Merge → knowledge corpus) with a confirmed-clean
// resolution keyed on the EXACT (host, alert_rule) the classifier's novelty gate passes to knowledge.Count,
// and asserts Count flips from 0 (novel) to >0 (has precedent). If ingest/classify normalized the signature
// differently than the corpus stored it, the corpus would look populated but Count would stay 0 and novelty
// would never drop — a silent no-op. This proves it does not.
func TestNoveltyWritebackRoundTrip(t *testing.T) {
	// The EXACT signature the classifier reads: host = the ACTION TARGET (inv.Proposal.Action.Target), rule =
	// env.AlertRule. This is the shape ClassifyInput passes to PriorIncidents → knowledge.Count.
	const host, rule = "librespeed01", "NginxDown"

	// Before the writeback: genuinely novel — no precedent, so the gate would force a poll.
	if n := knowledge.NewLexicalRetriever(nil).Count(host, rule); n != 0 {
		t.Fatalf("an empty corpus must be novel for the signature, Count=%d want 0", n)
	}

	// The confirmed-clean resolution the reconcile writeback emits, keyed on the SAME signature.
	ri := ResolvedIncident{
		ExternalRef: "TG-124-1", Host: host, AlertRule: rule, Site: "nl",
		Summary: "nginx worker crashed", Action: "restart nginx",
		Verdict: safety.VerdictMatch, ConfirmedClear: true,
	}
	merged, added := Merge(nil, []ResolvedIncident{ri})
	if added != 1 {
		t.Fatalf("a confirmed-clean resolution must contribute one net-new lesson, added=%d", added)
	}
	r := knowledge.NewLexicalRetriever(merged)

	// THE assertion: the classifier's Count — using the EXACT host+rule it passes — now sees the precedent.
	if n := r.Count(host, rule); n == 0 {
		t.Fatal("writer key must equal reader key — Count must be >0 for the exact classifier signature after a writeback")
	}
	// eqFold tolerance: case/whitespace drift between the write and the read still matches, so a normalization
	// difference between ingest and the corpus can never silently zero novelty.
	if n := r.Count(" LIBRESPEED01 ", "nginxdown"); n == 0 {
		t.Fatal("Count must be case/space-insensitive (eqFold) so ingest/classify normalization drift never silently zeroes novelty")
	}
	// A concrete-host precedent de-novels ONLY its own host — a different host stays novel.
	if n := r.Count("otherhost", rule); n != 0 {
		t.Fatalf("a concrete-host precedent must not de-novel a different host, Count=%d want 0", n)
	}

	// A deviation resolution must NEVER be written back as precedent (it would falsely de-novel an incident
	// where reality diverged from the model).
	dev := ri
	dev.ExternalRef, dev.Verdict = "TG-124-2", safety.VerdictDeviation
	m2, add2 := Merge(nil, []ResolvedIncident{dev})
	if add2 != 0 {
		t.Fatalf("a deviation must contribute no lesson, added=%d", add2)
	}
	if n := knowledge.NewLexicalRetriever(m2).Count(host, rule); n != 0 {
		t.Fatalf("a deviation must never de-novel the signature, Count=%d want 0", n)
	}
}
