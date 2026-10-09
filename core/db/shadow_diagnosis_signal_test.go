package db

import "testing"

// TestShadowDiagnosisSignal pins the one property the proposals lane exists to add (TG-307): the SIGNAL an
// operator reads at proposal-review time is the SAME "grounded contradiction" the judge scored, derived
// through the same core/proposal.Diagnosis decode — never a second, laxer copy. It runs without a database,
// exactly like diagnosisRead's parity oracle, because this projection is where the load-bearing fact can be
// lost in silence: re-derive `cited` from "the id is non-empty" and every downstream surface still renders
// while quietly promoting a fabricated citation to evidence.
//
// Each case carries a mutation that reddens it: the grounded case fails if HasContradiction is dropped or
// cited is re-derived from a non-empty id; the uncited case fails if an ungrounded contradiction is counted
// as a real one; the null/empty/unbound cases fail if a missing claim is served as a false all-clear; the
// malformed case fails if a bad row errors instead of reading as "no gradeable claim" (which would blank the
// whole list for one damaged row).
func TestShadowDiagnosisSignal(t *testing.T) {
	cases := []struct {
		name             string
		raw              string
		wantRecorded     bool
		wantContradicted bool
		wantUncited      int
	}{
		{
			name:             "grounded contradiction is the A2 signal",
			raw:              `{"root_cause":"guest down after unclean shutdown","contradicting":[{"id":"pve-task-history-101","claim":"operator ran vzstop deliberately","cited":true}]}`,
			wantRecorded:     true,
			wantContradicted: true,
			wantUncited:      0,
		},
		{
			name: "an UNCITED contradiction is not a grounded one — a signal conjured from an uncaptured id is not a signal",
			raw:  `{"root_cause":"x","contradicting":[{"id":"ghost-obs","claim":"argues against","cited":false}]}`,
			// contradicted stays FALSE (the ref matched no gathered observation); but it IS an uncited assertion.
			wantRecorded:     true,
			wantContradicted: false,
			wantUncited:      1,
		},
		{
			name:             "supporting + ruled_out uncited assertions are counted, cited ones are not",
			raw:              `{"root_cause":"x","supporting":[{"id":"a","claim":"for","cited":true},{"id":"","claim":"unbacked","cited":false}],"ruled_out":[{"cause":"oom","reason":"41% used","id":"","cited":false}]}`,
			wantRecorded:     true,
			wantContradicted: false,
			wantUncited:      2,
		},
		{
			name:             "a claim with only a root cause is recorded, grounded, uncontradicted",
			raw:              `{"root_cause":"disk full"}`,
			wantRecorded:     true,
			wantContradicted: false,
			wantUncited:      0,
		},
		{
			name:             "the honest-uncertainty shape (ruled_out only, no root cause) is still a recorded claim",
			raw:              `{"ruled_out":[{"cause":"oom","reason":"memory fine","id":"host-metrics","cited":true}]}`,
			wantRecorded:     true,
			wantContradicted: false,
			wantUncited:      0,
		},
		{
			name:         "NULL column — a pre-migration-0056 session recorded no claim, never a false all-clear",
			raw:          "",
			wantRecorded: false,
		},
		{
			name:         "an explicit empty object — the agent bound nothing — is not Present()",
			raw:          `{}`,
			wantRecorded: false,
		},
		{
			name:         "a malformed blob reads as no-claim (batch-safe), never an error that blanks the list",
			raw:          `{"root_cause":`,
			wantRecorded: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var raw []byte
			if tc.raw != "" {
				raw = []byte(tc.raw)
			}
			recorded, contradicted, uncited := shadowDiagnosisSignal(raw)
			if recorded != tc.wantRecorded {
				t.Errorf("recorded = %v, want %v", recorded, tc.wantRecorded)
			}
			if contradicted != tc.wantContradicted {
				t.Errorf("contradicted = %v, want %v — the operator's A2 signal disagrees with the judge's "+
					"grounded-contradiction definition", contradicted, tc.wantContradicted)
			}
			if uncited != tc.wantUncited {
				t.Errorf("uncited = %d, want %d", uncited, tc.wantUncited)
			}
		})
	}
}
