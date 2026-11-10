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
		{Name: "admin-token", Ref: config.SecretRef("env:TG_ADMIN_TOKEN")},         // violation
		{Name: "netbox", Ref: config.SecretRef("bao:secret/data/tg/netbox#token")}, // compliant
		{Name: "seal", Ref: config.SecretRef("store:tg-seal")},                     // compliant
		{Name: "ssh-key", Ref: config.SecretRef("file:/secrets/one_key")},          // violation (file: not a backend)
		// A REAL member of the closed exemption set. This fixture used to be a made-up "runtime-dsn",
		// which is how the exemption looked unconditional: any name could claim it. It also encoded a
		// claim the code never had — the policy doc said database connection strings were exempt, and no
		// production entry has ever marked a DSN exempt.
		{Name: "TG_OPENBAO_TOKEN_REF", Ref: config.SecretRef("env:TG_OPENBAO_TOKEN"), Exempt: true},
		{Name: "optional-off", Ref: config.SecretRef("")},         // skipped (unset)
		{Name: "inline", Ref: config.SecretRef("s.abcdef123456")}, // violation (literal)
	}
	rep := CheckSecretPolicy(entries)
	gotV := map[string]string{}
	for _, v := range rep.Violations {
		gotV[v.Name] = v.Scheme
	}
	if len(gotV) != 3 || gotV["admin-token"] != "env" || gotV["ssh-key"] != "file" || gotV["inline"] != "literal" {
		t.Fatalf("violations wrong: %+v", rep.Violations)
	}
	if len(rep.Exempted) != 1 || !strings.Contains(rep.Exempted[0], "TG_OPENBAO_TOKEN_REF") {
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

// TestEnforceErrorCitesTheDeploymentsOwnWorkingReference pins the fix for a trap this repo shipped and
// then walked into on 2026-08-02, live.
//
// TWENTY-ONE business secrets default to `env:<NAME>` in cmd/worker, and deploy/.env.example documents
// eighteen of them that way. On a deployment running policy=enforce — which the live one does — following
// the shipped documentation is a FATAL boot. Configuring the Matrix notifier with the documented
// `TG_MATRIX_TOKEN_REF=env:MATRIX_TOKEN` would have refused to start the worker.
//
// The gate was right to refuse. What it did not do was say what to write instead: it named the offending
// keys and told the operator to "move them to a secret backend", leaving them to invent the exact path.
// The deployment nearly always already contains the answer — another secret that IS on a backend — so the
// error now cites it.
//
// KILLING MUTATION: drop the Exemplar branch from EnforceSecretPolicy so it always emits the generic
// shape. RED.
func TestEnforceErrorCitesTheDeploymentsOwnWorkingReference(t *testing.T) {
	rep := CheckSecretPolicy([]SecretEntry{
		{Name: "TG_YOUTRACK_TOKEN_REF", Ref: "bao:secret/data/tg/youtrack#token"},
		{Name: "TG_MATRIX_TOKEN_REF", Ref: "env:MATRIX_TOKEN"},
	})
	if len(rep.Violations) != 1 {
		t.Fatalf("want exactly the matrix violation, got %+v", rep.Violations)
	}
	if rep.Exemplar != "TG_YOUTRACK_TOKEN_REF=bao:secret/data/tg/youtrack#token" {
		t.Fatalf("the compliant reference was not captured as an exemplar: %q", rep.Exemplar)
	}
	err := EnforceSecretPolicy(PolicyEnforce, rep)
	if err == nil {
		t.Fatal("a plaintext business secret did not fail under enforce")
	}
	msg := err.Error()
	if !strings.Contains(msg, "TG_MATRIX_TOKEN_REF") {
		t.Errorf("the error does not name the offending key: %s", msg)
	}
	// THE POINT: the operator is shown a concrete, working reference from their own deployment.
	if !strings.Contains(msg, "bao:secret/data/tg/youtrack#token") {
		t.Errorf("the error gives a diagnosis but no instruction — it must cite the deployment's own "+
			"already-working backend reference so the fix can be copied: %s", msg)
	}
}

// With NOTHING yet on a backend there is no exemplar to cite, and the error must still say what shape to
// write — otherwise a first-time enforce deployment gets the least help exactly when it needs the most.
func TestEnforceErrorFallsBackToTheGenericShape(t *testing.T) {
	rep := CheckSecretPolicy([]SecretEntry{{Name: "TG_MATRIX_TOKEN_REF", Ref: "env:MATRIX_TOKEN"}})
	if rep.Exemplar != "" {
		t.Fatalf("no compliant reference exists, so there must be no exemplar: %q", rep.Exemplar)
	}
	msg := EnforceSecretPolicy(PolicyEnforce, rep).Error()
	if !strings.Contains(msg, "bao:") || !strings.Contains(msg, "kv-mount") {
		t.Errorf("without an exemplar the error must still show the reference SHAPE: %s", msg)
	}
}

// An exemplar is a LOCATION, never a value. This is the property that makes citing one safe at all.
func TestExemplarCarriesAReferenceNotASecret(t *testing.T) {
	rep := CheckSecretPolicy([]SecretEntry{
		{Name: "TG_A_TOKEN_REF", Ref: "bao:secret/data/tg/a#token"},
		{Name: "TG_B_TOKEN_REF", Ref: "env:B"},
	})
	// Deterministic: the FIRST compliant name, so the message does not reshuffle between boots.
	if rep.Exemplar != "TG_A_TOKEN_REF=bao:secret/data/tg/a#token" {
		t.Fatalf("exemplar not deterministic/first-by-name: %q", rep.Exemplar)
	}
	if strings.Contains(rep.Exemplar, "MATRIX_TOKEN=") {
		t.Fatal("the exemplar must never carry a resolved value")
	}
}
