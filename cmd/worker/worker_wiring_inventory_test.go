package main

// THE EXTRACTION-SAFETY NET for the cmd/worker/main.go god-file split (LOC-debt paydown).
//
// main() is a 6,000-line composition root being carved into cohesive phase files. Every seam bind,
// temporal workflow/activity registration, background job, and probe offer MUST survive the move. A
// wiring line dropped or renamed mid-extraction is INVISIBLE to behavioural unit tests — each half
// works in isolation, and the composition root has no seam to test through (the same reasoning
// core/seal/composition_root_test.go and boot_posture_order_test.go rely on) — and it only surfaces
// as a dark seam / missing workflow on a LIVE boot, after the deploy.
//
// So this guard reads the ENTIRE cmd/worker package source (not just main.go, because a call moving
// from main.go into a new phase file must still count) and asserts each wiring line is present. It
// pins the INVENTORY, not the file it lives in — exactly the invariant the split must preserve.
//
// KILLING MUTATION: delete any one of the seams / workflows / activities / jobs / probe-offers below
// from cmd/worker — RED. That is the drop this net exists to catch.

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// declaredSeams reads the CLOSED seam vocabulary from core/wiring/seam.go — the source of truth this
// guard must not drift from. It fails loudly on an empty result: a parse that silently found nothing
// would make the seam half of this net vacuous while it still reported PASS.
func declaredSeams(t *testing.T) []string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "core", "wiring", "seam.go"))
	if err != nil {
		t.Fatalf("read the seam vocabulary: %v", err)
	}
	var out []string
	for _, m := range regexp.MustCompile(`(Seam[A-Za-z0-9_]+)\s+Seam\s+=`).FindAllStringSubmatch(string(b), -1) {
		out = append(out, m[1])
	}
	if len(out) < 10 {
		t.Fatalf("declaredSeams parsed %d seam constant(s) from core/wiring/seam.go — the parse broke, and a "+
			"vacuous seam check reports PASS while covering nothing", len(out))
	}
	sort.Strings(out)
	return out
}

// definedJobs derives the background-job inventory from the package source: every `func startXxxJob(`
// definition. It fails loudly on an implausibly small result for the same reason declaredSeams does — a
// broken parse would leave this half vacuous while reporting PASS.
func definedJobs(t *testing.T, src string) []string {
	t.Helper()
	seen := map[string]bool{}
	for _, m := range regexp.MustCompile(`func (start[A-Za-z0-9_]+Job)\(`).FindAllStringSubmatch(src, -1) {
		seen[m[1]] = true
	}
	out := make([]string, 0, len(seen))
	for j := range seen {
		out = append(out, j)
	}
	if len(out) < 12 {
		t.Fatalf("definedJobs parsed %d job definition(s) — the parse broke, and a vacuous job check reports "+
			"PASS while covering nothing", len(out))
	}
	sort.Strings(out)
	return out
}

// registeredWorkflows is the inventory of temporal workflows cmd/worker must register. It is a named
// var (not an inline literal) so the completeness check below can measure the list against the source —
// see that check for why a drop-ratchet alone is not enough.
var registeredWorkflows = []string{
	"configwrite.ConfigWriteWorkflow", "configwrite.SecretPutWorkflow", "configwrite.SecretRewrapWorkflow",
	"credentialsync.CredentialSyncWorkflow", "escsched.FireDueWorkflow", "manifestwrite.ManifestTransitionWorkflow",
	"modetransition.ModeTransitionWorkflow", "moduletest.TestModuleWorkflow", "nativerule.NativeRuleWriteWorkflow",
	"opclassratify.OpClassVerbWorkflow", "policytrace.PolicyTraceWorkflow", "rulesetwrite.RulesetWriteWorkflow",
	"runner.CommitConfirmWorkflow", "runner.RollbackWorkflow", "runner.RunnerWorkflow",
	"runner.TransactionPlanWorkflow",
	"skillgen.GeneratorWorkflow", "skilljudge.JudgeWorkflow", "skilltrial.FinalizerWorkflow",
	"skillwrite.TransitionWorkflow",
	"enginetoggle.EngineToggleWorkflow", "ledgeranchor.WitnessAnchorWorkflow",
	"objectgroup.ObjectGroupWriteWorkflow", "tggov.FrontierCrossCheckWorkflow", "tggov.JudgeLivenessWorkflow",
	// ★ FOUND BY THE COMPLETENESS CHECK BELOW, 2026-08-23. These five were registered on the worker and
	// named NOWHERE in this inventory — so the drop-net covered 20 of 25 workflows while reading as
	// complete, and dropping any of them in an extraction would have gone unnoticed until a live dispatch
	// failed. That is precisely the failure this file exists to prevent, reachable through the file itself.
}

// directlyRegisteredActivities is the inventory of activities cmd/worker registers by name (the runner's
// own set registers through temporal/runner.RegisterActivities, which has its own guard).
var directlyRegisteredActivities = []string{
	"configWriteActs.ApplyConfigActivity", "configWriteActs.PutSecretActivity", "configWriteActs.RewrapSecretsActivity",
	"credSyncActs.SyncSourceActivity", "escActs.FireDueActivity", "manifestWriteActs.ManifestTransitionActivity",
	"modeTransitionActs.ApplyModeTransitionActivity", "moduleTestActs.TestModuleActivity",
	"nativeRuleActs.ApplyNativeRuleWriteActivity", "opClassVerbActs.OpClassVerbActivity",
	"policyTraceActs.PolicyTraceActivity", "rulesetWriteActs.ApplyRulesetWriteActivity",
	"skillGenActs.GenerateActivity", "skillJudgeActs.JudgeBatchActivity", "skillTrialActs.FinalizeActivity",
	"skillWriteActs.TransitionActivity",
	// Same finding, same day: these five were registered and unlisted.
	"acts.FrontierCrossCheckActivity", "acts.JudgeLivenessActivity", "engineToggleActs.ApplyEngineToggleActivity",
	"ledgeranchor.WitnessAnchorActivity", "objectGroupActs.ApplyObjectGroupWriteActivity",
}

// workerPackageSource concatenates every non-test .go file in cmd/worker, so the inventory is checked
// across the whole package regardless of which phase file a call has been extracted into.
func workerPackageSource(t *testing.T) string {
	t.Helper()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read cmd/worker dir: %v", err)
	}
	var b strings.Builder
	for _, e := range entries {
		n := e.Name()
		if !strings.HasSuffix(n, ".go") || strings.HasSuffix(n, "_test.go") {
			continue
		}
		src, err := os.ReadFile(filepath.Clean(n))
		if err != nil {
			t.Fatalf("read %s: %v", n, err)
		}
		b.Write(src)
		b.WriteByte('\n')
	}
	if b.Len() < 50_000 {
		t.Fatalf("VACUITY FLOOR: cmd/worker non-test source is only %d bytes — either the package moved "+
			"or this guard is reading the wrong dir; every assertion below would pass on a stub.", b.Len())
	}
	return b.String()
}

// TestWorkerWiringInventoryIsComplete pins main()'s observable composition so the god-file split
// cannot silently drop a capability.
func TestWorkerWiringInventoryIsComplete(t *testing.T) {
	src := workerPackageSource(t)

	// --- 11 seams bound (or Absent-registered) into the wiring manifest. Both wiring.Bind(...) and
	// wiring.Absent[...](...) carry `wiringManifest, wiring.SeamX`, so match on that. A dropped bind =
	// a capability the agent silently loses (the tg_wiring_seam_dark class). ---
	// DERIVED, not hand-listed (2026-08-23). This list used to name eleven seams while core/wiring declared
	// twelve — SeamTrackerImport was handled in the worker but named nowhere here, so the guard's own
	// inventory had drifted from the closed vocabulary it guards. A seam DECLARED and never wired is the
	// governed-dark class this net exists to catch, and a hand list cannot catch the seam it never heard of.
	// Reading the constants means a new seam is covered the day it is declared.
	for _, seam := range declaredSeams(t) {
		if !strings.Contains(src, "wiringManifest, wiring."+seam) {
			t.Errorf("cmd/worker no longer registers %s into the wiring manifest — a seam dropped in the "+
				"main() split goes dark on the next boot with nothing else asserting it", seam)
		}
	}

	// --- 20 temporal workflows registered on the worker. A dropped RegisterWorkflow = that workflow
	// cannot run (its start would fail at dispatch), invisible until exercised live. ---
	for _, wf := range registeredWorkflows {
		if !strings.Contains(src, "RegisterWorkflow("+wf) {
			t.Errorf("cmd/worker no longer registers workflow %s — it would fail closed at dispatch, "+
				"unseen until that lane is exercised on a live worker", wf)
		}
	}

	// --- 16 temporal activities registered. Same failure mode as workflows. ---
	for _, act := range directlyRegisteredActivities {
		if !strings.Contains(src, "RegisterActivity("+act) {
			t.Errorf("cmd/worker no longer registers activity %s — the workflow that calls it stalls "+
				"at that step on a live worker", act)
		}
	}

	// --- Background metric/coverage jobs, DERIVED (2026-08-23). Each is DEFINED in its own file and
	// CALLED at boot, so a present-and-called job appears at least twice in the package; a dropped CALL
	// (the extraction risk) leaves only the definition (once) and the metric silently stops publishing.
	//
	// The list used to be hand-maintained at fifteen while the package defined sixteen — and the
	// unlisted one, startSelfDepConcentrationJob, was defined-but-never-started: exactly the condition
	// the <2 rule below detects, sitting unchecked because the name was missing from the list. (It was
	// a superseded single-capability variant of the wired Multi job; deleted in this commit.) Deriving
	// the names means a new job is checked the day it is written.
	for _, job := range definedJobs(t, src) {
		if strings.Count(src, job) < 2 {
			t.Errorf("cmd/worker %s appears <2 times (definition + boot call) — the boot call was likely "+
				"dropped in the split, leaving the job defined-but-never-started (a dark metric)", job)
		}
	}

	// --- COMPLETENESS, the half a drop-ratchet cannot give you. Everything above catches a wiring line
	// that DISAPPEARS. None of it catches one that APPEARS and is never added to the list — and that is the
	// likelier mistake, because adding a workflow is normal work while remembering a test's inventory is
	// not. Measured 2026-08-23: registering TransactionPlanWorkflow required a hand-edit here, and nothing
	// but memory would have caught its absence; the fifth new registration would simply have gone unlisted,
	// leaving this guard quietly covering less than it claims (the same shape as the env-parity target list
	// that had fallen four wire files behind on the same day).
	//
	// So: count the registrations in the SOURCE and require the list to name at least as many. A new
	// workflow/activity fails here until it is listed, which is the one-line edit that keeps the net whole.
	// The comparison is >= (never ==): a list may legitimately name something the counter cannot see (a
	// registration behind a build tag), and this must fail on the under-covered direction only.
	// Count DISTINCT registered SYMBOLS, not call sites: a workflow registered on two workers is one
	// workflow, and `RegisterWorkflow(any)` is the generic registrar's own signature, not a registration.
	distinct := func(pattern string) map[string]bool {
		out := map[string]bool{}
		for _, m := range regexp.MustCompile(pattern).FindAllStringSubmatch(src, -1) {
			if sym := m[1]; sym != "any" && strings.Contains(sym, ".") {
				out[sym] = true
			}
		}
		return out
	}
	for _, c := range []struct {
		what    string
		pattern string
		listed  int
	}{
		{"temporal workflow", `RegisterWorkflow\(([A-Za-z0-9_.]+)\)`, len(registeredWorkflows)},
		{"activity registered directly in cmd/worker", `w\.RegisterActivity\(([A-Za-z0-9_.]+)\)`, len(directlyRegisteredActivities)},
	} {
		if n := len(distinct(c.pattern)); n > c.listed {
			t.Errorf("cmd/worker makes %d %s registration(s) but this guard's inventory names only %d — a "+
				"registration was added without being listed here, so the net now covers LESS than it claims "+
				"and the next dropped one goes unnoticed. Add it to the list (that is the whole fix).",
				n, c.what, c.listed)
		}
	}

	// --- 11 module probes offered to the probe sweep registry. A dropped offer = that connector is
	// never sweep-probed, so its reachability regressions go undetected (the TG-450 class). ---
	if n := strings.Count(src, "probeReg.offer("); n < 11 {
		t.Errorf("cmd/worker offers %d module probes to the sweep registry, expected >=11 — a probe.offer "+
			"dropped in the split stops sweep-probing that connector (the class TG-468 alerts on)", n)
	}
}
