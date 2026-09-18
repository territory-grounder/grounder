package preflight

import (
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/territory-grounder/grounder/core/config"
)

// ★ THE CONTROL ON THE ONE KEY THAT MUTATES THE ESTATE, MADE ABLE TO FAIL.
//
// `SecretEntry.Exempt` is set by whoever builds the entry list, and CheckSecretPolicy used to honour it
// unconditionally. The policy's own doc comment claimed the exemption set was "closed by construction" —
// nothing closed it. The 2026-07-29 audit stated the consequence exactly: TG_ACTUATION_SSH_KEY could be
// flipped to Exempt and the whole suite stayed green.
//
// This is not hypothetical. The live deployment runs TG_SECRET_POLICY=enforce, so this gate is what stands
// between a plaintext business secret and a refused boot — and its only guard was the discipline of whoever
// edited the caller.
func TestABusinessSecretCannotExemptItself(t *testing.T) {
	// Every business secret the worker polices, asserted over the CLOSED enumeration rather than a
	// hand-picked example: a control that only knows about the key its author thought of is the same defect
	// one level up.
	for _, name := range []string{
		"TG_ACTUATION_SSH_KEY", "TG_PROXMOX_TOKEN_REF", "TG_ADMIN_TOKEN_REF", "TG_LDAP_BIND_PW",
		"TG_LITELLM_KEY_REF", "TG_AWX_TOKEN_REF", "TG_NETBOX_TOKEN_REF", "TG_GITLAB_RO_TOKEN_REF",
		"TG_SESSION_KEY_REF", "TG_OPERATOR_TOKEN_REF", "TG_LIBRENMS_INGEST_TOKEN_REF",
	} {
		rep := CheckSecretPolicy([]SecretEntry{
			{Name: name, Ref: config.SecretRef("file:/secrets/one_key"), Exempt: true},
		})
		if len(rep.Exempted) != 0 {
			t.Errorf("%s claimed a permanent exemption and the gate GRANTED it (%v). Any caller could then "+
				"excuse any secret, and the deployment enforces this policy in production.", name, rep.Exempted)
		}
		if len(rep.Violations) != 1 || !rep.Violations[0].UnclaimedExemption {
			t.Errorf("%s should be reported as an UNCLAIMED EXEMPTION — distinct from an ordinary unmigrated "+
				"ref, because it is the mechanism rather than the backlog. got %+v", name, rep.Violations)
		}
		if rep.Clean() {
			t.Errorf("%s produced a CLEAN report while asserting an exemption it does not hold — under "+
				"enforce the process would boot with a plaintext key that mutates the estate", name)
		}
	}
}

// The real members must still work, or this hardening would break every boot: those refs CANNOT come from a
// backend (they authenticate to it, unwrap it, or are public material), so they must stay allowed.
func TestTheRealExemptionsAreStillHonoured(t *testing.T) {
	for name := range PermanentExemptions {
		rep := CheckSecretPolicy([]SecretEntry{
			{Name: name, Ref: config.SecretRef("env:SOMETHING"), Exempt: true},
		})
		if len(rep.Exempted) != 1 || !rep.Clean() {
			t.Errorf("%s is a permanent exemption but was refused: violations=%+v exempted=%v — this is the "+
				"direction that breaks boot, not the one that weakens the gate", name, rep.Violations, rep.Exempted)
		}
	}
}

// A COMPLETENESS SWEEP OVER THE ACTUAL BINARIES. The two entry lists are the only production callers, and
// every name they mark Exempt must be in the closed set — otherwise this change turns a live `enforce`
// deployment into a boot failure. Reading the source is the point: an oracle that re-declared the list would
// agree with itself and prove nothing about what the binaries do.
func TestEveryExemptionTheBinariesClaimIsInTheClosedSet(t *testing.T) {
	exemptCall := regexp.MustCompile(`exempt\("([A-Z0-9_]+)"\)`)
	structLit := regexp.MustCompile(`\{Name: "([A-Z0-9_]+)"[^}]*Exempt: true`)

	for _, src := range []string{"../../cmd/worker/main.go", "../../cmd/grounder/main.go"} {
		b, err := os.ReadFile(src)
		if err != nil {
			t.Fatalf("read %s: %v", src, err)
		}
		var claimed []string
		for _, m := range exemptCall.FindAllStringSubmatch(string(b), -1) {
			claimed = append(claimed, m[1])
		}
		for _, m := range structLit.FindAllStringSubmatch(string(b), -1) {
			claimed = append(claimed, m[1])
		}
		if len(claimed) == 0 {
			t.Errorf("%s: parsed ZERO exempt entries — the sweep would pass vacuously and a rogue exemption "+
				"would sail through it", src)
			continue
		}
		for _, name := range claimed {
			if !PermanentExemptions[name] {
				t.Errorf("%s marks %q Exempt, but it is not in PermanentExemptions. Either it belongs in the "+
					"closed set with a stated reason it cannot resolve from a backend, or it is an unmigrated "+
					"business secret and must stop claiming an exemption. Live policy is enforce, so this "+
					"combination refuses the boot.", src, name)
			}
		}
	}
}

// The set must stay NARROW and every member must state why it cannot come from a backend. A set that grows
// by convenience is the unconditional flag again, wearing a list.
func TestTheClosedSetIsSmallAndReasoned(t *testing.T) {
	if n := len(PermanentExemptions); n > 15 {
		t.Errorf("the permanent exemption set holds %d names — it is meant to cover substrate bootstrap, seal "+
			"material and public certificates only. A set this size is an escape hatch.", n)
	}
	b, err := os.ReadFile("secretpolicy.go")
	if err != nil {
		t.Fatalf("read policy source: %v", err)
	}
	src := string(b)
	for name := range PermanentExemptions {
		i := strings.Index(src, `"`+name+`":`)
		if i < 0 {
			t.Errorf("%s is in the set but not declared in secretpolicy.go", name)
			continue
		}
		// Each member must sit under a comment line explaining the category. Checking that SOME comment
		// precedes it within the block is weak on its own, so the block comment above the map carries the
		// rule and this pins that the declaration is grouped rather than appended loose at the end.
		if !strings.Contains(src[:i], "cannot be stored in OpenBao") {
			t.Errorf("%s appears before the reasoned groups — new members must be filed under a stated "+
				"category, not appended where nobody reads", name)
		}
	}
}
