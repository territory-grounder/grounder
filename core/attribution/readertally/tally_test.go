package readertally

import (
	"strings"
	"sync"
	"testing"

	"github.com/territory-grounder/grounder/core/metrics"
)

// find returns the sample for a family+domain, and whether it was emitted at all. The distinction matters:
// "not emitted" and "emitted as zero" are the two readings this package exists to keep apart.
func find(t *testing.T, ss []metrics.Sample, name, domain string) (metrics.Sample, bool) {
	t.Helper()
	for _, s := range ss {
		if s.Name == name && s.Labels["domain"] == domain {
			return s, true
		}
	}
	return metrics.Sample{}, false
}

// THE DEFECT: A SILENT READER AND A PRODUCTIVE READER LOOKED IDENTICAL.
//
// The composition root merges every domain's evidence into one slice and only logs FAILURES, so a reader
// that answered promptly with nothing was indistinguishable from the one carrying the whole result set.
// "5 of 6 evidence readers have returned zero rows all-time" was asserted in status reviews for weeks and
// the system could not produce it.
//
// This is the shape the assertion has to take: a domain that was ASKED and answered NOTHING must emit
// rows=0 explicitly, beside a non-zero read count. Omitting it — the tempting "don't emit empty series"
// optimisation — recreates the exact ambiguity, because an absent series and a zero series read the same
// in a graph and differently in an alert.
func TestAZeroRowReadIsRecordedNotOmitted(t *testing.T) {
	ta := New()
	ta.Read("journal", 0) // armed, asked, returned nothing — THE case
	ta.Read("journal", 0)
	ta.Read("pve", 3) // productive
	ta.Failed("awx")  // unreachable — a third, distinct state

	ss := ta.Collect()

	// journal: asked twice, zero rows, and the zero is PRESENT
	reads, ok := find(t, ss, MetricReads, "journal")
	if !ok || reads.Value != 2 {
		t.Errorf("journal reads: got %v present=%v, want 2 — a read that returns nothing must still count as a read", reads.Value, ok)
	}
	rows, ok := find(t, ss, MetricRows, "journal")
	if !ok {
		t.Fatal("journal rows series was OMITTED — an absent series and a zero series read identically in a graph; the zero IS the finding")
	}
	if rows.Value != 0 {
		t.Errorf("journal rows: got %v, want 0", rows.Value)
	}

	// pve: productive, and distinguishable from journal
	if s, ok := find(t, ss, MetricRows, "pve"); !ok || s.Value != 3 {
		t.Errorf("pve rows: got %v present=%v, want 3", s.Value, ok)
	}

	// awx: a FAILURE IS NOT A ZERO-ROW READ. Counting it as one would make an unreachable reader
	// indistinguishable from an empty-scope reader — opposite remedies (fix the endpoint vs widen the scope).
	if s, ok := find(t, ss, MetricFailures, "awx"); !ok || s.Value != 1 {
		t.Errorf("awx failures: got %v present=%v, want 1", s.Value, ok)
	}
	if s, ok := find(t, ss, MetricReads, "awx"); !ok || s.Value != 0 {
		t.Errorf("awx reads: got %v, want 0 — a failed read is not a read", s.Value)
	}

	// every emitted family is a counter (a monotonically rising total), never a gauge
	for _, s := range ss {
		if s.Kind != metrics.Counter {
			t.Errorf("%s{domain=%s}: Kind=%q, want counter", s.Name, s.Labels["domain"], s.Kind)
		}
		if strings.TrimSpace(s.Help) == "" {
			t.Errorf("%s{domain=%s}: empty Help — an operator reading /metrics cold cannot interpret it", s.Name, s.Labels["domain"])
		}
	}
}

// Summary is the sentence form of the same fact, and it must distinguish THREE states, not two: never
// asked, asked-and-empty, asked-and-productive. A reader that was never asked is not "silent" — it is
// unwired, a different defect with a different fix, and reporting it as silent sends the reader on a hunt
// through a reader that is working fine.
func TestSummarySeparatesNeverAskedFromAskedAndEmpty(t *testing.T) {
	ta := New()
	ta.Read("journal", 0)
	ta.Read("netbox", 0)
	ta.Read("pve", 7)
	ta.Failed("awx") // never successfully asked

	silent, productive := ta.Summary()
	if got := strings.Join(silent, ","); got != "journal,netbox" {
		t.Errorf("silent = %q, want \"journal,netbox\" — awx only FAILED, it was never asked and answered", got)
	}
	if got := strings.Join(productive, ","); got != "pve" {
		t.Errorf("productive = %q, want \"pve\"", got)
	}
}

// The tally sits on the evidence path, which fans out across readers. It must never be the reason that
// path slows or breaks: concurrent use is safe, and the nil receiver is a no-op rather than a panic.
func TestTallyIsConcurrencySafeAndNilTolerant(t *testing.T) {
	var nilT *Tally
	nilT.Read("pve", 1) // must not panic
	nilT.Failed("pve")
	if s := nilT.Collect(); s != nil {
		t.Errorf("nil tally Collect() = %v, want nil", s)
	}
	if a, b := nilT.Summary(); a != nil || b != nil {
		t.Errorf("nil tally Summary() = %v,%v, want nil,nil", a, b)
	}

	ta := New()
	var wg sync.WaitGroup
	for range 64 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ta.Read("pve", 1)
			ta.Read("journal", 0)
			ta.Failed("awx")
		}()
	}
	wg.Wait()
	if s, _ := find(t, ta.Collect(), MetricRows, "pve"); s.Value != 64 {
		t.Errorf("pve rows after 64 concurrent reads = %v, want 64 (lost updates)", s.Value)
	}
	if s, _ := find(t, ta.Collect(), MetricReads, "journal"); s.Value != 64 {
		t.Errorf("journal reads after 64 concurrent reads = %v, want 64", s.Value)
	}
}

// An empty tally must emit NOTHING. An estate with no armed reader has no reader to report on, and a
// family that can never move is noise in every dashboard that picks it up.
func TestAnUnarmedEstateEmitsNoSeries(t *testing.T) {
	if s := New().Collect(); len(s) != 0 {
		t.Errorf("empty tally emitted %d samples, want 0", len(s))
	}
}
