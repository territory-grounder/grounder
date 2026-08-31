package main

import (
	"os"
	"regexp"
	"testing"
)

// TestWorkerSecretEntriesCompleteness is the drift guard for REQ-2402: it scans this binary's OWN source for
// every secret reference it reads — the definitive config.SecretRef(getenv("X")) pattern AND the getenv("X_REF")
// convention — and asserts each is enumerated in workerSecretEntries(). A newly-added SecretRef that escapes
// the enumeration would be a plaintext hole the boot gate cannot see; this test fails the build instead. (The
// exact review finding that caught the grounder gap, here automated so the worker's ~30-ref surface cannot
// silently drift.)
func TestWorkerSecretEntriesCompleteness(t *testing.T) {
	src, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatalf("read worker source: %v", err)
	}
	// Every var the worker reads as a secret: a config.SecretRef(getenv("X"…)) read, or a getenv("X_REF"…).
	read := map[string]bool{}
	for _, re := range []*regexp.Regexp{
		regexp.MustCompile(`config\.SecretRef\(getenv\("([A-Z_]+)"`),
		regexp.MustCompile(`getenv\("([A-Z_]+_REF)"`),
	} {
		for _, m := range re.FindAllStringSubmatch(string(src), -1) {
			read[m[1]] = true
		}
	}
	enumerated := map[string]bool{}
	for _, e := range workerSecretEntries(func(string) string { return "" }) {
		enumerated[e.Name] = true
	}
	// A few getenv("X_REF") hits are NOT secret references (they name a resolved value var inside a comment or
	// a default string); the allowlist records the ones intentionally not policed so a real omission still fails.
	notASecret := map[string]bool{
		"TG_PVE_API_TOKEN_REF": true, // appears only as a default-scheme target, not a read ref
	}
	var missing []string
	for v := range read {
		if !enumerated[v] && !notASecret[v] {
			missing = append(missing, v)
		}
	}
	if len(missing) > 0 {
		t.Fatalf("worker reads %d secret reference(s) NOT in workerSecretEntries() (plaintext holes — add them, exempt if bootstrap/public): %v", len(missing), missing)
	}
}

// The bootstrap/public refs must be exempt; the core business secrets must not be.
func TestWorkerSecretEntriesExemptions(t *testing.T) {
	exempt := map[string]bool{}
	business := map[string]bool{}
	for _, e := range workerSecretEntries(func(string) string { return "" }) {
		if e.Exempt {
			exempt[e.Name] = true
		} else {
			business[e.Name] = true
		}
	}
	for _, n := range []string{"TG_OPENBAO_TOKEN_REF", "TG_OPENBAO_ROLE_ID_REF", "TG_OPENBAO_SECRET_ID_REF", "TG_OPENBAO_JWT_REF", "TG_LDAP_CA"} {
		if !exempt[n] {
			t.Fatalf("%s must be exempt (substrate bootstrap / public material)", n)
		}
	}
	for _, n := range []string{"TG_NETBOX_TOKEN_REF", "TG_PVE_TOKEN_REF", "TG_AWX_TOKEN_REF", "TG_LITELLM_KEY_REF", "TG_LDAP_BIND_PW"} {
		if !business[n] {
			t.Fatalf("%s must be a policed business secret (not exempt)", n)
		}
	}
}
