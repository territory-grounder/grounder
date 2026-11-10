package preflight

// ORACLES FOR THE REF THAT ACTUALLY WINS (TG-306).
//
// THE DEFECT. TG-302/TG-304 taught the policy gate to see the SSH key refs inside TG_SYSLOGNG_DEPLOYMENTS
// and TG_HOSTDIAG_DEPLOYMENTS. Those turned out not to be the refs the agent authenticates with.
//
// The credential engine resolves most-specific-wins across sources; AWX registers at precedence 20, the
// native hostdiag source at 100. AWX wins every time, and its key comes from TG_AWX_CRED_REF_MAP — which
// no preflight and no policy gate enumerated. Live on 2026-08-04, 35 of 36 resolutions in 24h were
// `source=awx rule=jt:60:cred:24 shadowed=native-hostdiag`.
//
// So the gate reported a clean deployment while the winning ref pointed at an UNRESTRICTED estate root key
// (`ssh -i <ref> root@host id` -> `uid=0(root)`), and setting that variable back to `file:` would have
// kept the boot green under enforce. The two variables the earlier tickets fixed were the ones that did
// not matter.

import (
	"strings"
	"testing"

	"github.com/territory-grounder/grounder/core/config"
)

// KILLING MUTATION: drop the AWXCredRefMapEntries call from DeploymentSecretEntries (the shipped state).
// RED — this is the live configuration the gate reported clean while the worker held root on the fleet.
func TestTheAwxCredRefMapIsPolicedLikeAnyOtherBusinessSecret(t *testing.T) {
	env := map[string]string{
		// The real live shape: the AWX credential name contains parentheses and, crucially, could contain
		// '=' — the split has to take the LAST one.
		"TG_AWX_CRED_REF_MAP": "SSH ED25519 (one_key)=file:/secrets/one_key;SSH Lab=file:/secrets/lab",
	}
	got := DeploymentSecretEntries(func(k string) string { return env[k] })
	want := "TG_AWX_CRED_REF_MAP[SSH ED25519 (one_key)]"
	var found bool
	for _, e := range got {
		if e.Name == want {
			found = true
			if config.IsBackendScheme(e.Ref) {
				t.Errorf("%s resolved to %q — the fixture is meant to reproduce the plaintext live state", want, e.Ref)
			}
		}
	}
	if !found {
		t.Fatalf("%s is absent from the policed set. This is the ref the engine ACTUALLY hands out — the "+
			"gate has nothing to judge, so it reports a clean deployment while the winning credential is a "+
			"file on disk.", want)
	}
	if v := CheckSecretPolicy(got).Violations; len(v) < 2 {
		t.Fatalf("policy reported %d violation(s) over two file: refs — enumerating a credential the gate "+
			"then declines to judge is the same silence with extra steps", len(v))
	}
}

// The control. Once migrated, the gate must go quiet — a gate that fires on the fixed state gets switched
// off. This is the configuration the live box runs today.
func TestABackendBackedAwxRefIsNotAViolation(t *testing.T) {
	env := map[string]string{
		"TG_AWX_CRED_REF_MAP": "SSH ED25519 (one_key)=bao:secret/data/tg/hostdiag#key",
	}
	got := DeploymentSecretEntries(func(k string) string { return env[k] })
	for _, v := range CheckSecretPolicy(got).Violations {
		if strings.HasPrefix(v.Name, "TG_AWX_CRED_REF_MAP") {
			t.Fatalf("flagged a backend-resolved ref as plaintext (%s, scheme %q)", v.Name, v.Scheme)
		}
	}
}

// An AWX credential name may legitimately contain '='. Splitting on the FIRST one would take the ref from
// the middle of the name and police a string that is not a reference at all.
func TestTheSplitTakesTheLastEqualsSoNamesMayContainOne(t *testing.T) {
	got := AWXCredRefMapEntries("weird=name=bao:secret/data/tg/x#key")
	if len(got) != 1 {
		t.Fatalf("got %d entries, want 1: %+v", len(got), got)
	}
	if got[0].Name != "TG_AWX_CRED_REF_MAP[weird=name]" {
		t.Errorf("name parsed as %q — the credential name was truncated at the first '='", got[0].Name)
	}
	if string(got[0].Ref) != "bao:secret/data/tg/x#key" {
		t.Errorf("ref parsed as %q — the gate would police a string that is not a reference", got[0].Ref)
	}
}

// Malformed pairs contribute nothing. modules/bootstrap.parseAWXCredRefMap is the authority on the format
// and fails closed there; failing the boot twice for one typo helps nobody, and a second strict parser
// could disagree about which pairs exist and police a set the engine does not use.
func TestMalformedPairsContributeNothingRatherThanFailingTheBoot(t *testing.T) {
	for _, spec := range []string{"", "   ", ";;", "noequals", "=onlyref", "name=", ";  ;"} {
		if got := AWXCredRefMapEntries(spec); len(got) != 0 {
			t.Errorf("spec %q produced %d entry/entries: %+v", spec, len(got), got)
		}
	}
}

// VACUITY FLOOR on the REAL production string. If the format ever changes, this enumerator silently
// returns nothing and the gate goes quiet again — which is exactly how the original defect looked.
func TestTheEnumeratorHandlesTheRealProductionString(t *testing.T) {
	const live = "SSH ED25519 (one_key)=bao:secret/data/tg/hostdiag#key"
	got := AWXCredRefMapEntries(live)
	if len(got) != 1 {
		t.Fatalf("the real production value %q yielded %d entries — the parser no longer matches the "+
			"format, so the winning credential is invisible to the gate again", live, len(got))
	}
}

// The SSH-key preflight must resolve+parse the winning ref too, not only the policy gate.
//
// THE DEFECT THIS COVERS: on 2026-08-04 the boot line read "credential preflight OK — 3 SSH key ref(s)
// resolve+parse" and named actuation, hostdiag and syslog-ng. None of those three was the ref the engine
// actually handed out. A broken or unreadable key behind TG_AWX_CRED_REF_MAP would have sailed through
// that check and failed at the first real read — reported, as ever, as "host unreachable".
//
// KILLING MUTATION: remove the AWXCredRefMapEntries loop from SSHKeyRefsFromEnv (the shipped state). RED.
func TestTheSshPreflightAlsoResolvesTheWinningRef(t *testing.T) {
	env := map[string]string{
		"TG_ACTUATION_SSH_KEY":    "bao:secret/data/tg/actuator#key",
		"TG_HOSTDIAG_DEPLOYMENTS": "dc1|dc1*|root|bao:secret/data/tg/hostdiag#key",
		"TG_AWX_CRED_REF_MAP":     "SSH ED25519 (one_key)=bao:secret/data/tg/awx-winner#key",
	}
	got := SSHKeyRefsFromEnv(func(k string) string { return env[k] })
	var found bool
	for _, r := range got {
		if string(r.Ref) == "bao:secret/data/tg/awx-winner#key" {
			found = true
		}
	}
	if !found {
		t.Fatalf("the AWX-mapped ref is absent from the SSH preflight set (%d ref(s) checked). That is the "+
			"ref the engine actually hands out — a broken key behind it would pass the boot check and fail "+
			"at the first real read, reported as 'host unreachable'.", len(got))
	}
}

// Dedup must still hold: the same ref reachable through two variables is one check, not two.
func TestTheWinningRefIsDedupedAgainstTheOtherVariables(t *testing.T) {
	const ref = "bao:secret/data/tg/hostdiag#key"
	env := map[string]string{
		"TG_HOSTDIAG_DEPLOYMENTS": "dc1|dc1*|root|" + ref,
		"TG_AWX_CRED_REF_MAP":     "SSH ED25519 (one_key)=" + ref,
	}
	n := 0
	for _, r := range SSHKeyRefsFromEnv(func(k string) string { return env[k] }) {
		if string(r.Ref) == ref {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("the same ref was checked %d times — the boot line would double-count and an operator "+
			"comparing the number against their config would be misled", n)
	}
}
