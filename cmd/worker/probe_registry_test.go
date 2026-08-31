package main

import (
	"context"
	"errors"
	"testing"

	"github.com/territory-grounder/grounder/core/selftest"
	"github.com/territory-grounder/grounder/temporal/moduletest"
)

type fakeTester struct {
	res  selftest.Result
	err  error
	saw  string
	call int
}

func (f *fakeTester) SelfTest(_ context.Context, operator string) (selftest.Result, error) {
	f.call++
	f.saw = operator
	return f.res, f.err
}

type notATester struct{}

// A module that cannot self-test must be COUNTED but not registered, so the boot report can name it.
// Reporting only the registrations cannot distinguish a small fleet from a broken filter — which is
// exactly how "lane registered over 1 prober(s)" stayed true for weeks beside twenty-nine dialogs.
func TestAModuleWithNoProbeIsCountedAndNamedRatherThanSilentlyDropped(t *testing.T) {
	r := newProbeRegistry()
	if ok := r.offer("tracker", "youtrack", &fakeTester{}); !ok {
		t.Fatal("a real Tester was refused")
	}
	if ok := r.offer("cmdb", "netbox", notATester{}); ok {
		t.Fatal("a module with no SelfTest was registered as having a probe")
	}
	if got := r.constructed(); got != 2 {
		t.Errorf("constructed() = %d, want 2 — the denominator must count every module offered", got)
	}
	if got := r.keys(); len(got) != 1 || got[0] != "tracker/youtrack" {
		t.Errorf("keys() = %v", got)
	}
	if got := r.declinedKeys(); len(got) != 1 || got[0] != "cmdb/netbox" {
		t.Errorf("declinedKeys() = %v — the untestable module must be nameable, not just missing", got)
	}
}

// A module offered SEVERAL instances counts ONCE, and the one that can self-test wins regardless of order.
//
// librenms is constructed four times — a registry module, an estate source, an alert source and a tool set
// — and only some hold a live network client. Counting offers instead of identities would report it as
// four modules and three phantom gaps.
func TestSeveralInstancesOfOneModuleCountOnceAndTheTestableOneWins(t *testing.T) {
	r := newProbeRegistry()
	r.offer("ingest", "librenms", notATester{}) // the offline Normalize-only module
	live := &fakeTester{res: selftest.Result{Summary: "read 37 devices"}}
	r.offer("ingest", "librenms", live) // the alert source, holding a real client
	if got := r.constructed(); got != 1 {
		t.Errorf("constructed() = %d, want 1 — one module, several instances", got)
	}
	if got := r.declinedKeys(); len(got) != 0 {
		t.Errorf("declinedKeys() = %v — the module HAS a probe; it must not be reported as a gap", got)
	}
	p, ok := r.set()["ingest/librenms"]
	if !ok {
		t.Fatal("no prober registered")
	}
	summary, _, err := p.Probe(context.Background(), moduletest.Request{Operator: "@ops:example"})
	if err != nil || summary != "read 37 devices" {
		t.Fatalf("the wrong instance won: summary=%q err=%v", summary, err)
	}
	if live.saw != "@ops:example" {
		t.Errorf("the operator was not carried to the probe: %q", live.saw)
	}
}

// A TYPED NIL must not register. Otherwise an unconfigured module looks like a module with a probe and
// pressing TEST panics the activity instead of reporting that no test is implemented.
func TestATypedNilInstanceIsNotAProbe(t *testing.T) {
	r := newProbeRegistry()
	var nilTester *fakeTester
	if ok := r.offer("notifier", "matrix", nilTester); ok {
		t.Fatal("a typed-nil module registered as having a probe")
	}
	if got := r.declinedKeys(); len(got) != 1 {
		t.Errorf("a typed nil must still COUNT as constructed-but-unprobeable, got %v", got)
	}
}

// A blank identity is refused rather than stored under an empty key: a probe reachable only by a key
// nobody can name is a probe nobody can press.
func TestABlankIdentityIsRefused(t *testing.T) {
	r := newProbeRegistry()
	if r.offer("", "matrix", &fakeTester{}) || r.offer("notifier", "", &fakeTester{}) {
		t.Fatal("a module with no identity was registered")
	}
	if r.constructed() != 0 {
		t.Error("a blank identity polluted the denominator")
	}
}

// A FAILING probe must surface the module's own actionable Detail, not be flattened to a bare error.
func TestAFailedProbeCarriesTheModulesOwnDiagnosis(t *testing.T) {
	r := newProbeRegistry()
	r.offer("tracker", "jira", &fakeTester{
		res: selftest.Result{Summary: "could not reach Jira", Detail: "the API token was rejected"},
		err: errors.New("status 401"),
	})
	summary, detail, err := r.set()["tracker/jira"].Probe(context.Background(), moduletest.Request{})
	if err == nil {
		t.Fatal("a failed probe reported success")
	}
	if summary != "could not reach Jira" || detail != "the API token was rejected" {
		t.Errorf("the module's diagnosis was lost: summary=%q detail=%q", summary, detail)
	}
}

// A probe that fails WITHOUT classifying falls through to the raw error rather than inventing one.
func TestAnUnclassifiedFailureFallsThroughToTheRawError(t *testing.T) {
	r := newProbeRegistry()
	r.offer("cmdb", "pve", &fakeTester{err: errors.New("boom")})
	_, detail, err := r.set()["cmdb/pve"].Probe(context.Background(), moduletest.Request{})
	if err == nil || detail != "boom" {
		t.Errorf("detail=%q err=%v — an unclassified failure must show the real error", detail, err)
	}
}

// A PASS that reports nothing observed is named as such rather than rendered as a confident "ok".
//
// A probe that cannot say what it saw cannot distinguish a correctly configured module from one pointed
// at the wrong instance — the failure a green TEST is most likely to hide.
func TestASilentPassIsNotDressedUpAsOK(t *testing.T) {
	r := newProbeRegistry()
	r.offer("knowledge", "awxplaybooks", &fakeTester{})
	summary, _, err := r.set()["knowledge/awxplaybooks"].Probe(context.Background(), moduletest.Request{})
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	if summary == "ok" || summary == "" {
		t.Errorf("a probe that observed nothing reported %q", summary)
	}
}
