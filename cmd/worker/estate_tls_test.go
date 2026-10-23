package main

import (
	"crypto/tls"
	"net/http"
	"testing"
)

// estateHTTPClient defaults to strict verification and only disables it when explicitly opted in.
func TestEstateHTTPClientTLSPolicy(t *testing.T) {
	// Strict (default) path: TLS verification stays ON — it must NOT install an InsecureSkipVerify
	// transport (a nil Transport falls back to the verifying default). It now ALWAYS carries a timeout so a
	// slow estate endpoint can no longer hang the refresh forever (the old http.DefaultClient had none).
	strict := estateHTTPClient(false)
	if tr, ok := strict.Transport.(*http.Transport); ok && tr.TLSClientConfig != nil && tr.TLSClientConfig.InsecureSkipVerify {
		t.Fatalf("strict client must verify TLS, got InsecureSkipVerify=true")
	}
	if strict.Timeout <= 0 {
		t.Fatalf("strict client must set a bounded timeout (no infinite-hang), got %v", strict.Timeout)
	}
	// Opt-in insecure path: skip verification (self-signed internal estate endpoint), still bounded.
	c := estateHTTPClient(true)
	tr, ok := c.Transport.(*http.Transport)
	if !ok || tr.TLSClientConfig == nil || !tr.TLSClientConfig.InsecureSkipVerify {
		t.Fatalf("opt-in insecure client must skip verification, got %+v", c.Transport)
	}
	if c.Timeout <= 0 {
		t.Fatalf("insecure client must set a bounded timeout, got %v", c.Timeout)
	}
	var _ = tls.Config{} // the opt-in path must reference crypto/tls
}

func TestTruthyEnv(t *testing.T) {
	for _, v := range []string{"1", "true", "TRUE", "Yes", "on"} {
		t.Setenv("TG_TEST_FLAG", v)
		if !truthyEnv("TG_TEST_FLAG") {
			t.Fatalf("%q must be truthy", v)
		}
	}
	for _, v := range []string{"", "0", "false", "no", "off", "maybe"} {
		t.Setenv("TG_TEST_FLAG", v)
		if truthyEnv("TG_TEST_FLAG") {
			t.Fatalf("%q must be falsy", v)
		}
	}
}
