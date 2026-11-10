package ingest

import (
	"testing"
	"time"
)

// THE ORACLE THAT DECIDES WHETHER THIS FEATURE IS SAFE TO SHIP.
//
// Suppression drops alerts. The failure mode is not "too much noise" — it is an incident nobody sees, and a
// benchmark that reports detection of faults it never detected. So the load-bearing test is not "does it
// suppress repeats" (easy, and any wrong design passes it) but "does a RE-INJECTION after a recovery still
// get through" — the case a 24h time-window design gets wrong while looking correct.

var t0 = time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC)

// TestARepeatOfAnOpenIncidentIsSuppressed — the feature actually doing its job. Measured on live traffic:
// 73.3% of organic alerts are repeats (400 alerts / 107 keys over 7 days, worst key 25 fires).
func TestARepeatOfAnOpenIncidentIsSuppressed(t *testing.T) {
	h := []Fire{{At: t0}, {At: t0.Add(5 * time.Minute)}}
	d := DecideSuppress(h, t0.Add(10*time.Minute))
	if !d.Suppress {
		t.Fatalf("a third fire of a still-open incident was ADMITTED (%s) — the repeat traffic this exists "+
			"to remove is 73%% of organic alerts", d.Reason)
	}
	if !d.OpenSince.Equal(t0) {
		t.Errorf("OpenSince = %v, want the FIRST fire of the open run %v — an operator reviewing a "+
			"suppression needs to see when the incident began, not when the last repeat landed", d.OpenSince, t0)
	}
}

// ★ TestAReInjectionAfterRecoveryIsALWAYSAdmitted is THE control this design exists for.
//
// The fault harness re-injects the same (host, rule) many times a day — 34 fires of Service-up/down on one
// guest in 24h, and on the six noisiest hosts EVERY alert arrived inside an injected-fault window. Under a
// time-windowed dedup the 2nd..Nth injection's FIRST alert is dropped as a duplicate, those faults score as
// UNDETECTED, and A1 collapses while the change reads as a pure noise win.
//
// A recovery between two fires means two DIFFERENT incidents. Both must be admitted.
func TestAReInjectionAfterRecoveryIsAlwaysAdmitted(t *testing.T) {
	h := []Fire{
		{At: t0}, // fault injected, alert raised
		{At: t0.Add(2 * time.Minute), Recovered: true}, // healed, alert cleared
	}
	// The harness injects the SAME fault on the SAME host again, well inside any 24h window.
	d := DecideSuppress(h, t0.Add(4*time.Minute))
	if d.Suppress {
		t.Fatalf("a re-injection AFTER a recovery was suppressed (%s). This is the defect that collapses "+
			"A1: the second fault's first alert never reaches ingest_alert, the detection-recall query finds "+
			"no row to correlate, and the fault is scored as UNDETECTED — while the suppression looks like a "+
			"noise improvement", d.Reason)
	}
}

// TestManyInjectRecoverCyclesAreEveryOneAdmitted — the harness's actual shape, not a single round trip. If
// only the first cycle survived, A1 would degrade with every additional injection on the same host.
func TestManyInjectRecoverCyclesAreEveryOneAdmitted(t *testing.T) {
	var h []Fire
	at := t0
	for cycle := 0; cycle < 8; cycle++ {
		d := DecideSuppress(h, at)
		if d.Suppress {
			t.Fatalf("cycle %d: the first alert of a fresh injection was suppressed (%s) — that fault "+
				"would score as UNDETECTED", cycle, d.Reason)
		}
		h = append(h, Fire{At: at})                                   // raised
		h = append(h, Fire{At: at.Add(time.Minute), Recovered: true}) // cleared
		at = at.Add(30 * time.Minute)
	}
}

// TestAStaleOpenIncidentAdmitsRatherThanSuppressingForever — a recovery that never arrives is a monitoring
// gap, not a quiet estate. Without this bound the key would be suppressed indefinitely and the noise filter
// would become a blind spot.
func TestAStaleOpenIncidentAdmitsRatherThanSuppressingForever(t *testing.T) {
	h := []Fire{{At: t0}}
	if d := DecideSuppress(h, t0.Add(MaxOpenIncident+time.Minute)); d.Suppress {
		t.Errorf("an incident open past the staleness bound with no recovery is still suppressing (%s) — "+
			"a lost recovery must not silence a key forever", d.Reason)
	}
	// ...and just inside the bound it must still suppress, or the bound is doing nothing.
	if d := DecideSuppress(h, t0.Add(MaxOpenIncident-time.Minute)); !d.Suppress {
		t.Errorf("a repeat well inside the staleness bound was admitted (%s) — if this never suppresses, "+
			"the test above passes for the wrong reason", d.Reason)
	}
}

// TestAnUnseenKeyIsAdmitted — the ordinary first sighting.
func TestAnUnseenKeyIsAdmitted(t *testing.T) {
	if d := DecideSuppress(nil, t0); d.Suppress {
		t.Errorf("a key with no history was suppressed (%s)", d.Reason)
	}
}

// TestEveryDecisionCarriesItsReason — a suppression that records only its verdict cannot be reviewed. This
// project has already been unable to answer whether 140 novelty polls were right, for exactly that reason.
func TestEveryDecisionCarriesItsReason(t *testing.T) {
	cases := map[string][]Fire{
		"unseen":      nil,
		"open repeat": {{At: t0}},
		"recovered":   {{At: t0}, {At: t0.Add(time.Minute), Recovered: true}},
	}
	for name, h := range cases {
		if d := DecideSuppress(h, t0.Add(2*time.Minute)); d.Reason == "" {
			t.Errorf("%s: decision carries no reason — a dropped alert must be explainable after the fact", name)
		}
	}
}
