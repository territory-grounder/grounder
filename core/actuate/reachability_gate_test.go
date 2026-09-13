package actuate

import (
	"context"
	"strings"
	"testing"

	"github.com/territory-grounder/grounder/core/audit"
	"github.com/territory-grounder/grounder/core/manifest"
	"github.com/territory-grounder/grounder/core/safety"
)

// probingActuator is a fakeActuator that also proves (or refuses) its transport — the gate-4h3 capability.
type probingActuator struct {
	fakeActuator
	reachable bool
	detail    string
	probes    int
}

func (a *probingActuator) ProbeReachable(_ context.Context, _ string) (bool, string) {
	a.probes++
	return a.reachable, a.detail
}

// TG-81 b4: an unreachable target refuses BEFORE the effect, with the probe's detail; a reachable one
// executes; a leaf without the capability is untouched pass-through (every other test in this package
// runs the plain fakeActuator and pins that). KILLING MUTATION: drop the !reachable refusal in gate
// 4h3 — the execs assertion fails (the effect fired against a dead transport).
func TestPreFlightReachabilityAbortsBeforeTouchingAnything(t *testing.T) {
	act := &probingActuator{reachable: false, detail: "tcp dial web01:22: connection refused"}
	i := actuatingInterceptor(safety.NewActuatingChokepoint(), act, audit.NewLedger())

	out, err := i.Do(context.Background(), goodRequest(t))
	if err != nil {
		t.Fatalf("a refusal must be quiet: %v", err)
	}
	if !out.Refused || act.execs != 0 {
		t.Fatalf("unreachable must refuse pre-effect: refused=%v execs=%d", out.Refused, act.execs)
	}
	if !strings.Contains(out.Reason, "reachability probe failed") || !strings.Contains(out.Reason, "connection refused") {
		t.Fatalf("the refusal must name the gate and carry the dial detail, got %q", out.Reason)
	}
	if act.probes != 1 {
		t.Fatalf("exactly one probe, got %d", act.probes)
	}

	act2 := &probingActuator{reachable: true}
	i2 := actuatingInterceptor(safety.NewActuatingChokepoint(), act2, audit.NewLedger())
	out2, err := i2.Do(context.Background(), goodRequest(t))
	if err != nil {
		t.Fatal(err)
	}
	if out2.Refused || act2.execs != 1 || act2.probes != 1 {
		t.Fatalf("reachable must execute once after one probe: %+v execs=%d probes=%d", out2, act2.execs, act2.probes)
	}
}

// STRICT per-target validation: a probing leaf refuses an EMPTY target outright — nothing to probe is a
// refusal, never a pass (the same no-exemption posture as the admission bucket).
func TestProbingLeafRefusesAnEmptyTarget(t *testing.T) {
	act := &probingActuator{reachable: true}
	i := actuatingInterceptor(safety.NewActuatingChokepoint(), act, audit.NewLedger())

	// Sealed THROUGH manifest.New — mutating goodRequest's sealed manifest would (rightly) trip the
	// action_id seal upstream; the empty-target case this gate defends against is one sealed that way.
	m, err := manifest.New(manifest.Action{Target: "   ", OpClass: "restart-service", Op: "restart", Reversible: true}, safety.BandAuto, "plan#1", "pred#1")
	if err != nil {
		t.Fatalf("a whitespace target seals today (nothing upstream refuses it) — that is why gate 4h3 must: %v", err)
	}
	r := goodRequest(t)
	r.Manifest = m
	out, err := i.Do(context.Background(), r)
	if err != nil {
		t.Fatal(err)
	}
	if !out.Refused || act.execs != 0 || act.probes != 0 {
		t.Fatalf("an empty target must refuse before probing or executing: %+v execs=%d probes=%d", out, act.execs, act.probes)
	}
	if !strings.Contains(out.Reason, "probeable target") {
		t.Fatalf("the refusal must say why, got %q", out.Reason)
	}
}
