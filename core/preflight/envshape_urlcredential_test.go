package preflight

// THE SHAPE RULE'S OWN BLIND SPOT.
//
// envshape.go exists because the enumerating gate could only see what someone remembered to declare. Its
// header says so: "a control reporting success over the exact condition it exists to detect". The shape rule
// closed that by deciding from the NAME — TOKEN/KEY/PASS/PASSWORD/SECRET — and needing nothing declared.
//
// A connection string declares its credential STRUCTURALLY instead. `TG_DB_DSN` matches none of those
// tokens, so on dc1tg01 the live value
//
//	postgres://tg_runtime:<password>@postgres:5432/grounder?sslmode=disable
//
// was never in the scanned population at all, and the worker printed, truthfully and uselessly:
//
//	secret policy=enforce: env shape scan: 144 var(s) scanned, 14 secret-shaped,
//	0 raw plaintext violation(s)
//
// Same class of blind spot, one level down. These tests pin the new structural eye and — just as importantly
// — pin that it does NOT yet refuse anything, because TG_SECRET_POLICY=enforce is live and a gate that
// starts refusing before its subject is migrated takes the deployment down at the next restart.

import (
	"strings"
	"testing"
)

// THE LIVE SHAPE, verbatim, as the primary fixture. A rule written against an invented example is a rule
// written against the author's idea of the problem.
//
// KILLING MUTATION: delete the HasInlineURLCredential branch from CheckEnvShape (the state before this
// change). RED — a live database password sits in the process environment and the gate says nothing.
func TestADSNPasswordIsSeenEvenThoughItsNameDeclaresNothing(t *testing.T) {
	rep := CheckEnvShape([]string{
		"TG_DB_DSN=postgres://tg_runtime:s3cr3t@postgres:5432/grounder?sslmode=disable",
		"TG_RUNTIME_DSN=postgres://tg_runtime:s3cr3t@postgres:5432/grounder?sslmode=disable",
	}, nil)

	if len(rep.InlineURLCredential) != 2 {
		t.Fatalf("the scan reported %d inline-URL credential(s), want 2 — a database password in the "+
			"process environment is invisible to a rule that decides from the variable NAME: %v",
			len(rep.InlineURLCredential), rep.InlineURLCredential)
	}
	// Neither name is secret-shaped, so this must NOT have gone through the name path.
	if rep.Shaped != 0 {
		t.Errorf("Shaped = %d, want 0 — these names declare no credential, and counting them as shaped "+
			"would mean the finding came from the rule that misses them", rep.Shaped)
	}
	// AND IT MUST NOT REFUSE. enforce is live; promoting this to a violation today fails the boot.
	if len(rep.Violations) != 0 {
		t.Fatalf("an inline-URL credential was raised as a VIOLATION (%d). TG_SECRET_POLICY=enforce is live "+
			"on this deployment, so this would refuse the worker's boot on the next restart, before the DSN "+
			"has anywhere else to live", len(rep.Violations))
	}
}

// The report must SAY it has not refused. A gate that names a credential in its output and stays silent
// about its own disposition reads as a refusal that happened.
func TestTheSummarySaysItHasNotRefusedTheURLCredential(t *testing.T) {
	rep := SecretPolicyReport{EnvScan: CheckEnvShape([]string{
		"TG_DB_DSN=postgres://u:p@postgres:5432/grounder",
	}, nil)}
	got := rep.EnvScanSummary()
	for _, want := range []string{"TG_DB_DSN", "has NOT refused", "behind a reference"} {
		if !strings.Contains(got, want) {
			t.Errorf("the summary does not contain %q, so a reader cannot tell whether the gate acted on "+
				"what it just named.\nGot: %s", want, got)
		}
	}
	// The VALUE must never appear. Same rule the rest of this file lives by.
	if strings.Contains(got, ":p@") || strings.Contains(got, "p@postgres") {
		t.Fatalf("the summary echoed the credential: %s", got)
	}
}

// PRECISION. A rule that fires on ordinary URLs would be switched off within a week, and an unfalsifiable
// rule is worse than none — the file's own reason for refusing an entropy heuristic.
//
// KILLING MUTATION: return true whenever the value contains "://" and "@". RED on the path and no-password
// cases below.
func TestTheURLRuleDoesNotFireOnOrdinaryValues(t *testing.T) {
	for _, c := range []struct {
		label string
		value string
		want  bool
	}{
		{"password present", "postgres://user:pw@host:5432/db", true},
		{"amqp with password", "amqp://svc:hunter2@rabbit:5672/", true},
		{"no password component", "postgres://user@host:5432/db", false},
		{"plain https", "https://awx.example.net/api/v2/ping/", false},
		{"at-sign inside the PATH, not userinfo", "https://host/mail/a:b@example.com", false},
		{"no scheme separator at all", "user:password@host", false},
		{"empty password after the colon", "postgres://user:@host/db", false},
		{"a SecretRef, not a URL", "bao:secret/data/tg/pve#token", false},
		{"empty", "", false},
	} {
		rep := CheckEnvShape([]string{"TG_SOMETHING=" + c.value}, nil)
		got := len(rep.InlineURLCredential) == 1
		if got != c.want {
			t.Errorf("%s: flagged=%v want=%v (value shape %q)", c.label, got, c.want, c.value)
		}
	}
}

// NO DOUBLE-COUNTING. A variable the NAME rule already catches must be reported once, as the violation it
// is — not also as an inline-URL finding, which would inflate the count an operator works through and make
// the two dispositions look like two separate credentials.
func TestASecretShapedNameIsNotAlsoCountedAsAURLCredential(t *testing.T) {
	rep := CheckEnvShape([]string{
		"SOME_PASSWORD_URL=postgres://user:pw@host/db",
	}, nil)
	if len(rep.InlineURLCredential) != 0 {
		t.Errorf("a secret-SHAPED name was also counted as an inline-URL credential (%v) — one credential, "+
			"two dispositions", rep.InlineURLCredential)
	}
	if len(rep.Violations) != 1 {
		t.Errorf("the name rule should have raised exactly 1 violation, got %d — this test is not exercising "+
			"the overlap it claims to", len(rep.Violations))
	}
}

// VACUITY FLOOR. An environment with no credential-bearing URL must produce an empty list AND a non-zero
// scan, so "nothing found" can never be confused with "nothing looked at".
func TestACleanEnvironmentStillProvesTheScanRan(t *testing.T) {
	rep := CheckEnvShape([]string{
		"TG_PVE_URL=https://dc1pve01:8006",
		"TG_LOG_LEVEL=info",
	}, nil)
	if len(rep.InlineURLCredential) != 0 {
		t.Fatalf("clean environment produced findings: %v", rep.InlineURLCredential)
	}
	if rep.Scanned != 2 {
		t.Fatalf("Scanned = %d, want 2 — an empty finding list over an empty scan says nothing", rep.Scanned)
	}
}
