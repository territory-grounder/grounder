package config

import (
	"os"
	"testing"
)

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
