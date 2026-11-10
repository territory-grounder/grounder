package runner

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/territory-grounder/grounder/core/attribution"
	"github.com/territory-grounder/grounder/core/audit"
	"github.com/territory-grounder/grounder/core/holds"
	"github.com/territory-grounder/grounder/core/risk"
	"github.com/territory-grounder/grounder/core/safety"
)

// THE INTEGRATED LOOKBACK-EXPIRY DRILL (TG-667 + TG-668, owner prompt §2.E). The actor-evidence window is 30
// minutes: an operator's deliberate stop is seen by the PVE task log, the fallback approver HOLDS (a person made
// this change), and the hold is written to hold_decision. Forty minutes later the alert re-fires. The stop is now
// outside the lookback, so attribution reads UNATTRIBUTABLE — the reading that heals — and before TG-667 the
// start-guest went ahead over the operator's change. The prior-hold rule must carry the first decision: the
// re-detection polls prior-hold-still-open and the fallback approver holds again. The CONTROL (no hold memory)
// heals, which proves the test is measuring the memory and not a stale attribution.
//
// Runs the REAL activities end to end — AttributeActivity → ClassifyActivity → the fallback approver →
// RecordVoteActivity (the hold write) → the next session's ClassifyActivity (the hold read) — with the workflow's
// own glue reproduced in the test: the ClassifyInput fields the workflow copies from AttributeResult, and the
// RecordVoteInput the poll stage records for an autonomous resolution.

// memHoldMemory is the in-memory twin of db.HoldStore for the two seams the activities use.
type memHoldMemory struct{ rows []holds.HoldDecision }

func (m *memHoldMemory) Record(_ context.Context, h holds.HoldDecision) error {
	m.rows = append(m.rows, h)
	return nil
}

func (m *memHoldMemory) OpenFor(_ context.Context, host, rule string, now time.Time) (holds.HoldDecision, bool, error) {
	for i := len(m.rows) - 1; i >= 0; i-- {
		h := m.rows[i]
		if h.Host == host && h.AlertRule == rule && h.Open(now) && holds.Sticky(h.ReasonKey) {
			return h, true, nil
		}
	}
	return holds.HoldDecision{}, false, nil
}

func TestAnIntentionalStopStaysHeldAfterTheActorEvidenceLookbackExpires(t *testing.T) {
	ctx := context.Background()
	now := time.Now()
	const host, rule, class = "web01", "Devices-up/down", "start-guest"

	// The operator's stop, as the PVE task log records it. root@pam is sanctioned in the test config
	// (attributeDeps), so the reading is attributed-authorized ⇒ stand-down ⇒ HOLD.
	stopAt := func(age time.Duration) fakeActorReader {
		return fakeActorReader{domain: "pve", ev: []attribution.Evidence{
			{Domain: "pve", Actor: "root@pam", ActionKind: "vzstop", Target: host, ObservedAt: now.Add(-age), Ref: "UPID:op", Covered: true},
		}}
	}
	build := func(pve fakeActorReader, mem *memHoldMemory) *Activities {
		deps := Deps{Ledger: audit.NewLedger(), AutonomousResolver: true, PredictionEligible: func(string) bool { return true }, HoldRedetectWindow: holds.DefaultRedetectWindow}
		attributeDeps(t, pve)(&deps)
		if mem != nil {
			deps.Holds = mem
			deps.PriorHold = mem.OpenFor
		}
		return &Activities{D: deps}
	}
	// The workflow's glue (workflow.go: the attribute → classify hand-off), reproduced verbatim in shape.
	classify := func(a *Activities, attr AttributeResult, ref string) risk.Decision {
		t.Helper()
		dec, err := a.ClassifyActivity(ctx, ClassifyInput{
			ExternalRef: ref, ActionID: "a-start", PlanHash: "p", RiskLevel: "low", OpClass: class, Op: "start", Reversible: true,
			Host: host, IncidentHost: host, AlertRule: rule,
			AttributionStandDown: attr.StandDown, AttributionSecurity: attr.Security, AttributionEscalate: attr.Escalate,
			Attribution: attr.Finding.Taxonomy.String(), AttributionIndeterminate: strings.Join(attr.Finding.IndeterminateDomains, ","),
		})
		if err != nil {
			t.Fatal(err)
		}
		return dec
	}

	// ---- t0: the stop is 5 minutes old, inside the window. Attributed to a person; held; remembered. ----
	mem := &memHoldMemory{}
	a0 := build(stopAt(5*time.Minute), mem)
	attr0, err := a0.AttributeActivity(ctx, AttributeInput{Host: host, FaultClass: class})
	if err != nil {
		t.Fatal(err)
	}
	if !attr0.StandDown || attr0.Finding.Taxonomy != attribution.AttributedAuthorized {
		t.Fatalf("t0: an operator's stop inside the window must read attributed-authorized / stand-down, got %+v", attr0)
	}
	dec0 := classify(a0, attr0, "INC-t0")
	if dec0.Band != safety.BandPollPause || dec0.Signals["poll_reason"] != "actor-attributed-authorized" {
		t.Fatalf("t0: the stand-down must poll on actor-attributed-authorized, got %s %v", dec0.Band, dec0.Signals)
	}
	r0 := autonomousResolutionOf(dec0)
	if !r0.Resolved || r0.Approve {
		t.Fatalf("t0: the fallback approver must HOLD a person's change, got %+v", r0)
	}
	// The poll stage's record for an autonomous resolution (workflow.go record closure), with the hold payload.
	if _, err := a0.RecordVoteActivity(ctx, RecordVoteInput{
		Decision: autonomousLedgerDecision(r0), ActionID: "a-start", ExternalRef: "INC-t0", Voter: AutonomousApproverPrincipal,
		Host: host, AlertRule: rule, OpClass: class, Why: r0.Why, ActorAttribution: attr0.Finding.Taxonomy.String(),
	}); err != nil {
		t.Fatal(err)
	}
	if len(mem.rows) != 1 || mem.rows[0].ReasonKey != "actor-attributed-authorized" || !holds.Sticky(mem.rows[0].ReasonKey) {
		t.Fatalf("t0: the hold must be remembered under its sticky reason, got %+v", mem.rows)
	}

	// ---- t0+40m: the same stop is now outside the 30-minute lookback. Attribution has forgotten it. ----
	a1 := build(stopAt(40*time.Minute), mem)
	attr1, err := a1.AttributeActivity(ctx, AttributeInput{Host: host, FaultClass: class})
	if err != nil {
		t.Fatal(err)
	}
	if attr1.StandDown || attr1.Finding.Taxonomy != attribution.Unattributable {
		t.Fatalf("t0+40m: the stop is outside the window and must read unattributable (the reading that HEALS), got %+v", attr1)
	}
	dec1 := classify(a1, attr1, "INC-t1")
	if dec1.Band != safety.BandPollPause || dec1.Signals["poll_reason"] != holds.ReasonPriorHold {
		t.Fatalf("t0+40m: the prior-hold rule must carry the operator's stop past the lookback, got %s %v", dec1.Band, dec1.Signals)
	}
	if r1 := autonomousResolutionOf(dec1); !r1.Resolved || r1.Approve {
		t.Fatalf("t0+40m: the fallback approver must HOLD on the remembered decision, got %+v", r1)
	}
	if detail := dec1.Signals["prior_hold"]; !strings.Contains(detail, "INC-t0") {
		t.Fatalf("the re-detection must name the prior hold it honours, got %q", detail)
	}

	// ---- CONTROL: the same stale attribution with NO hold memory heals — so the hold above is the memory's. ----
	c := build(stopAt(40*time.Minute), nil)
	attrC, err := c.AttributeActivity(ctx, AttributeInput{Host: host, FaultClass: class})
	if err != nil {
		t.Fatal(err)
	}
	if decC := classify(c, attrC, "INC-ctl"); decC.Band != safety.BandAuto {
		t.Fatalf("control: without hold memory the forgotten stop heals (AUTO), got %s %v — the test would not be measuring the memory", decC.Band, decC.Signals)
	}

	// ---- And a NON-sticky prior hold does not carry: a prose mismatch held once is judged afresh. ----
	prose := &memHoldMemory{rows: []holds.HoldDecision{{ExternalRef: "INC-prose", Host: host, AlertRule: rule,
		ReasonKey: "rationale-names-a-different-host", DecidedAt: now.Add(-time.Hour), ExpiresAt: now.Add(23 * time.Hour)}}}
	p := build(stopAt(40*time.Minute), prose)
	attrP, _ := p.AttributeActivity(ctx, AttributeInput{Host: host, FaultClass: class})
	if decP := classify(p, attrP, "INC-p"); decP.Band != safety.BandAuto {
		t.Fatalf("a non-sticky prior hold is not carried, got %s %v", decP.Band, decP.Signals)
	}
}
