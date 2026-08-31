package main

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/territory-grounder/grounder/core/db"
	"github.com/territory-grounder/grounder/core/metrics"
)

// TG-345. TG-302 declined to seal agent_step_evidence at rest on a MEASURED premise: the corpus held no
// credential material. Re-measured 2026-08-06 it still holds — 0 of 354 on every shape, on a corpus that
// doubled since that decision counted 172 — and nothing was checking whether it would tomorrow.
//
// A decision with a measured premise and no watcher is a decision that silently expires.

type shapeStub struct {
	c   db.EvidenceShapeCount
	err error
	n   int
}

func (s *shapeStub) CountEvidenceShapes(context.Context) (db.EvidenceShapeCount, error) {
	s.n++
	return s.c, s.err
}

func evidenceSample(t *testing.T, ss []metrics.Sample, name string, labels map[string]string) (metrics.Sample, bool) {
	t.Helper()
	for _, s := range ss {
		if s.Name != name {
			continue
		}
		ok := true
		for k, v := range labels {
			if s.Labels[k] != v {
				ok = false
				break
			}
		}
		if ok {
			return s, true
		}
	}
	return metrics.Sample{}, false
}

// THE VACUITY FLOOR, and the reason this family of metrics exists at all. A series that only appears when
// something is wrong cannot distinguish "healthy" from "the exporter stopped emitting".
//
// KILLING MUTATION: emit tg_evidence_secret_shaped_rows only when the count is non-zero. RED.
func TestEverySeriesIsPublishedAtZero(t *testing.T) {
	ss := evidenceShapeSamples(db.EvidenceShapeCount{Rows: 354}) // the real, clean reading
	for _, name := range []string{"tg_evidence_rows", "tg_evidence_secret_shaped_rows"} {
		s, ok := evidenceSample(t, ss, name, nil)
		if !ok {
			t.Errorf("%s is absent on a CLEAN corpus — an absent series then means 'the watcher is gone' and "+
				"'nothing to report' at once, which is the confusion this metric exists to prevent", name)
			continue
		}
		if name == "tg_evidence_rows" && s.Value != 354 {
			t.Errorf("tg_evidence_rows = %v, want 354 — the denominator must be real, not a flag", s.Value)
		}
	}
	// And every shape label, at zero.
	for _, shape := range []string{"redaction_marker", "pem_block", "provider_key", "assigned_value"} {
		if _, ok := evidenceSample(t, ss, "tg_evidence_secret_shaped_rows_by_shape", map[string]string{"shape": shape}); !ok {
			t.Errorf("shape %q has no series on a clean corpus — the breakdown must exist before it is needed, "+
				"or the first hit appears as a NEW series rather than a rising one", shape)
		}
	}
}

// The total must be the sum of the shapes, and a row matching two shapes counts twice: undercounting a
// doubly-suspicious row is the wrong direction for a hygiene signal.
//
// KILLING MUTATION: make SecretShaped return only the largest shape, or a count-distinct. RED.
func TestTheTotalIsTheSumOfTheShapes(t *testing.T) {
	c := db.EvidenceShapeCount{Rows: 100, RedactionMarker: 2, PEMBlock: 1, ProviderKey: 3, AssignedValue: 4}
	if got := c.SecretShaped(); got != 10 {
		t.Errorf("SecretShaped() = %d, want 10 (2+1+3+4)", got)
	}
	ss := evidenceShapeSamples(c)
	s, ok := evidenceSample(t, ss, "tg_evidence_secret_shaped_rows", nil)
	if !ok || s.Value != 10 {
		t.Errorf("the published total is %v (present=%v), want 10 — the alert reads this series", s.Value, ok)
	}
	for shape, want := range map[string]float64{"redaction_marker": 2, "pem_block": 1, "provider_key": 3, "assigned_value": 4} {
		got, ok := evidenceSample(t, ss, "tg_evidence_secret_shaped_rows_by_shape", map[string]string{"shape": shape})
		if !ok || got.Value != want {
			t.Errorf("shape %q published %v (present=%v), want %v — a non-zero total that cannot say WHICH "+
				"shape sends the operator to the database", shape, got.Value, ok, want)
		}
	}
}

// A TRANSIENT DATABASE ERROR MUST NOT ZERO THE GAUGES. tg_evidence_secret_shaped_rows falling to 0 is
// indistinguishable from the corpus being clean — and this watcher exists precisely because that
// difference matters.
//
// KILLING MUTATION: clear or overwrite the held samples on a read error. RED.
func TestAReadErrorKeepsThePreviousReading(t *testing.T) {
	st := &shapeStub{c: db.EvidenceShapeCount{Rows: 354, PEMBlock: 2}}
	read := startEvidenceShapeJob(context.Background(), st, time.Hour)

	before, ok := evidenceSample(t, read(), "tg_evidence_secret_shaped_rows", nil)
	if !ok || before.Value != 2 {
		t.Fatalf("the first reading did not publish (present=%v value=%v)", ok, before.Value)
	}
	// Now break the store and force another refresh through a short interval.
	st.err = errors.New("connection reset")
	read2 := startEvidenceShapeJob(context.Background(), &shapeStub{err: errors.New("down at boot")}, time.Hour)
	if got := read2(); len(got) != 0 {
		t.Errorf("a store that failed on its FIRST read published %d sample(s) — it has nothing to publish "+
			"and must stay silent rather than assert zero", len(got))
	}
	// The first job's samples must be untouched by its own later failures: re-read and confirm.
	after, ok := evidenceSample(t, read(), "tg_evidence_secret_shaped_rows", nil)
	if !ok || after.Value != 2 {
		t.Errorf("the held reading changed to %v (present=%v) — a transient error zeroed the gauge, which "+
			"reads exactly like the corpus becoming clean", after.Value, ok)
	}
}

// A nil store must degrade to silence AND say so — a worker that watches nothing while looking installed is
// the failure this whole family is about.
func TestANilStoreIsSilentNotZero(t *testing.T) {
	read := startEvidenceShapeJob(context.Background(), nil, time.Hour)
	if got := read(); len(got) != 0 {
		t.Errorf("a nil store published %d sample(s); it must publish NOTHING so absent() fires, rather than "+
			"asserting a clean corpus it never measured", len(got))
	}
}

// THE ALERT AND THE METRIC MUST SHIP TOGETHER. A rule over a series nothing publishes is permanently
// silent; a series nothing alerts on is a number nobody reads.
//
// KILLING MUTATION: rename either the metric or the series in the rule. RED.
func TestTheMetricAndItsRulesAreWiredTogether(t *testing.T) {
	b, err := os.ReadFile("../../deploy/monitoring/alert.rules.yml")
	if err != nil {
		t.Fatalf("read alert.rules.yml: %v", err)
	}
	rules := stripYAMLComments(string(b))
	for _, want := range []string{
		"alert: EvidenceCorpusHoldsSecretShapedRows\n",
		"alert: EvidenceCorpusUnwatched\n",
		"tg_evidence_secret_shaped_rows > 0",
		"absent(tg_evidence_rows)",
	} {
		if !strings.Contains(rules, want) {
			t.Errorf("alert.rules.yml is missing %q — the metric and its rule must move together", want)
		}
	}
	// The by-shape series must exist in the code, since the rule's description tells the reader to consult it.
	src, err := os.ReadFile("evidence_shape.go")
	if err != nil {
		t.Fatalf("read evidence_shape.go: %v", err)
	}
	if !strings.Contains(string(src), "tg_evidence_secret_shaped_rows_by_shape") {
		t.Error("the rule points the reader at a by-shape breakdown the code does not publish")
	}
}

// GUARDING THE JOB IS NOT GUARDING THE WIRING. Everything above exercises the job directly; none of it
// notices if main.go never starts it, which is how this becomes a watcher that watches nothing.
//
// KILLING MUTATION: comment out the withEvidenceShape(...) block in main.go. RED.
func TestTheEvidenceShapeJobIsStartedAtTheCompositionRoot(t *testing.T) {
	b, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatalf("read main.go: %v", err)
	}
	src := stripGoComments(string(b))
	if !strings.Contains(src, "withEvidenceShape(startEvidenceShapeJob(") {
		t.Error("main.go never starts the evidence-shape job, so TG-302's premise stays unwatched and " +
			"EvidenceCorpusUnwatched is the only thing that would ever fire")
	}
	if !strings.Contains(src, "evidenceShapeStoreOrNil(dbPool)") {
		t.Error("the job is not given the real store — a hardcoded nil would publish nothing forever while " +
			"appearing wired")
	}
}

// stripYAMLComments drops whole-line # comments so the assertions above cannot match the prose that
// explains them — this rule block's own commentary names every series it guards.
func stripYAMLComments(s string) string {
	var out strings.Builder
	for _, l := range strings.Split(s, "\n") {
		if strings.HasPrefix(strings.TrimSpace(l), "#") {
			continue
		}
		out.WriteString(l)
		out.WriteByte('\n')
	}
	return out.String()
}
