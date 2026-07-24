package credential

import (
	"fmt"
	"strings"
	"testing"

	"github.com/territory-grounder/grounder/core/config"
)

// unregister clears a scheme so the process-global resolver registry does not leak between tests.
func unregister(scheme string) { config.RegisterSchemeResolver(scheme, nil) }

func TestDeliveryDisabledIsBehaviourPreserving(t *testing.T) {
	c := DeliveryConfig{} // no address ⇒ substrate OFF
	if c.Enabled() {
		t.Fatal("empty address must be disabled")
	}
	if err := c.Validate(); err != nil {
		t.Fatalf("disabled config must validate: %v", err)
	}
	// Register is a no-op; a bao: reference must still fail closed (unregistered scheme), never empty.
	if err := c.Register("bao", func(string) (string, error) { return "x", nil }, nil); err != nil {
		t.Fatalf("disabled Register must not error: %v", err)
	}
	if _, err := config.SecretRef("bao:secret/data/tg/x#k").Resolve(); err == nil {
		t.Fatal("bao: reference must fail closed when the substrate is disabled")
	}
	// env: references keep working regardless.
	t.Setenv("TG_TEST_DELIVERY_ENVVAR", "plain")
	if v, err := config.SecretRef("env:TG_TEST_DELIVERY_ENVVAR").Resolve(); err != nil || v != "plain" {
		t.Fatalf("env: reference must be unaffected, got %q err=%v", v, err)
	}
}

func TestDeliveryValidateFailClosed(t *testing.T) {
	cases := []struct {
		name    string
		cfg     DeliveryConfig
		wantErr string
	}{
		{"enabled but no token", DeliveryConfig{Addr: "https://bao:8200"}, "bootstrap token"},
		{"token from the substrate itself", DeliveryConfig{Addr: "https://bao:8200", TokenRef: "bao:secret/data/tg/self#t"}, "env: or file:"},
		{"token via store scheme", DeliveryConfig{Addr: "https://bao:8200", TokenRef: "store:tok"}, "env: or file:"},
		{"env token ok", DeliveryConfig{Addr: "https://bao:8200", TokenRef: "env:TG_OPENBAO_TOKEN"}, ""},
		{"file token ok", DeliveryConfig{Addr: "https://bao:8200", TokenRef: "file:/secrets/bao-token"}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.cfg.Validate()
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("want ok, got %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("want error containing %q, got %v", tc.wantErr, err)
			}
		})
	}
}

func TestDeliveryEnabledRequiresInjectedResolver(t *testing.T) {
	c := DeliveryConfig{Addr: "https://bao:8200", TokenRef: "env:TG_OPENBAO_TOKEN"}
	if err := c.Register("bao", nil, nil); err == nil {
		t.Fatal("enabled substrate with a nil resolver must fail closed")
	}
	// nothing should have been registered
	if _, err := config.SecretRef("bao:secret/data/tg/x#k").Resolve(); err == nil {
		t.Fatal("bao: must still fail closed after a refused Register")
	}
}

// REQ-2200: with the substrate enabled, a high-value secret referenced as a bao: SecretRef resolves FROM the
// substrate (the injected resolver), not from a plaintext environment value.
func TestDeliveryResolvesFromSubstrate(t *testing.T) {
	const scheme = "bao"
	defer unregister(scheme)
	called := ""
	resolver := func(ref string) (string, error) { called = ref; return "s3cr3t-from-openbao", nil }
	c := DeliveryConfig{Addr: "https://bao:8200", TokenRef: "env:TG_OPENBAO_TOKEN"}
	if err := c.Register(scheme, resolver, nil); err != nil {
		t.Fatalf("Register: %v", err)
	}
	ref := config.SecretRef("bao:secret/data/tg/litellm#master_key")
	got, err := ref.Resolve()
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if got != "s3cr3t-from-openbao" {
		t.Fatalf("want substrate value, got %q", got)
	}
	if called != string(ref) {
		t.Fatalf("resolver must receive the full ref, got %q", called)
	}
}

// REQ-2204: an unresolvable credential refuses with no plaintext fallback — the resolver's error propagates
// and the returned value is empty (never a degraded default).
func TestDeliveryUnresolvableRefusesNoFallback(t *testing.T) {
	const scheme = "bao"
	defer unregister(scheme)
	resolver := func(string) (string, error) { return "", fmt.Errorf("openbao: 403 permission denied") }
	c := DeliveryConfig{Addr: "https://bao:8200", TokenRef: "env:TG_OPENBAO_TOKEN"}
	if err := c.Register(scheme, resolver, nil); err != nil {
		t.Fatalf("Register: %v", err)
	}
	got, err := config.SecretRef("bao:secret/data/tg/litellm#master_key").Resolve()
	if err == nil {
		t.Fatal("unresolvable bao: reference must refuse (error), not fall back")
	}
	if got != "" {
		t.Fatalf("refused resolution must return no value, got %q", got)
	}
}
