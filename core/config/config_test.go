package config

import (
	"os"
	"strings"
	"testing"
)

// The unsupported-scheme / bare-literal Resolve error path must NEVER echo the reference value: a
// misconfigured inline secret (INV-13 forbids inline secrets) would otherwise leak into any log that
// records the error. Only the SCHEME name (never a secret) or the value's LENGTH may be surfaced.
func TestSecretRefResolveRedactsValue(t *testing.T) {
	// (1) a bare literal with NO scheme prefix — the whole string may be a secret.
	secret := "sk-super-secret-abc123xyz789"
	_, err := SecretRef(secret).Resolve()
	if err == nil {
		t.Fatal("a bare literal must fail closed")
	}
	if strings.Contains(err.Error(), secret) {
		t.Fatalf("error leaked the bare-literal secret value: %q", err.Error())
	}

	// (2) a bare value that happens to contain a colon → an unregistered "scheme"; the VALUE tail must not leak.
	tail := "the-secret-tail-9f8e7d6c"
	_, err = SecretRef("notascheme:" + tail).Resolve()
	if err == nil {
		t.Fatal("an unregistered scheme must fail closed")
	}
	if strings.Contains(err.Error(), tail) {
		t.Fatalf("error leaked the value tail after the colon: %q", err.Error())
	}
	// the scheme name itself is safe to surface (it is not a secret) and aids diagnosis.
	if !strings.Contains(err.Error(), "notascheme") {
		t.Fatalf("error should name the unsupported scheme for diagnosis, got %q", err.Error())
	}
}

// A registered custom scheme (bao:/vault:/…, REQ-1613) resolves through its connector; an unregistered
// scheme still fails closed.
func TestSecretRefCustomScheme(t *testing.T) {
	RegisterSchemeResolver("testscheme", func(ref string) (string, error) {
		return "resolved-" + strings.TrimPrefix(ref, "testscheme:"), nil
	})
	defer RegisterSchemeResolver("testscheme", nil)

	v, err := SecretRef("testscheme:abc").Resolve()
	if err != nil || v != "resolved-abc" {
		t.Fatalf("custom scheme resolve = (%q, %v), want (resolved-abc, nil)", v, err)
	}
	if _, err := SecretRef("unregistered:x").Resolve(); err == nil {
		t.Fatal("an unregistered scheme must fail closed")
	}
}

func TestSecretRefResolve(t *testing.T) {
	t.Setenv("TG_TEST_SECRET", "s3cr3t-value")
	v, err := SecretRef("env:TG_TEST_SECRET").Resolve()
	if err != nil || v != "s3cr3t-value" {
		t.Fatalf("env resolve = %q,%v", v, err)
	}
	if _, err := SecretRef("env:TG_MISSING_XYZ").Resolve(); err == nil {
		t.Fatal("missing env secret must error")
	}
	if _, err := SecretRef("gitlab-token-literal").Resolve(); err == nil {
		t.Fatal("a bare literal must NOT be accepted as a secret reference")
	}
	if _, err := SecretRef("").Resolve(); err == nil {
		t.Fatal("empty reference must error")
	}

	f, _ := os.CreateTemp(t.TempDir(), "sec")
	f.WriteString("file-secret\n")
	f.Close()
	v, err = SecretRef("file:" + f.Name()).Resolve()
	if err != nil || v != "file-secret" {
		t.Fatalf("file resolve = %q,%v", v, err)
	}
}

func TestSecretRefStoreScheme(t *testing.T) {
	// Unwired: the store: scheme fails closed — never an empty value (task #27 Phase D, REQ-524).
	RegisterStoreResolver(nil)
	if _, err := SecretRef("store:librenms.token").Resolve(); err == nil {
		t.Fatal("store: with no wired sealed store must error")
	}
	if _, err := SecretRef("store:").Resolve(); err == nil {
		t.Fatal("store: with an empty name must error")
	}
	// Wired: the composition-registered resolver serves the value.
	RegisterStoreResolver(func(name string) (string, error) {
		if name != "librenms.token" {
			t.Fatalf("resolver got name %q", name)
		}
		return "sealed-value-for-test", nil
	})
	defer RegisterStoreResolver(nil)
	v, err := SecretRef("store:librenms.token").Resolve()
	if err != nil || v != "sealed-value-for-test" {
		t.Fatalf("store resolve = %q,%v", v, err)
	}
}

func TestLintForbiddenSecrets(t *testing.T) {
	// A real high-entropy literal must be flagged.
	dirty := []byte(`api_key = "sk-Abc123XyZ987QwErTyUiOpLkJhGfDsA0"` + "\n")
	if f := LintForbiddenSecrets(dirty); len(f) == 0 {
		t.Fatal("high-entropy literal secret must be flagged")
	}
	// Reference schemes + placeholders must NOT be flagged.
	clean := []byte("ZAI_API_KEY = env:ZAI_API_KEY\n" +
		"token = file:/run/secrets/zai\n" +
		"note = your-token-here-changeme-placeholder\n")
	if f := LintForbiddenSecrets(clean); len(f) != 0 {
		t.Fatalf("clean config must not be flagged, got %+v", f)
	}
}

// ★ THE SHAPE GATE'S ONLY WINDOW ONTO A VALUE (TG-284). HasReferenceScheme answers "is this a LOCATION or a
// CREDENTIAL?" for an env value nobody declared. Two properties have to hold and neither is obvious:
//
//  1. It must recognise every scheme the resolvers accept. A scheme missing here turns a correctly-migrated
//     bao: reference into a reported "raw plaintext credential" — the gate would punish the exact migration
//     it demands, and the operator's fix would be to switch the gate off.
//  2. It must never hand back any part of the value. SchemeOf returns the text before the first colon, so on
//     a raw credential containing a colon it returns a FRAGMENT OF THE SECRET, and callers log what they are
//     handed. That is why the shape gate calls this bool and not SchemeOf.
func TestHasReferenceSchemeCoversEveryResolvableScheme(t *testing.T) {
	// Derived from the resolver's own set, not retyped: a hand-copied list agrees with itself and drifts.
	if len(backendSchemes) == 0 {
		t.Fatal("no backend schemes declared — this sweep would pass vacuously")
	}
	for scheme := range backendSchemes {
		if !HasReferenceScheme(scheme + ":wherever#field") {
			t.Errorf("%q resolves through a backend but is not recognised as a reference — a deployment that "+
				"migrated this credential would be failed for holding plaintext it does not hold", scheme)
		}
	}
	// The built-in and connector schemes the shape gate must also accept as locations.
	for _, scheme := range []string{"env", "file", "store", "oidc", "ansible-vault"} {
		if !HasReferenceScheme(scheme + ":wherever") {
			t.Errorf("%q is a resolvable reference scheme but is not recognised as one", scheme)
		}
	}
	// And the things that are NOT references: blank, a bare literal, a colon-bearing literal.
	for _, v := range []string{"", "   ", "a-plain-32-char-looking-api-token", "notascheme:still-a-literal"} {
		if HasReferenceScheme(v) {
			t.Errorf("%q is not a reference — treating a raw credential as a location is how a plaintext secret "+
				"passes the gate unnoticed", v)
		}
	}
}

// spec/022 T-022-5 (REQ-2205): a MALFORMED "reference" is exactly where a pasted inline secret lands,
// and the resolver's refusal is the log line most likely to record it. RedactedRef must render scheme +
// shape only; LoggableRef must pass a genuine reference verbatim and route everything else through
// RedactedRef. The canary asserts by ABSENCE — the value never appears, whatever the arm.
func TestRedactedAndLoggableRefNeverEchoThePayload(t *testing.T) {
	const canary = "hunter2-canary"
	for _, v := range []string{canary, "vault:" + canary, "weird-scheme:" + canary, ":" + canary} {
		if got := RedactedRef(v); strings.Contains(got, canary) {
			t.Errorf("RedactedRef(%d chars) echoed the payload: %q", len(v), got)
		}
	}
	if got := RedactedRef("vault:" + canary); got != "vault:<14 chars>" {
		t.Errorf("RedactedRef must keep the scheme and the length for diagnosis, got %q", got)
	}
	if got := LoggableRef("env:NETBOX_TOKEN"); got != "env:NETBOX_TOKEN" {
		t.Errorf("a genuine reference is safe to log VERBATIM (SecretRef's contract), got %q", got)
	}
	if got := LoggableRef(canary); strings.Contains(got, canary) {
		t.Errorf("LoggableRef echoed a schemeless payload: %q", got)
	}
}
