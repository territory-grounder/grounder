package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/territory-grounder/grounder/adapters/actuation"
	"github.com/territory-grounder/grounder/core/actuate"
	"github.com/territory-grounder/grounder/core/audit"
	"github.com/territory-grounder/grounder/core/manifest"
	"github.com/territory-grounder/grounder/core/policy"
	"github.com/territory-grounder/grounder/core/safety"
	"github.com/territory-grounder/grounder/core/verify"
)

func TestAnEmptyScopeRefusesEverythingWhileTheConditionEnforces(t *testing.T) {
	for _, raw := range []string{"", "   ", ",;,", "\t\n"} {
		s := resolveActuationScope(raw, true)
		if s == nil {
			t.Fatalf("%q: a missing scope left the chain unrestricted", raw)
		}
		if s.Admits("host01") || s.Len() != 0 {
			t.Fatalf("%q: admits=%v len=%d, want nothing admitted", raw, s.Admits("host01"), s.Len())
		}
	}
}

func TestOnlyTheExplicitWordAllLiftsAnEnforcedScope(t *testing.T) {
	for _, raw := range []string{"all", " ALL ", "All"} {
		if s := resolveActuationScope(raw, true); s != nil {
			t.Fatalf("%q: got a scope of %d host(s), want none", raw, s.Len())
		}
	}
	s := resolveActuationScope("all,host01", true)
	if s == nil || !s.Admits("host01") || s.Admits("host02") {
		t.Fatalf("a list that merely contains the word must stay a list: %+v", s)
	}
}

func TestAConfiguredScopeAdmitsOnlyItsHosts(t *testing.T) {
	s := resolveActuationScope("host01,host02", true)
	if s == nil || !s.Admits("host01") || !s.Admits("host02") || s.Admits("host03") {
		t.Fatalf("scope = %+v", s)
	}
}

func TestAnEmptyScopeWithoutEnforcementKeepsTheOldMeaning(t *testing.T) {
	for _, raw := range []string{"", "all"} {
		if s := resolveActuationScope(raw, false); s != nil {
			t.Fatalf("%q: got a scope of %d host(s), want none", raw, s.Len())
		}
	}
}

func scopeTestRequest(m *manifest.ActionManifest) actuate.Request {
	return actuate.Request{Manifest: m, Gated: true, Argv: []string{"start", "host01"}, Band: safety.BandAuto, Evidence: []actuate.Evidence{{ToolResultID: "t1", Captured: true, Successful: true, Recent: true, Relevant: true}},
		Observe:      func(context.Context) ([]verify.ObservedAlert, bool) { return nil, true },
		StillFaulted: func(context.Context) (bool, bool) { return true, true }}
}

type scopeTestDecider struct{}

func (scopeTestDecider) Decide(_ context.Context, in policy.EvalInput) (policy.PolicyDecision, error) {
	return policy.NewPolicyDecision(policy.VerdictAuto, "test-permissive", in.Band, nil, in.Mode, "test", policy.DecisionAudit{}), nil
}

func scopeTestMode() policy.Mode { return policy.ModeFullAuto }

type scopeTestLeaf struct{ execs int }

func (l *scopeTestLeaf) Capability() string { return "test" }
func (l *scopeTestLeaf) ReadOnly() bool     { return false }
func (l *scopeTestLeaf) Exec(context.Context, []string, []byte) (actuation.Result, error) {
	l.execs++
	return actuation.Result{}, nil
}

func TestTheEffectChainRefusesOutsideItsScopeBeforeTheLeaf(t *testing.T) {
	a := manifest.Action{OpClass: "start-guest", Op: "start", Target: "host01", Params: map[string]string{"guest": "host01"}, Reversible: true}
	m, err := manifest.New(a, safety.BandAuto, "plan", "prediction")
	if err != nil {
		t.Fatal(err)
	}
	for name, scope := range map[string]*actuate.ActuationScope{
		"refuse all":  actuate.RefuseAllScope(),
		"other hosts": actuate.ParseActuationScope("host02"),
	} {
		leaf := &scopeTestLeaf{}
		chain := boundEffectChain(safety.NewActuatingChokepoint(), leaf, audit.NewLedger(), scope, false)
		chain = chain.WithPolicyDecider(scopeTestDecider{}, scopeTestMode)
		out, err := chain.Do(context.Background(), scopeTestRequest(m))
		if err != nil {
			t.Fatal(err)
		}
		if !out.Refused || leaf.execs != 0 || !strings.Contains(out.Reason, "outside the configured actuation scope") {
			t.Fatalf("%s: %+v execs=%d", name, out, leaf.execs)
		}
	}
}

func TestTheEffectChainConsultsTheConditionWhenEnforcing(t *testing.T) {
	a := manifest.Action{OpClass: "start-guest", Op: "start", Target: "host01", Params: map[string]string{"guest": "host01"}, Reversible: true}
	m, err := manifest.New(a, safety.BandAuto, "plan", "prediction")
	if err != nil {
		t.Fatal(err)
	}
	prior := activeCondition.Swap(nil)
	t.Cleanup(func() { activeCondition.Store(prior) })
	leaf := &scopeTestLeaf{}
	chain := boundEffectChain(safety.NewActuatingChokepoint(), leaf, audit.NewLedger(), actuate.ParseActuationScope("host01"), true)
	chain = chain.WithPolicyDecider(scopeTestDecider{}, scopeTestMode)
	out, err := chain.Do(context.Background(), scopeTestRequest(m))
	if err != nil {
		t.Fatal(err)
	}
	if !out.Refused || leaf.execs != 0 || !strings.Contains(out.Reason, "could not be read") {
		t.Fatalf("%+v execs=%d", out, leaf.execs)
	}
}

func TestEveryEffectChainIsBuiltInOnePlace(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		c := strings.Count(string(b), "actuate.NewInterceptor(")
		if c > 0 && f != "actuation_scope_wire.go" {
			t.Errorf("%s builds an effect chain outside boundEffectChain", f)
		}
		n += c
	}
	if n != 1 {
		t.Fatalf("actuate.NewInterceptor appears %d time(s), want 1", n)
	}
}
