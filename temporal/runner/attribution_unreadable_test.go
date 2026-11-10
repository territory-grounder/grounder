package runner

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/territory-grounder/grounder/adapters/actorevidence"
	"github.com/territory-grounder/grounder/core/attribution"
	"github.com/territory-grounder/grounder/core/risk"
	"github.com/territory-grounder/grounder/core/safety"
)

// TG-652 (owner-ruled 2026-11-07, "retry briefly, then hold"). On 2026-11-07 the PVE task-log read for a
// guest an admin had just shut down got Proxmox 596; the failure counted as "no evidence", the stop read as a
// crash, and the start-guest was auto-approved. Now a start-guest whose lifecycle read still fails escalates,
// and the fallback approver holds it.

var errPVE596 = errors.New("pve: GET /api2/json/nodes/pve04/tasks → 596: ")

func attributeWith(t *testing.T, faultClass string, readers ...fakeActorReader) AttributeResult {
	t.Helper()
	rs := make([]actorevidence.Reader, 0, len(readers))
	for _, r := range readers {
		rs = append(rs, r)
	}
	deps := Deps{}
	attributeDeps(t, rs...)(&deps)
	res, err := (&Activities{D: deps}).AttributeActivity(context.Background(), AttributeInput{Host: "web01", FaultClass: faultClass})
	if err != nil {
		t.Fatalf("a reader failure is advisory; the activity must not fail: %v", err)
	}
	return res
}

func TestStartGuestWithAnUnreadableLifecycleLogEscalates(t *testing.T) {
	res := attributeWith(t, "start-guest", fakeActorReader{domain: "pve", err: errPVE596})
	if !res.Escalate {
		t.Fatalf("a start-guest whose PVE task-log read failed must escalate (hold), got %+v", res)
	}
}

func TestAnUnreadableLifecycleLogLeavesOtherClassesAsBefore(t *testing.T) {
	// Only start-guest depends on the PVE task log to tell a deliberate stop from a crash.
	if res := attributeWith(t, "start-service", fakeActorReader{domain: "pve", err: errPVE596}); res.Escalate || res.StandDown || res.Security {
		t.Fatalf("a start-service must keep today's advisory behaviour on a failed PVE read, got %+v", res)
	}
	// And another domain failing does not take away the start-guest's lifecycle witness.
	if res := attributeWith(t, "start-guest", fakeActorReader{domain: "journal", err: errors.New("journal: unreachable")}); res.Escalate {
		t.Fatalf("a failed NON-lifecycle reader must not hold a start-guest, got %+v", res)
	}
}

func TestAStrongerDispositionWinsOverAnUnreadableLog(t *testing.T) {
	// One PVE path failed, another answered with the admin's stop: that is a stand-down (coordinate), and it
	// stays a stand-down rather than becoming a generic escalation.
	admin := fakeActorReader{domain: "pve", ev: []attribution.Evidence{
		{Domain: "pve", Actor: "root@pam", ActionKind: "vzstop", Target: "web01", ObservedAt: time.Now().Add(-2 * time.Minute), Ref: "UPID:9", Covered: true},
	}}
	res := attributeWith(t, "start-guest", fakeActorReader{domain: "pve", err: errPVE596}, admin)
	if !res.StandDown || res.Escalate {
		t.Fatalf("an admin stop seen by another reader must stay a stand-down, got %+v", res)
	}
}

func TestAReadableCrashStillHeals(t *testing.T) {
	// The vacuity control: a readable log with no lifecycle actor (an organic crash) must NOT escalate.
	covered := fakeActorReader{domain: "pve", ev: []attribution.Evidence{attribution.CoverageMarker("pve", "web01", time.Now())}}
	if res := attributeWith(t, "start-guest", covered); res.Escalate || res.StandDown || res.Security {
		t.Fatalf("a readable, actor-free log is a crash and must heal as before, got %+v", res)
	}
}

// End to end through the approver: the escalation becomes a HOLD even when the only other poll reason is the
// novelty the approver is allowed to clear.
func TestAnUnreadableLifecycleLogIsHeldByTheFallbackApprover(t *testing.T) {
	gi := risk.GatedInput{OpClass: "start-guest", Reversible: risk.Reversible, HasPrediction: true, NovelIncident: true}
	if r := resolvePollAutonomously(gi, risk.Classify(gi)); !r.Resolved || !r.Approve {
		t.Fatalf("vacuity control: a novel readable start-guest is acted on, got %+v", r)
	}
	gi.AttributionEscalate = true
	d := risk.Classify(gi)
	if d.Band != safety.BandPollPause {
		t.Fatalf("an attribution escalation must poll, got %s", d.Band)
	}
	if r := resolvePollAutonomously(gi, d); !r.Resolved || r.Approve {
		t.Fatalf("the fallback approver must HOLD an unreadable-attribution start-guest, got %+v", r)
	}
}
