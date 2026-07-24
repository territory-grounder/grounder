package regime

import (
	"context"
	"errors"
	"testing"

	"github.com/territory-grounder/grounder/adapters/actuation"
	"github.com/territory-grounder/grounder/core/credential"
)

// ---------------------------------------------------------------------------------------------------------
// Shared test doubles + fixtures (used across regime_test.go and composition_test.go, same package).
// ---------------------------------------------------------------------------------------------------------

// fakeActuator is an actuation.Actuator that records whether Exec was reached — so a refusal can be proven to
// NOT execute (the effect never fires around the interceptor). It is the ONLY leaf a regime lane wraps in
// tests; the composition test wires it into the real spec/013 interceptor.
type fakeActuator struct {
	cap   string
	ro    bool
	execs int
}

func (a *fakeActuator) Capability() string { return a.cap }
func (a *fakeActuator) ReadOnly() bool     { return a.ro }
func (a *fakeActuator) Exec(context.Context, []string, []byte) (actuation.Result, error) {
	a.execs++
	return actuation.Result{ExitCode: 0}, nil
}

// hostRule / globRule / groupRule / classRule build config-not-code regime rules over the SHARED object-model
// (credential.Selector, REQ-1703) — the same grammar policy + credential match on.
func hostRule(id, host string, r Regime) Rule {
	return Rule{ID: id, Selector: credential.Selector{Kind: credential.KindHost, Pattern: host}, Regime: r}
}
func globRule(id, glob string, r Regime) Rule {
	return Rule{ID: id, Selector: credential.Selector{Kind: credential.KindHostGlob, Pattern: glob}, Regime: r}
}
func groupRule(id, group string, r Regime) Rule {
	return Rule{ID: id, Selector: credential.Selector{Kind: credential.KindGroup, Pattern: group}, Regime: r}
}
func classRule(id, class string, r Regime) Rule {
	return Rule{ID: id, Selector: credential.Selector{Kind: credential.KindDeviceClass, Pattern: class}, Regime: r}
}

// fullRegistry wires the two foundation lanes: native-ssh over a read-only fake leaf, and the awx-job lane
// (its real actuator is T-017-3, so it carries the refusing placeholder).
func fullRegistry() []Lane {
	return []Lane{
		NewNativeSSHLane(&fakeActuator{cap: "ssh", ro: true}),
		NewAWXJobLane(),
	}
}

// ---------------------------------------------------------------------------------------------------------
// Regime enum (REQ-1700 closed set).
// ---------------------------------------------------------------------------------------------------------

func TestRegimeClosedSet(t *testing.T) {
	if got := AllRegimes(); len(got) != 6 {
		t.Fatalf("the regime set must be exactly the six sanctioned regimes, got %d: %v", len(got), got)
	}
	for _, r := range AllRegimes() {
		if !r.Valid() {
			t.Fatalf("enumerated regime %q must be Valid", r)
		}
	}
	for _, bad := range []Regime{"", "ssh", "kubectl", "NATIVE-SSH", "docker"} {
		if bad.Valid() {
			t.Fatalf("unknown regime %q must NOT be Valid (closed set, fail closed)", bad)
		}
	}
	// AllRegimes returns a copy — mutating it cannot poison the canonical set.
	AllRegimes()[0] = "tampered"
	if AllRegimes()[0] != RegimeNativeSSH {
		t.Fatal("AllRegimes must return a copy, not the canonical slice")
	}
}

// ---------------------------------------------------------------------------------------------------------
// REQ-1700: a target resolves to exactly one regime and its lane, from config DATA (config-not-code).
// ---------------------------------------------------------------------------------------------------------

func TestSelectLaneResolvesExactlyOneRegimeFromConfig(t *testing.T) {
	rules := []Rule{
		hostRule("host:dc1tg01", "dc1tg01", RegimeNativeSSH),
		globRule("glob:awxhost*", "awxhost*", RegimeAWXJob),
	}
	e := NewEngine(rules, fullRegistry())

	// the exact-host target resolves to native-ssh ...
	lane, err := e.SelectLane(credential.Target{Host: "dc1tg01"})
	if err != nil {
		t.Fatalf("expected a resolved lane, got refusal: %v", err)
	}
	if lane.Regime() != RegimeNativeSSH {
		t.Fatalf("expected native-ssh lane, got %q", lane.Regime())
	}
	// ... the glob target resolves to awx-job.
	lane2, err := e.SelectLane(credential.Target{Host: "awxhost07"})
	if err != nil || lane2.Regime() != RegimeAWXJob {
		t.Fatalf("expected awx-job lane, got lane=%v err=%v", laneRegime(lane2), err)
	}

	// config-not-code: changing only the DATA (not code) re-routes the SAME target to a different lane.
	e2 := NewEngine([]Rule{hostRule("host:dc1tg01", "dc1tg01", RegimeAWXJob)}, fullRegistry())
	lane3, err := e2.SelectLane(credential.Target{Host: "dc1tg01"})
	if err != nil || lane3.Regime() != RegimeAWXJob {
		t.Fatalf("config-not-code: a data-only rule change must re-route the lane, got lane=%v err=%v", laneRegime(lane3), err)
	}
}

func laneRegime(l Lane) any {
	if l == nil {
		return "<nil>"
	}
	return l.Regime()
}

// ---------------------------------------------------------------------------------------------------------
// REQ-1701: unknown regime → operator default lane WHERE declared, else refuse; ambiguous → fail closed.
// ---------------------------------------------------------------------------------------------------------

func TestUnknownRegimeFallsBackToDefaultLane(t *testing.T) {
	rules := []Rule{hostRule("host:known", "known-host", RegimeAWXJob)}
	defaultLane := NewNativeSSHLane(&fakeActuator{cap: "ssh", ro: true})

	// WITH a declared default: an unknown target resolves to the operator default (native-ssh).
	e := NewEngine(rules, fullRegistry(), WithDefaultLane(defaultLane))
	lane, err := e.SelectLane(credential.Target{Host: "totally-unknown-host"})
	if err != nil {
		t.Fatalf("an unknown target with a declared default must resolve, got %v", err)
	}
	if lane.Regime() != RegimeNativeSSH {
		t.Fatalf("the default lane must be native-ssh, got %q", lane.Regime())
	}

	// WITHOUT a declared default: an unknown target is refused (ErrNoRegime), no lane.
	eNoDefault := NewEngine(rules, fullRegistry())
	lane2, err := eNoDefault.SelectLane(credential.Target{Host: "totally-unknown-host"})
	if lane2 != nil || !errors.Is(err, ErrNoRegime) || !IsRefused(err) {
		t.Fatalf("an unknown target with no default must refuse ErrNoRegime, got lane=%v err=%v", laneRegime(lane2), err)
	}
}

func TestAmbiguousRegimeFailsClosed(t *testing.T) {
	// two rules of EQUAL specificity (both exact-host on the same host) naming DIFFERENT regimes.
	rules := []Rule{
		hostRule("a", "dup01", RegimeNativeSSH),
		hostRule("b", "dup01", RegimeAWXJob),
	}
	// even WITH a declared default, ambiguity must NOT fall back to it — it fails closed.
	e := NewEngine(rules, fullRegistry(), WithDefaultLane(NewNativeSSHLane(&fakeActuator{ro: true})))

	lane, err := e.SelectLane(credential.Target{Host: "dup01"})
	if lane != nil {
		t.Fatalf("an ambiguous target must NOT return a lane (never guessed), got %q", lane.Regime())
	}
	if !errors.Is(err, ErrAmbiguousRegime) {
		t.Fatalf("an ambiguous target must fail closed with ErrAmbiguousRegime, got %v", err)
	}
	// ErrAmbiguousRegime wraps ErrNoRegime, so IsRefused treats every refusal uniformly.
	if !IsRefused(err) || !errors.Is(err, ErrNoRegime) {
		t.Fatalf("ErrAmbiguousRegime must satisfy IsRefused / wrap ErrNoRegime, got %v", err)
	}

	// two equal-specificity rules naming the SAME regime are NOT ambiguous — one regime per target resolves.
	agree := NewEngine([]Rule{hostRule("a", "same01", RegimeAWXJob), hostRule("b", "same01", RegimeAWXJob)}, fullRegistry())
	l, err := agree.SelectLane(credential.Target{Host: "same01"})
	if err != nil || l.Regime() != RegimeAWXJob {
		t.Fatalf("two rules agreeing on one regime must resolve it, got lane=%v err=%v", laneRegime(l), err)
	}
}

// most-specific-wins: an exact host beats a glob beats a group beats a device-class, over the SHARED model.
func TestMostSpecificWins(t *testing.T) {
	rules := []Rule{
		classRule("class", "linux", RegimeAPI),
		groupRule("group", "edge", RegimeGitOpsMR),
		globRule("glob", "dc1*", RegimeK8sDeclarative),
		hostRule("host", "dc1tg01", RegimeNativeSSH),
	}
	lanes := []Lane{
		NewNativeSSHLane(&fakeActuator{ro: true}),
		newTestLane(RegimeK8sDeclarative),
		newTestLane(RegimeGitOpsMR),
		newTestLane(RegimeAPI),
	}
	e := NewEngine(rules, lanes)
	// a target matching ALL four selectors resolves to the MOST specific (the exact host → native-ssh).
	lane, err := e.SelectLane(credential.Target{Host: "dc1tg01", Groups: []string{"edge"}, DeviceClass: "linux"})
	if err != nil || lane.Regime() != RegimeNativeSSH {
		t.Fatalf("most-specific-wins must pick the exact-host regime, got lane=%v err=%v", laneRegime(lane), err)
	}
	// a target matching only the glob resolves to that (less specific) regime.
	lane2, err := e.SelectLane(credential.Target{Host: "dc1db09"})
	if err != nil || lane2.Regime() != RegimeK8sDeclarative {
		t.Fatalf("the glob rule must win when no more-specific rule matches, got lane=%v err=%v", laneRegime(lane2), err)
	}
}

// A regime that resolves but has no wired lane refuses (fail closed) rather than returning a nil lane.
func TestResolvedRegimeWithNoWiredLaneRefuses(t *testing.T) {
	// resolves to gitops-mr, but the registry only wires native-ssh + awx-job.
	e := NewEngine([]Rule{hostRule("h", "gitopshost", RegimeGitOpsMR)}, fullRegistry())
	lane, err := e.SelectLane(credential.Target{Host: "gitopshost"})
	if lane != nil || !IsRefused(err) {
		t.Fatalf("a resolved regime with no wired lane must refuse, got lane=%v err=%v", laneRegime(lane), err)
	}
}

// A rule that names an unknown regime is refused rather than routing down an undefined channel.
func TestUnknownRegimeInRuleRefuses(t *testing.T) {
	e := NewEngine([]Rule{{ID: "bad", Selector: credential.Selector{Kind: credential.KindHost, Pattern: "h"}, Regime: "docker"}}, fullRegistry())
	lane, err := e.SelectLane(credential.Target{Host: "h"})
	if lane != nil || !IsRefused(err) {
		t.Fatalf("a rule naming an unknown regime must refuse, got lane=%v err=%v", laneRegime(lane), err)
	}
}

// The ZERO engine (no rules, no lanes, no default) refuses everything.
func TestZeroEngineRefusesEverything(t *testing.T) {
	if lane, err := (&Engine{}).SelectLane(credential.Target{Host: "anything"}); lane != nil || !IsRefused(err) {
		t.Fatalf("the zero engine must refuse everything, got lane=%v err=%v", laneRegime(lane), err)
	}
}

// ---------------------------------------------------------------------------------------------------------
// REQ-1703: regime resolution keys off the SAME estate object-model as policy + credential.
// ---------------------------------------------------------------------------------------------------------

func TestSharedObjectModel(t *testing.T) {
	target := credential.Target{Host: "dc1tg01", Groups: []string{"edge"}, DeviceClass: "cisco-asa"}
	sel := credential.Selector{Kind: credential.KindHostGlob, Pattern: "dc1*"}

	// regime keys off the SHARED credential.Selector/Target/Match — a Rule IS a credential.Selector.
	e := NewEngine([]Rule{{ID: "r", Selector: sel, Regime: RegimeAWXJob}}, fullRegistry())
	lane, err := e.SelectLane(target)
	regimeMatched := err == nil && lane != nil

	// the credential/policy engines match the SAME target with the SAME shared primitive — one grammar.
	credentialMatched := credential.Match(sel, target)

	if !regimeMatched || !credentialMatched {
		t.Fatalf("the shared object-model did not agree: regime=%v credential=%v", regimeMatched, credentialMatched)
	}
	// Compile-time proof there is no second inventory grammar: regime's Rule.Selector and SelectLane's target
	// ARE credential.Selector / credential.Target — the one model built once in core/credential.
	var _ credential.Selector = e.rules[0].Selector
	var _ = func(t credential.Target) (Lane, error) { return e.SelectLane(t) }
}

// newTestLane builds a minimal Lane for an arbitrary regime over a refusing leaf — for registry/precedence
// tests that need lanes beyond the two foundation lanes without pulling in later-task actuators.
func newTestLane(r Regime) Lane { return testLane{r: r} }

type testLane struct{ r Regime }

func (l testLane) Regime() Regime                 { return l.r }
func (l testLane) effectLeaf() actuation.Actuator { return pendingActuator{regime: l.r} }

// TestLaneForRegime covers the effect-kind-driven lane accessor (REQ-1700): a wired regime returns its lane;
// an unwired regime fails closed (ok=false) so the caller never actuates a resolved-but-unwired channel.
func TestLaneForRegime(t *testing.T) {
	e := NewEngine(nil, fullRegistry())
	if lane, ok := e.LaneForRegime(RegimeAWXJob); !ok || lane == nil || lane.Regime() != RegimeAWXJob {
		t.Fatalf("LaneForRegime(awx-job) must return the awx lane, got ok=%v lane=%v", ok, lane)
	}
	if lane, ok := e.LaneForRegime(RegimeNativeSSH); !ok || lane == nil || lane.Regime() != RegimeNativeSSH {
		t.Fatalf("LaneForRegime(native-ssh) must return the ssh lane, got ok=%v", ok)
	}
	// An engine with ONLY the ssh lane fails closed for the awx-job regime.
	e2 := NewEngine(nil, []Lane{NewNativeSSHLane(&fakeActuator{cap: "ssh", ro: true})})
	if _, ok := e2.LaneForRegime(RegimeAWXJob); ok {
		t.Fatal("LaneForRegime for an unwired regime must return ok=false (fail closed)")
	}
}

// TestProxmoxLaneFailsClosedUntilWired: the proxmox lane's default leaf is the fail-closed pendingActuator
// (read-only, refuses), and the engine registers it under RegimeProxmox for kind-routing.
func TestProxmoxLaneFailsClosedUntilWired(t *testing.T) {
	l := NewProxmoxLane()
	if l.Regime() != RegimeProxmox {
		t.Fatalf("proxmox lane Regime()=%q, want %q", l.Regime(), RegimeProxmox)
	}
	e := NewEngine(nil, []Lane{NewProxmoxLane()})
	lane, ok := e.LaneForRegime(RegimeProxmox)
	if !ok || lane == nil {
		t.Fatal("engine must register the proxmox lane for kind-routing")
	}
	if !RegimeProxmox.Valid() {
		t.Fatal("RegimeProxmox must be a Valid regime")
	}
}
