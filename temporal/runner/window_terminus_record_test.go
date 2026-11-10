package runner

// TG-662 final review, finding 1: a commit-confirmed class is WITHHELD at the session terminus (window still armed)
// and credited or demoted by the window's resolution, so the window appends its OWN terminus_record row
// (source=window) carrying the decision it actually fed. Without it the append-only record the credit audit (TG-666)
// and the G4A pre-registration read shows "withheld" for every run the ladder credited.
//
// KILLING MUTATION (executed 2026-11-07, whole-package run, restored from a copy): delete the
// `a.appendWindowTerminusRecord(...)` call at the end of ResolveCommitConfirmActivity. RED: every subtest of
// TestResolveCommitConfirmAppendsTheWindowsOwnRecord ("want exactly one window row").

import (
	"context"
	"errors"
	"testing"

	"github.com/territory-grounder/grounder/core/audit"
	"github.com/territory-grounder/grounder/core/db"
)

func TestResolveCommitConfirmAppendsTheWindowsOwnRecord(t *testing.T) {
	type want struct {
		disposition    string
		fed, clean     bool
		causality      string
		collateral     string
		confirmedClear bool
	}
	cases := []struct {
		name    string
		state   string
		facts   func(context.Context, string) (TerminusFacts, bool, error)
		feedErr error
		unwired bool // RecordGraduation not wired at all
		want    want
	}{
		{"confirmed and tg-caused credits", db.CommitConfirmConfirmed, settledQuietFacts, nil, false,
			want{"clean:window-confirmed", true, true, CausalityTGCaused, "quiet", true}},
		{"confirmed but indeterminate is withheld", db.CommitConfirmConfirmed,
			factsReader(TerminusFacts{Mutated: true, ConfirmedClear: true, CollateralKnown: true, Causality: CausalityIndeterminate}, true), nil, false,
			want{"withheld:window-confirmed-causality-indeterminate", false, false, CausalityIndeterminate, "quiet", true}},
		{"confirmed with unreadable facts is withheld", db.CommitConfirmConfirmed,
			func(context.Context, string) (TerminusFacts, bool, error) {
				return TerminusFacts{}, false, errors.New("db down")
			}, nil, false,
			want{"withheld:window-confirmed-facts-unreadable", false, false, CausalityIndeterminate, "unknown", false}},
		{"a fired revert demotes", db.CommitConfirmReverted, settledQuietFacts, nil, false,
			want{"not-clean:window-reverted", true, false, CausalityTGCaused, "quiet", true}},
		{"elapsed unconfirmed feeds nothing", db.CommitConfirmElapsedUnconfirmed, settledQuietFacts, nil, false,
			want{"withheld:window-elapsed_unconfirmed", false, false, CausalityTGCaused, "quiet", true}},
		// The disposition says what REACHED the ladder (review of the final TG-662 commit): a feed that did not land
		// downgrades it, so no "clean:"/"not-clean:" row ever carries credit_fed=false.
		{"a failed ladder feed is recorded as not fed", db.CommitConfirmConfirmed, settledQuietFacts, errors.New("ladder down"), false,
			want{"withheld:window-feed-failed", false, false, CausalityTGCaused, "quiet", true}},
		{"a failed demotion feed is recorded as not fed", db.CommitConfirmReverted, settledQuietFacts, errors.New("ladder down"), false,
			want{"withheld:window-feed-failed", false, false, CausalityTGCaused, "quiet", true}},
		{"an unwired ladder feed is recorded as not fed", db.CommitConfirmConfirmed, settledQuietFacts, nil, true,
			want{"withheld:window-feed-unwired", false, false, CausalityTGCaused, "quiet", true}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fake := newCCT0292Fake()
			if err := fake.ArmCommitConfirm(context.Background(), db.CommitConfirmRow{
				ActionID: "act-w-1", ExternalRef: "TG-w-1", OpClass: "restart-service", TargetHost: "web01", Site: "nl", WindowSeconds: 600}); err != nil {
				t.Fatal(err)
			}
			var recs []TerminusRecord
			type feedCall struct {
				opClass, ref string
				clean        bool
			}
			var feeds []feedCall
			d := Deps{CommitConfirm: fake, Ledger: audit.NewLedger(), TriageFacts: tc.facts,
				RecordGraduation: func(_ context.Context, opClass, ref string, clean bool) error {
					feeds = append(feeds, feedCall{opClass, ref, clean})
					return tc.feedErr
				},
				TerminusRecord: func(_ context.Context, r TerminusRecord) error {
					recs = append(recs, r)
					return nil
				}}
			if tc.unwired {
				d.RecordGraduation = nil
			}
			if err := NewActivities(d).ResolveCommitConfirmActivity(context.Background(), ResolveCommitConfirmInput{
				ActionID: "act-w-1", ExternalRef: "TG-w-1", State: tc.state, Detail: "drill"}); err != nil {
				t.Fatal(err)
			}
			if len(recs) != 1 {
				t.Fatalf("want exactly one window row, got %d: %+v", len(recs), recs)
			}
			r := recs[0]
			if r.Source != "window" || r.WindowState != tc.state || r.OpClass != "restart-service" || r.ActionID != "act-w-1" || r.TargetHost != "web01" {
				t.Fatalf("row identity mismatch: %+v", r)
			}
			got := want{r.CreditDisposition, r.CreditFed, r.CreditClean, r.Causality, r.Collateral, r.ConfirmedClear}
			if got != tc.want {
				t.Fatalf("got %+v, want %+v", got, tc.want)
			}
			if r.CreditClean && r.Causality != CausalityTGCaused {
				t.Fatal("a clean window row must be tg-caused (the table CHECK would refuse it)")
			}
			// The row is the ladder's mirror: a fed row carries exactly what the feed was called with.
			if r.CreditFed {
				if len(feeds) != 1 || feeds[0].opClass != r.OpClass || feeds[0].ref != r.ExternalRef || feeds[0].clean != r.CreditClean {
					t.Fatalf("a fed row must mirror the single ladder call: row %+v, calls %+v", r, feeds)
				}
			}
		})
	}
}
