package wiring

// THE ALARM ITSELF (TG-271).
//
// The per-read observer is only half the gate; what an operator actually needs is the register turning
// "every read failed" into a REPORTED state. These assert the end of that chain — that a hostdiag lane
// which is bound, running, and producing nothing reports STARVED and appears in the findings, rather than
// looking indistinguishable from an estate where nothing has alerted.

import (
	"testing"
	"time"
)

func hostDiagState(t *testing.T, findings []YieldFinding) (YieldFinding, bool) {
	t.Helper()
	for _, f := range findings {
		if f.Seam == SeamHostDiag {
			return f, true
		}
	}
	return YieldFinding{}, false
}

// KILLING MUTATION: classify offered>0 && produced==0 as anything but Starved. RED — this is the exact
// production state (every read attempted, none returning host output) and it must be the alarm.
func TestAHostDiagLaneThatReadsAndReturnsNothingIsStarved(t *testing.T) {
	now := time.Now().UTC()
	r := NewYieldRegister()
	for i := 0; i < 12; i++ {
		r.Observe(SeamHostDiag, 1, 0, now) // twelve reads attempted, nothing produced
	}
	findings, _ := r.Report(now)
	f, ok := hostDiagState(t, findings)
	if !ok {
		t.Fatal("a hostdiag lane that produced nothing across 12 reads is ABSENT from the findings — " +
			"which is exactly the silence this seam exists to break")
	}
	if f.State != YieldStarved {
		t.Fatalf("state=%v, want starved (offered=%d produced=%d)", f.State, f.Offered, f.Produced)
	}
}

// The control that stops the alarm becoming wallpaper: a lane that IS returning host output must not be
// reported. An alarm that fires on a healthy lane is an alarm that gets muted.
func TestAWorkingHostDiagLaneIsNotReported(t *testing.T) {
	now := time.Now().UTC()
	r := NewYieldRegister()
	r.Observe(SeamHostDiag, 3, 3, now)
	findings, _ := r.Report(now)
	if f, ok := hostDiagState(t, findings); ok {
		t.Fatalf("a flowing hostdiag lane was reported as %v — the alarm does not discriminate", f.State)
	}
}

// KILLING MUTATION: default an un-exercised seam to flowing/idle instead of UNOBSERVED. RED — "nobody has
// ever measured this" must never render as "this is fine", which is the property that makes the whole
// register trustworthy rather than decorative.
func TestAHostDiagLaneNobodyEverObservedIsUnobservedNotFine(t *testing.T) {
	now := time.Now().UTC()
	findings, _ := NewYieldRegister().Report(now)
	f, ok := hostDiagState(t, findings)
	if !ok {
		t.Fatal("an un-observed hostdiag seam produced no finding — a register instrumented at zero seams " +
			"would then report a clean estate, which is the failure it exists to detect")
	}
	if f.State != YieldUnobserved {
		t.Fatalf("state=%v, want unobserved", f.State)
	}
}

// A PARTIALLY working lane is a disagreement, not an alarm — some hosts covered by known_hosts, some not,
// which is precisely the 16-of-38 state production was in. It is published (both numbers) rather than
// alarmed, so the operator can see the gap without the seam crying wolf on a lane that half-works.
func TestAPartiallyCoveredLaneIsPublishedNotAlarmed(t *testing.T) {
	now := time.Now().UTC()
	r := NewYieldRegister()
	r.Observe(SeamHostDiag, 38, 16, now) // the measured production ratio
	findings, samples := r.Report(now)
	if f, ok := hostDiagState(t, findings); ok {
		t.Fatalf("a half-working lane was alarmed as %v — filtering is normal and this would train the "+
			"operator to ignore the seam", f.State)
	}
	var offered, produced float64
	for _, s := range samples {
		if s.Labels["seam"] != string(SeamHostDiag) {
			continue
		}
		switch s.Name {
		case "tg_wiring_seam_offered_total":
			offered = s.Value
		case "tg_wiring_seam_produced_total":
			produced = s.Value
		}
	}
	if offered != 38 || produced != 16 {
		t.Fatalf("gauges say offered=%v produced=%v, want 38/16 — BOTH numbers must be published or the "+
			"22 hosts the agent cannot read are invisible", offered, produced)
	}
}
