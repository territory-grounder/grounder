package bootstrap

// ORACLE for the credential-engine bootstrap (spec/016 wiring): a config with every source present builds a
// SyncEngine that registers each source on the correct plane at the declared precedence and wires the native
// fallback; a partial/invalid source config FAILS CLOSED (returns an error, never a half-wired engine).

import (
	"testing"

	"github.com/territory-grounder/grounder/core/credential"
	"github.com/territory-grounder/grounder/modules/credsource/openbao"
)

// fullConfig is a complete, valid config for all four sources (SecretRefs, never literals). Construction never
// touches the network, so no live backend is needed.
func fullConfig() CredentialConfig {
	return CredentialConfig{
		NativeRules:       "host:host-native|root|22|ssh|env:NATIVE_KEY",
		OpenBaoAddr:       "https://openbao.example:8200",
		OpenBaoAuthMethod: "token",
		OpenBaoTokenRef:   "env:OPENBAO_TOKEN",
		OpenBaoKVPrefix:   "hosts",
		AWXAddr:           "https://awx.example",
		AWXTokenRef:       "env:AWX_TOKEN",
		SemaphoreAddr:     "http://semaphore.example:3000",
		SemaphoreTokenRef: "env:SEMAPHORE_TOKEN",
		LDAPURLs:          "ldaps://ipa.example:636",
		LDAPBindDNRef:     "env:LDAP_BIND_DN",
		LDAPBindPWRef:     "env:LDAP_BIND_PW",
	}
}

func TestBuildSyncEngine_RegistersEverySourceOnItsPlane(t *testing.T) {
	t.Cleanup(func() { openbao.RegisterResolver(nil) }) // OpenBao registers the bao: scheme globally; clean up
	se, sources, err := BuildSyncEngine(fullConfig())
	if err != nil {
		t.Fatalf("BuildSyncEngine: unexpected error: %v", err)
	}
	if se == nil {
		t.Fatal("BuildSyncEngine returned a nil engine")
	}
	// Exactly the four configured sources, each on the right plane at the declared precedence.
	want := map[string]struct {
		plane credential.Plane
		prec  int
	}{
		"openbao":   {credential.PlaneMachine, precedenceOpenBao},
		"awx":       {credential.PlaneMachine, precedenceAWX},
		"semaphore": {credential.PlaneMachine, precedenceSemaphore},
		"ldap":      {credential.PlaneHuman, precedenceLDAP},
	}
	if len(sources) != len(want) {
		t.Fatalf("registered %d sources, want %d: %+v", len(sources), len(want), sources)
	}
	for _, rs := range sources {
		w, ok := want[rs.ID]
		if !ok {
			t.Errorf("unexpected source %q registered", rs.ID)
			continue
		}
		if rs.Plane != w.plane {
			t.Errorf("source %q plane = %q, want %q", rs.ID, rs.Plane, w.plane)
		}
		if rs.Precedence != w.prec {
			t.Errorf("source %q precedence = %d, want %d", rs.ID, rs.Precedence, w.prec)
		}
	}
	// The native fallback is wired: a target only the native rule covers resolves through it (no source synced).
	res, err := se.Resolve(credential.Target{Host: "host-native"})
	if err != nil {
		t.Fatalf("native fallback resolve: unexpected error: %v", err)
	}
	if !res.Native {
		t.Errorf("expected a native-fallback resolution, got source %q", res.Source)
	}
}

func TestBuildSyncEngine_NoSourcesIsValidNativeOnly(t *testing.T) {
	se, sources, err := BuildSyncEngine(CredentialConfig{NativeRules: "host:h|root|22|ssh|env:K"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if se == nil || len(sources) != 0 {
		t.Fatalf("expected a valid engine with zero registered sources, got engine=%v sources=%+v", se != nil, sources)
	}
	// An uncovered target still fails closed (never a default identity).
	if _, err := se.Resolve(credential.Target{Host: "nope"}); !credential.IsRefused(err) {
		t.Errorf("uncovered target must fail closed, got %v", err)
	}
}

// The native hostdiag allowlist registers as a lowest-precedence machine-plane source, and once synced it
// resolves exactly the (user, keyref) the allowlist row declares — the read-only SSH investigation path now
// resolves identity THROUGH the engine (fail-closed), reusing the SAME hostdiag grammar (no second parser).
func TestBuildSyncEngine_NativeHostDiagSource(t *testing.T) {
	se, sources, err := BuildSyncEngine(CredentialConfig{
		HostDiagDeployments: "nl|dc1*|root|file:/secrets/hostdiag_key",
	})
	if err != nil {
		t.Fatalf("BuildSyncEngine: %v", err)
	}
	if len(sources) != 1 || sources[0].ID != "native-hostdiag" ||
		sources[0].Plane != credential.PlaneMachine || sources[0].Precedence != precedenceNativeHostDiag {
		t.Fatalf("expected one native-hostdiag machine source at precedence %d, got %+v", precedenceNativeHostDiag, sources)
	}
	if _, err := se.SyncAll(nil); err != nil {
		t.Fatalf("SyncAll: %v", err)
	}
	res, err := se.Resolve(credential.Target{Host: "dc1librespeed01"})
	if err != nil {
		t.Fatalf("resolve covered host: %v", err)
	}
	if res.Bundle.User() != "root" || string(res.Bundle.SSHKeyRef()) != "file:/secrets/hostdiag_key" {
		t.Errorf("resolved bundle = user %q keyref %q", res.Bundle.User(), res.Bundle.SSHKeyRef())
	}
	// A host outside the allowlist fails closed — no default identity.
	if _, err := se.Resolve(credential.Target{Host: "dc2x"}); !credential.IsRefused(err) {
		t.Errorf("uncovered host must fail closed, got %v", err)
	}
}

// TestBuildSyncEngine_PartialConfigFailsClosed proves each half-configured source aborts the boot rather than
// silently dropping — a misconfigured credential source must never let actuation resolve a wrong/blank identity.
func TestBuildSyncEngine_PartialConfigFailsClosed(t *testing.T) {
	t.Cleanup(func() { openbao.RegisterResolver(nil) })
	cases := map[string]func(*CredentialConfig){
		"awx addr without token ref": func(c *CredentialConfig) {
			c.AWXAddr, c.AWXTokenRef = "https://awx.example", ""
		},
		"semaphore addr without token ref": func(c *CredentialConfig) {
			c.SemaphoreAddr, c.SemaphoreTokenRef = "http://sem.example:3000", ""
		},
		"openbao approle missing role/secret": func(c *CredentialConfig) {
			c.OpenBaoAddr, c.OpenBaoAuthMethod = "https://bao.example:8200", "approle"
			c.OpenBaoRoleIDRef, c.OpenBaoSecretIDRef = "", ""
		},
		"openbao unknown auth method": func(c *CredentialConfig) {
			c.OpenBaoAddr, c.OpenBaoAuthMethod = "https://bao.example:8200", "bogus"
		},
		"openbao token method missing token ref": func(c *CredentialConfig) {
			c.OpenBaoAddr, c.OpenBaoAuthMethod, c.OpenBaoTokenRef = "https://bao.example:8200", "token", ""
		},
		"ldap urls without bind refs": func(c *CredentialConfig) {
			c.LDAPURLs, c.LDAPBindDNRef, c.LDAPBindPWRef = "ldaps://ipa.example:636", "", ""
		},
		"awx non-integer inventory id": func(c *CredentialConfig) {
			c.AWXAddr, c.AWXTokenRef, c.AWXInventoryID = "https://awx.example", "env:AWX_TOKEN", "not-a-number"
		},
		"malformed native rules": func(c *CredentialConfig) {
			c.NativeRules = "this-is-not-a-valid-rule"
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			cfg := CredentialConfig{}
			mutate(&cfg)
			se, sources, err := BuildSyncEngine(cfg)
			if err == nil {
				t.Fatalf("expected fail-closed error, got engine=%v sources=%+v", se != nil, sources)
			}
			if se != nil || sources != nil {
				t.Errorf("on failure BuildSyncEngine must return (nil, nil, err), got engine=%v sources=%+v", se != nil, sources)
			}
		})
	}
}

// TestParseAWXCredRefMap covers the job-template cred-name → SecretRef map parser: valid ';'-separated pairs
// (including names/refs that contain spaces or a ':'), empty ⇒ nil (JT mode off), and malformed entries fail
// closed rather than silently dropping a binding.
func TestParseAWXCredRefMap(t *testing.T) {
	t.Run("valid", func(t *testing.T) {
		m, err := parseAWXCredRefMap("SSH ED25519 (one_key)=file:/secrets/one_key ; SSH Lab Common=file:/secrets/lab-common")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if m["SSH ED25519 (one_key)"] != "file:/secrets/one_key" || m["SSH Lab Common"] != "file:/secrets/lab-common" {
			t.Fatalf("parsed map wrong: %+v", m)
		}
	})
	t.Run("empty is nil (JT mode off)", func(t *testing.T) {
		if m, err := parseAWXCredRefMap("   "); err != nil || m != nil {
			t.Fatalf("empty must be (nil,nil), got (%+v,%v)", m, err)
		}
	})
	for _, bad := range []string{"noequalsign", "=file:/x", "name="} {
		t.Run("malformed/"+bad, func(t *testing.T) {
			if _, err := parseAWXCredRefMap(bad); err == nil {
				t.Fatalf("malformed entry %q must fail closed", bad)
			}
		})
	}
}
