package notifier

import (
	"strings"
	"testing"
)

// Redact must mask credentials in every common shape a notice body carries — including quoted JSON keys
// (endemic to API error bodies) and a full "Bearer <token>" run — and email PII. Each case names a secret
// substring that MUST NOT survive redaction. (Regression for the review that found the JSON gap.)
func TestRedactMasksCredentialsAndPII(t *testing.T) {
	cases := map[string]string{
		"password=hunter2":                        "hunter2",
		"password: hunter2":                       "hunter2",
		`{"password":"hunter2"}`:                  "hunter2",
		`{"password": "hunter2"}`:                 "hunter2",
		`{"status":"error","api_key":"xyz12345"}`: "xyz12345",
		"x-api-key=abcdef123":                     "abcdef123",
		"Authorization: Bearer ghp_secrettoken":   "ghp_secrettoken",
		"token=deadbeefcafe":                      "deadbeefcafe",
		"contact ops@example.com now":             "ops@example.com",
	}
	for in, secret := range cases {
		out := Redact(in)
		if strings.Contains(out, secret) {
			t.Errorf("Redact(%q) = %q — still leaks %q", in, out, secret)
		}
		if !strings.Contains(out, "REDACTED") {
			t.Errorf("Redact(%q) = %q — expected a redaction marker", in, out)
		}
	}
}

func TestRedactLeavesCleanTextAlone(t *testing.T) {
	clean := "restart web01; cordon node then drain"
	if got := Redact(clean); got != clean {
		t.Errorf("Redact must not alter credential-free text: %q -> %q", clean, got)
	}
}

func TestAuthenticateFailsClosed(t *testing.T) {
	if Authenticate("", []string{"a"}) {
		t.Error("an empty sender must be denied")
	}
	if Authenticate("x", nil) {
		t.Error("an empty approver set must deny (fail closed)")
	}
	if Authenticate("x", []string{"a", "b"}) {
		t.Error("a non-member must be denied")
	}
	if !Authenticate("b", []string{"a", "b"}) {
		t.Error("a member must be accepted")
	}
}
