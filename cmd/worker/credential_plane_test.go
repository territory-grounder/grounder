package main

import (
	"os"
	"strings"
	"testing"

	"github.com/territory-grounder/grounder/core/credential"
	"github.com/territory-grounder/grounder/core/preflight"
	tg "github.com/territory-grounder/grounder/temporal"
)

// credential_plane_test.go — the ADVERSARIAL oracles for TG-153.
//
// The threat these encode: an attacker who has achieved code execution inside the TRIAGE worker (the process
// that reads untrusted alert bodies, device syslog and host command output, and feeds them to an LLM). The
// question is not "is the actuation call guarded?" — an attacker inside the process does not call the guarded
// function, they read the key out of memory or ask OpenBao for it with the identity the process already holds.
// The question is whether the ESTATE-MUTATING CREDENTIAL IS PRESENT AT ALL. These tests answer that by
// exercising the only path by which cmd/worker can obtain it: planeEnv.

// withPlane installs a plane for the duration of one test and restores it. credentialPlane is package state
// for the reason documented in credential_plane.go (planeEnv is consulted from call sites spread across the
// composition root); tests must therefore restore it or they leak into each other.
func withPlane(t *testing.T, p credential.ProcessPlane) {
	t.Helper()
	prev := credentialPlane
	credentialPlane = p
	t.Cleanup(func() { credentialPlane = prev })
}

// setEnv sets a real process environment variable for one test. getenv reads the console config layer first
// and os.LookupEnv second; no boot config is installed in a unit test, so os.Setenv is what getenv sees.
func setEnv(t *testing.T, k, v string) {
	t.Helper()
	t.Setenv(k, v)
}

// TestTriagePlaneCannotReachAnyActuationCredential is THE adversarial oracle: with the estate-mutating
// configuration fully present in the process environment — exactly the .env of today's single worker — a
// process running the triage plane must be unable to obtain ANY of it.
//
// KILLING MUTATION (executed 2026-08-04): in planeEnv, delete the withheldFromPlane branch so it degrades to
// `return getenv(real, def)`. This test then FAILS with
//
//	"TG_ACTUATION_SSH_KEY resolved to a non-empty value on the TRIAGE plane — the process that reads
//	 untrusted alert/syslog/host content can obtain the credential that mutates the estate ..."
//
// restored, green. That message is the real-world consequence, not a shape complaint: a non-empty value here
// means sshactuation.NewNativeRunner is handed a live key reference in the same address space as the agent.
func TestTriagePlaneCannotReachAnyActuationCredential(t *testing.T) {
	// The adversary's premise: the operator mis-split, or an image was deployed with the full co-holding
	// .env. EVERY actuation variable is present in the environment.
	for _, k := range actuationPlaneEnvKeys {
		setEnv(t, k, "bao:tg/actuator#value-that-must-never-be-reachable")
	}
	withPlane(t, credential.ProcessPlaneTriage)

	// VACUITY FLOOR. This oracle scans a list; a list that had gone empty (a refactor, a bad merge) would
	// make every assertion below pass over nothing at all.
	if len(actuationPlaneEnvKeys) == 0 {
		t.Fatal("actuationPlaneEnvKeys is EMPTY — this test would pass while withholding nothing; the plane " +
			"filter would be a no-op and every actuation credential would be readable from the triage worker")
	}
	if !containsStr(actuationPlaneEnvKeys, "TG_ACTUATION_SSH_KEY") {
		t.Fatal("actuationPlaneEnvKeys no longer names TG_ACTUATION_SSH_KEY — the ONE credential that mutates " +
			"the estate over SSH. Withholding everything else while leaking that is the whole defect")
	}

	for _, k := range actuationPlaneEnvKeys {
		// Sanity: the raw environment DOES carry it, so a "" below is the plane filter refusing, never an
		// unset variable. Without this the test could pass on an empty environment and prove nothing.
		if got := getenv(k, ""); got == "" {
			t.Fatalf("guard: %s is not set in the process environment, so withholding it proves nothing", k)
		}
		if got := planeEnv(k, "fallback-default"); got != "" {
			t.Fatalf("%s resolved to %q on the TRIAGE plane — the process that reads untrusted alert/syslog/host "+
				"content can obtain the credential that mutates the estate, which is TG-153 exactly as filed. "+
				"An off-plane key must return \"\" REGARDLESS of the caller's default, or the default re-arms "+
				"the construction the plane exists to omit.", k, got)
		}
	}

	// And the consequence at the seam that matters most: the boot SSH preflight does os.ReadFile +
	// ssh.ParsePrivateKey, i.e. it pulls PRIVATE KEY MATERIAL into memory. On the triage plane it must not
	// even list the actuation key.
	refs := preflight.SSHKeyRefsFromEnv(func(k string) string { return planeEnv(k, "") })
	for _, r := range refs {
		if strings.Contains(r.Name, "TG_ACTUATION_SSH_KEY") {
			t.Fatalf("the boot SSH preflight enumerated the actuation key on the triage plane (%q) — it would "+
				"read and parse the estate-mutating private key into this process before the first alert is "+
				"triaged, reintroducing the defect inside a health check", r.Name)
		}
	}
}

// TestActuationPlaneCannotReachAnyUntrustedContentSource is the mirror, and it is the direction people skip.
// Keeping the actuation key away from the agent is only half the split; the other half is that the process
// which HOLDS the key must never read attacker-authored text, or it becomes the thing that gets popped.
//
// KILLING MUTATION (executed): remove the triage branch from withheldFromPlane
// (`if !p.HoldsTriage() && contains(triagePlaneEnvKeys, k)`) ⇒ RED with the message below.
func TestActuationPlaneCannotReachAnyUntrustedContentSource(t *testing.T) {
	for _, k := range triagePlaneEnvKeys {
		real := k
		if alias, ok := planeEnvAlias[k]; ok {
			real = alias
		}
		setEnv(t, real, "nl|syslog01|tg|file:/secrets/one_key|/var/log")
	}
	withPlane(t, credential.ProcessPlaneActuation)

	if len(triagePlaneEnvKeys) == 0 {
		t.Fatal("triagePlaneEnvKeys is EMPTY — this test would pass while the actuation worker still ingested " +
			"untrusted alert bodies and ran the agent's host readers")
	}
	for _, want := range []string{"TG_SYSLOGNG_DEPLOYMENTS", "TG_HOSTDIAG_DEPLOYMENTS", "TG_LIBRENMS_ALERT_POLL_INTERVAL"} {
		if !containsStr(triagePlaneEnvKeys, want) {
			t.Fatalf("triagePlaneEnvKeys no longer names %s — that untrusted-content reader would be constructed "+
				"inside the process holding the estate-mutating key", want)
		}
	}
	for _, k := range triagePlaneEnvKeys {
		if got := planeEnv(k, "fallback-default"); got != "" {
			t.Fatalf("%s resolved to %q on the ACTUATION plane — the process holding the credential that mutates "+
				"the estate would construct an untrusted-content reader (alert bodies / device syslog / host "+
				"command output), which is the exact chain the July-2026 HuggingFace intrusion walked", k, got)
		}
	}
}

// TestBothPlaneIsIdenticalToPreSplitBehaviour is the NO-REGRESSION oracle for the composition root: under the
// DEFAULT plane, planeEnv must be indistinguishable from getenv for EVERY key it governs. Any divergence is a
// behaviour change forced on every existing single-worker deployment at upgrade time, which is the one thing
// this ticket is not allowed to do.
//
// KILLING MUTATION (executed): change withheldFromPlane's first line to `if !p.HoldsActuation() ||
// p == credential.ProcessPlaneBoth` (i.e. withhold on `both` too). This test then FAILS with
//
//	"planeEnv(TG_ACTUATION_SSH_KEY) returned "" under the DEFAULT plane but getenv returned ... — an
//	 existing single-worker deployment would silently lose its actuation configuration on upgrade"
//
// restored, green.
func TestBothPlaneIsIdenticalToPreSplitBehaviour(t *testing.T) {
	all := append(append([]string{}, actuationPlaneEnvKeys...), triagePlaneEnvKeys...)
	if len(all) == 0 {
		t.Fatal("no governed keys — this no-regression proof would be vacuous")
	}
	for _, k := range all {
		real := k
		if alias, ok := planeEnvAlias[k]; ok {
			real = alias
		}
		setEnv(t, real, "configured-by-the-operator")
	}
	withPlane(t, credential.ProcessPlaneBoth)
	for _, k := range all {
		real := k
		if alias, ok := planeEnvAlias[k]; ok {
			real = alias
		}
		want, got := getenv(real, "d"), planeEnv(k, "d")
		if want != got {
			t.Fatalf("planeEnv(%s) returned %q under the DEFAULT plane but getenv returned %q — an existing "+
				"single-worker deployment would silently lose its configuration on upgrade, and a security fix "+
				"that breaks every installation is one nobody deploys", k, got, want)
		}
	}
	// A key governed by NEITHER list must pass through untouched on every plane — planeEnv is a filter over a
	// named set, never a general gate that could quietly starve an unrelated subsystem.
	setEnv(t, "TG_TEMPORAL_HOSTPORT", "temporal:7233")
	for _, p := range []credential.ProcessPlane{credential.ProcessPlaneBoth, credential.ProcessPlaneTriage, credential.ProcessPlaneActuation} {
		withPlane(t, p)
		if got := planeEnv("TG_TEMPORAL_HOSTPORT", ""); got != "temporal:7233" {
			t.Fatalf("plane %q swallowed an ungoverned key (TG_TEMPORAL_HOSTPORT=%q) — planeEnv must filter a "+
				"named set, not act as a general gate", p, got)
		}
	}
}

// TestActuationPlaneKeysCoverEveryActuationRefTheWorkerDeclares pins the withhold list against the PlaneSet
// the worker actually asserts on at boot (main.go). Those two lists are the same claim written twice: "these
// are the references that mutate the estate". If a new actuation reference is added to the boot assertion but
// not to the withhold list, the boot check refuses a mis-split deployment while planeEnv happily hands the new
// credential to the triage plane of a correctly-split one — a control that is right about the config it can
// see and wrong about the process it protects.
func TestActuationPlaneKeysCoverEveryActuationRefTheWorkerDeclares(t *testing.T) {
	// The actuation half of the PlaneSet built in main(). Kept here as the ONE place the two lists meet; the
	// scan below proves each is really read from main.go, so this cannot drift into a private fiction.
	declared := []string{"TG_ACTUATION_SSH_KEY", "TG_PROXMOX_TOKEN_REF", "TG_AWXJOB_LAUNCH_TOKEN_REF",
		// TG-423: the ssh-CA signed-cert acquisition keys must be withheld from the triage plane too, or a
		// triage worker would construct a live sshca.Engine holding the actuation-cert signing token.
		"TG_SSHCA_ADDR", "TG_SSHCA_MOUNT", "TG_SSHCA_ROLE", "TG_SSHCA_TOKEN_REF", "TG_SSHCA_CA"}
	src, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatalf("read main.go: %v", err)
	}
	found := 0
	for _, k := range declared {
		if !strings.Contains(string(src), k) {
			t.Fatalf("%s is not read anywhere in cmd/worker/main.go — this guard is asserting over a stale list, "+
				"which is exactly the vacuous-scan failure mode it exists to avoid", k)
		}
		found++
		if !containsStr(actuationPlaneEnvKeys, k) {
			t.Fatalf("%s is asserted as an ACTUATION reference by the boot plane check but is NOT withheld from "+
				"the triage plane by actuationPlaneEnvKeys — a correctly-split triage worker would still be "+
				"handed it. Add it to actuationPlaneEnvKeys in credential_plane.go.", k)
		}
	}
	if found == 0 {
		t.Fatal("scanned zero references — vacuous")
	}
}

// TestOffPlaneWorkerRefusesToRun proves the Temporal half fails LOUDLY. The stub registers nothing and polls
// nothing; if a wiring mistake ever handed it to the run loop, the process must die at boot rather than come
// up healthy, register nothing, and serve no work — "green and reached by nothing" is this codebase's
// signature defect, and it would be a spectacularly bad way to discover the actuation plane had gone dark.
func TestOffPlaneWorkerRefusesToRun(t *testing.T) {
	w := newOffPlaneWorker(tg.TaskQueueActuate, credential.ProcessPlaneTriage)
	w.RegisterWorkflow(func() {}) // accepted and discarded — nothing is registered anywhere
	w.RegisterActivity(func() {})
	err := w.Start()
	if err == nil {
		t.Fatal("the off-plane worker must REFUSE to start — silently starting it would re-merge the two planes " +
			"the deployment split, and nothing would say so")
	}
	if !strings.Contains(err.Error(), tg.TaskQueueActuate) || !strings.Contains(err.Error(), "TG-153") {
		t.Fatalf("the refusal must name the queue and the ticket so an operator can act on it: %v", err)
	}
	if rerr := w.Run(nil); rerr == nil {
		t.Fatal("Run must refuse for the same reason as Start")
	}
	w.Stop() // must not panic
}

// TestPlaneWithheldKeysReportsOnlyWhatWasConfigured guards the BOOT LOG against the vacuity this repo has
// paid for repeatedly: "withheld 9 keys" over an .env that declared none of them reads like protection and
// measures nothing.
func TestPlaneWithheldKeysReportsOnlyWhatWasConfigured(t *testing.T) {
	withPlane(t, credential.ProcessPlaneTriage)
	if got := planeWithheldKeys(credential.ProcessPlaneTriage); len(got) != 0 {
		t.Fatalf("nothing is configured, so nothing was withheld; got %v — a count over unset variables is theatre", got)
	}
	setEnv(t, "TG_ACTUATION_SSH_KEY", "bao:tg/actuator#key")
	got := planeWithheldKeys(credential.ProcessPlaneTriage)
	if len(got) != 1 || got[0] != "TG_ACTUATION_SSH_KEY" {
		t.Fatalf("the report must name the configured key that was withheld; got %v", got)
	}
	if n := len(planeWithheldKeys(credential.ProcessPlaneBoth)); n != 0 {
		t.Fatalf("plane=both withholds nothing by definition; got %d", n)
	}
}

func containsStr(list []string, k string) bool {
	for _, e := range list {
		if e == k {
			return true
		}
	}
	return false
}
