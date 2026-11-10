package main

import "testing"

// REQ-2402 completeness regression guard: the boot secret-policy gate must ENUMERATE every grounder secret
// reference — the envConfig SecretRef fields AND the bootstrap/CA refs read inline at composition. A missing
// ref is a plaintext hole the gate cannot see (the review finding this test pins). The bootstrap/CA refs are
// exempt; the business secrets are not.
func TestSecretEntriesCompleteness(t *testing.T) {
	got := map[string]bool{} // name -> exempt
	for _, e := range (envConfig{}).secretEntries() {
		got[e.Name] = e.Exempt
	}
	businessRefs := []string{"TG_LITELLM_KEY_REF", "TG_SESSION_KEY_REF", "TG_OPERATOR_TOKEN_REF", "TG_ADMIN_TOKEN_REF", "TG_LIBRENMS_INGEST_TOKEN_REF"}
	for _, n := range businessRefs {
		exempt, present := got[n]
		if !present {
			t.Fatalf("business secret %s missing from the policy enumeration (plaintext hole)", n)
		}
		if exempt {
			t.Fatalf("business secret %s must NOT be exempt", n)
		}
	}
	// The bootstrap + CA refs must be enumerated AND exempt (they cannot come from the backend they
	// unseal/authenticate; the CA is public material).
	exemptRefs := []string{"TG_LDAP_CA", "TG_SEAL_KEY_REF", "TG_OPENBAO_TOKEN_REF", "TG_SEAL_TRANSIT_TOKEN_REF", "TG_OPENBAO_CA"}
	for _, n := range exemptRefs {
		exempt, present := got[n]
		if !present {
			t.Fatalf("bootstrap/CA ref %s missing from the policy enumeration (REQ-2402 completeness)", n)
		}
		if !exempt {
			t.Fatalf("bootstrap/CA ref %s must be exempt", n)
		}
	}
}
