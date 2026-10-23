package preflight

// ORACLES FOR THE SSH KEY REFS THE POLICY GATE COULD NOT SEE (TG-302).
//
// THE DEFECT, live on 2026-08-04: the deployment ran TG_SECRET_POLICY=enforce, the boot gate reported
// "0 raw plaintext violation(s)", and both SSH read lanes were reading every estate host with
// `file:/secrets/one_key` — a private key on a bind mount shared with the worker, which is exactly the
// plaintext-bearing scheme this gate exists to refuse.
//
// It survived because it looked fine from two directions. This enumerator never listed the variable, so
// the policy gate had nothing to judge. And CheckSSHKeys DOES parse the same variable, but only asserts
// the key resolves and parses — so the boot log said "credential preflight OK — 2 SSH key ref(s)
// resolve+parse" about a key later measured to authenticate to 0 of 20 hosts (TG-300). One gate was
// silent; the other was actively reassuring.
//
// TG-284 taught this enumerator to read a credential out of a compound variable and then taught it about
// exactly one. This is the rest of the class.

import (
	"strings"
	"testing"

	"github.com/territory-grounder/grounder/core/config"
)

func refFor(t *testing.T, entries []SecretEntry, name string) config.SecretRef {
	t.Helper()
	for _, e := range entries {
		if e.Name == name {
			return e.Ref
		}
	}
	return ""
}

// KILLING MUTATION: drop the SSHDeploymentEntries call from DeploymentSecretEntries (restores the shipped
// state). RED — this is the exact live configuration the gate reported clean.
func TestTheSshKeyRefsAreEnumeratedSoThePolicyGateCanJudgeThem(t *testing.T) {
	env := map[string]string{
		"TG_HOSTDIAG_DEPLOYMENTS": "dc1|dc1*|root|file:/secrets/one_key",
		"TG_SYSLOGNG_DEPLOYMENTS": "dc1|dc1syslogng01|root|file:/secrets/one_key|/mnt/logs/syslog-ng|nllei;" +
			"dc2|dc2syslogng01|root|file:/secrets/one_key|/mnt/logs/syslog-ng|grskg",
	}
	got := DeploymentSecretEntries(func(k string) string { return env[k] })

	for _, want := range []string{
		"TG_HOSTDIAG_DEPLOYMENTS[dc1]",
		"TG_SYSLOGNG_DEPLOYMENTS[dc1]",
		"TG_SYSLOGNG_DEPLOYMENTS[dc2]",
	} {
		ref := refFor(t, got, want)
		if ref == "" {
			t.Errorf("%s is absent from the policed set — the gate has nothing to judge, so it reports a "+
				"clean deployment while an SSH private key sits on a shared bind mount", want)
			continue
		}
		if config.IsBackendScheme(ref) {
			t.Errorf("%s resolved to %q, which the test fixture did not intend — the fixture is meant to "+
				"reproduce the plaintext-bearing live config", want, ref)
		}
	}

	// And the gate must actually CALL it a violation, or enumerating changes nothing.
	rep := CheckSecretPolicy(got)
	if len(rep.Violations) < 3 {
		t.Fatalf("policy reported %d violation(s) over three file: SSH key refs — enumerating a credential "+
			"the gate then declines to judge is the same silence with extra steps", len(rep.Violations))
	}
}

// The control that stops this becoming noise: a key already delivered from a backend must NOT be flagged.
// TG_ACTUATION_SSH_KEY is already bao: on the live box, which is the proof the migration is possible at all.
func TestABackendBackedSshKeyIsNotAViolation(t *testing.T) {
	env := map[string]string{
		"TG_HOSTDIAG_DEPLOYMENTS": "dc1|dc1*|root|bao:secret/data/tg/hostdiag#key",
	}
	got := DeploymentSecretEntries(func(k string) string { return env[k] })
	ref := refFor(t, got, "TG_HOSTDIAG_DEPLOYMENTS[dc1]")
	if ref == "" {
		t.Fatal("the bao:-backed row was not enumerated at all")
	}
	for _, v := range CheckSecretPolicy(got).Violations {
		if strings.HasPrefix(v.Name, "TG_HOSTDIAG_DEPLOYMENTS") {
			t.Fatalf("flagged a backend-resolved key as plaintext (%s, scheme %q) — a gate that fires on "+
				"the fixed state is a gate that gets switched off", v.Name, v.Scheme)
		}
	}
}

// A row with no key reference contributes nothing. Rows are operator-authored and ragged; inventing an
// empty entry would make the gate fail over a credential that was never declared.
func TestRowsWithoutAKeyReferenceContributeNothing(t *testing.T) {
	env := map[string]string{
		"TG_HOSTDIAG_DEPLOYMENTS": "dc1|dc1*|root|;dc2|dc2*|root",
		"TG_SYSLOGNG_DEPLOYMENTS": "",
	}
	if got := SSHDeploymentEntries(func(k string) string { return env[k] }); len(got) != 0 {
		t.Fatalf("invented %d entry/entries from rows declaring no key: %+v", len(got), got)
	}
}

// VACUITY FLOOR. If the row shape changes and field 3 stops being the key, this enumerator silently
// returns nothing and the gate goes quiet again — which is precisely how the original defect looked.
func TestTheEnumeratorFailsLoudlyRatherThanQuietlyMatchingNothing(t *testing.T) {
	live := "dc1|dc1*|root|file:/secrets/one_key"
	got := SSHDeploymentEntries(func(k string) string {
		if k == "TG_HOSTDIAG_DEPLOYMENTS" {
			return live
		}
		return ""
	})
	if len(got) == 0 {
		t.Fatalf("the real production row %q yielded no entries — the field index no longer matches the "+
			"row shape, so every SSH key ref is invisible to the policy gate again", live)
	}
}
