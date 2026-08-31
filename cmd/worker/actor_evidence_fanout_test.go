package main

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/territory-grounder/grounder/adapters/actorevidence"
	"github.com/territory-grounder/grounder/core/attribution"
	"github.com/territory-grounder/grounder/core/attribution/readertally"
	"github.com/territory-grounder/grounder/core/metrics"
)

// THIS IS THE CONTROL THAT THE TALLY'S OWN CONTROLS COULD NOT BE.
//
// core/attribution/readertally has five mutation controls, all proven RED. Every one of them would still
// pass if this fan-out never called the tally — the package would be perfectly correct and contribute
// nothing, which is precisely the "nine RED controls proved nothing" failure the standing rule warns about:
// a control that encodes a repair path the system does not take.
//
// So this drives the REAL makeActorEvidenceReader over readers whose per-domain answers are known, and
// asserts the /metrics series that come out the other side. It fails if the wiring is removed, if a domain
// is tallied against the wrong name, or if a failure is booked as a read.

type fakeReader struct {
	domain string
	rows   int
	err    error
	calls  int
}

func (f *fakeReader) Domain() string { return f.domain }
func (f *fakeReader) ReadOnly() bool { return true }
func (f *fakeReader) Read(_ context.Context, target string, _, _ time.Time) ([]attribution.Evidence, error) {
	f.calls++
	if f.err != nil {
		return nil, f.err
	}
	out := make([]attribution.Evidence, 0, f.rows)
	for i := 0; i < f.rows; i++ {
		out = append(out, attribution.Evidence{Domain: f.domain, Target: target, Actor: "someone", ActionKind: "test", Covered: true})
	}
	return out, nil
}

func sampleFor(ss []metrics.Sample, name, domain string) (float64, bool) {
	for _, s := range ss {
		if s.Name == name && s.Labels["domain"] == domain {
			return s.Value, true
		}
	}
	return 0, false
}

func TestFanOutTalliesEveryDomainItReads(t *testing.T) {
	productive := &fakeReader{domain: "pve", rows: 4}
	silent := &fakeReader{domain: "journal", rows: 0} // armed, asked, answers nothing — THE case
	broken := &fakeReader{domain: "awx", err: errors.New("dial tcp: connection refused")}

	tally := readertally.New()
	read := makeActorEvidenceReader([]actorevidence.Reader{productive, silent, broken, nil}, tally)

	ev, err := read(context.Background(), "dc1nc02", time.Now().Add(-30*time.Minute), time.Now())
	if err != nil {
		t.Fatalf("read returned an error with one reader still succeeding: %v (a partial outage must stay advisory)", err)
	}
	if len(ev) != 4 {
		t.Fatalf("merged evidence = %d rows, want 4 — the fan-out is not returning what the readers gave it", len(ev))
	}
	for _, r := range []*fakeReader{productive, silent, broken} {
		if r.calls != 1 {
			t.Errorf("reader %s was called %d times, want 1", r.domain, r.calls)
		}
	}

	ss := tally.Collect()
	if len(ss) == 0 {
		t.Fatal("the fan-out produced NO tally samples — the accounting is not wired, and readertally's own " +
			"RED controls prove nothing about this system")
	}

	// productive: 1 read, 4 rows
	if v, ok := sampleFor(ss, readertally.MetricReads, "pve"); !ok || v != 1 {
		t.Errorf("pve reads = %v present=%v, want 1", v, ok)
	}
	if v, ok := sampleFor(ss, readertally.MetricRows, "pve"); !ok || v != 4 {
		t.Errorf("pve rows = %v present=%v, want 4", v, ok)
	}
	// silent: 1 read, 0 rows, AND THE ZERO IS EMITTED — the whole point
	if v, ok := sampleFor(ss, readertally.MetricReads, "journal"); !ok || v != 1 {
		t.Errorf("journal reads = %v present=%v, want 1 — a reader that answers nothing was still asked", v, ok)
	}
	v, ok := sampleFor(ss, readertally.MetricRows, "journal")
	if !ok {
		t.Error("journal rows series absent — a silent reader must be visibly zero, not missing")
	} else if v != 0 {
		t.Errorf("journal rows = %v, want 0", v)
	}
	// broken: a failure, and NOT a read
	if v, ok := sampleFor(ss, readertally.MetricFailures, "awx"); !ok || v != 1 {
		t.Errorf("awx failures = %v present=%v, want 1", v, ok)
	}
	if v, _ := sampleFor(ss, readertally.MetricReads, "awx"); v != 0 {
		t.Errorf("awx reads = %v, want 0 — an unreachable reader is not an empty one", v)
	}

	// and the sentence form answers the question that started this
	silentDomains, productiveDomains := tally.Summary()
	if len(silentDomains) != 1 || silentDomains[0] != "journal" {
		t.Errorf("silent = %v, want [journal]", silentDomains)
	}
	if len(productiveDomains) != 1 || productiveDomains[0] != "pve" {
		t.Errorf("productive = %v, want [pve]", productiveDomains)
	}
}

// The pre-existing behaviour this refactor must not have changed: a TOTAL reader outage is an error, so the
// tool reports UNKNOWN instead of the empty-result message. "No actor evidence" from a dead reader is how a
// reader failure becomes a confident false causal claim.
func TestFanOutErrorsWhenEveryReaderFails(t *testing.T) {
	a := &fakeReader{domain: "pve", err: errors.New("boom")}
	b := &fakeReader{domain: "journal", err: errors.New("boom")}
	tally := readertally.New()
	read := makeActorEvidenceReader([]actorevidence.Reader{a, b}, tally)

	ev, err := read(context.Background(), "h1", time.Now().Add(-time.Hour), time.Now())
	if err == nil {
		t.Fatal("every reader failed and the fan-out returned nil error — the tool would report 'no actor " +
			"evidence' for a total outage, which reads as a confident negative")
	}
	if ev != nil {
		t.Errorf("evidence = %v, want nil on total failure", ev)
	}
	ss := tally.Collect()
	for _, d := range []string{"pve", "journal"} {
		if v, ok := sampleFor(ss, readertally.MetricFailures, d); !ok || v != 1 {
			t.Errorf("%s failures = %v present=%v, want 1", d, v, ok)
		}
		if v, _ := sampleFor(ss, readertally.MetricReads, d); v != 0 {
			t.Errorf("%s reads = %v, want 0", d, v)
		}
	}
}

// A nil tally must not be able to take the evidence path down. The composition root only passes one when
// readers are armed, so the nil path is real.
func TestFanOutToleratesANilTally(t *testing.T) {
	r := &fakeReader{domain: "pve", rows: 2}
	read := makeActorEvidenceReader([]actorevidence.Reader{r}, nil)
	ev, err := read(context.Background(), "h1", time.Now().Add(-time.Hour), time.Now())
	if err != nil || len(ev) != 2 {
		t.Fatalf("nil tally changed the evidence result: %d rows, err=%v", len(ev), err)
	}
}
