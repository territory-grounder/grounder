package attribution

// The shared self-correlation pairing (TG-684 journal, TG-693 pve): a record with TG's identity is TG's own only when
// one TG-recorded execution, not yet claimed by another record, sits inside [record − before, record + after].

import (
	"testing"
	"time"
)

func tg693Records(self string, at ...time.Time) []Evidence {
	out := []Evidence{{Domain: "pve", Actor: "root@pam", ActionKind: "vzstop", ObservedAt: at[0].Add(-time.Minute)}}
	for _, a := range at {
		out = append(out, Evidence{Domain: "pve", Actor: self, ActionKind: "vzstart", ObservedAt: a})
	}
	return out
}

// KILLING MUTATIONS: drop the `used` claim in PairSelf (one execution explains every record) — RED on "the second
// record beside one execution"; widen unusedExecutionFor's bounds by a second — RED on the edge cases.
func TestPairSelfMatchesEachRecordToAtMostOneExecutionInsideTheTolerance(t *testing.T) {
	const self = "tg-actuate@pve!lane"
	before, after := 30*time.Second, 120*time.Second
	at := time.Date(2026, 11, 8, 6, 46, 54, 0, time.UTC)
	cases := []struct {
		name  string
		recs  []time.Time
		execs []time.Time
		want  []bool // per self record, oldest first: correlated?
	}{
		{"the live heal: executed 5.7 s after the task", []time.Time{at}, []time.Time{at.Add(5700 * time.Millisecond)}, []bool{true}},
		{"late edge", []time.Time{at}, []time.Time{at.Add(after)}, []bool{true}},
		{"just past the late edge", []time.Time{at}, []time.Time{at.Add(after + time.Second)}, []bool{false}},
		{"early edge", []time.Time{at}, []time.Time{at.Add(-before)}, []bool{true}},
		{"just past the early edge", []time.Time{at}, []time.Time{at.Add(-before - time.Second)}, []bool{false}},
		{"no execution at all", []time.Time{at}, nil, []bool{false}},
		{"the second record beside one execution", []time.Time{at, at.Add(10 * time.Second)}, []time.Time{at.Add(5 * time.Second)}, []bool{true, false}},
		{"two records, two executions", []time.Time{at, at.Add(10 * time.Second)}, []time.Time{at.Add(15 * time.Second), at.Add(5 * time.Second)}, []bool{true, true}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out := tg693Records(self, tc.recs...)
			mine := SelfRecords(out, self)
			if len(mine) != len(tc.recs) {
				t.Fatalf("SelfRecords found %d of TG's records, want %d", len(mine), len(tc.recs))
			}
			PairSelf(out, mine, tc.execs, before, after)
			if out[0].SelfCorrelated || out[0].SelfUnexplained {
				t.Fatalf("a stranger's record must never be marked, got %+v", out[0])
			}
			for k, i := range mine {
				if out[i].SelfCorrelated != tc.want[k] || out[i].SelfUnexplained == tc.want[k] {
					t.Fatalf("record %d: correlated=%v unexplained=%v, want correlated=%v (exactly one mark)", k,
						out[i].SelfCorrelated, out[i].SelfUnexplained, tc.want[k])
				}
			}
		})
	}
}

// An unknown identity names nothing, and the records come back oldest first whatever order the reader produced.
func TestSelfRecordsIsEmptyForAnUnknownIdentityAndOrdersByTime(t *testing.T) {
	at := time.Date(2026, 11, 8, 7, 0, 0, 0, time.UTC)
	out := []Evidence{
		{Actor: "tg", ObservedAt: at.Add(time.Minute)},
		{Actor: "root@pam", ObservedAt: at},
		{Actor: "tg", ObservedAt: at},
	}
	if got := SelfRecords(out, ""); got != nil {
		t.Fatalf("an empty identity must match no record, got %v", got)
	}
	if got := SelfRecords(out, "tg"); len(got) != 2 || got[0] != 2 || got[1] != 0 {
		t.Fatalf("TG's records must come back oldest first, got %v", got)
	}
}
