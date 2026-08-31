package main

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/territory-grounder/grounder/core/audit"
	"github.com/territory-grounder/grounder/core/wiring"
)

// fakeGovAppender captures the boot rows so the APPEND GUARD itself is testable — the same seam
// appendConfigGapReport uses, and for the same reason: without it the "only append when something is
// actually dark" rule lives inline in main() and nothing exercises it.
type fakeGovAppender struct{ appended []audit.GovDecision }

func (f *fakeGovAppender) Append(d audit.GovDecision) (audit.LedgerEntry, error) {
	f.appended = append(f.appended, d)
	return audit.LedgerEntry{Seq: int64(len(f.appended))}, nil
}

// TestDarkSeamReachesLedgerNotStdout is the oracle for the ORIGINAL incident shape: escalations that
// reach a log file and nothing else. A governance page whose only trace is stdout is a page that did not
// happen, as far as any durable record is concerned.
//
// KILLING MUTATION: replace the ledger append with a log.Printf. The fake then sees zero appends.
func TestDarkSeamReachesLedgerNotStdout(t *testing.T) {
	// A manifest where nothing was bound — exactly the shipped state before this change.
	m := wiring.New()
	findings, _ := m.Report(time.Now().UTC())
	if len(findings) == 0 {
		t.Fatal("an unbound manifest must produce findings, or this oracle proves nothing")
	}

	f := &fakeGovAppender{}
	if err := appendWiringDarkReport(f, findings); err != nil {
		t.Fatalf("append: %v", err)
	}
	if len(f.appended) != 1 {
		t.Fatalf("exactly ONE ledger row for the boot report, got %d", len(f.appended))
	}
	row := f.appended[0]
	if row.Decision != "wiring:dark-seam-at-boot" {
		t.Fatalf("decision: %q", row.Decision)
	}
	if !row.Withheld {
		t.Fatal("a dark seam is autonomy WITHHELD — recording it otherwise files an outage as bookkeeping")
	}
	// The row must name the seam AND what its darkness costs; "gov.notify: dark" is not actionable.
	if !strings.Contains(row.Reason, string(wiring.SeamGovNotify)) {
		t.Fatalf("the row must name the seam: %q", row.Reason)
	}
	if !strings.Contains(row.Reason, "reach NO operator") {
		t.Fatalf("the row must carry the CONSEQUENCE verbatim: %q", row.Reason)
	}
}

// TestCleanWiringAppendsNothing is the other half of the guard: a fully-wired boot must not write a
// no-op row every restart, or the ledger becomes noise and stops being read.
func TestCleanWiringAppendsNothing(t *testing.T) {
	// Bind EVERY seam: "clean" means the whole closed set is live. This test bound only gov.notify and
	// began failing the moment escalation.page joined the set — the closed-set property doing its job.
	// The friction is deliberate: adding a seam SHOULD force every "nothing is dark" claim to be re-made.
	m := wiring.New()
	wiring.Bind(m, wiring.SeamGovNotify, func(context.Context, struct{}) error { return nil })
	wiring.Bind(m, wiring.SeamEscalationPage, boundPager{notify: func(context.Context, struct{}) error { return nil }})
	wiring.Bind(m, wiring.SeamLessonsFeed, func() {})
	wiring.Bind(m, wiring.SeamWikiCompile, func() {})
	wiring.Bind(m, wiring.SeamWorldDiscovery, func() {})
	wiring.Bind(m, wiring.SeamSuppression, func() {})
	wiring.Bind(m, wiring.SeamTrackerEntry, func() {})
	wiring.Bind(m, wiring.SeamTrackerImport, func() {})
	wiring.Bind(m, wiring.SeamDiscoveryService, func() {})
	wiring.Bind(m, wiring.SeamVoteInbound, func() {})
	wiring.Bind(m, wiring.SeamHostDiag, []struct{}{})
	wiring.Bind(m, wiring.SeamSyslogRead, []struct{}{})
	findings, _ := m.Report(time.Now().UTC())

	f := &fakeGovAppender{}
	if err := appendWiringDarkReport(f, findings); err != nil {
		t.Fatalf("append: %v", err)
	}
	if len(f.appended) != 0 {
		t.Fatalf("a clean boot must append nothing, got %d row(s)", len(f.appended))
	}
	// And a nil ledger must be inert, not a panic.
	if err := appendWiringDarkReport(nil, findings); err != nil {
		t.Fatalf("nil ledger must be inert: %v", err)
	}
}

// boundPager mirrors notifierPager's shape (a struct whose inner sink carries the required tag) so the
// clean-tree assertion exercises the same walk production does.
type boundPager struct {
	notify func(context.Context, struct{}) error `wiring:"required"`
}
