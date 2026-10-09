package actuate

import (
	"context"
	"strings"
	"testing"

	"github.com/territory-grounder/grounder/core/audit"
	"github.com/territory-grounder/grounder/core/breaker"
	"github.com/territory-grounder/grounder/core/credential"
	"github.com/territory-grounder/grounder/core/policy"
	"github.com/territory-grounder/grounder/core/safety"
	"github.com/territory-grounder/grounder/core/territory"
)

// THE GATE DRILL MATRIX (roadmap P4-8).
//
// WHY THIS EXISTS. Measured on the live estate: of the twelve gates in the interception chain, only FOUR had
// ever recorded a refusal (execute 58, policy 10, breaker 1, and verify's non-match verdicts). The other
// EIGHT — admission, never-auto-floor, structure, evidence, territory, verifiability, mode-chokepoint and
// host-match — had refused ZERO times in the system's entire history.
//
// (TG-166 added two more: the actuation-frequency governor and the necessity re-check. Both are drilled here
// from the day they landed, rather than joining the unexercised eight.)
//
// That is not evidence they are broken; it is evidence they are UNEXERCISED. No incident has yet arrived
// carrying a floor-class op, an unacknowledged territory, or absent evidence. But an unexercised control is
// an ASSUMED control, and this project has now twice shipped one that was silently inert: the protected-paths
// CI gate ran on every commit while examining nothing, and SelfTest treated a missing policy authorizer as a
// pass-through in an actuating posture. Both looked fine until something made them prove themselves.
//
// So each gate is drilled here: given a request that violates exactly that gate, the chain must REFUSE and
// the effect leaf must never be reached. Running in CI on every commit is deliberately stronger than the
// one-off production ledger rows the roadmap proposed — a stale row proves a gate worked once, in a build
// nobody is running any more.
//
// SAFE BY CONSTRUCTION: every case uses a fake, read-only actuator, so even a gate that WRONGLY passed
// mutates nothing — the assertion on act.execs is what catches it.

func TestGateDrillMatrix_EveryGateCanRefuse(t *testing.T) {
	// Each case violates exactly ONE gate, having satisfied the ones before it in chain order:
	// admission -> never-auto-floor -> stateful-floor -> structure -> evidence -> territory -> verifiability ->
	// policy -> breaker -> mode-chokepoint -> host-match -> actuation-limit -> baseline -> necessity ->
	// execute -> verify.
	cases := []struct {
		gate         string
		why          string
		expectReason string // when set, the refusal must carry this substring — the gate's OWN words
		build        func(t *testing.T) (*Interceptor, *fakeActuator, Request)
	}{
		{
			gate: "admission",
			why:  "a POLL_PAUSE band with no recorded human approval must not auto-execute (INV-12)",
			build: func(t *testing.T) (*Interceptor, *fakeActuator, Request) {
				act := &fakeActuator{}
				i := wired(safety.NewActuatingChokepoint(), act)
				r := goodRequest(t)
				r.Manifest = pollReversibleManifest(t)
				r.Band = safety.BandPollPause // fresh band demands a vote
				r.Approved = false            // and none is on file
				return i, act, r
			},
		},
		{
			gate: "never-auto-floor",
			why:  "an irreversible/destructive op is refused at the adapter regardless of band or policy (INV-09)",
			build: func(t *testing.T) (*Interceptor, *fakeActuator, Request) {
				act := &fakeActuator{}
				i := wired(safety.NewActuatingChokepoint(), act)
				r := goodRequest(t)
				r.Manifest = floorManifest(t) // a floor-class op
				return i, act, r
			},
		},
		{
			gate:         "stateful-floor",
			why:          "a stateful identity visible ONLY in the params must not execute under a non-voted band (TG-146 A3 — the mariadb-in-params shape)",
			expectReason: "stateful floor",
			build: func(t *testing.T) (*Interceptor, *fakeActuator, Request) {
				act := &fakeActuator{}
				i := wired(safety.NewActuatingChokepoint(), act)
				return i, act, paramsRequest(t, safety.BandAuto, map[string]string{"unit": "mariadb.service"})
			},
		},
		{
			gate: "structure",
			why:  "an action with no committed prediction may not proceed — predict-before-act (INV-10)",
			build: func(t *testing.T) (*Interceptor, *fakeActuator, Request) {
				act := &fakeActuator{}
				i := wired(safety.NewActuatingChokepoint(), act)
				r := goodRequest(t)
				r.Gated = false // no prediction gate produced this
				return i, act, r
			},
		},
		{
			gate: "evidence",
			why:  "a mutating action citing no BOUND orchestrator-captured evidence is inadmissible (INV-11)",
			build: func(t *testing.T) (*Interceptor, *fakeActuator, Request) {
				act := &fakeActuator{}
				i := wired(safety.NewActuatingChokepoint(), act)
				r := goodRequest(t)
				r.Evidence = nil // no evidence at all
				return i, act, r
			},
		},
		{
			gate: "verifiability",
			why:  "an action whose post-state cannot be observed must not execute — no blind mutation (TG-182)",
			// TG-234: the reason must be GATE 4c's OWN wording. A fail-closed backstop now sits at the
			// baseline step (interceptor.go) so a nil observer can never panic the chain — which also
			// means deleting 4c no longer crashes this drill; it refuses via the backstop's DIFFERENT
			// sentence ("nil observer reached the baseline step"). Pinning 4c's sentence keeps its
			// deletion RED: refusing at 4c (before territory/policy work runs) is the gate's real
			// contribution, and the backstop absorbing it silently would un-prove exactly that.
			// RED CONTROL EXECUTED 2026-08-03: 4c deleted → this drill refused via the backstop, no
			// panic, and THIS assertion failed on the wording; restored → green.
			expectReason: "no post-execution observer wired",
			build: func(t *testing.T) (*Interceptor, *fakeActuator, Request) {
				act := &fakeActuator{}
				i := wired(safety.NewActuatingChokepoint(), act)
				r := goodRequest(t)
				r.Observe = nil // no observer wired ⇒ the verdict could not be computed
				return i, act, r
			},
		},
		{
			gate: "policy",
			why:  "a policy DENY is unconditional — no recorded approval lifts it (REQ-1506)",
			build: func(t *testing.T) (*Interceptor, *fakeActuator, Request) {
				act := &fakeActuator{}
				i := NewInterceptor(safety.NewActuatingChokepoint(), act, audit.NewLedger()).
					WithPolicyDecider(&fakeDecider{verdict: policy.VerdictDeny},
						func() policy.Mode { return policy.ModeFullAuto })
				r := goodRequest(t)
				r.Approved = true // even WITH an approval on file, a deny stands
				return i, act, r
			},
		},
		{
			gate:         "authn-compose",
			why:          "a policy-authorized target the operator declared no identity for must refuse at its own authn layer, never reach an effect leaf (spec/016 REQ-1604)",
			expectReason: "authn compose",
			build: func(t *testing.T) (*Interceptor, *fakeActuator, Request) {
				act := &fakeActuator{}
				i := wired(safety.NewActuatingChokepoint(), act).
					WithComposer(func(context.Context, string) (string, error) {
						return "", credential.ErrUnresolved
					})
				return i, act, goodRequest(t)
			},
		},
		{
			gate: "mode-chokepoint",
			why:  "a read-only posture refuses every mutation however it was authorized (INV-09)",
			build: func(t *testing.T) (*Interceptor, *fakeActuator, Request) {
				act := &fakeActuator{}
				i := wired(safety.NewReadOnlyChokepoint(), act) // Shadow
				return i, act, goodRequest(t)
			},
		},
		{
			gate:         "necessity",
			why:          "an action whose fault ALREADY cleared is no longer necessary, however safe it is (TG-166b)",
			expectReason: "NO LONGER NECESSARY",
			build: func(t *testing.T) (*Interceptor, *fakeActuator, Request) {
				act := &fakeActuator{}
				i := wired(safety.NewActuatingChokepoint(), act)
				r := goodRequest(t)
				r.StillFaulted = func(context.Context) (bool, bool) { return false, true } // healed since the investigation
				return i, act, r
			},
		},
		{
			gate:         "actuation-limit",
			why:          "a session that has spent its actuation budget is throttled, not executed (TG-166a)",
			expectReason: RefusalRateLimited,
			build: func(t *testing.T) (*Interceptor, *fakeActuator, Request) {
				act := &fakeActuator{}
				// A budget of ONE, already spent by a prior identical actuation: the SECOND request is
				// individually as admissible as the first, which is the whole point of this gate.
				i := wired(safety.NewActuatingChokepoint(), act).
					WithActuationLimiter(NewActuationLimiter(nil).WithLimits(ActuationLimits{SessionPerWindow: 1, TargetPerWindow: 1}))
				spent := goodRequest(t)
				spent.ExternalRef = "TG-drill"
				if out, err := i.Do(context.Background(), spent); err != nil || !out.Executed {
					t.Fatalf("drill setup: the FIRST actuation must execute so the budget is genuinely spent (%+v %v)", out, err)
				}
				act.execs = 0 // the matrix asserts the leaf is not reached by the REFUSED request
				r := goodRequest(t)
				r.ExternalRef = "TG-drill"
				return i, act, r
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.gate, func(t *testing.T) {
			i, act, r := tc.build(t)
			out, err := i.Do(context.Background(), r)
			if err != nil {
				t.Fatalf("gate %q: the chain failed loud instead of refusing: %v", tc.gate, err)
			}
			if out.Executed {
				t.Fatalf("gate %q DID NOT REFUSE and the action EXECUTED — %s", tc.gate, tc.why)
			}
			if !out.Refused {
				t.Fatalf("gate %q: want a recorded refusal, got %+v — %s", tc.gate, out, tc.why)
			}
			if act.execs != 0 {
				t.Fatalf("gate %q: the effect leaf was reached %d time(s) despite the refusal — %s",
					tc.gate, act.execs, tc.why)
			}
			if out.Reason == "" {
				t.Fatalf("gate %q refused with no reason; an unexplained refusal cannot be audited", tc.gate)
			}
			if tc.expectReason != "" && !strings.Contains(out.Reason, tc.expectReason) {
				t.Fatalf("gate %q refused with %q, want the gate's own wording (…%s…) — a DIFFERENT "+
					"layer absorbed the refusal, so this gate's contribution is no longer proven",
					tc.gate, out.Reason, tc.expectReason)
			}
			t.Logf("gate %-18s REFUSED as required: %s", tc.gate, out.Reason)
		})
	}
}

// The territory gate, drilled separately because it needs a high-stakes manifest whose territory was never
// acknowledged. An action reaching production infrastructure without its operating manual loaded is refused
// (INV-21 territory control).
func TestGateDrill_TerritoryRefusesWithoutAcknowledgement(t *testing.T) {
	act := &fakeActuator{}
	i := wired(safety.NewActuatingChokepoint(), act)
	r := goodRequest(t)
	r.Manifest = k8sManifest(t)                     // a high-stakes territory
	r.Acknowledged = map[territory.Territory]bool{} // nothing acknowledged
	out, err := i.Do(context.Background(), r)
	if err != nil {
		t.Fatalf("failed loud instead of refusing: %v", err)
	}
	if out.Executed || act.execs != 0 {
		t.Fatalf("an unacknowledged high-stakes territory must refuse: %+v execs=%d", out, act.execs)
	}
	t.Logf("gate territory          REFUSED as required: %s", out.Reason)
}

// The breaker, drilled separately: once tripped it forces Shadow, so a subsequent action is refused even
// though every other gate would pass. This is the cross-process kill the canary plan depends on.
func TestGateDrill_TrippedBreakerRefuses(t *testing.T) {
	act := &fakeActuator{}
	cp := safety.NewActuatingChokepoint()
	mb, mberr := safety.NewMutationBreaker(cp, breaker.NewMemStore(), 1, nil)
	if mberr != nil {
		t.Fatalf("arm breaker: %v", mberr)
	}
	i := wired(cp, act).WithMutationBreaker(mb)
	if _, err := mb.Trip(context.Background(), "drill: prove the breaker refuses"); err != nil {
		t.Fatalf("trip: %v", err)
	}
	out, err := i.Do(context.Background(), goodRequest(t))
	if err != nil {
		t.Fatalf("failed loud instead of refusing: %v", err)
	}
	if out.Executed || act.execs != 0 {
		t.Fatalf("a tripped breaker must refuse every mutation: %+v execs=%d", out, act.execs)
	}
	if cp.MayActuate() {
		t.Fatal("a tripped breaker must force the chokepoint to Shadow — the kill did not propagate")
	}
	t.Logf("gate breaker            REFUSED as required: %s", out.Reason)
}
