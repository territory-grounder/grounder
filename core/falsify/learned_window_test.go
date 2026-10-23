package falsify

import (
	"context"
	"testing"
	"time"

	"github.com/territory-grounder/grounder/core/predict"
	"github.com/territory-grounder/grounder/core/verify"
)

// ---- TG-220 / spec/002 REQ-110: the LEARNED falsifiability window, on the REAL scorer path ----
//
// Every oracle below drives the production *Scorer.ScoreDue over its designated oracle twin (MemStore), the
// production confusion matrix (predict.ScoreControl) and the production verdict author. Nothing here
// re-implements the window: the assertions read what the real scorer wrote back. The one thing the oracles
// synthesize is the WORLD — a cascade that takes a known time to manifest, and an estate whose durable
// ledger has (or has not) seen that edge behave that way before.

// committedAt anchors every scenario; the clock is advanced explicitly so "the cascade fires at T+15m" is a
// fact about the world, not a sleep.
var learnedCommitted = time.Date(2026, 7, 18, 11, 0, 0, 0, time.UTC)

// cascadeAt builds an Observer whose alerts only become visible once the clock reaches fireAt — a synthetic
// cascade with a known propagation delay. Before that it reports a REAL observation of a quiet estate
// (ok=true, no alerts), which is exactly what the live LibreNMS surface would report: the cascade has not
// happened yet. That is what makes early scoring record a miss that never happened.
func cascadeAt(clock *time.Time, fireAt time.Time, alerts []verify.ObservedAlert) Observer {
	return func(context.Context, string, string) ([]verify.ObservedAlert, bool) {
		if clock.Before(fireAt) {
			return nil, true
		}
		return alerts, true
	}
}

// observedLatency wires a durable-latency twin: the samples TG's own ledger has recorded for these edges.
// (db.CascadeLatencyStore over ingest_alert is the production implementation of this same seam.)
func observedLatency(edges map[CascadeEdge][]time.Duration) LatencyReader {
	return func(context.Context, []string, time.Time) (map[CascadeEdge][]time.Duration, bool) {
		return edges, true
	}
}

// learnedScorer builds the production Scorer over a world: a clock the test advances, an observer, and an
// optional latency seam. Bounds are the production defaults unless a test overrides them.
func learnedScorer(store *MemStore, clock *time.Time, obs Observer, lat LatencyReader) *Scorer {
	return &Scorer{
		Unscored: store, Scores: store, ForecastVerdicts: store, CascadeStats: store,
		Observe: obs, Baseline: emptyBaseline, Latency: lat,
		WindowFloor: DefaultWindowFloor, WindowCap: DefaultWindowCap,
		Now: func() time.Time { return *clock },
	}
}

// THE ORACLE THIS CHANGE EXISTS FOR (REQ-110). A cascade that manifests 15 minutes after the prediction is
// committed must adjudicate as a HIT — which is what the predecessor's max(900s, 2xp95) rule produces (its
// floor alone spans 900s, and its scoring interval is half-open (start, start+window]) and what TG's fixed
// 10-minute window could not: TG scored at +10m, saw a quiet estate, and recorded the cascade as never
// having happened.
//
// TWO ARMS, because the rule has two halves and the floor alone would only cover the first:
//
//	A. NO observed history for the edge — the 900s FLOOR carries the 15-minute cascade.
//	B. A LEARNED window — the edge's own history says it propagates in ~15m, so 2xp95 = 30m, and a cascade
//	   on that edge landing at +25m is STILL adjudicated on what happened. A floor-only implementation fails
//	   this arm.
func TestScoreDueFifteenMinuteCascadeIsAHitUnderTheLearnedWindow(t *testing.T) {
	cascade := []verify.ObservedAlert{
		{Host: "n8n01", Rule: "HostDown", Site: "nl"},
		{Host: "litellm01", Rule: "HostDown", Site: "nl"},
	}

	t.Run("A: an unobserved edge — the 900s floor carries a 15-minute cascade", func(t *testing.T) {
		store := NewMemStore()
		store.Seed(samplePrediction("plan-220-a", "act-220-a"), learnedCommitted)
		clock := learnedCommitted
		// No latency seam at all: every edge falls back to the floor — the fail-safe path.
		sc := learnedScorer(store, &clock, cascadeAt(&clock, learnedCommitted.Add(15*time.Minute), cascade), nil)

		// A pass at +12m — the first 5-minute tick the RETIRED 10m window would have adjudicated on — must
		// score nothing: the cascade has not happened yet, so scoring here records a miss that never was.
		// (Deliberately NOT +10m: that lands exactly on the retired boundary, where the strict `committed_at <
		// olderThan` comparison would pass for the wrong reason and this assertion would not detect the revert.)
		clock = learnedCommitted.Add(12 * time.Minute)
		if res, err := sc.ScoreDue(context.Background()); err != nil || res.Scored != 0 {
			t.Fatalf("at +12m the cascade has not happened yet; scoring it there is the bias TG-220 closes: %+v %v", res, err)
		}

		// A pass after the 900s floor, by which time the cascade HAS manifested.
		clock = learnedCommitted.Add(16 * time.Minute)
		res, err := sc.ScoreDue(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if res.Scored != 1 || res.SumRealTP != 2 {
			t.Fatalf("the 15-minute cascade must adjudicate as a HIT, got %+v", res)
		}
		if got, _ := store.ScoreOf("plan-220-a"); got.TP != 2 || got.FP != 0 {
			t.Fatalf("confusion matrix = %+v, want tp=2 fp=0 — the cascade the prediction named DID happen", got)
		}
	})

	t.Run("B: a learned 30m window — a slow edge is adjudicated on what happened, not on a constant", func(t *testing.T) {
		store := NewMemStore()
		store.Seed(samplePrediction("plan-220-b", "act-220-b"), learnedCommitted)
		clock := learnedCommitted
		// The durable ledger has seen this edge propagate in ~15 minutes ⇒ p95=900s ⇒ window = 2x900s = 1800s.
		lat := observedLatency(map[CascadeEdge][]time.Duration{
			{Primary: "pve01", Dependent: "n8n01"}: {880 * time.Second, 890 * time.Second, 900 * time.Second},
		})
		sc := learnedScorer(store, &clock, cascadeAt(&clock, learnedCommitted.Add(25*time.Minute), cascade), lat)

		// Past the FLOOR but inside the LEARNED window: DEFERRED, not scored. A floor-only implementation
		// scores here, observes a quiet estate, and writes tp=0 — the miss that never happened.
		clock = learnedCommitted.Add(16 * time.Minute)
		res, err := sc.ScoreDue(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if res.Scored != 0 || res.Deferred != 1 {
			t.Fatalf("inside the learned window the prediction must be DEFERRED (nothing failed, the evidence is not in): %+v", res)
		}
		if res.WidestWindow != 1800*time.Second {
			t.Fatalf("learned window = %s, want 1800s = max(900s, 2 x p95=900s)", res.WidestWindow)
		}
		if _, scored := store.ScoreOf("plan-220-b"); scored {
			t.Fatal("a deferred prediction must remain unscored and retryable")
		}

		// After the learned window, on the real cascade.
		clock = learnedCommitted.Add(31 * time.Minute)
		res, err = sc.ScoreDue(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if res.Scored != 1 || res.SumRealTP != 2 || res.Deferred != 0 {
			t.Fatalf("the slow cascade must adjudicate as a HIT once its learned window elapses, got %+v", res)
		}
		if got, _ := store.ScoreOf("plan-220-b"); got.TP != 2 || got.FP != 0 {
			t.Fatalf("confusion matrix = %+v, want tp=2 fp=0", got)
		}
	})
}

// MUTATION CONTROL for the oracle above. Both arms are only worth their green if reverting to the FIXED
// window turns them red — otherwise they would pass on a codebase that still carries the defect. Here the
// IDENTICAL worlds are scored by the same production Scorer with the window logic mutated back:
//
//	A' — the retired constant restored (WindowFloor=10m, no learning): the 15-minute cascade reads tp=0.
//	B' — the learning removed but the new floor kept: the slow cascade STILL reads tp=0.
//
// A' is the pre-TG-220 behavior verbatim, so this is the diff the finding describes. B' isolates the LEARNED
// half: it proves arm B above is not passing on the floor alone.
func TestScoreDueFixedWindowRecordsAMissMutationControl(t *testing.T) {
	cascade := []verify.ObservedAlert{
		{Host: "n8n01", Rule: "HostDown", Site: "nl"},
		{Host: "litellm01", Rule: "HostDown", Site: "nl"},
	}

	t.Run("A': the retired fixed 10m window scores the 15-minute cascade as a MISS", func(t *testing.T) {
		store := NewMemStore()
		store.Seed(samplePrediction("plan-220-mut-a", "act-220-mut-a"), learnedCommitted)
		clock := learnedCommitted.Add(11 * time.Minute)
		sc := learnedScorer(store, &clock, cascadeAt(&clock, learnedCommitted.Add(15*time.Minute), cascade), nil)
		sc.WindowFloor = 10 * time.Minute // THE MUTATION: the retired TG_FALSIFIABILITY_WINDOW default
		res, err := sc.ScoreDue(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if res.Scored != 1 || res.SumRealTP != 0 {
			t.Fatalf("the mutation must reproduce the DEFECT (a premature score with zero hits), got %+v — "+
				"if this reads tp=2 the oracle above proves nothing", res)
		}
		if got, _ := store.ScoreOf("plan-220-mut-a"); got.TP != 0 || got.FP != 2 {
			t.Fatalf("under the fixed window the real cascade is recorded as two false positives, got %+v", got)
		}
		t.Log("mutation control holds: with the fixed 10m window the same 15-minute cascade adjudicates tp=0 fp=2 — a manufactured miss")
	})

	t.Run("B': the floor without the learning still misses the slow cascade", func(t *testing.T) {
		store := NewMemStore()
		store.Seed(samplePrediction("plan-220-mut-b", "act-220-mut-b"), learnedCommitted)
		clock := learnedCommitted.Add(16 * time.Minute)
		// THE MUTATION: latency seam removed, so no window is ever learned — only the 900s floor remains.
		sc := learnedScorer(store, &clock, cascadeAt(&clock, learnedCommitted.Add(25*time.Minute), cascade), nil)
		res, err := sc.ScoreDue(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if res.Scored != 1 || res.SumRealTP != 0 || res.Deferred != 0 {
			t.Fatalf("without learning, the floor alone scores the slow cascade early and misses it, got %+v — "+
				"if this defers, arm B above is passing on the floor rather than on the learned window", res)
		}
		t.Log("mutation control holds: the learned window, not the floor, is what saves the 25-minute cascade")
	})
}

// THE INV-22 TRIPWIRE UNDER A WIDER WINDOW (the constraint on this whole change). Widening the observation
// window is only legitimate if it recovers REAL cascade signal. If it instead swept in more ambient noise,
// the degree-preserving shuffled control would match at the same rate and control_ratio would rise in
// lockstep — the change would be laundering noise, not measuring topology.
//
// The SAME world is scored twice: narrow (the retired fixed window) and wide (learned). The assertion is
// directional — the control's hits and the control ratio must NOT rise as the window widens, while the real
// prediction's hits do.
func TestLearnedWindowDoesNotRaiseTheShuffledControlMatchRate(t *testing.T) {
	// A slow cascade landing ONLY on the hosts the real topology named. web09 (the shuffled control) is not
	// part of it and never alerts.
	cascade := []verify.ObservedAlert{
		{Host: "n8n01", Rule: "HostDown", Site: "nl"},
		{Host: "litellm01", Rule: "HostDown", Site: "nl"},
	}
	fireAt := learnedCommitted.Add(25 * time.Minute)
	lat := observedLatency(map[CascadeEdge][]time.Duration{
		{Primary: "pve01", Dependent: "n8n01"}: {900 * time.Second},
	})

	// NARROW arm — the retired fixed 10m window, scored before the cascade manifests.
	narrowStore := NewMemStore()
	narrowStore.Seed(samplePrediction("plan-220-ctrl-narrow", "act-220-ctrl-narrow"), learnedCommitted)
	narrowClock := learnedCommitted.Add(11 * time.Minute)
	narrowSc := learnedScorer(narrowStore, &narrowClock, cascadeAt(&narrowClock, fireAt, cascade), nil)
	narrowSc.WindowFloor = 10 * time.Minute
	narrow, err := narrowSc.ScoreDue(context.Background())
	if err != nil {
		t.Fatal(err)
	}

	// WIDE arm — the learned 30m window over the identical world.
	wideStore := NewMemStore()
	wideStore.Seed(samplePrediction("plan-220-ctrl-wide", "act-220-ctrl-wide"), learnedCommitted)
	wideClock := learnedCommitted.Add(31 * time.Minute)
	wideSc := learnedScorer(wideStore, &wideClock, cascadeAt(&wideClock, fireAt, cascade), lat)
	wide, err := wideSc.ScoreDue(context.Background())
	if err != nil {
		t.Fatal(err)
	}

	if narrow.Scored != 1 || wide.Scored != 1 {
		t.Fatalf("both arms must actually score (narrow=%+v wide=%+v)", narrow, wide)
	}
	// The window recovered REAL signal...
	if !(wide.SumRealTP > narrow.SumRealTP) {
		t.Fatalf("the wider window must recover real cascade hits: narrow real_tp=%d wide real_tp=%d",
			narrow.SumRealTP, wide.SumRealTP)
	}
	// ...and the CONTROL did not rise with it. This is the tripwire: if a wider window raised the control's
	// match rate in lockstep, the widening is laundering noise and the change is wrong.
	if wide.SumControlTP > narrow.SumControlTP {
		t.Fatalf("the shuffled control's hits ROSE with the window (narrow control_tp=%d wide control_tp=%d) — "+
			"a wider window that helps the random control equally is measuring noise, not topology",
			narrow.SumControlTP, wide.SumControlTP)
	}
	narrowRatio, wideRatio := ControlRatio(narrow.SumRealTP, narrow.SumControlTP), ControlRatio(wide.SumRealTP, wide.SumControlTP)
	if wideRatio > narrowRatio {
		t.Fatalf("control_ratio rose with the window (%.3f -> %.3f) — INV-22 says the widening added no signal", narrowRatio, wideRatio)
	}
	w := wideStore.Windows()
	if len(w) != 1 || !w[0].Falsifiable || w[0].ControlRatio > predict.ControlRatioCeiling {
		t.Fatalf("the widened window must record a FALSIFIABLE cascade-stats row (ratio <= %.2f), got %+v",
			predict.ControlRatioCeiling, w)
	}
	t.Logf("INV-22 holds under widening: real_tp %d -> %d while control_tp stayed %d, control_ratio %.3f -> %.3f",
		narrow.SumRealTP, wide.SumRealTP, wide.SumControlTP, narrowRatio, wideRatio)
}

// MUTATION CONTROL for the control-ratio assertion above: it is only load-bearing if a world in which the
// wider window DOES help the control equally would be caught. Same learned 30m window, but the late
// observation lands on the control host too — control_ratio climbs past the INV-22 ceiling and the scorer
// records a NON-falsifiable window. An assertion that could not fail is not a tripwire.
func TestLearnedWindowStillTripsINV22WhenTheControlMatchesMutationControl(t *testing.T) {
	store := NewMemStore()
	store.Seed(samplePrediction("plan-220-ctrl-armed", "act-220-ctrl-armed"), learnedCommitted)
	noisy := []verify.ObservedAlert{
		{Host: "n8n01", Rule: "HostDown", Site: "nl"}, // real: 1 hit
		{Host: "web09", Rule: "HostDown", Site: "nl"}, // THE CONTROL host: the widening helped it too
	}
	lat := observedLatency(map[CascadeEdge][]time.Duration{
		{Primary: "pve01", Dependent: "n8n01"}: {900 * time.Second},
	})
	clock := learnedCommitted.Add(31 * time.Minute)
	sc := learnedScorer(store, &clock, cascadeAt(&clock, learnedCommitted.Add(25*time.Minute), noisy), lat)
	res, err := sc.ScoreDue(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if res.SumControlTP != 1 {
		t.Fatalf("the control's hit must survive into the confusion matrix, got %+v", res)
	}
	if ratio := ControlRatio(res.SumRealTP, res.SumControlTP); ratio <= predict.ControlRatioCeiling {
		t.Fatalf("control_ratio = %.3f — a window that helps the control as much as the real prediction MUST "+
			"breach the %.2f ceiling, or the assertion in the test above cannot fail", ratio, predict.ControlRatioCeiling)
	}
	if w := store.Windows(); len(w) != 1 || w[0].Falsifiable {
		t.Fatalf("a window the control matched must be recorded NON-falsifiable, got %+v", w)
	}
	t.Log("tripwire armed: a widened window that lifts the control in lockstep still records control_ratio > 0.5 and falsifiable=false")
}

// FAIL-SAFE DIRECTION. An unreadable durable record must never SHORTEN the window: it falls back to the
// 900s floor, exactly as an edge with no observations does. A database blip cannot manufacture misses.
func TestScoreDueUnreadableLatencyFallsBackToTheFloorNeverShorter(t *testing.T) {
	store := NewMemStore()
	store.Seed(samplePrediction("plan-220-failsafe", "act-220-failsafe"), learnedCommitted)
	clock := learnedCommitted.Add(11 * time.Minute) // past the retired 10m constant, inside the 900s floor
	sc := learnedScorer(store, &clock, cascadeAt(&clock, learnedCommitted.Add(15*time.Minute), nil), nil)
	sc.Latency = func(context.Context, []string, time.Time) (map[CascadeEdge][]time.Duration, bool) {
		return nil, false // the durable record could not be read
	}
	if res, err := sc.ScoreDue(context.Background()); err != nil || res.Scored != 0 {
		t.Fatalf("an unreadable latency record must leave the FLOOR in force (nothing scored at +11m), got %+v %v", res, err)
	}
	// ...and it must not become an error the worker loop retries forever, nor a skip: the row is simply not due.
	clock = learnedCommitted.Add(16 * time.Minute)
	if res, err := sc.ScoreDue(context.Background()); err != nil || res.Scored != 1 {
		t.Fatalf("past the floor the prediction scores normally on the fallback path, got %+v %v", res, err)
	}
}

// A widened window may DEFER a prediction; it may never STRAND one. A pathological latency observation (the
// unbounded case the predecessor shipped) is clamped to WindowCap, so every prediction is adjudicated
// eventually.
func TestScoreDueLearnedWindowIsCappedSoNoPredictionIsStranded(t *testing.T) {
	store := NewMemStore()
	store.Seed(samplePrediction("plan-220-cap", "act-220-cap"), learnedCommitted)
	clock := learnedCommitted
	lat := observedLatency(map[CascadeEdge][]time.Duration{
		{Primary: "pve01", Dependent: "n8n01"}: {12 * time.Hour}, // one absurd reading
	})
	sc := learnedScorer(store, &clock, cascadeAt(&clock, learnedCommitted, nil), lat)

	clock = learnedCommitted.Add(DefaultWindowCap - time.Minute)
	res, err := sc.ScoreDue(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if res.Deferred != 1 || res.WidestWindow != DefaultWindowCap {
		t.Fatalf("the outlier must be clamped to the cap and defer, got %+v", res)
	}
	clock = learnedCommitted.Add(DefaultWindowCap + time.Minute)
	if res, err := sc.ScoreDue(context.Background()); err != nil || res.Scored != 1 {
		t.Fatalf("past the cap the prediction MUST score — an uncapped window strands the row forever: %+v %v", res, err)
	}
}

// The scorer asks the durable record only about the target hosts the due batch actually claims cascades from,
// deterministically ordered — a bounded, replay-stable query input rather than an estate-wide scan.
func TestScoreDueQueriesLatencyOnlyForTheDueBatchPrimaries(t *testing.T) {
	store := NewMemStore()
	a := samplePrediction("plan-220-q1", "act-220-q1")
	b := samplePrediction("plan-220-q2", "act-220-q2")
	b.Prediction.TargetHost = "aaa01"
	store.Seed(a, learnedCommitted)
	store.Seed(b, learnedCommitted)
	clock := learnedCommitted.Add(time.Hour)
	var gotPrimaries []string
	var gotSince time.Time
	sc := learnedScorer(store, &clock, cascadeAt(&clock, learnedCommitted, nil), nil)
	sc.LatencyLookback = 48 * time.Hour
	sc.Latency = func(_ context.Context, primaries []string, since time.Time) (map[CascadeEdge][]time.Duration, bool) {
		gotPrimaries, gotSince = primaries, since
		return nil, true
	}
	if _, err := sc.ScoreDue(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(gotPrimaries) != 2 || gotPrimaries[0] != "aaa01" || gotPrimaries[1] != "pve01" {
		t.Fatalf("latency query primaries = %v, want the due batch's distinct target hosts, sorted", gotPrimaries)
	}
	if want := clock.Add(-48 * time.Hour); !gotSince.Equal(want) {
		t.Fatalf("latency lookback horizon = %s, want %s", gotSince, want)
	}
}
