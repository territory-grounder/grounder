package skills

import (
	"regexp"
	"strings"
	"testing"

	"github.com/territory-grounder/grounder/core/actuate/opschema"
	"github.com/territory-grounder/grounder/core/execclass"
)

func hasAll(body string, subs ...string) bool {
	for _, s := range subs {
		if !strings.Contains(body, s) {
			return false
		}
	}
	return true
}

// A DEEP/STANDARD investigation gets the full protocols; the selection is deterministic.
func TestComposeDeepGetsFullProtocols(t *testing.T) {
	body, loaded := Default().Compose(Context{Phase: PhaseInvestigate, ExecClass: execclass.DeepInvestigation})
	if !hasAll(body, "Proving your work", "Investigation protocol", "Shortcuts to resist", "Conservative-remediation catalog", "Stop if your investigation stalls") {
		t.Fatalf("deep investigation must load the full behavioral set; loaded=%v", loaded)
	}
	// Composition is stable across calls (pure function).
	body2, _ := Default().Compose(Context{Phase: PhaseInvestigate, ExecClass: execclass.DeepInvestigation})
	if body != body2 {
		t.Fatal("Compose must be deterministic")
	}
}

// A FAST_AGENT gets the compact always-on skills but NOT the heavyweight protocols — the phase-aware
// prompt-compiler behavior (keep irrelevant instruction out of a cheap incident's prompt).
func TestComposeFastIsCompact(t *testing.T) {
	body, loaded := Default().Compose(Context{Phase: PhaseInvestigate, ExecClass: execclass.FastAgent})
	if !hasAll(body, "Proving your work", "Stop if your investigation stalls") {
		t.Fatalf("fast agent must still get the always-on essentials; loaded=%v", loaded)
	}
	if strings.Contains(body, "Investigation protocol") || strings.Contains(body, "Shortcuts to resist") {
		t.Fatalf("fast agent must NOT carry the heavyweight protocols; loaded=%v", loaded)
	}
}

// The conservative-remediation catalog (what to propose) and its hard floor are always present — the agent
// must always know which actions are never a reversible auto-fix.
func TestConservativeCatalogAlwaysPresentWithFloor(t *testing.T) {
	for _, c := range []execclass.Class{execclass.FastAgent, execclass.StandardAgent, execclass.DeepInvestigation} {
		body, _ := Default().Compose(Context{Phase: PhaseInvestigate, ExecClass: c})
		if !strings.Contains(body, "Conservative-remediation catalog") {
			t.Errorf("%s must carry the conservative catalog", c)
		}
		// the hard floor must name the stateful + irreversible classes
		if !hasAll(body, "etcd", "host reboot", "dropdb", "terraform destroy") {
			t.Errorf("%s conservative floor must name the never-auto classes", c)
		}
	}
}

// The re-authored behavioral skills (crit-3 / TG-69) must speak TG's JSON-directive, read-only wire format:
// the conservative catalog teaches a PREDICTION (feeding falsifiable_prediction), evidence is cited by
// OBSERVATION id, and NO skill carries the predecessor's chatops/execution idiom (fenced blocks, run-the-command,
// regression tests) that loop.go documents produced 0% proposals in an eval.
func TestSkillsSpeakWireFormatNotChatopsIdiom(t *testing.T) {
	body, _ := Default().Compose(Context{Phase: PhaseInvestigate, ExecClass: execclass.DeepInvestigation})
	if !strings.Contains(body, "predict:") || !strings.Contains(body, "PREDICTION") {
		t.Fatal("conservative-remediation must teach a falsifiable prediction (predict:) for each proposal")
	}
	if !strings.Contains(body, "evidence_ids") {
		t.Fatal("proving-your-work must ground claims in cited evidence_ids (wire format), not fenced blocks")
	}
	for _, banned := range []string{"fenced", "run the exact failing command", "add a regression test", "NOTE line", "Paste the actual output"} {
		if strings.Contains(body, banned) {
			t.Fatalf("re-authored skills must not carry the chatops/execution idiom %q (it conflicts with the read-only JSON wire format)", banned)
		}
	}
}

// The competence skills (#25a) are tool-grounded: a DEEP/STANDARD investigation gets the triage choreography
// and the per-alert-class playbooks, and both cite TG's ACTUAL read-only tools (not generic advice). A cheap
// FAST_AGENT does not carry them.
func TestComposeDeepGetsCompetenceSkills(t *testing.T) {
	body, loaded := Default().Compose(Context{Phase: PhaseInvestigate, ExecClass: execclass.DeepInvestigation})
	if !hasAll(body, "Triage protocol", "Alert-class playbooks") {
		t.Fatalf("deep investigation must load the competence skills; loaded=%v", loaded)
	}
	// Grounded in TG's real tools + the cascade signal — not model-invented capabilities.
	if !hasAll(body, "get-device-status", "get-device-eventlog", "get-active-alerts") {
		t.Fatalf("competence skills must reference TG's actual read-only tools; loaded=%v", loaded)
	}
	// Cascade awareness: co-alerting related hosts point UPSTREAM (propose against the shared cause).
	if !hasAll(body, "cascade", "upstream") {
		t.Fatalf("triage-protocol must carry the cascade / upstream-cause discipline; loaded=%v", loaded)
	}
	fast, _ := Default().Compose(Context{Phase: PhaseInvestigate, ExecClass: execclass.FastAgent})
	if strings.Contains(fast, "Alert-class playbooks") || strings.Contains(fast, "Triage protocol") {
		t.Fatal("a fast agent must NOT carry the heavyweight competence skills")
	}
}

// An UNCLASSIFIED context (empty ExecClass — no classification happened) composes the FULL set: seed
// composition fails toward more behavioral guidance, never toward less. Only an explicit FAST_AGENT
// classification earns the compact prompt.
func TestComposeUnclassifiedFailsTowardFullGuidance(t *testing.T) {
	body, loaded := Default().Compose(Context{Phase: PhaseInvestigate})
	if !hasAll(body, "Investigation protocol", "Triage protocol", "Alert-class playbooks") {
		t.Fatalf("an unclassified context must get the full behavioral set; loaded=%v", loaded)
	}
}

// The selector is a pure function of typed signals — no skill body leaks between contexts, and loaded names
// are reported for the audit record.
func TestLoadedNamesReported(t *testing.T) {
	_, loaded := Default().Compose(Context{Phase: PhaseInvestigate, ExecClass: execclass.StandardAgent})
	if len(loaded) == 0 {
		t.Fatal("Compose must report the loaded skill names")
	}
	want := map[string]bool{"proving-your-work": true, "conservative-remediation": true}
	got := map[string]bool{}
	for _, n := range loaded {
		got[n] = true
	}
	for n := range want {
		if !got[n] {
			t.Errorf("expected skill %q to load for a standard investigation", n)
		}
	}
}

// SKILL PROSE MUST NOT PROMISE A REMEDY TG DOES NOT HAVE.
//
// `alert-class-playbooks` instructed "Disk filling -> prune/trim/vacuum (verify free rose)" for TG's whole
// life. No prune, trim or vacuum op-class has ever existed. Measured over 96 injected disk-fill faults, the
// agent did the only two things that prose leaves open: it stood down (63, correct) or it reached for the
// nearest declared op-class, disk-grow (33, inapplicable — every guest here is an LXC on /dev/loopN).
//
// Behaviour tests cannot catch this: the prose is DATA, and every component around it works. Only an
// assertion over the CLOSED SET of declared op-classes can. Same shape as
// TestEveryDeclaredOpClassHasAPolicyRule, pointed the other way.
// EVERY REGISTRY OP-CLASS MUST BE MENTIONABLE IN SKILL PROSE.
//
// TestSkillProseNamesOnlyDeclaredOpClasses is a one-way check: it rejects prose naming a capability TG does
// not have. It cannot notice the opposite failure — a capability TG DOES have that the checker would reject
// — and that is the failure that actually occurred: start-container was registered, never added to the
// hand-written list, so the first skill to mention it would have broken the build over a verb TG can
// genuinely perform.
//
// This asserts the acceptance direction against the SAME regex the rejecter uses, so a registry class whose
// name the pattern matches is always admitted.
func TestEveryRegistryOpClassIsAcceptedInProse(t *testing.T) {
	declared := map[string]bool{}
	for _, spec := range opschema.Specs() {
		declared[spec.OpClass] = true
	}
	if len(declared) == 0 {
		t.Fatal("the registry returned no op-classes — this oracle would pass vacuously")
	}
	opish := regexp.MustCompile(`\b(?:restart|start|stop|reload|disk|guest|scale|prune|trim|vacuum)-[a-z]+\b`)
	checked := 0
	for class := range declared {
		if !opish.MatchString(class) {
			continue // the rejecter's pattern never sees this name, so it cannot reject it
		}
		checked++
		if !declared[class] {
			t.Errorf("op-class %q is in the registry but would be rejected as prose promising a capability "+
				"that does not exist — a real verb made unmentionable", class)
		}
	}
	if checked == 0 {
		t.Fatal("no registry op-class matched the op-ish pattern — the rejecter and this oracle are looking " +
			"at different things, so neither guards the other")
	}
}

func TestSkillProseNamesOnlyDeclaredOpClasses(t *testing.T) {
	// ★ THE CLOSED ENUMERATION IS READ FROM THE REGISTRY, NOT LISTED HERE.
	//
	// It used to be a hand-written map, with a comment arguing that "a hand-listed set is exactly what the
	// next addition goes missing from, so the test fails loudly rather than silently blessing an unknown
	// name". The first half came true and the second did not: start-container was registered and never added
	// here, so the list held 6 of the registry's 7 and nothing failed.
	//
	// The consequence is the opposite of a missing capability — it is a REAL capability made unmentionable.
	// This test flags an op-ish token that is NOT declared as "prose promising a capability that does not
	// exist", so the moment any skill mentions start-container the build breaks over a verb TG can actually
	// perform. A hand-kept mirror does not fail loudly; it fails in whichever direction its author last
	// guessed.
	//
	// agent/loop.go already imports opschema (it is the same registry the parser, interceptor, runner and
	// effect leaf read), so this adds no dependency — it removes a copy.
	declared := map[string]bool{}
	for _, spec := range opschema.Specs() {
		declared[spec.OpClass] = true
	}
	if len(declared) < 5 {
		t.Fatalf("only %d op-classes came back from the registry — the enumeration is not being read, so "+
			"every assertion below would pass vacuously", len(declared))
	}
	// Hyphenated tokens built from an op-class VERB — the shape an op-class name takes. Anything matching
	// this that is not declared is prose promising a capability that does not exist.
	opish := regexp.MustCompile(`\b(?:restart|start|stop|reload|disk|guest|scale|prune|trim|vacuum)-[a-z]+\b`)
	for _, sk := range Default().All() {
		for _, m := range opish.FindAllString(sk.Body, -1) {
			if !declared[m] {
				t.Errorf("skill %q names %q, which is not a declared op-class — prose must not instruct a "+
					"remedy the agent cannot actually propose; it will substitute the nearest one instead",
					sk.Name, m)
			}
		}
	}
}

// The disk-filling guidance must state the constraint that decides the answer, and must not instruct an
// undeclared remedy. This is the regression test for the 33 inapplicable disk-grow proposals.
func TestDiskGuidanceStatesTheLoopbackConstraint(t *testing.T) {
	var body string
	for _, sk := range Default().All() {
		if sk.Name == "alert-class-playbooks" {
			body = sk.Body
		}
	}
	if body == "" {
		t.Fatal("alert-class-playbooks is missing from the compiled registry")
	}
	if !strings.Contains(body, "LOOPBACK") {
		t.Error("the disk playbook must name the loopback constraint — it is what decides whether disk-grow " +
			"can work at all, and it is the reason 33 of 96 disk-fill faults got an inapplicable proposal")
	}
	if !strings.Contains(body, "NO prune/trim/vacuum op-class") {
		t.Error("the disk playbook must say plainly that no prune/trim/vacuum op-class exists — instructing " +
			"one that does not is how the agent came to substitute disk-grow")
	}
	// MUTATION CONTROL: the old prose instructed the remedy as an action. If that phrasing returns, this fails.
	if strings.Contains(body, "-> prune/trim/vacuum") {
		t.Error("the disk playbook instructs prune/trim/vacuum as an ACTION, but no such op-class exists")
	}
}

// get-active-alerts must be aimed at THIS host's duplicate alerts, not only at upstream cascade. One fault
// raises four LibreNMS rules here, so 3.4 agent runs are spent per device-down incident re-answering a
// question an earlier session already answered correctly.
func TestActiveAlertsIsAimedAtDuplicateWork(t *testing.T) {
	for _, name := range []string{"alert-class-playbooks", "triage-protocol"} {
		var body string
		for _, sk := range Default().All() {
			if sk.Name == name {
				body = sk.Body
			}
		}
		if !strings.Contains(body, "get-active-alerts on THIS host") {
			t.Errorf("skill %q must point get-active-alerts at THIS host's own duplicate alerts, not only at "+
				"upstream/sibling cascade — otherwise every duplicate alert is re-investigated from scratch", name)
		}
	}
}

// THE CONFIDENCE NUMBER HAD NO DEFINITION IN ANY PROMPT.
//
// Measured live by the calibrator: N=64, Brier 0.4633, ECE 0.5114. A Brier of 0.25 is what ALWAYS GUESSING
// 0.5 scores, so the agent's stated confidence was carrying less information than a coin. The distribution
// says why — of 625 sessions with a confidence, 210 sit at exactly 0.90, 146 at 0.85, 127 at 0.80.
//
// The cause was not a bad model. The ONLY mention of confidence in any composed skill was "lower your
// confidence and stop or escalate instead" — which says when to lower it and never what the number MEANS.
// Nothing ever told the agent it is a frequency claim scored against outcomes.
func TestTheConfidenceNumberIsDefinedForTheAgent(t *testing.T) {
	var body string
	for _, sk := range Default().All() {
		if sk.Name == "proving-your-work" {
			body = sk.Body
		}
	}
	if body == "" {
		t.Fatal("proving-your-work is missing from the compiled registry")
	}
	// It must state that confidence is scored against OUTCOMES — the property that makes it a measurement
	// rather than a mood. Without this the number is decoration.
	if !strings.Contains(body, "FREQUENCY CLAIM") {
		t.Error("the prompt must define confidence as a FREQUENCY CLAIM about the agent's own hit-rate — " +
			"otherwise there is nothing to calibrate and 0.9-on-everything is a rational answer")
	}
	// Anchors: without concrete levels the agent has no basis to choose a number other than "high".
	for _, anchor := range []string{"0.9+", "0.7", "0.5"} {
		if !strings.Contains(body, anchor) {
			t.Errorf("the prompt gives no anchor for %s — an undefined scale collapses to its top end", anchor)
		}
	}
	// The asymmetry is the point: a confident wrong answer is the failure the number exists to catch.
	if !strings.Contains(body, "Being wrong at 0.5 is honest") {
		t.Error("the prompt must say that a wrong answer at HIGH confidence is the failure mode — otherwise " +
			"the agent has no reason to ever go low")
	}
}

// The guidance must be on an ALWAYS-applied skill. On a `full`-gated one it would not reach the fast/human-led
// exec classes at all, and confidence is emitted on every proposal regardless of class.
func TestConfidenceGuidanceIsAlwaysApplied(t *testing.T) {
	for _, sk := range Default().All() {
		if sk.Name != "proving-your-work" {
			continue
		}
		if !sk.AppliesWhen(Context{Phase: PhaseInvestigate, ExecClass: execclass.FastAgent}) {
			t.Fatal("proving-your-work must apply to EVERY exec class — a proposal carries a confidence " +
				"whatever class produced it, so class-gating the definition leaves some sessions undefined")
		}
		return
	}
	t.Fatal("proving-your-work not found")
}

// spec/026 REQ-2601/REQ-2609 (T-026-1) — the v1.3.0 free-form propose duty.
//
// v1.2's "no slug -> STOP" made the catalog the measured stand-down generator. v1.3 must (a) declare the
// free-form duty with its full rigor (undo_sketch, no substitution, record-only honesty), (b) keep STOP
// only for the genuinely no-safe-action case, (c) state that actor evidence never suppresses a proposal
// (owner ruling 2026-07-31, fault 1406), and (d) keep the hard floor verbatim-intact. The rejecter above
// (TestSkillProseNamesOnlyDeclaredOpClasses) still guards the OTHER direction: free-form examples must not
// be spelled with registered-class-shaped verbs, so prose can never fabricate a registered capability.
func TestConservativeCatalogDeclaresTheFreeFormProposeDuty(t *testing.T) {
	var sk *Skill
	for _, s := range Default().All() {
		if s.Name == "conservative-remediation" {
			c := s
			sk = &c
		}
	}
	if sk == nil {
		t.Fatal("conservative-remediation missing from the compiled registry")
	}
	if sk.Version != "1.3.0" {
		t.Fatalf("conservative-remediation must be v1.3.0 (the free-form propose duty), got %s", sk.Version)
	}
	// (a) the duty, with its rigor
	if !hasAll(sk.Body, "free-form op_class", "undo_sketch", "never substitute", "RECORDED for operator review") {
		t.Error("v1.3.0 must declare the free-form propose duty with undo_sketch, the no-substitution rule, " +
			"and the record-only honesty statement")
	}
	// (b) STOP survives only as the no-safe-action outcome — the stand-down-generator phrasing must be gone
	if strings.Contains(sk.Body, "NO OP-CLASS EXISTS for these — the correct outcome is a STOP") {
		t.Error("the v1.2 stand-down-generator branch is back: a missing registry slug must yield a " +
			"free-form proposal, not an instructed STOP")
	}
	if !strings.Contains(sk.Body, "STOP remains correct ONLY when no safe conservative reversible action exists") {
		t.Error("v1.3.0 must keep STOP for the genuinely no-safe-action case — removing STOP entirely " +
			"overshoots the duty into recklessness")
	}
	// (c) actor evidence never suppresses (REQ-2609)
	if !hasAll(sk.Body, "ACTOR EVIDENCE", "NEVER suppresses the proposal") {
		t.Error("v1.3.0 must state that actor evidence never suppresses the proposal (REQ-2609)")
	}
	// (d) hard floor intact (same anchors TestConservativeCatalogAlwaysPresentWithFloor asserts)
	if !hasAll(sk.Body, "HARD FLOOR", "etcd", "host reboot", "dropdb", "terraform destroy") {
		t.Error("the hard floor must survive the v1.3.0 rewrite verbatim in intent")
	}
}
