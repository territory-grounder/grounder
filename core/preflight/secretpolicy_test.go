package preflight

import (
	"strings"
	"testing"

	"github.com/territory-grounder/grounder/core/config"
)

func TestParseSecretPolicy(t *testing.T) {
	cases := map[string]SecretPolicy{
		"": PolicyOff, "off": PolicyOff, "OFF": PolicyOff, "nonsense": PolicyOff,
		"warn": PolicyWarn, "Warn": PolicyWarn, "enforce": PolicyEnforce, "ENFORCE": PolicyEnforce,
	}
	for in, want := range cases {
		if got := ParseSecretPolicy(in); got != want {
			t.Fatalf("ParseSecretPolicy(%q) = %v, want %v", in, got, want)
		}
	}
	// An unknown value must be Off, never accidentally Enforce or Warn (a typo can't change the posture).
	if ParseSecretPolicy("enfroce") != PolicyOff {
		t.Fatal("a typo must default to off, not enforce")
	}
}

// A plaintext (env:) non-exempt business secret is a violation; a backend ref is compliant; an exempt
// plaintext ref is allowed-but-surfaced; an empty (unset) ref is skipped.
func TestCheckSecretPolicyClassifies(t *testing.T) {
	entries := []SecretEntry{
		{Name: "admin-token", Ref: config.SecretRef("env:TG_ADMIN_TOKEN")},               // violation
		{Name: "netbox", Ref: config.SecretRef("bao:secret/data/tg/netbox#token")},       // compliant
		{Name: "seal", Ref: config.SecretRef("store:tg-seal")},                           // compliant
		{Name: "ssh-key", Ref: config.SecretRef("file:/secrets/one_key")},                // violation (file: not a backend)
		{Name: "runtime-dsn", Ref: config.SecretRef("env:TG_RUNTIME_DSN"), Exempt: true}, // exempt
		{Name: "optional-off", Ref: config.SecretRef("")},                                // skipped (unset)
		{Name: "inline", Ref: config.SecretRef("s.abcdef123456")},                        // violation (literal)
	}
	rep := CheckSecretPolicy(entries)
	gotV := map[string]string{}
	for _, v := range rep.Violations {
		gotV[v.Name] = v.Scheme
	}
	if len(gotV) != 3 || gotV["admin-token"] != "env" || gotV["ssh-key"] != "file" || gotV["inline"] != "literal" {
		t.Fatalf("violations wrong: %+v", rep.Violations)
	}
	if len(rep.Exempted) != 1 || !strings.Contains(rep.Exempted[0], "runtime-dsn") {
		t.Fatalf("exempted must carry the exempt plaintext ref, got %+v", rep.Exempted)
	}
	if rep.Clean() {
		t.Fatal("a report with violations must not be Clean")
	}
}

func TestEnforceSecretPolicy(t *testing.T) {
	rep := CheckSecretPolicy([]SecretEntry{{Name: "admin-token", Ref: config.SecretRef("env:X")}})
	// off + warn never fatal; enforce fatals on a violation.
	if err := EnforceSecretPolicy(PolicyOff, rep); err != nil {
		t.Fatalf("off must never fail: %v", err)
	}
	if err := EnforceSecretPolicy(PolicyWarn, rep); err != nil {
		t.Fatalf("warn must never fail: %v", err)
	}
	err := EnforceSecretPolicy(PolicyEnforce, rep)
	if err == nil {
		t.Fatal("enforce must fail on a violation")
	}
	// The fatal error names the ref + scheme but never a value.
	if !strings.Contains(err.Error(), "admin-token") || strings.Contains(err.Error(), "env:X") {
		t.Fatalf("error must name the ref, not the value: %v", err)
	}
	// A clean report never fails, even under enforce.
	clean := CheckSecretPolicy([]SecretEntry{{Name: "netbox", Ref: config.SecretRef("bao:x#y")}})
	if err := EnforceSecretPolicy(PolicyEnforce, clean); err != nil {
		t.Fatalf("enforce on a clean report must pass: %v", err)
	}
}

// A dead / unset optional backend secret is not a plaintext violation (the feature is off, not insecure).
func TestUnsetSecretIsNotAViolation(t *testing.T) {
	rep := CheckSecretPolicy([]SecretEntry{{Name: "optional", Ref: config.SecretRef("")}})
	if !rep.Clean() {
		t.Fatalf("an unset optional secret must not be a violation, got %+v", rep.Violations)
	}
}
