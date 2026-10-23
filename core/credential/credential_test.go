package credential

import (
	"errors"
	"testing"

	"github.com/territory-grounder/grounder/core/config"
)

func mustBundle(t *testing.T, spec BundleSpec) Bundle {
	t.Helper()
	b, err := NewBundle(spec)
	if err != nil {
		t.Fatalf("NewBundle(%+v): %v", spec, err)
	}
	return b
}

// REQ-1601: a bundle is constructible only with all required fields; the zero value is invalid.
func TestBundleRequiredFields(t *testing.T) {
	if (Bundle{}).Valid() {
		t.Fatal("zero Bundle must be invalid (no blank identity)")
	}
	// missing user
	if _, err := NewBundle(BundleSpec{Port: 22, Scheme: SchemeSSH, SSHKeyRef: "env:K"}); err == nil {
		t.Fatal("expected error for missing user")
	}
	// missing scheme-appropriate secret
	if _, err := NewBundle(BundleSpec{User: "root", Port: 22, Scheme: SchemeSSH}); err == nil {
		t.Fatal("expected error for missing ssh key on ssh scheme")
	}
	// bad port
	if _, err := NewBundle(BundleSpec{User: "root", Port: 0, Scheme: SchemeSSH, SSHKeyRef: "env:K"}); err == nil {
		t.Fatal("expected error for invalid port")
	}
	// unknown scheme
	if _, err := NewBundle(BundleSpec{User: "root", Port: 22, Scheme: "telnet", SSHKeyRef: "env:K"}); err == nil {
		t.Fatal("expected error for unknown scheme")
	}
	b := mustBundle(t, BundleSpec{User: "root", Port: 22, Scheme: SchemeSSH, SSHKeyRef: "env:K"})
	if !b.Valid() || b.User() != "root" || b.Port() != 22 || b.Scheme() != SchemeSSH {
		t.Fatalf("valid bundle accessors wrong: %+v", b)
	}
}

// REQ-1603: a plaintext secret can never enter a bundle — only sealed references.
func TestBundleRejectsPlaintextSecret(t *testing.T) {
	if _, err := NewBundle(BundleSpec{User: "root", Port: 22, Scheme: SchemeSSH, SSHKeyRef: "ssh-ed25519 AAAAliteralkeymaterial"}); err == nil {
		t.Fatal("expected rejection of a plaintext ssh key literal")
	}
	if _, err := NewBundle(BundleSpec{User: "root", Port: 22, Scheme: SchemeSSH, SSHKeyRef: "env:K", Become: "s3cr3tpassword"}); err == nil {
		t.Fatal("expected rejection of a plaintext become literal")
	}
}

// REQ-1603: SecretRefs resolve at use time through the sealed store.
func TestBundleResolveSecrets(t *testing.T) {
	t.Setenv("TG_TEST_KEY", "KEYMATERIAL")
	b := mustBundle(t, BundleSpec{User: "ops", Port: 22, Scheme: SchemeSSH, SSHKeyRef: "env:TG_TEST_KEY"})
	r, err := b.Resolve()
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if r.SSHKey != "KEYMATERIAL" || r.User != "ops" {
		t.Fatalf("resolved wrong: %+v", r)
	}
	// unresolvable ref → fail closed
	b2 := mustBundle(t, BundleSpec{User: "ops", Port: 22, Scheme: SchemeSSH, SSHKeyRef: "env:TG_TEST_UNSET_KEY"})
	if _, err := b2.Resolve(); err == nil {
		t.Fatal("expected fail-closed on unresolvable secret ref")
	}
	// zero bundle refuses
	if _, err := (Bundle{}).Resolve(); !IsRefused(err) {
		t.Fatalf("zero bundle Resolve should refuse, got %v", err)
	}
}

// REQ-1602: an empty/zero engine refuses everything; no default identity.
func TestFailClosedEmptyEngine(t *testing.T) {
	e := NewEngine(nil)
	_, err := e.Resolve(Target{Host: "anything"})
	if !IsRefused(err) {
		t.Fatalf("empty engine must refuse, got %v", err)
	}
	var nilEngine *Engine
	if _, err := nilEngine.Resolve(Target{Host: "x"}); !IsRefused(err) {
		t.Fatalf("nil engine must refuse, got %v", err)
	}
}

// REQ-1600/1606: most-specific-wins across host > glob > group > device-class.
func TestResolvePrecedence(t *testing.T) {
	rules := []Rule{
		{ID: "dc", Selector: Selector{KindDeviceClass, "linux"}, Bundle: mustBundle(t, BundleSpec{User: "dcuser", Port: 22, Scheme: SchemeSSH, SSHKeyRef: "env:A"})},
		{ID: "grp", Selector: Selector{KindGroup, "edge"}, Bundle: mustBundle(t, BundleSpec{User: "grpuser", Port: 22, Scheme: SchemeSSH, SSHKeyRef: "env:B"})},
		{ID: "glob", Selector: Selector{KindHostGlob, "dc1*"}, Bundle: mustBundle(t, BundleSpec{User: "globuser", Port: 22, Scheme: SchemeSSH, SSHKeyRef: "env:C"})},
		{ID: "host", Selector: Selector{KindHost, "dc1tg01"}, Bundle: mustBundle(t, BundleSpec{User: "hostuser", Port: 22, Scheme: SchemeSSH, SSHKeyRef: "env:D"})},
	}
	e := NewEngine(rules)
	tgt := Target{Host: "dc1tg01", Groups: []string{"edge"}, DeviceClass: "linux"}
	b, err := e.Resolve(tgt)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if b.User() != "hostuser" || b.RuleID() != "host" {
		t.Fatalf("expected exact-host to win, got %q (%s)", b.User(), b.RuleID())
	}

	// remove the host rule → glob should win over group/device-class.
	e2 := NewEngine(rules[:3])
	b2, err := e2.Resolve(tgt)
	if err != nil || b2.RuleID() != "glob" {
		t.Fatalf("expected glob to win, got %s err=%v", b2.RuleID(), err)
	}
}

// REQ-1606: equal-specificity conflict fails closed.
func TestEqualSpecificityRefuses(t *testing.T) {
	rules := []Rule{
		{ID: "h1", Selector: Selector{KindHost, "dup01"}, Bundle: mustBundle(t, BundleSpec{User: "a", Port: 22, Scheme: SchemeSSH, SSHKeyRef: "env:A"})},
		{ID: "h2", Selector: Selector{KindHost, "dup01"}, Bundle: mustBundle(t, BundleSpec{User: "b", Port: 22, Scheme: SchemeSSH, SSHKeyRef: "env:B"})},
	}
	e := NewEngine(rules)
	_, err := e.Resolve(Target{Host: "dup01"})
	if !errors.Is(err, ErrAmbiguous) || !IsRefused(err) {
		t.Fatalf("equal-specificity conflict must refuse as ambiguous, got %v", err)
	}
}

// REQ-1605: the resolver and a policy-style matcher key off the SAME shared object-model — one grammar.
func TestSharedObjectModel(t *testing.T) {
	sel := Selector{KindHostGlob, "dc1*"}
	tgt := Target{Host: "dc1tg01"}
	// resolver path
	e := NewEngine([]Rule{{ID: "r", Selector: sel, Bundle: mustBundle(t, BundleSpec{User: "u", Port: 22, Scheme: SchemeSSH, SSHKeyRef: "env:A"})}})
	if _, err := e.Resolve(tgt); err != nil {
		t.Fatalf("resolver match failed: %v", err)
	}
	// a policy-engine-style direct call over the SAME primitive must agree.
	if !Match(sel, tgt) {
		t.Fatal("shared Match primitive disagreed with the resolver")
	}
}

// REQ-1600: config-not-code — rules parse from operator config data.
func TestParseRules(t *testing.T) {
	// a well-formed multi-rule config, ssh key and file ref.
	rules, err := ParseRules("host:h1|root|22|ssh|env:K; group:edge|ops|22|ssh|file:/run/secrets/edge")
	if err != nil || len(rules) != 2 {
		t.Fatalf("ParseRules well-formed: n=%d err=%v", len(rules), err)
	}
	if rules[0].Selector.Kind != KindHost || rules[1].Selector.Kind != KindGroup {
		t.Fatalf("parsed selectors wrong: %+v", rules)
	}
	// an api rule with an empty token → NewBundle rejects, so the parse fails closed (never a blank identity).
	if _, err := ParseRules("device-class:cisco-asa|admin|443|api"); err == nil {
		t.Fatal("expected error: api scheme with no api token ref")
	}
	// malformed → error, never a wildcard.
	if _, err := ParseRules("garbage-no-fields"); err == nil {
		t.Fatal("expected error on malformed rule")
	}
	// a plaintext secret in config → rejected.
	if _, err := ParseRules("host:h1|root|22|ssh|literal-key-material"); err == nil {
		t.Fatal("expected error: plaintext secret in config")
	}
	_ = config.SecretRef("")
}
