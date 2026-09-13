package preflight

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// envFrom builds a getenv accessor over a fixed map (the deployment env a binary would boot with).
func envFrom(kv map[string]string) func(string) string {
	return func(k string) string { return kv[k] }
}

// ★ THE CREDENTIALS NO BINARY DECLARED (TG-278). Before this, `grep -c TG_AM_INGEST_TOKEN_REF cmd/ core/`
// returned 0: the bearer was a plaintext literal with no reference variable in the tree, so no configuration
// could move it to a vault and the boot gate never looked at it.
//
// KILLING MUTATION (executed 2026-08-04): return `nil` from DeploymentSecretEntries. This test then fails
// with "the deployment holds TG_AM_INGEST_TOKEN in plaintext and no entry polices it" — the TG-278 state.
func TestDeploymentSecretEntriesPoliceTheUndeclaredCredentials(t *testing.T) {
	get := envFrom(map[string]string{
		"TG_AM_INGEST_TOKEN":      "64-chars-of-alertmanager-bearer",
		"TG_LIBRENMS_DEPLOYMENTS": "nl|https://nms.nl/api/v0|env:LIBRENMS_TOKEN;gr|https://nms.gr/api/v0|env:LIBRENMS_GR_TOKEN|Europe/Athens",
	})
	byName := map[string]SecretEntry{}
	for _, e := range DeploymentSecretEntries(get) {
		byName[e.Name] = e
	}

	am, ok := byName["TG_AM_INGEST_TOKEN_REF"]
	if !ok {
		t.Fatalf("the deployment holds TG_AM_INGEST_TOKEN in plaintext and no entry polices it — that is the "+
			"TG-278 state the boot gate reported green over. got %v", byName)
	}
	if string(am.Ref) != "env:TG_AM_INGEST_TOKEN" {
		t.Errorf("an unset _REF must default to env:<NAME> so an existing deployment resolves exactly as it "+
			"does today; got %q", am.Ref)
	}
	if am.Exempt {
		t.Error("the Alertmanager bearer is a BUSINESS secret — it can resolve from OpenBao, so it is unmigrated, not exempt")
	}

	// The LibreNMS tokens are policed where they actually live: the tokenref field of the compound
	// deployment variable, which is the reference path that ALREADY exists and is ALREADY read.
	for site, want := range map[string]string{"nl": "env:LIBRENMS_TOKEN", "gr": "env:LIBRENMS_GR_TOKEN"} {
		e, ok := byName["TG_LIBRENMS_DEPLOYMENTS["+site+"]"]
		if !ok {
			t.Fatalf("site %q's token reference is not enumerated — this is the 64/32-char plaintext pair the "+
				"live gate could not see. got %v", site, byName)
		}
		if string(e.Ref) != want {
			t.Errorf("site %q ref = %q, want %q", site, e.Ref, want)
		}
		if e.Exempt {
			t.Errorf("site %q's API token is a business secret; it resolves from OpenBao today", site)
		}
	}

	// And all of it must be RED under enforce, because every one of them is plaintext right now.
	rep := CheckSecretPolicy(DeploymentSecretEntries(get))
	if err := EnforceSecretPolicy(PolicyEnforce, rep); err == nil {
		t.Fatal("three plaintext business credentials produced a clean enforce — the gate is green over exactly " +
			"what it exists to catch")
	}
}

// ★ A GATE MUST NOT FAIL A PROCESS OVER A CREDENTIAL IT DOES NOT HOLD. TG_OPUS_SIDECAR_KEY lives in the
// LITELLM container and TG_AM_INGEST_TOKEN in the grounder; applying the env:<NAME> default unconditionally
// would fail the WORKER's boot over two secrets that are not in its environment and that its operator cannot
// fix there. Noise like that is how a control gets switched off.
func TestARefIsOnlyPolicedWhereTheCredentialActuallyIs(t *testing.T) {
	// A worker-shaped env: neither credential present, no LibreNMS configured.
	if got := DeploymentSecretEntries(envFrom(map[string]string{"PATH": "/usr/bin"})); len(got) != 0 {
		t.Fatalf("a process holding neither credential must be told about neither; got %v", got)
	}
	// Explicitly setting the ref counts as holding it, even with no raw value — that is the migrated state,
	// and it must still be enumerated so the report shows it resolving from the backend.
	got := DeploymentSecretEntries(envFrom(map[string]string{
		"TG_OPUS_SIDECAR_KEY_REF": "bao:secret/data/tg/claude-proxy#bearer",
	}))
	if len(got) != 1 || string(got[0].Ref) != "bao:secret/data/tg/claude-proxy#bearer" {
		t.Fatalf("an explicitly-set ref must be enumerated as configured; got %v", got)
	}
	if rep := CheckSecretPolicy(got); !rep.Clean() {
		t.Errorf("a bao: reference is the migrated end state and must pass; got %+v", rep.Violations)
	}
}

// An unconfigured LibreNMS contributes nothing (the feature is simply off), and a malformed row contributes
// nothing rather than a bogus entry — but a row WITH a reference must always yield one, or the compound
// variable becomes another place credentials hide.
func TestLibrenmsDeploymentEntriesParsing(t *testing.T) {
	for _, tc := range []struct {
		name, spec string
		want       int
	}{
		{"unconfigured", "", 0},
		{"blank", "   ", 0},
		{"row without a token field", "nl|https://nms.nl/api/v0", 0},
		{"row with an empty token field", "nl|https://nms.nl/api/v0|", 0},
		{"one site", "nl|https://nms.nl/api/v0|env:LIBRENMS_TOKEN", 1},
		{"two sites with timezone", "nl|https://a|env:A;gr|https://b|bao:x#y|Europe/Athens", 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := LibrenmsDeploymentEntries(tc.spec); len(got) != tc.want {
				t.Fatalf("got %d entries, want %d: %v", len(got), tc.want, got)
			}
		})
	}
	// The site must be NAMED, or a violation cannot tell the operator which site's token is still plaintext.
	got := LibrenmsDeploymentEntries("gr|https://b|env:LIBRENMS_GR_TOKEN")
	if len(got) != 1 || !strings.Contains(got[0].Name, "gr") {
		t.Fatalf("the entry name must identify the site; got %v", got)
	}
}

// ★ THE WIRING PROOF FOR TG_AM_INGEST_TOKEN_REF. The reason the LibreNMS pair did NOT get a *_REF knob is
// that nothing would read it — and the same objection applies to the two knobs that WERE added, so each must
// have a real consumer. An operator who sets one of these to bao: and gets a green gate with an unchanged
// credential is worse off than before it existed.
//
// KILLING MUTATION (executed 2026-08-04): delete the provisionAlertmanagerPushAuth(...) call from
// cmd/grounder/main.go. This test then fails with "TG_AM_INGEST_TOKEN_REF is declared but nothing reads it".
func TestEveryLiteralOnlyRefHasARealConsumer(t *testing.T) {
	if len(LiteralOnlySecretRefs) == 0 {
		t.Fatal("no refs declared — this sweep would pass vacuously")
	}
	// Each ref names the consumer that must read it, and where. These are REGEXPS, not substrings, and the
	// Go one is anchored to a CALL at statement position (`^\s*name(`) rather than just the identifier —
	// because `func provisionAlertmanagerPushAuth(ctx …` contains the identifier too, so a substring check
	// stayed green when the call was deleted from main() and the function left orphaned. That failure was
	// found by executing the mutation, and it is this repo's own defect in miniature: a gate that matches the
	// definition of the thing instead of its use.
	consumers := map[string]struct{ file, callSite string }{
		// The grounder boot provisions the prometheus-alertmanager sources row from this ref.
		"TG_AM_INGEST_TOKEN_REF": {"../../cmd/grounder/main.go", `(?m)^\s+provisionAlertmanagerPushAuth\(ctx,`},
		// The litellm-secrets init resolves this ref into the litellm container's env file (REQ-2403 — the
		// third-party-container case the in-process gate structurally cannot cover).
		"TG_OPUS_SIDECAR_KEY_REF": {"../../deploy/docker-compose.yml", `(?m)^\s+- TG_OPUS_SIDECAR_KEY=\$\{TG_OPUS_SIDECAR_KEY_REF`},
	}
	for _, r := range LiteralOnlySecretRefs {
		c, ok := consumers[r.Name]
		if !ok {
			t.Errorf("%s was added with no named consumer. A reference knob nothing resolves is worse than no "+
				"knob: setting it to bao: would give a green gate and an unmoved credential.", r.Name)
			continue
		}
		b, err := os.ReadFile(c.file)
		if err != nil {
			t.Fatalf("read %s: %v", c.file, err)
		}
		if !regexp.MustCompile(c.callSite).MatchString(string(b)) {
			t.Errorf("%s is declared but nothing CALLS it: %s no longer matches %q. The knob is inert and an "+
				"operator migrating this credential to the vault would silently keep using the plaintext.",
				r.Name, c.file, c.callSite)
		}
		if !strings.Contains(string(b), r.Name) {
			t.Errorf("%s does not appear in %s at all — it cannot be configured where it is consumed", r.Name, c.file)
		}
	}
}

// The consumer and the gate must read the SAME default, or the knob means two different things at the two
// ends of one MR. DefaultRefFor is the single source; this pins that the binary actually uses it.
func TestTheConsumerAndTheGateShareOneDefault(t *testing.T) {
	if got := DefaultRefFor("TG_AM_INGEST_TOKEN_REF"); got != "env:TG_AM_INGEST_TOKEN" {
		t.Fatalf("DefaultRefFor = %q, want env:TG_AM_INGEST_TOKEN (behaviour-preserving: it is what the live "+
			"sources row already holds)", got)
	}
	if got := DefaultRefFor("NOT_A_DECLARED_REF"); got != "" {
		t.Errorf("an unknown name must yield no default rather than a plausible-looking one; got %q", got)
	}
	b, err := os.ReadFile("../../cmd/grounder/main.go")
	if err != nil {
		t.Fatalf("read grounder: %v", err)
	}
	// The grounder must take the default from here, not retype the string.
	got := regexp.MustCompile(`get\("TG_AM_INGEST_TOKEN_REF",\s*([^)]+)\)`).FindStringSubmatch(string(b))
	if got == nil {
		t.Fatal("the grounder no longer reads TG_AM_INGEST_TOKEN_REF from the environment")
	}
	if !strings.Contains(got[1], "DefaultRefFor") {
		t.Errorf("the grounder hard-codes its own default (%s) instead of sharing the gate's. The two agreeing "+
			"today by coincidence is how a knob drifts into meaning different things at each end.", strings.TrimSpace(got[1]))
	}
}
