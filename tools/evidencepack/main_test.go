package main

import "testing"

// Usage errors are caught before anything connects or writes; a ref is held to the pack's own file-name rule.
func TestCandidateAndRefValidation(t *testing.T) {
	if _, err := parseCandidates([]string{"triage-protocol=290", "k8s-triage=12"}); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{"triage-protocol", "=290", "x=0", "x=abc", "x=12abc", " =3"} {
		if _, err := parseCandidates([]string{bad}); err == nil {
			t.Fatalf("%q must be refused", bad)
		}
	}
	for _, bad := range []string{"", "..", "../x", "a/b", "a\\b", "with\nnewline"} {
		if err := safeRef(bad); err == nil {
			t.Fatalf("ref %q must be refused", bad)
		}
	}
	if err := safeRef("librenms-dc1-192097"); err != nil {
		t.Fatal(err)
	}
}
