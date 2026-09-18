package forensic

import (
	"reflect"
	"testing"
	"time"
)

// TG-168 — the first cross-incident view in the tree.
//
// Per-incident reconstruction is complete (core/trace.Assemble joins eleven corpora for ONE external_ref).
// Nothing takes a WINDOW and returns an ordered narrative across incidents. These oracles pin the one
// property that makes such a narrative citable: DETERMINISM. A reconstruction that renders differently on
// two runs over the same window cannot be compared with yesterday's, and an operator would be reading
// artefacts of map iteration as if they were changes in the estate.

func at(s string) time.Time {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		panic(err)
	}
	return t
}

// THE LOAD-BEARING ORACLE. Same input, shuffled between runs, must produce byte-identical output.
func TestMergeIsTotallyOrderedRegardlessOfInputOrder(t *testing.T) {
	same := at("2026-08-07T10:00:00Z")
	// Four events sharing ONE instant, across four corpora — the case sort stability alone cannot decide.
	ledger := []Event{{At: same, Source: SourceLedger, Kind: "classify:AUTO", SubjectRef: "r1"}}
	ingest := []Event{{At: same, Source: SourceIngest, Kind: "accepted", SubjectRef: "r1"}}
	step := []Event{{At: same, Source: SourceAgentStep, Kind: "agent-cycle", SubjectRef: "r1"}}
	cred := []Event{{At: same, Source: SourceCredential, Kind: "resolve", SubjectRef: "r1"}}

	a, _ := Merge(ledger, ingest, step, cred)
	b, _ := Merge(cred, step, ingest, ledger) // same events, different group order
	if !reflect.DeepEqual(a, b) {
		t.Fatalf("Merge is order-dependent — the same window renders differently on two runs, so no "+
			"reconstruction can be compared with an earlier one.\n a=%v\n b=%v", kinds(a), kinds(b))
	}

	// And the order is CAUSAL, not alphabetical: alert -> classify -> investigate -> credential -> ledger.
	want := []string{"accepted", "agent-cycle", "resolve", "classify:AUTO"}
	if got := kinds(a); !reflect.DeepEqual(got, want) {
		t.Errorf("same-instant events ordered %v, want the causal sequence %v", got, want)
	}
}

func kinds(es []Event) []string {
	out := make([]string, 0, len(es))
	for _, e := range es {
		out = append(out, e.Kind)
	}
	return out
}

// Time still dominates the tie-break, or the causal rank would reorder genuinely sequential events.
func TestTimeDominatesTheSourceRank(t *testing.T) {
	early := []Event{{At: at("2026-08-07T10:00:00Z"), Source: SourceLedger, Kind: "late-source-early-time"}}
	later := []Event{{At: at("2026-08-07T11:00:00Z"), Source: SourceIngest, Kind: "early-source-later-time"}}

	got, _ := Merge(later, early)
	if got[0].Kind != "late-source-early-time" {
		t.Errorf("the source rank overrode the clock: got %v — an event that happened later must never "+
			"render first because its corpus sorts earlier", kinds(got))
	}
}

// A ZERO TIMESTAMP IS DROPPED AND COUNTED. Rendering it at the epoch would place it ahead of everything,
// and position on a timeline is read as sequence.
func TestAnUndatedEventIsDroppedAndReported(t *testing.T) {
	got, dropped := Merge([]Event{
		{At: at("2026-08-07T10:00:00Z"), Source: SourceIngest, Kind: "real"},
		{Source: SourceLedger, Kind: "undated"},
	})
	if dropped != 1 {
		t.Errorf("dropped = %d, want 1 — a silent drop makes a truncated narrative indistinguishable from "+
			"a complete one", dropped)
	}
	if len(got) != 1 || got[0].Kind != "real" {
		t.Errorf("stream = %v, want only the dated event", kinds(got))
	}
}

// An UNKNOWN source sorts after every known one — never at 0, which would silently reorder the causal
// sequence the known ranks encode.
func TestAnUnknownSourceSortsAfterEveryKnownOne(t *testing.T) {
	same := at("2026-08-07T10:00:00Z")
	got, _ := Merge(
		[]Event{{At: same, Source: Source("brand-new-corpus"), Kind: "unknown"}},
		[]Event{{At: same, Source: SourceLedger, Kind: "known-last"}},
	)
	if got[len(got)-1].Kind != "unknown" {
		t.Errorf("order = %v — an unrecognised corpus must sort last, or adding one silently reorders "+
			"every existing narrative", kinds(got))
	}
}

// THE WINDOW IS HALF-OPEN, so adjacent windows tile without double-counting a boundary event. Paging
// through a long period is otherwise quietly wrong at every seam.
func TestTheWindowIsHalfOpenSoAdjacentWindowsDoNotDoubleCount(t *testing.T) {
	w1 := Window{From: at("2026-08-07T10:00:00Z"), To: at("2026-08-07T11:00:00Z")}
	w2 := Window{From: at("2026-08-07T11:00:00Z"), To: at("2026-08-07T12:00:00Z")}
	boundary := at("2026-08-07T11:00:00Z")

	if w1.Contains(boundary) {
		t.Error("the boundary instant belongs to the FIRST window — two adjacent windows would each " +
			"count it and every seam in a paged reconstruction would duplicate an event")
	}
	if !w2.Contains(boundary) {
		t.Error("the boundary instant belongs to NEITHER window — a paged reconstruction would lose an " +
			"event at every seam, which is worse")
	}
}

// AN UNBOUNDED WINDOW IS REFUSED. Over 9,719 ledger rows and 18,736 agent steps, "no From" is not a
// forensic question; it is a data dump the caller almost certainly did not mean to ask for.
func TestAnUnboundedWindowIsRefused(t *testing.T) {
	if (Window{}).Valid() {
		t.Error("a zero window validated — an unbounded reconstruction is a dump, not an answer")
	}
	if (Window{To: at("2026-08-07T10:00:00Z")}).Valid() {
		t.Error("a window with no From validated")
	}
	if (Window{From: at("2026-08-07T11:00:00Z"), To: at("2026-08-07T10:00:00Z")}).Valid() {
		t.Error("an inverted window validated")
	}
	if !(Window{From: at("2026-08-07T10:00:00Z")}).Valid() {
		t.Error("an open-ended window from a real start must be valid — 'everything since T' is a " +
			"legitimate forensic question")
	}
}

// Hosts is the blast-radius line an operator reads first.
func TestHostsAreDistinctAndSorted(t *testing.T) {
	got := Hosts([]Event{
		{Host: "web02"}, {Host: "web01"}, {Host: "web02"}, {Host: ""}, {Host: "  "},
	})
	want := []string{"web01", "web02"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Hosts = %v, want %v (distinct, sorted, blanks excluded)", got, want)
	}
}
