package preflight

import (
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/territory-grounder/grounder/core/config"
)

// The two credentials that were sitting in the live worker's process env on 2026-08-04, in plaintext, while
// TG_SECRET_POLICY=enforce and the boot gate reported green. Their LENGTH is recorded because the audit
// recorded it — nothing in the gate may key on it (see the no-heuristic note in envshape.go).
const (
	liveGRToken = "7f3a91c2e5b84d06a1f7c3e9b25d48fa6c07e13b9d52a8f4c6e0b73d19a5f28c" // 64 chars, LIBRENMS_GR_TOKEN
	liveNLToken = "b41d9e7a2c580f36a8d1e4b70c93f52a"                                 // 32 chars, LIBRENMS_TOKEN
)

// ★ THE ORACLE FOR TG-284: A RAW CREDENTIAL IN THE PROCESS ENV MUST FAIL THE BOOT.
//
// KILLING MUTATION (executed 2026-08-04): in CheckEnvShape, replace the final
//
//	rep.Violations = append(rep.Violations, SecretViolation{Name: name, Scheme: rawValueScheme, RawPlaintext: true})
//
// with `continue` — i.e. restore the pre-TG-284 behaviour where the gate only classified declared references.
// This test then fails with:
//
//	LIBRENMS_GR_TOKEN holds a raw 64-char API token in the process env and the gate returned CLEAN.
//	This is the exact live state on 2026-08-04: policy=enforce, boot green, credential in plaintext.
//
// which is the real-world consequence stated in the real-world terms: the control reports success over the
// condition it exists to detect.
func TestRawSecretShapedEnvVarIsAViolation(t *testing.T) {
	// The declared set is the one the live worker had: LibreNMS is configured, but through a compound
	// deployment variable that names NO env var these tokens live in. Nothing declares them. That is the
	// whole defect — the tokens are not merely unpoliced, they are invisible.
	declared := []SecretEntry{{Name: "TG_NETBOX_TOKEN_REF", Ref: config.SecretRef("bao:secret/data/tg/netbox#token")}}
	environ := []string{
		"LIBRENMS_GR_TOKEN=" + liveGRToken,
		"LIBRENMS_TOKEN=" + liveNLToken,
		"PATH=/usr/bin",
	}

	rep := CheckSecretPolicyWithEnv(declared, environ)
	if rep.Clean() {
		t.Fatalf("LIBRENMS_GR_TOKEN holds a raw %d-char API token in the process env and the gate returned "+
			"CLEAN. This is the exact live state on 2026-08-04: policy=enforce, boot green, credential in "+
			"plaintext.", len(liveGRToken))
	}
	got := map[string]SecretViolation{}
	for _, v := range rep.Violations {
		got[v.Name] = v
	}
	for _, name := range []string{"LIBRENMS_GR_TOKEN", "LIBRENMS_TOKEN"} {
		v, ok := got[name]
		if !ok {
			t.Fatalf("%s is a raw credential in the process env but the gate did not report it (reported: %v)", name, rep.Violations)
		}
		if !v.RawPlaintext {
			t.Errorf("%s must be flagged RawPlaintext — the remediation differs from an unmigrated reference: "+
				"the plaintext VARIABLE has to be deleted, not just repointed", name)
		}
	}

	// Under enforce this must be fatal, and the error must say what to do about it.
	err := EnforceSecretPolicy(PolicyEnforce, rep)
	if err == nil {
		t.Fatal("policy=enforce returned no error over two raw credentials in the process env — the boot would " +
			"proceed exactly as it did on the live box")
	}
	for _, want := range []string{"LIBRENMS_GR_TOKEN", "raw value in process env", "removed from the"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("enforce error must contain %q so the operator knows which variable and what to do; got: %v", want, err)
		}
	}

	// Under warn it must NOT be fatal (the migration window), but the report must still carry the finding.
	if err := EnforceSecretPolicy(PolicyWarn, rep); err != nil {
		t.Errorf("policy=warn must not fail the boot — it is the migration window the enforce error points at; got %v", err)
	}
	if len(rep.EnvScan.Violations) != 2 {
		t.Errorf("warn must still SEE both raw credentials so it can log them loudly; got %+v", rep.EnvScan.Violations)
	}
}

// ★ THE FALSE-POSITIVE SIDE OF THE SAME ORACLE. A gate that fails the boot over a key NAME, a file PATH and
// its own MODE string would be turned off within a day, and a control that gets turned off protects nothing.
//
// KILLING MUTATION (executed 2026-08-04): delete the `if _, excused := NotASecretEnvVars[name]` block from
// CheckEnvShape. This test then fails with the three live values named — tg-seal, /secrets/tg.key, enforce —
// none of which is a credential.
func TestAllowlistedNonSecretsDoNotTripTheGate(t *testing.T) {
	// The real live values, not invented ones: an allowlist justified by hypothetical inputs is unfalsifiable.
	environ := []string{
		"TG_SEAL_TRANSIT_KEY=tg-seal",             // an OpenBao Transit key NAME
		"TG_OPENBAO_KEY=/secrets/tg.key",          // a filesystem PATH
		"TG_SECRET_POLICY=enforce",                // this gate's own MODE
		"TG_OIDC_TOKEN_URL=https://idp/token",     // an endpoint URL
		"LANGFUSE_PUBLIC_KEY=pk-lf-00000000-0000", // the PUBLIC half of a key pair
	}
	rep := CheckSecretPolicyWithEnv(nil, environ)
	if !rep.Clean() {
		t.Fatalf("the gate failed over things that are not credentials: %+v. A key NAME, a PATH, a MODE, an "+
			"endpoint and public material are not secrets — this is the direction that gets the control "+
			"disabled, not the one that weakens it.", rep.Violations)
	}
	if len(rep.EnvScan.Allowlisted) != 5 {
		t.Errorf("all five must be RECORDED as allowlisted, not silently dropped — an exemption nobody can see "+
			"is the defect this ticket fixed; got %v", rep.EnvScan.Allowlisted)
	}
	if err := EnforceSecretPolicy(PolicyEnforce, rep); err != nil {
		t.Errorf("policy=enforce must boot cleanly over a deployment whose only secret-shaped vars are these; got %v", err)
	}
}

// No length or entropy heuristic: the SHORT value must be caught and the LONG allowlisted one must not.
// Stated as its own test because "just check if it looks random" is the design that was explicitly rejected,
// and a heuristic sneaking back in would pass every test above.
func TestClassificationIsStructuralNotLengthBased(t *testing.T) {
	rep := CheckSecretPolicyWithEnv(nil, []string{
		"ACME_TOKEN=x", // 1 char, unmistakably a raw credential by SHAPE
		"TG_SEAL_TRANSIT_KEY=tg-seal-transit-key-with-a-deliberately-long-name-0000000000",
	})
	if len(rep.Violations) != 1 || rep.Violations[0].Name != "ACME_TOKEN" {
		t.Fatalf("a 1-char raw token must be a violation and a 70-char key NAME must not — classification is "+
			"structural (name declares a credential + value is not a reference), never a length guess; got %+v",
			rep.Violations)
	}
}

// A value that IS a reference is a LOCATION, not a credential — for every scheme the resolvers accept.
// If this regressed, every correctly-migrated deployment would fail the boot, i.e. migrating to the vault
// would be punished by the gate that demanded it.
func TestReferenceValuedVarsAreNotViolations(t *testing.T) {
	var environ []string
	for _, scheme := range []string{"env", "file", "store", "bao", "vault", "oidc", "ansible-vault", "vw", "passbolt"} {
		environ = append(environ, "SOME_"+strings.ToUpper(strings.ReplaceAll(scheme, "-", "_"))+"_TOKEN="+scheme+":somewhere/else#field")
	}
	rep := CheckSecretPolicyWithEnv(nil, environ)
	if !rep.Clean() {
		t.Fatalf("reference-valued vars were reported as raw plaintext: %+v — these are the exact schemes the "+
			"gate tells operators to migrate TO", rep.Violations)
	}
	if rep.EnvScan.Shaped != len(environ) {
		t.Errorf("all %d should have been SHAPE-matched and then cleared by their scheme, not skipped before "+
			"the name test; shaped=%d", len(environ), rep.EnvScan.Shaped)
	}
}

// ★ THE RULE THAT KEEPS `enforce` SATISFIABLE ON THE REAL BOX.
//
// TG_SEAL_KEY_REF defaults to env:TG_SEAL_KEY, and the seal key is PERMANENTLY exempt — it unwraps what the
// backend returns, so it cannot come from the backend. Without inheritance the shape scan would flag the raw
// TG_SEAL_KEY, making enforce impossible to satisfy by construction. An unsatisfiable control gets switched
// off, which is how this gate came to be green over plaintext in the first place.
func TestExemptReferenceTargetInheritsTheExemption(t *testing.T) {
	declared := []SecretEntry{
		{Name: "TG_SEAL_KEY_REF", Ref: config.SecretRef("env:TG_SEAL_KEY"), Exempt: true},
		{Name: "TG_OPENBAO_TOKEN_REF", Ref: config.SecretRef("env:OPENBAO_TOKEN"), Exempt: true},
	}
	environ := []string{"TG_SEAL_KEY=" + liveNLToken, "OPENBAO_TOKEN=s.aaaaaaaaaaaaaaaaaaaa"}
	rep := CheckSecretPolicyWithEnv(declared, environ)
	if !rep.Clean() {
		t.Fatalf("the seal key and the OpenBao bootstrap token are permanently exempt — flagging the raw values "+
			"they resolve to makes enforce unsatisfiable and the whole control gets disabled; got %+v", rep.Violations)
	}
	if len(rep.EnvScan.InheritedExempt) != 2 {
		t.Errorf("both must be RECORDED as exempt-by-inheritance so the boot log shows them; got %v", rep.EnvScan.InheritedExempt)
	}

	// The exemption must NOT be assertable by a caller: an UNCLAIMED exemption inherits nothing, or any raw
	// credential could be excused by pointing a self-declared-exempt ref at it.
	rogue := []SecretEntry{{Name: "TG_ACTUATION_SSH_KEY", Ref: config.SecretRef("env:LIBRENMS_GR_TOKEN"), Exempt: true}}
	rep = CheckSecretPolicyWithEnv(rogue, []string{"LIBRENMS_GR_TOKEN=" + liveGRToken})
	if rep.Clean() {
		t.Fatal("a caller-asserted exemption laundered a raw credential clean — that is the mechanism by which " +
			"ANY secret could be excused")
	}
}

// One credential must be reported ONCE. TG_NETBOX_TOKEN_REF=env:NETBOX_TOKEN is a single secret reachable by
// two names; reporting both doubles the list the operator has to work through and makes the count meaningless.
func TestDeclaredReferenceTargetIsNotReportedTwice(t *testing.T) {
	declared := []SecretEntry{{Name: "TG_NETBOX_TOKEN_REF", Ref: config.SecretRef("env:NETBOX_TOKEN")}}
	rep := CheckSecretPolicyWithEnv(declared, []string{"NETBOX_TOKEN=" + liveNLToken})
	if len(rep.Violations) != 1 {
		t.Fatalf("one plaintext credential must produce one violation, not %d: %+v", len(rep.Violations), rep.Violations)
	}
	if rep.Violations[0].Name != "TG_NETBOX_TOKEN_REF" {
		t.Errorf("report it under the REFERENCE name, which is the thing the operator edits to fix it; got %q", rep.Violations[0].Name)
	}
	if len(rep.EnvScan.Accounted) != 1 {
		t.Errorf("the raw variable must still be RECORDED as accounted-for, so a reader can tell it was seen "+
			"rather than missed; got %v", rep.EnvScan.Accounted)
	}
}

// ★ INV-13: THE GATE MUST NEVER HANDLE, RETURN OR LOG A SECRET VALUE. A raw credential that happens to
// contain a colon is the trap — config.SchemeOf would return the text before it, which is a FRAGMENT OF THE
// SECRET, and the report prints what it is handed.
func TestNoFragmentOfAValueEverReachesTheReportOrTheError(t *testing.T) {
	const value = "abcd1234:efgh5678-the-rest-of-a-real-looking-token"
	rep := CheckSecretPolicyWithEnv(nil, []string{"ACME_API_TOKEN=" + value})
	if len(rep.Violations) != 1 {
		t.Fatalf("expected the raw token to be caught; got %+v", rep.Violations)
	}
	err := EnforceSecretPolicy(PolicyEnforce, rep)
	haystack := err.Error() + " " + rep.EnvScanSummary() + " " + rep.Violations[0].Name + " " + rep.Violations[0].Scheme
	for _, leak := range []string{value, "abcd1234", "efgh5678"} {
		if strings.Contains(haystack, leak) {
			t.Fatalf("the gate leaked %q from the secret VALUE into its own output. It is read by log "+
				"shippers and pasted into tickets; a gate that leaks the credential it is protecting is worse "+
				"than no gate. Output was: %s", leak, haystack)
		}
	}
	if rep.Violations[0].Scheme != rawValueScheme {
		t.Errorf("a raw value has no scheme — record the pseudo-scheme %q instead of the text before its first "+
			"colon; got %q", rawValueScheme, rep.Violations[0].Scheme)
	}
}

// An unset variable holds nothing, so it exposes nothing — the same rule CheckSecretPolicy applies to an
// empty ref. Without this, every `${FOO:-}` passthrough in docker-compose.yml (there are dozens) would fail
// the boot of a deployment that uses none of them.
func TestBlankValuesAreNotViolations(t *testing.T) {
	rep := CheckSecretPolicyWithEnv(nil, []string{"ACME_TOKEN=", "OTHER_KEY=   ", "THIRD_SECRET="})
	if !rep.Clean() {
		t.Fatalf("blank passthroughs are not plaintext credentials; got %+v", rep.Violations)
	}
	if rep.EnvScan.Scanned != 0 {
		t.Errorf("blank vars must not inflate the scanned count either — that would make the reach figure lie; got %d", rep.EnvScan.Scanned)
	}
}

// ★ VACUITY FLOOR. This gate FILTERS the whole process environment, and a filter that has silently stopped
// matching is this repo's most-repeated bug. "0 violations" out of 0 scanned and "0 violations" out of a
// fully-scanned environment must never read alike, so the reach is carried in the report and printed at boot.
//
// KILLING MUTATION (executed 2026-08-04): set `secretNameTokens = []string{}` — the matcher then matches
// nothing and every raw credential passes. Every raw-secret test above goes RED, and this one fails on
// Shaped == 0, which is the signal a human reading the boot log would need.
func TestScanReachIsCountedAndPrinted(t *testing.T) {
	environ := []string{"PATH=/usr/bin", "TG_MODE=live", "ACME_TOKEN=" + liveNLToken, "TG_SECRET_POLICY=enforce"}
	rep := CheckSecretPolicyWithEnv(nil, environ)
	if rep.EnvScan.Scanned != 4 {
		t.Errorf("all 4 non-blank vars must be counted as scanned; got %d", rep.EnvScan.Scanned)
	}
	if rep.EnvScan.Shaped != 2 {
		t.Fatalf("ACME_TOKEN and TG_SECRET_POLICY are secret-SHAPED names; got shaped=%d. Shaped==0 over a "+
			"non-empty environment means the matcher has stopped matching and the gate is dead.", rep.EnvScan.Shaped)
	}
	summary := rep.EnvScanSummary()
	for _, want := range []string{"4 var(s) scanned", "2 secret-shaped", "1 raw plaintext violation"} {
		if !strings.Contains(summary, want) {
			t.Errorf("the boot log must state the scan's REACH (%q) so a dead scan is visible without a debugger; got %q", want, summary)
		}
	}
}

// Every allowlist entry must still be matched by the shape rule and must carry a written reason. An entry the
// matcher no longer matches is DEAD COVER: it looks like a considered decision while excusing nothing, and
// the next person reads it as proof the name was thought about.
func TestAllowlistEntriesAreSecretShapedAndReasoned(t *testing.T) {
	if len(NotASecretEnvVars) == 0 {
		t.Fatal("the allowlist is empty — this test would then pass vacuously while proving nothing")
	}
	for name, reason := range NotASecretEnvVars {
		if !IsSecretShapedName(name) {
			t.Errorf("%s is allowlisted but the shape rule no longer matches it — the entry excuses nothing and "+
				"is dead cover; remove it or fix the rule", name)
		}
		if len(strings.TrimSpace(reason)) < 20 {
			t.Errorf("%s must say what it actually holds (a name, a path, a mode, public material); got %q", name, reason)
		}
	}
	// The allowlist must stay tiny. It is NOT the mechanism that keeps the live deployment bootable — the
	// structural accounting is — so growth here means someone is excusing credentials.
	if n := len(NotASecretEnvVars); n > 10 {
		t.Errorf("the non-secret allowlist holds %d names. It covers key NAMES, PATHS, MODES and public "+
			"material only; a list this size is an escape hatch for real credentials.", n)
	}
}

// ★ THE SCAN IS PINNED TO THE REAL DEPLOYMENT, NOT TO ITS OWN FIXTURES. Reading docker-compose.yml is the
// point: a test that re-declared the variable names would agree with itself and prove nothing about the
// environment the binaries actually boot in. This is the check that would have caught TG-284 in CI.
func TestTheLiveDeploymentsCredentialNamesAreSeenByTheShapeRule(t *testing.T) {
	b, err := os.ReadFile("../../deploy/docker-compose.yml")
	if err != nil {
		t.Fatalf("read the deployment: %v", err)
	}
	// Env keys inside a service's `environment:` block are indented six spaces in this file.
	envKey := regexp.MustCompile(`(?m)^      ([A-Z][A-Z0-9_]*):`)
	names := map[string]bool{}
	for _, m := range envKey.FindAllStringSubmatch(string(b), -1) {
		names[m[1]] = true
	}
	if len(names) == 0 {
		t.Fatal("parsed ZERO environment variables out of deploy/docker-compose.yml — the sweep would pass " +
			"vacuously and a plaintext credential would sail straight through it")
	}
	var shaped int
	for n := range names {
		if IsSecretShapedName(n) {
			shaped++
		}
	}
	if shaped == 0 {
		t.Fatalf("out of %d deployment variables NOT ONE is secret-shaped. The deployment carries dozens of "+
			"credentials, so the shape rule has stopped matching and this gate is inert.", len(names))
	}
	// The three the 2026-08-04 audit found in plaintext. If a rename ever makes one of these invisible again,
	// this fails rather than the gate quietly going green.
	for _, name := range []string{"LIBRENMS_TOKEN", "LIBRENMS_GR_TOKEN", "TG_AM_INGEST_TOKEN"} {
		if !names[name] {
			t.Errorf("%s is no longer set by deploy/docker-compose.yml — if it was renamed, the shape gate and "+
				"the *_REF wiring must follow it", name)
		}
		if !IsSecretShapedName(name) {
			t.Errorf("%s is one of the credentials this ticket exists for and the shape rule does not match it", name)
		}
	}
	// And the allowlist must describe THIS deployment, not a hypothetical one.
	for name := range NotASecretEnvVars {
		if !names[name] {
			t.Errorf("%s is allowlisted as a non-secret but no service sets it — an allowlist of names nobody "+
				"uses is unfalsifiable cover", name)
		}
	}
}

// HasReferenceScheme is the only way this package touches a value, so its scheme set must not drift from the
// resolvers'. A scheme missing here turns a correctly-migrated reference into a false "raw credential".
func TestEveryBackendSchemeCountsAsAReference(t *testing.T) {
	for _, ref := range []string{"bao:x#y", "vault:x#y", "store:x", "vw:x", "passbolt:x"} {
		if !config.IsBackendScheme(config.SecretRef(ref)) {
			t.Fatalf("%s is not a backend scheme — this test's premise has drifted", ref)
		}
		if !config.HasReferenceScheme(ref) {
			t.Errorf("%s resolves through a backend but the shape gate does not recognise it as a reference, so "+
				"a migrated deployment would be failed for holding plaintext it does not hold", ref)
		}
	}
}
