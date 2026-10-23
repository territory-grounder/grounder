package tracker

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

type fakeHistory struct {
	name   string
	out    []HistoricalIncident
	err    error
	panics bool
	before func()
	mu     sync.Mutex
	calls  int
	limits []int
}

func (f *fakeHistory) SearchIncidents(_ context.Context, host, rule string, limit int) ([]HistoricalIncident, error) {
	if f.before != nil {
		f.before()
	}
	if f.panics {
		panic("backend exploded")
	}
	f.mu.Lock()
	f.calls++
	f.limits = append(f.limits, limit)
	f.mu.Unlock()
	return f.out, f.err
}

func at(s string) time.Time {
	t, err := time.Parse("2006-01-02", s)
	if err != nil {
		panic(err)
	}
	return t
}

// A site running ServiceNow for ITSM and YouTrack for engineering work has its incident record split
// across both. Reading one of them is reading half the estate's memory.
func TestMultiHistoryMergesEverySourceNewestFirst(t *testing.T) {
	sn := &fakeHistory{out: []HistoricalIncident{
		{ID: "INC0010023", Summary: "mealie unreachable", Filed: at("2026-07-12")},
		{ID: "INC0009980", Summary: "disk full", Filed: at("2026-06-02")},
	}}
	yt := &fakeHistory{out: []HistoricalIncident{
		{ID: "IFR-2198", Summary: "mealie down again", Filed: at("2026-07-20")},
	}}

	got, err := NewMultiHistory(map[string]History{"servicenow": sn, "youtrack": yt}).
		SearchIncidents(context.Background(), "dc1mealie01", "", 10)
	if err != nil {
		t.Fatalf("SearchIncidents: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("want 3 merged incidents, got %d — a source was dropped", len(got))
	}
	wantOrder := []string{"IFR-2198", "INC0010023", "INC0009980"}
	for i, w := range wantOrder {
		if got[i].ID != w {
			t.Fatalf("merged order is not newest-first: position %d is %q, want %q", i, got[i].ID, w)
		}
	}
	// Ids live in per-vendor namespaces; an unqualified "INC0010023" tells a reader nothing about where
	// to go look it up.
	for _, inc := range got {
		if inc.Source == "" {
			t.Errorf("incident %s carries no source stamp", inc.ID)
		}
	}
	if got[0].Source != "youtrack" || got[1].Source != "servicenow" {
		t.Errorf("source stamps are misattributed: %q / %q", got[0].Source, got[1].Source)
	}
}

// One source down must not erase the sources that answered.
func TestMultiHistoryOneFailedSourceDoesNotEraseTheOthers(t *testing.T) {
	down := &fakeHistory{err: errors.New("instance 503")}
	up := &fakeHistory{out: []HistoricalIncident{{ID: "IFR-2198", Filed: at("2026-07-20")}}}

	got, err := NewMultiHistory(map[string]History{"servicenow": down, "youtrack": up}).
		SearchIncidents(context.Background(), "web01", "", 10)
	if err != nil {
		t.Fatalf("one live source answered; this must succeed: %v", err)
	}
	if len(got) != 1 || got[0].ID != "IFR-2198" {
		t.Fatalf("the live source's history was lost: %+v", got)
	}
}

// THE ASYMMETRY. All sources failing is an ERROR, because an empty result here is indistinguishable
// from a site that genuinely has no record — and the agent would believe the second.
func TestMultiHistoryAllSourcesFailingIsAnErrorNotAnEmptyResult(t *testing.T) {
	a := &fakeHistory{err: errors.New("instance 503")}
	b := &fakeHistory{err: errors.New("token expired")}

	got, err := NewMultiHistory(map[string]History{"servicenow": a, "youtrack": b}).
		SearchIncidents(context.Background(), "web01", "", 10)
	if err == nil {
		t.Fatalf("both trackers were unreadable and the merge reported %d incidents with no error — "+
			"the agent would conclude this estate has no incident history", len(got))
	}
	for _, want := range []string{"servicenow", "youtrack"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the error does not name the failed source %q: %v", want, err)
		}
	}
}

// A source that genuinely has no matching incidents is a FINDING, and must stay distinct from an outage.
func TestMultiHistoryEmptyButHealthyIsSuccess(t *testing.T) {
	a := &fakeHistory{out: nil}
	got, err := NewMultiHistory(map[string]History{"servicenow": a}).
		SearchIncidents(context.Background(), "web01", "", 10)
	if err != nil {
		t.Fatalf("a healthy source with no matches is a finding, not an outage: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("want no incidents, got %d", len(got))
	}
}

// No configured source is likewise an error, not a quiet empty answer.
func TestMultiHistoryWithNoSourcesIsAnError(t *testing.T) {
	if _, err := NewMultiHistory(nil).SearchIncidents(context.Background(), "web01", "", 10); err == nil {
		t.Fatal("an empty merge reported success; a caller cannot tell that from a real empty history")
	}
	// A nil source is not a tracker. Counting it would let an all-nil merge claim it queried something.
	m := NewMultiHistory(map[string]History{"servicenow": nil})
	if m.Len() != 0 {
		t.Fatalf("nil sources must not be counted, Len()=%d", m.Len())
	}
	if _, err := m.SearchIncidents(context.Background(), "web01", "", 10); err == nil {
		t.Fatal("a merge of nil sources reported success")
	}
}

// The caller wants the N most relevant incidents OVERALL. Pre-dividing the budget per source would drop
// one source's best match to make room for another's worst.
func TestMultiHistoryGivesEachSourceTheFullLimitAndTruncatesOnce(t *testing.T) {
	mk := func(prefix string, n int) *fakeHistory {
		out := make([]HistoricalIncident, n)
		for i := range out {
			out[i] = HistoricalIncident{ID: prefix, Filed: at("2026-07-01").Add(time.Duration(i) * time.Hour)}
		}
		return &fakeHistory{out: out}
	}
	a, b := mk("a", 4), mk("b", 4)
	got, err := NewMultiHistory(map[string]History{"aa": a, "bb": b}).
		SearchIncidents(context.Background(), "web01", "", 5)
	if err != nil {
		t.Fatalf("SearchIncidents: %v", err)
	}
	if len(a.limits) != 1 || a.limits[0] != 5 {
		t.Fatalf("source was not given the full limit: %v", a.limits)
	}
	if len(got) != 5 {
		t.Fatalf("the merged result must be truncated once, at the end: got %d, want 5", len(got))
	}
}

// Sources are queried concurrently, proven by a barrier rather than a stopwatch: each blocks until all
// have arrived, which a serial implementation can never satisfy. Two slow ITSM instances queried in
// series double the agent's wait inside a live incident.
func TestMultiHistoryQueriesSourcesConcurrently(t *testing.T) {
	const n = 3
	var wg sync.WaitGroup
	wg.Add(n)
	arrive := func() { wg.Done(); wg.Wait() }
	srcs := map[string]History{}
	for i := 0; i < n; i++ {
		srcs[string(rune('a'+i))] = &fakeHistory{before: arrive}
	}
	done := make(chan error, 1)
	go func() {
		_, err := NewMultiHistory(srcs).SearchIncidents(context.Background(), "web01", "", 10)
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("healthy sources, got error: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the merge queried sources SERIALLY — every added tracker lengthens the agent's wait " +
			"inside a live incident")
	}
}

// A panicking backend must not take the worker down, and must not be counted as having answered.
func TestMultiHistoryPanickingSourceIsAFailureNotAnAnswer(t *testing.T) {
	boom := &fakeHistory{panics: true}
	ok := &fakeHistory{out: []HistoricalIncident{{ID: "IFR-1", Filed: at("2026-07-20")}}}
	got, err := NewMultiHistory(map[string]History{"aa": boom, "bb": ok}).
		SearchIncidents(context.Background(), "web01", "", 10)
	if err != nil {
		t.Fatalf("one source answered: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("want the surviving source's history, got %d", len(got))
	}
	if _, err := NewMultiHistory(map[string]History{"aa": &fakeHistory{panics: true}}).
		SearchIncidents(context.Background(), "web01", "", 10); err == nil {
		t.Fatal("a merge whose only source panicked reported SUCCESS")
	}
}

// A zero Filed means UNKNOWN. It must not sort as if it were the newest incident on the estate.
func TestMultiHistoryUnknownDatesSortLastNotFirst(t *testing.T) {
	src := &fakeHistory{out: []HistoricalIncident{
		{ID: "undated"},
		{ID: "dated", Filed: at("2026-01-01")},
	}}
	got, _ := NewMultiHistory(map[string]History{"aa": src}).
		SearchIncidents(context.Background(), "web01", "", 10)
	if len(got) != 2 || got[0].ID != "dated" {
		t.Fatalf("an undated incident outranked a dated one: %+v", got)
	}
}
