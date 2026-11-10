package main

// Guards for the dead-man on TG's own input (TG-336).
//
// The defect these exist to prevent is not a crash — it is the watcher quietly watching nothing, which is
// how the intake collapsed for five days without a signal. Every test below is aimed at a way this could
// go silent while still looking installed.

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/territory-grounder/grounder/core/db"
	"github.com/territory-grounder/grounder/core/metrics"
)

type fakeFreshness struct {
	rows     []db.IngestFreshness
	err      error
	n        int
	never    []db.DeclaredButSilent
	neverErr error
}

func (f *fakeFreshness) Sources(context.Context, time.Duration) ([]db.IngestFreshness, error) {
	f.n++
	return f.rows, f.err
}

func (f *fakeFreshness) SourcesNeverSeen(context.Context, []string) ([]db.DeclaredButSilent, error) {
	return f.never, f.neverErr
}

func sampleByName(ss []metrics.Sample, name string, src string) (metrics.Sample, bool) {
	for _, s := range ss {
		if s.Name != name {
			continue
		}
		if src == "" || s.Labels["source_id"] == src {
			return s, true
		}
	}
	return metrics.Sample{}, false
}

// The pair must both be published. An age with no denominator cannot be judged, and a rule written on the
// age alone pages on estates that are simply quiet.
func TestIngestFreshnessPublishesAgeBesideItsDenominator(t *testing.T) {
	now := time.Date(2026, time.August, 5, 12, 0, 0, 0, time.UTC)
	rows := []db.IngestFreshness{
		{SourceID: "librenms-dc1", LastSeen: now.Add(-2 * time.Hour), RecentTotal: 2263},
		{SourceID: "pve-liveness", LastSeen: now.Add(-120 * time.Hour), RecentTotal: 200},
	}
	ss := ingestFreshnessSamples(rows, now)

	age, ok := sampleByName(ss, "tg_ingest_source_last_seen_seconds", "pve-liveness")
	if !ok {
		t.Fatal("no per-source age published")
	}
	if age.Value != (120 * time.Hour).Seconds() {
		t.Errorf("age = %v, want %v", age.Value, (120 * time.Hour).Seconds())
	}
	den, ok := sampleByName(ss, "tg_ingest_source_recent_total", "pve-liveness")
	if !ok {
		t.Fatal("the age was published with NO denominator — a rule over it cannot tell a dead source " +
			"from a quiet one, which is how this family of alerts gets muted")
	}
	if den.Value != 200 {
		t.Errorf("denominator = %v, want 200", den.Value)
	}
}

// A source that has NEVER delivered must not publish an age. A huge age there is indistinguishable from a
// source that died, and only one of those is an incident.
func TestSourceThatNeverDeliveredPublishesNoAge(t *testing.T) {
	now := time.Date(2026, time.August, 5, 12, 0, 0, 0, time.UTC)
	ss := ingestFreshnessSamples([]db.IngestFreshness{{SourceID: "never", RecentTotal: 0}}, now)
	if _, ok := sampleByName(ss, "tg_ingest_source_last_seen_seconds", "never"); ok {
		t.Error("a source that has never delivered published an age. That is indistinguishable from a " +
			"source that stopped, and would page for a transport that was never wired.")
	}
	if _, ok := sampleByName(ss, "tg_ingest_source_recent_total", "never"); !ok {
		t.Error("the source vanished entirely — it must still be counted, or a configured-but-silent " +
			"source is invisible rather than merely quiet")
	}
}

// THE VACUITY FLOOR, and the reason it is a published gauge rather than a comment. With zero rows every
// per-source series is absent and both dead-man rules go quiet — silence that reads as health.
func TestSourcesKnownIsAlwaysEmittedEvenWithNoRows(t *testing.T) {
	ss := ingestFreshnessSamples(nil, time.Date(2026, time.August, 5, 12, 0, 0, 0, time.UTC))
	known, ok := sampleByName(ss, "tg_ingest_sources_known", "")
	if !ok {
		t.Fatal("tg_ingest_sources_known was not emitted for an empty read. It is the ONLY series that " +
			"survives an empty intake, so without it 'the watcher sees nothing' and 'the estate is " +
			"healthy' publish identically — the exact failure this dead-man exists to prevent.")
	}
	if known.Value != 0 {
		t.Errorf("sources_known = %v with no rows, want 0", known.Value)
	}
	if _, ok := sampleByName(ss, "tg_ingest_last_seen_seconds", ""); ok {
		t.Error("a fleet age was published with no sources at all — there is no newest delivery to " +
			"measure from, and a fabricated one would be read as a real reading")
	}
}

// A read error must NOT clear the last good reading. Zeroed gauges look exactly like the estate going
// deaf, so a transient database blip would page for an outage that is not happening.
func TestReadErrorKeepsThePreviousReadingRatherThanZeroingIt(t *testing.T) {
	now := time.Now().UTC()
	f := &fakeFreshness{rows: []db.IngestFreshness{
		{SourceID: "librenms-dc1", LastSeen: now.Add(-time.Minute), RecentTotal: 500},
	}}
	read := startIngestFreshnessJob(context.Background(), f, nil, time.Hour, 7*24*time.Hour)
	first := read()
	if len(first) == 0 {
		t.Fatal("the job published nothing on its immediate first refresh — the gauges would not exist " +
			"until the first tick, leaving a window where the dead-man is absent")
	}

	// Now make the store fail and refresh through the same path the ticker uses.
	f.err = errors.New("connection refused")
	f2 := &fakeFreshness{err: errors.New("connection refused")}
	read2 := startIngestFreshnessJob(context.Background(), f2, nil, time.Hour, 7*24*time.Hour)
	if got := read2(); len(got) != 0 {
		t.Errorf("a job whose FIRST read failed published %d samples; it must publish nothing rather "+
			"than fabricate a reading", len(got))
	}

	// The first job's samples must be untouched by its own later failure.
	if len(read()) != len(first) {
		t.Error("a read error cleared the previously published samples. Zeroed intake gauges are " +
			"indistinguishable from the estate going deaf, so a transient DB error would page for an " +
			"outage that is not happening.")
	}
}

// A nil store must degrade to silence, not a panic — and must say so, because an unwatched input is the
// condition TG-336 is about.
func TestNilStoreDegradesToSilenceNotPanic(t *testing.T) {
	read := startIngestFreshnessJob(context.Background(), nil, nil, time.Hour, time.Hour)
	if got := read(); got != nil {
		t.Errorf("a nil store published %d samples, want none", len(got))
	}
}

// The typed-nil trap: NewIngestFreshnessStore(nil) inside an interface is NON-nil, so a naive
// `store == nil` check would not fire and the first query would panic on a pool-less worker.
func TestTypedNilPoolDoesNotBecomeANonNilReader(t *testing.T) {
	if r := ingestFreshnessStoreOrNil(nil); r != nil {
		t.Error("a nil pool produced a non-nil reader — the nil guard in startIngestFreshnessJob cannot " +
			"fire, and a worker without a database would panic on the first refresh instead of logging " +
			"that its input is unwatched")
	}
}

// A DECLARED SOURCE THAT HAS NEVER DELIVERED IS INVISIBLE TO THE FRESHNESS PAIR.
//
// Sources() discovers sources FROM THE DATA, so a source with zero rows has no row to go stale. TG-291 is
// the standing instance: CrowdSec is advertised in the boot log and has never appeared in ingest_alert at
// all — the all-time distinct source list has four entries and CrowdSec is not one of them.
func TestDeclaredButNeverSeenIsPublishedAsOneNotAbsent(t *testing.T) {
	// Declared values are source TYPES, matching the module registry — not the per-site source IDs the
	// freshness gauges carry. Getting these two confused marked librenms (2,692 alerts) as never-delivered.
	declared := []string{"crowdsec", "librenms"}
	ss := declaredSilentSamples(declared, []db.DeclaredButSilent{{SourceID: "crowdsec"}})

	got := map[string]float64{}
	for _, s := range ss {
		if s.Name == "tg_ingest_source_never_delivered" {
			got[s.Labels["source_type"]] = s.Value
		}
	}
	if got["crowdsec"] != 1 {
		t.Errorf("a declared source that never delivered published %v, want 1", got["crowdsec"])
	}
	// The healthy one must be published AT ZERO, not omitted. A metric that only appears when broken
	// cannot distinguish healthy from "the exporter stopped emitting".
	v, ok := got["librenms"]
	if !ok {
		t.Error("a healthy declared source published no series at all. A rule over a metric that only " +
			"exists when something is wrong cannot tell healthy from absent, which is the failure this " +
			"whole family exists to avoid.")
	} else if v != 0 {
		t.Errorf("a delivering source published %v, want 0", v)
	}
	// Vacuity floor: the declared count must always be emitted.
	var sawCount bool
	for _, s := range ss {
		if s.Name == "tg_ingest_sources_declared" {
			sawCount = true
			if s.Value != 2 {
				t.Errorf("declared count = %v, want 2", s.Value)
			}
		}
	}
	if !sawCount {
		t.Error("tg_ingest_sources_declared was not emitted — without it, zero declared sources and " +
			"every declared source healthy publish identically")
	}
}

// THE ATTRIBUTION NUMERATOR MUST REACH /metrics, NOT JUST THE STORE (TG-373 item 5).
//
// core/db computes RecentUnattributed and core/db's own tests prove the SQL. That is the resolver. This is
// the wiring, and removing the sample from the builder survived those tests completely — the tenth time
// this session a value was computed correctly and published nowhere.
//
// KILLING MUTATION: delete the tg_ingest_source_recent_unattributed append from ingestFreshnessSamples. RED.
func TestTheUnattributedNumeratorIsPublishedBesideItsDenominator(t *testing.T) {
	now := time.Date(2026, time.August, 6, 12, 0, 0, 0, time.UTC)
	rows := []db.IngestFreshness{
		// The live shape on 2026-08-06: 48 of 165 named no machine.
		{SourceID: "prometheus-alertmanager", LastSeen: now.Add(-7 * time.Hour), RecentTotal: 165, RecentUnattributed: 48},
		// A source that attributes everything must still publish the zero.
		{SourceID: "librenms-dc1", LastSeen: now.Add(-time.Hour), RecentTotal: 2781, RecentUnattributed: 0},
	}
	ss := ingestFreshnessSamples(rows, now)

	un, ok := sampleByName(ss, "tg_ingest_source_recent_unattributed", "prometheus-alertmanager")
	if !ok {
		t.Fatal("the unattributed count is computed by the store and published nowhere — it was reachable " +
			"only by querying Postgres by hand, which is the state TG-373 item 5 exists to end")
	}
	if un.Value != 48 {
		t.Errorf("unattributed = %v, want 48", un.Value)
	}
	den, ok := sampleByName(ss, "tg_ingest_source_recent_total", "prometheus-alertmanager")
	if !ok || den.Value != 165 {
		t.Fatalf("the numerator was published without its denominator (ok=%v v=%v) — 48 alone is not a "+
			"rate, and a rule over it would fire on a busy healthy source and stay silent on a broken quiet one",
			ok, den.Value)
	}
	// ZERO IS PUBLISHED, NOT OMITTED. A source that attributes everything must be distinguishable from one
	// that stopped being measured.
	clean, ok := sampleByName(ss, "tg_ingest_source_recent_unattributed", "librenms-dc1")
	if !ok {
		t.Fatal("a fully-attributed source published NO unattributed series — absent and zero would then " +
			"mean the same thing, which is the confusion this whole metric family exists to close")
	}
	if clean.Value != 0 {
		t.Errorf("fully-attributed source reported %v unattributed, want 0", clean.Value)
	}
}
