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

// TG-352. There were NO prediction metrics — `tg_predict*` and `tg_blast*` both returned no series — so
// the only way to say anything about the model the actuation gate reasons over was to query Postgres by
// hand. That is how the ticket came to quote "~44 hosts per incident", an average taken over the SCORED
// rows (354 of 2047), which are heavily biased toward wide predictions:
//
//	scored   354 rows  avg 43.9 hosts  45.5% wide
//	unscored 1693 rows avg  6.2 hosts  10.8% wide
//	empty    1386 of 2047 (67.7%)

type fakePredictionReader struct {
	w   db.PredictionWidth
	err error
	got int // the threshold it was called with
}

func (f *fakePredictionReader) CountPredictionWidth(_ context.Context, threshold int) (db.PredictionWidth, error) {
	f.got = threshold
	return f.w, f.err
}

func predSample(ss []metrics.Sample, name string) (metrics.Sample, bool) {
	for _, s := range ss {
		if s.Name == name {
			return s, true
		}
	}
	return metrics.Sample{}, false
}

// TestTheEmptyRateIsPublished is the finding the scoped average hid.
func TestTheEmptyRateIsPublished(t *testing.T) {
	f := &fakePredictionReader{w: db.PredictionWidth{Rows: 2047, Empty: 1386, Wide: 343, Scored: 354}}
	ss := startPredictionWidthJob(context.Background(), f, 8, time.Hour)()

	for name, want := range map[string]float64{
		"tg_prediction_rows":   2047,
		"tg_prediction_empty":  1386,
		"tg_prediction_wide":   343,
		"tg_prediction_scored": 354,
	} {
		s, ok := predSample(ss, name)
		if !ok {
			t.Errorf("%s is not published", name)
			continue
		}
		if s.Value != want {
			t.Errorf("%s = %v, want %v", name, s.Value, want)
		}
	}
}

// TestTheScoredBaseIsPublished pins the series that stops a precision figure being quoted without its
// base. Scoring is biased toward wide predictions, so an average over scored rows is not an average over
// predictions — the exact error this register exists to prevent being repeated.
func TestTheScoredBaseIsPublished(t *testing.T) {
	f := &fakePredictionReader{w: db.PredictionWidth{Rows: 2047, Scored: 354}}
	if s, ok := predSample(startPredictionWidthJob(context.Background(), f, 8, time.Hour)(), "tg_prediction_scored"); !ok || s.Value != 354 {
		t.Errorf("tg_prediction_scored = %v (present=%v), want 354 — without it, a precision computed over "+
			"354 biased rows reads as a property of all 2047", s.Value, ok)
	}
}

// TestTheThresholdComesFromTheCaller. A register with its OWN copy of the boundary would report on a line
// the risk classifier does not enforce.
func TestTheThresholdComesFromTheCaller(t *testing.T) {
	f := &fakePredictionReader{w: db.PredictionWidth{Rows: 10}}
	ss := startPredictionWidthJob(context.Background(), f, 25, time.Hour)()

	if f.got != 25 {
		t.Errorf("the store was queried with threshold %d, want the caller's 25 — a register that picks "+
			"its own boundary measures a line nothing enforces", f.got)
	}
	if s, ok := predSample(ss, "tg_prediction_wide_threshold"); !ok || s.Value != 25 {
		t.Errorf("tg_prediction_wide_threshold = %v (present=%v), want 25 — the counts are unreadable "+
			"without the boundary they were taken against", s.Value, ok)
	}
}

func TestATransientErrorKeepsThePredictionReading(t *testing.T) {
	failing := &fakePredictionReader{err: errors.New("connection refused")}
	if ss := startPredictionWidthJob(context.Background(), failing, 8, time.Hour)(); len(ss) != 0 {
		t.Errorf("a reader whose FIRST read fails published %d sample(s) — it has never seen the database, "+
			"so it must publish nothing rather than a fabricated zero-empty reading", len(ss))
	}
}

func TestANilPredictionStorePublishesNothing(t *testing.T) {
	if ss := startPredictionWidthJob(context.Background(), nil, 8, time.Hour)(); len(ss) != 0 {
		t.Errorf("a nil store published %d sample(s)", len(ss))
	}
}

// TestThePredictionRegisterIsWired — guarding the job is not guarding the wiring.
func TestThePredictionRegisterIsWired(t *testing.T) {
	raw, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatalf("read main.go: %v", err)
	}
	src := stripGoComments(string(raw))
	for _, want := range []string{"startPredictionWidthJob(", "withPredictionWidth(", "predictionWidthStoreOrNil("} {
		if !strings.Contains(src, want) {
			t.Errorf("main.go does not call %s — the register would be computed and published by nothing", want)
		}
	}
	// It must read the SAME env key the risk classifier's threshold comes from.
	i := strings.Index(src, "startPredictionWidthJob(")
	if i >= 0 {
		end := i + 260
		if end > len(src) {
			end = len(src)
		}
		if !strings.Contains(src[i:end], "TG_BLAST_RADIUS_WIDE_THRESHOLD") {
			t.Errorf("the register is not fed TG_BLAST_RADIUS_WIDE_THRESHOLD, so it counts \"wide\" against "+
				"a different boundary than the one that forces review:\n%s", src[i:end])
		}
	}
}

// TestBareEmptinessIsNotTheAlertableSignal pins the correction. My first version of this register counted
// bare emptiness and logged a warning when it exceeded half the population — which would have fired
// permanently on correct behaviour. Measured 2026-08-07: all 1386 empty predictions were on targets with
// in-degree ZERO, and TG's actuation allowlist is specifically leaf app guests, so an empty blast radius
// is the RIGHT answer for them.
func TestBareEmptinessIsNotTheAlertableSignal(t *testing.T) {
	// The live shape: everything empty, nothing connected. This is HEALTH.
	f := &fakePredictionReader{w: db.PredictionWidth{Rows: 2047, Empty: 1386, Wide: 343, Scored: 354, EmptyOnConnected: 0}}
	ss := startPredictionWidthJob(context.Background(), f, 8, time.Hour)()

	s, ok := predSample(ss, "tg_prediction_empty_on_connected")
	if !ok {
		t.Fatal("tg_prediction_empty_on_connected is not published — without it the only empty-related " +
			"series counts a mostly-correct behaviour, and a rule on it would page on health")
	}
	if s.Value != 0 {
		t.Errorf("empty_on_connected = %v, want 0 for the live shape", s.Value)
	}
	// The denominator must still be published: 0-of-1386 is meaningful, a bare 0 is not.
	if e, ok := predSample(ss, "tg_prediction_empty"); !ok || e.Value != 1386 {
		t.Errorf("tg_prediction_empty = %v (present=%v), want 1386 — it is the denominator that makes the "+
			"zero above readable", e.Value, ok)
	}
}

// TestTheBlindCaseIsCounted — the state that actually means the predictor failed.
func TestTheBlindCaseIsCounted(t *testing.T) {
	f := &fakePredictionReader{w: db.PredictionWidth{Rows: 100, Empty: 60, EmptyOnConnected: 7}}
	ss := startPredictionWidthJob(context.Background(), f, 8, time.Hour)()

	if s, ok := predSample(ss, "tg_prediction_empty_on_connected"); !ok || s.Value != 7 {
		t.Errorf("empty_on_connected = %v (present=%v), want 7 — a predictor saying \"nothing is affected\" "+
			"about a host with dependents is the blind case", s.Value, ok)
	}
}

// TestTheWarningFiresOnTheBlindCaseNotOnEmptiness guards the log line, which my sample assertions do not
// reach. Reverting it to `w.Empty*2 > w.Rows` — my first version — survived every other test here, and
// that condition is TRUE on the live healthy shape (1386 of 2047), so the worker would announce a defect
// on every boot forever and the line would be ignored within a day.
func TestTheWarningFiresOnTheBlindCaseNotOnEmptiness(t *testing.T) {
	raw, err := os.ReadFile("prediction_width.go")
	if err != nil {
		t.Fatalf("read prediction_width.go: %v", err)
	}
	src := stripGoComments(string(raw))

	// Anchored on the WARNING's own text, not on the shared "prediction width:" prefix — the file has two
	// such log lines and the first is the read-failure one, whose condition is `if err != nil`. Matching
	// the prefix found that instead and reported a false failure.
	i := strings.Index(src, "said NOTHING IS AFFECTED")
	if i < 0 {
		t.Fatal("VACUITY FLOOR: the blind-case warning is gone — this guard is anchored on a line that no " +
			"longer exists, and would otherwise pass while checking nothing")
	}
	// Walk back to the enclosing condition.
	head := src[:i]
	j := strings.LastIndex(head, "if ")
	if j < 0 {
		t.Fatal("the warning is not inside a condition at all — it would log unconditionally")
	}
	cond := src[j:i]

	if !strings.Contains(cond, "EmptyOnConnected") {
		t.Errorf("the prediction-width warning is not gated on EmptyOnConnected: %q\n"+
			"Bare emptiness is CORRECT behaviour here — all 1386 empty predictions measured on 2026-08-07 "+
			"were on in-degree-zero leaves — so a warning keyed on the empty RATIO announces a defect on "+
			"every boot and gets ignored.", strings.TrimSpace(cond))
	}
}

// TestTheInDegreeQueryIgnoresTheSmallPlaneSnapshot. The estate refresh interleaves a ~17-edge
// actuation-plane snapshot with the full triage one (TG-346). Keying the in-degree on the NEWEST row
// would compute every host's in-degree as 0 and report every empty prediction as correct — the reassuring
// direction, and the one that would hide the blind case entirely.
func TestTheInDegreeQueryIgnoresTheSmallPlaneSnapshot(t *testing.T) {
	raw, err := os.ReadFile("../../core/db/prediction_width.go")
	if err != nil {
		t.Fatalf("read core/db/prediction_width.go: %v", err)
	}
	src := stripGoComments(string(raw))

	if !strings.Contains(src, "indeg") {
		t.Fatal("the query no longer computes an in-degree, so empty_on_connected cannot be derived")
	}
	if !strings.Contains(src, "jsonb_array_length(graph_json->'nodes') >") {
		t.Error("the in-degree CTE does not filter snapshots by node count. The estate writes a ~17-edge " +
			"actuation-plane snapshot interleaved with the full one, so an unfiltered `ORDER BY captured_at " +
			"DESC LIMIT 1` can select it — every in-degree then reads 0 and every empty prediction is " +
			"reported as correct.")
	}
}
