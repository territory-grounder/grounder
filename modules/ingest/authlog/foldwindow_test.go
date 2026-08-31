package authlog

import (
	"strings"
	"testing"
	"time"
)

// TG-315 — WITHOUT A TIME COMPONENT IN THE REF, THIS SOURCE ADMITS EACH (host, kind, principal) ONCE, EVER.
//
// `core/db/alert_log.go` inserts `ON CONFLICT (external_ref) DO NOTHING` — deliberately, so the first
// acceptance is canonical and a retrying source cannot accumulate duplicates. The ref folded on
// (host, kind, principal) alone, under a comment claiming a growing burst "updates one incident". It does
// not update. It drops. So the SECOND authentication-failure burst on a host — and every one after it,
// forever — was destined to be silently discarded, while the source looked healthy: rows present, gauge
// delivered, nothing erroring.
//
// These oracles pin the two halves that have to hold at once. Either alone is easy and useless: a ref that
// never repeats makes the collector mint a new incident every poll, and a ref that always repeats is the
// defect.

func failure(host, principal string, first, last time.Time) Event {
	return Event{Host: host, Kind: KindFailure, Principal: principal, Count: 3, FirstSeen: first, LastSeen: last}
}

func refOf(t *testing.T, m *Module, e Event) string {
	t.Helper()
	env, err := m.ToEnvelope(e)
	if err != nil {
		t.Fatalf("ToEnvelope: %v", err)
	}
	return env.ExternalRef
}

// ACROSS WINDOWS the ref must DIFFER, or the store drops every burst after the first.
func TestTheSameBurstInADifferentWindowGetsADistinctRef(t *testing.T) {
	m := New(WithFoldWindow(time.Hour))
	t0 := time.Date(2026, 8, 7, 10, 5, 0, 0, time.UTC)

	a := refOf(t, m, failure("web01", "root", t0, t0.Add(time.Minute)))
	b := refOf(t, m, failure("web01", "root", t0.Add(2*time.Hour), t0.Add(2*time.Hour+time.Minute)))

	if a == b {
		t.Fatalf("two bursts two hours apart produced the SAME external_ref %q — the append-only store's "+
			"ON CONFLICT DO NOTHING would drop the second one, and every later one, permanently", a)
	}
}

// WITHIN one window the ref must MATCH, or a collector re-reading the same trailing lines mints a new
// incident on every tick. This is the half that makes `tail -n` idempotent without a cursor.
func TestTheSameEventReReadInsideOneWindowFoldsToOneRef(t *testing.T) {
	m := New(WithFoldWindow(time.Hour))
	t0 := time.Date(2026, 8, 7, 10, 5, 0, 0, time.UTC)

	// Same fold, re-read a few minutes later with more lines accumulated: same window, higher count.
	a := refOf(t, m, failure("web01", "root", t0, t0.Add(time.Minute)))
	grown := failure("web01", "root", t0, t0.Add(20*time.Minute))
	grown.Count = 247
	b := refOf(t, m, grown)

	if a != b {
		t.Fatalf("the same burst re-read inside one window produced two refs (%q vs %q) — the collector "+
			"would mint a fresh incident every poll and the dedup/flap stages could never see it as one "+
			"ongoing thing", a, b)
	}
}

// The discriminating fields must still discriminate — otherwise the window alone would fold every host's
// events together, which is a different and worse defect.
func TestHostKindAndPrincipalStillSeparateWithinOneWindow(t *testing.T) {
	m := New(WithFoldWindow(time.Hour))
	t0 := time.Date(2026, 8, 7, 10, 5, 0, 0, time.UTC)
	base := failure("web01", "root", t0, t0)

	other := base
	other.Host = "web02"
	esc := base
	esc.Kind = KindEscalation
	who := base
	who.Principal = "deploy"

	seen := map[string]string{}
	for name, e := range map[string]Event{"base": base, "other-host": other, "other-kind": esc, "other-principal": who} {
		r := refOf(t, m, e)
		if prev, dup := seen[r]; dup {
			t.Errorf("%s and %s share the external_ref %q — one would be dropped by the store", name, prev, r)
		}
		seen[r] = name
	}
}

// A line whose RFC3164 stamp did not parse carries NEITHER FirstSeen nor LastSeen. It must still be
// separable across windows — falling back to the ingest clock — rather than collapsing into one immortal
// ref, which is the original defect wearing a different hat.
func TestAnEventWithNoParsedTimestampIsStillSeparableAcrossWindows(t *testing.T) {
	clock := time.Date(2026, 8, 7, 10, 5, 0, 0, time.UTC)
	m := New(WithFoldWindow(time.Hour), WithClock(func() time.Time { return clock }))

	a := refOf(t, m, Event{Host: "web01", Kind: KindFailure, Principal: "root", Count: 1})
	clock = clock.Add(3 * time.Hour)
	b := refOf(t, m, Event{Host: "web01", Kind: KindFailure, Principal: "root", Count: 1})

	if a == b {
		t.Fatalf("a timestamp-less event produced the same ref %q three hours apart — it would be admitted "+
			"once and then dropped forever", a)
	}
}

// A zero or negative fold window would truncate every event to the same instant and reinstate the defect,
// so the option must refuse it rather than accept it silently.
func TestANonPositiveFoldWindowIsRefusedNotAccepted(t *testing.T) {
	m := New(WithFoldWindow(0))
	if m.foldWindow != DefaultFoldWindow {
		t.Errorf("a zero fold window was accepted (%v) — every event would truncate to the same instant and "+
			"the ref would fold forever", m.foldWindow)
	}
	if n := New(WithFoldWindow(-time.Hour)); n.foldWindow != DefaultFoldWindow {
		t.Errorf("a negative fold window was accepted (%v)", n.foldWindow)
	}
}

// The bucket must be in the ref at all. Cheap, and it is the assertion that fails loudly if someone
// removes the concatenation while leaving foldBucket in place.
func TestTheRefCarriesTheBucket(t *testing.T) {
	m := New(WithFoldWindow(time.Hour))
	t0 := time.Date(2026, 8, 7, 10, 5, 0, 0, time.UTC)
	e := failure("web01", "root", t0, t0)

	got := refOf(t, m, e)
	want := m.foldBucket(e)
	if !strings.HasSuffix(got, "-"+want) {
		t.Errorf("external_ref %q does not end with the fold bucket %q", got, want)
	}
	if want != "20260807T100000Z" {
		t.Errorf("bucket %q is not the hour-truncated UTC start of the fold", want)
	}
}
