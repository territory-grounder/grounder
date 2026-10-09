package main

import (
	"reflect"
	"testing"
	"time"

	"github.com/territory-grounder/grounder/core/knowledge"
)

var ts = time.Date(2026, 7, 20, 10, 0, 0, 0, time.UTC)

func corpus() []knowledge.Incident {
	return []knowledge.Incident{
		{ExternalRef: "librenms-1", Host: "app01", AlertRule: "Service-up/down", Summary: "s1", Resolution: "r1"},
		{ExternalRef: "pred-ik-9", Host: "app02", AlertRule: "Devices-up/down", Summary: "s2", Resolution: "r2"},
		{ExternalRef: "librenms-2", Host: "app03", AlertRule: "Device-Down", ResolvedAt: ts.Add(-48 * time.Hour)},
	}
}

// TestBackfillNeverInventsADate is the load-bearing property. A corpus row with no supplied timestamp
// must keep its zero value and keep rendering "[age unknown]" — which is TRUE. An invented date would be
// invented evidence, and it feeds straight into a ranking signal (the recency channel) and into what the
// agent is told about how much to trust a precedent.
//
// KILLING MUTATION: fall back to time.Now() (or any default) for an unmatched ref. Every undated seed row
// then becomes maximally fresh, outranking real resolved incidents that carry a genuine date. RED.
func TestBackfillNeverInventsADate(t *testing.T) {
	got, st := Backfill(corpus(), map[string]time.Time{"librenms-1": ts})

	if !got[0].ResolvedAt.Equal(ts) {
		t.Fatalf("a supplied timestamp must be stamped, got %v", got[0].ResolvedAt)
	}
	if !got[1].ResolvedAt.IsZero() {
		t.Fatalf("an UNMATCHED ref must stay undated, got %v — a guess here is invented evidence", got[1].ResolvedAt)
	}
	if st.Dated != 1 || st.Undated != 1 || st.AlreadyDated != 1 {
		t.Fatalf("stats wrong: %+v", st)
	}
}

// TestBackfillChangesNothingElse: the tool exists to add one field. If it can also reorder rows, drop
// them, or rewrite text, then its diff is not reviewable and an operator cannot safely apply it to a
// live corpus.
//
// KILLING MUTATION: emit rows in map order (or sorted), or write through a struct that omits a field.
// The order/content comparison below goes RED.
func TestBackfillChangesNothingElse(t *testing.T) {
	in := corpus()
	got, _ := Backfill(in, map[string]time.Time{"librenms-1": ts})

	if !reflect.DeepEqual(sortedRefs(in), sortedRefs(got)) {
		t.Fatal("the row SET changed")
	}
	for i := range in {
		if in[i].ExternalRef != got[i].ExternalRef {
			t.Fatalf("row ORDER changed at %d: %q -> %q — an unreadable diff is an unreviewable one",
				i, in[i].ExternalRef, got[i].ExternalRef)
		}
		a, b := in[i], got[i]
		a.ResolvedAt, b.ResolvedAt = time.Time{}, time.Time{} // the only field allowed to differ
		if !reflect.DeepEqual(a, b) {
			t.Fatalf("row %d changed beyond ResolvedAt:\n in=%+v\nout=%+v", i, a, b)
		}
	}
	// And the INPUT must be untouched, so a caller can diff original against result.
	if !in[0].ResolvedAt.IsZero() {
		t.Fatal("Backfill mutated its input; the caller can no longer diff")
	}
}

// TestBackfillIsIdempotent — re-running against an already-dated corpus must be a no-op, so an operator
// can apply it twice without wondering whether they have double-stamped anything.
func TestBackfillIsIdempotent(t *testing.T) {
	dates := map[string]time.Time{"librenms-1": ts, "librenms-2": ts}
	once, _ := Backfill(corpus(), dates)
	twice, st := Backfill(once, dates)
	if !reflect.DeepEqual(once, twice) {
		t.Fatal("a second run changed the corpus")
	}
	if st.Dated != 0 {
		t.Fatalf("a second run must date nothing new, got %d", st.Dated)
	}
	// An already-dated row must NOT be overwritten by the map: the corpus is the record, and a re-run
	// with a different map must not silently rewrite history.
	other, _ := Backfill(once, map[string]time.Time{"librenms-1": ts.Add(500 * time.Hour)})
	if !other[0].ResolvedAt.Equal(ts) {
		t.Fatalf("an existing date was overwritten: %v", other[0].ResolvedAt)
	}
}

// TestUnmatchedTimestampsAreReported: a map full of refs that are not in the corpus means the operator
// joined the wrong things. Reporting zero dated rows without saying why would look like success.
func TestUnmatchedTimestampsAreReported(t *testing.T) {
	_, st := Backfill(corpus(), map[string]time.Time{"nope-1": ts, "nope-2": ts})
	if st.Unmatched != 2 || st.Dated != 0 {
		t.Fatalf("a wholly-unmatched map must be reported, got %+v", st)
	}
}
