package credential

import (
	"strings"
	"testing"

	"github.com/territory-grounder/grounder/core/config"
)

func TestPlaneSetDisjointOK(t *testing.T) {
	p := PlaneSet{
		ReadTriage: []config.SecretRef{"file:/secrets/tg-syslog-ro", "env:NETBOX_TOKEN", "file:/secrets/tg-openbao-token"},
		Actuation:  []config.SecretRef{"file:/secrets/tg-actuator", "file:/secrets/pve-actuate-token", "env:AWX_TOKEN"},
	}
	if err := p.Validate(); err != nil {
		t.Fatalf("disjoint planes must validate: %v", err)
	}
	if !strings.Contains(p.Summary(), "3 read-triage ref(s) disjoint from 3 actuation ref(s)") {
		t.Fatalf("summary: %s", p.Summary())
	}
}

func TestPlaneSetCrossFailsClosed(t *testing.T) {
	// the SAME key used for read AND actuation — exactly the REQ-2203 violation.
	p := PlaneSet{
		ReadTriage: []config.SecretRef{"file:/secrets/tg-syslog-ro", "file:/secrets/tg-actuator"},
		Actuation:  []config.SecretRef{"file:/secrets/tg-actuator"},
	}
	err := p.Validate()
	if err == nil {
		t.Fatal("a reference in both planes must fail closed")
	}
	if !strings.Contains(err.Error(), "tg-actuator") || !strings.Contains(err.Error(), "REQ-2203") {
		t.Fatalf("error must name the crossing ref + the requirement: %v", err)
	}
}

func TestPlaneSetEmptyRefsIgnored(t *testing.T) {
	// unconfigured ("") references never cross, even if both planes have them.
	p := PlaneSet{ReadTriage: []config.SecretRef{"", "env:R"}, Actuation: []config.SecretRef{"", "env:A"}}
	if err := p.Validate(); err != nil {
		t.Fatalf("empty refs must be ignored: %v", err)
	}
}

func TestCredentialClassString(t *testing.T) {
	if ClassActuation.String() != "actuation" || ClassReadTriage.String() != "read-triage" {
		t.Fatal("class strings wrong")
	}
}

// ---------------------------------------------------------------------------------------------------------
// TG-153 — the PROCESS-plane assertion.
// ---------------------------------------------------------------------------------------------------------

// TestTriagePlaneRefusesAnyActuationRef is THE boot oracle of the process split: a worker that declares
// TG_CREDENTIAL_PLANE=triage and is nonetheless handed the estate-mutating SSH key must REFUSE TO BOOT. The
// refs here are disjoint — the pre-TG-153 Validate() passes them happily — which is the whole point: two
// disjoint references in ONE address space is exactly the co-holding posture the split exists to end.
//
// KILLING MUTATION (executed 2026-08-04): delete the `case ProcessPlaneTriage:` arm from
// PlaneSet.ValidateFor (so it degrades to the historic Validate()). This test then FAILS with
//
//	"a triage-plane process handed the actuation SSH key MUST refuse to boot — it got a clean pass,
//	 so a deployment labelled `triage` would run the LLM agent over untrusted alert/syslog/host content
//	 in the same process that holds the key which mutates the estate. That is TG-153 unfixed."
//
// restored, green.
func TestTriagePlaneRefusesAnyActuationRef(t *testing.T) {
	p := PlaneSet{
		ReadTriage: []config.SecretRef{"bao:tg/hostdiag#key", "env:NETBOX_TOKEN"},
		Actuation:  []config.SecretRef{"bao:tg/actuator#key"},
	}
	if err := p.Validate(); err != nil {
		t.Fatalf("guard: these refs are DISJOINT, so the historic check must pass — otherwise this test would "+
			"prove the plane assertion while actually exercising the old one: %v", err)
	}
	err := p.ValidateFor(ProcessPlaneTriage)
	if err == nil {
		t.Fatal("a triage-plane process handed the actuation SSH key MUST refuse to boot — it got a clean pass, " +
			"so a deployment labelled `triage` would run the LLM agent over untrusted alert/syslog/host content " +
			"in the same process that holds the key which mutates the estate. That is TG-153 unfixed.")
	}
	// The refusal has to be actionable: name the offending REFERENCE (safe — INV-13, a ref is not a secret)
	// and the plane, or an operator is told only that "something crossed".
	for _, want := range []string{"bao:tg/actuator#key", "triage", "TG-153"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("refusal must name %q so the operator knows which .env line to move; got: %v", want, err)
		}
	}
}

// TestActuationPlaneRefusesAnyReadTriageRef is the mirror. An actuation process holding the estate READ
// tokens is a process an intruder pivots INTO the triage plane from — the same blast-radius merge measured
// from the other end.
//
// KILLING MUTATION (executed): delete the `case ProcessPlaneActuation:` arm ⇒ RED with the message below.
func TestActuationPlaneRefusesAnyReadTriageRef(t *testing.T) {
	p := PlaneSet{
		ReadTriage: []config.SecretRef{"env:NETBOX_TOKEN"},
		Actuation:  []config.SecretRef{"bao:tg/actuator#key"},
	}
	err := p.ValidateFor(ProcessPlaneActuation)
	if err == nil {
		t.Fatal("an actuation-plane process handed the estate read token MUST refuse to boot — a compromise of " +
			"the mutating worker would then also yield the read plane, which is the split collapsing in the " +
			"direction people forget to check")
	}
	if !strings.Contains(err.Error(), "env:NETBOX_TOKEN") || !strings.Contains(err.Error(), "actuation") {
		t.Fatalf("refusal must name the read ref and the plane: %v", err)
	}
}

// TestBothPlaneIsByteIdenticalToTheHistoricCheck is the NO-REGRESSION oracle for every existing deployment.
// `both` is the default; on upgrade, every installation that sets no TG_CREDENTIAL_PLANE must behave exactly
// as it did before TG-153. ValidateFor(both) must therefore agree with Validate() on EVERY input — including
// the co-holding configurations that are the norm today (one worker, actuation key AND read tokens).
//
// KILLING MUTATION (executed): make ValidateFor's switch fall through to the triage arm for ProcessPlaneBoth
// (`case ProcessPlaneTriage, ProcessPlaneBoth:`). This test then FAILS with
//
//	"plane=both must behave EXACTLY like the pre-TG-153 check ... every existing single-worker deployment
//	 would refuse to boot on upgrade"
//
// which is the real-world consequence: a security fix that bricks every installation is a security fix
// nobody runs. Restored, green.
func TestBothPlaneIsByteIdenticalToTheHistoricCheck(t *testing.T) {
	cases := []struct {
		name string
		set  PlaneSet
	}{
		{"today's single co-holding worker", PlaneSet{
			ReadTriage: []config.SecretRef{"env:NETBOX_TOKEN", "env:PVE_TOKEN"},
			Actuation:  []config.SecretRef{"file:/secrets/one_key", "env:PROXMOX_TOKEN", "env:AWX_TOKEN"},
		}},
		{"triage refs only", PlaneSet{ReadTriage: []config.SecretRef{"env:NETBOX_TOKEN"}}},
		{"actuation refs only", PlaneSet{Actuation: []config.SecretRef{"file:/secrets/one_key"}}},
		{"nothing configured", PlaneSet{}},
		{"unset refs on both planes", PlaneSet{ReadTriage: []config.SecretRef{""}, Actuation: []config.SecretRef{""}}},
		{"a genuinely crossing ref (must still refuse)", PlaneSet{
			ReadTriage: []config.SecretRef{"file:/secrets/one_key"},
			Actuation:  []config.SecretRef{"file:/secrets/one_key"},
		}},
	}
	if len(cases) == 0 { // vacuity floor: a table-driven no-regression proof over zero rows proves nothing
		t.Fatal("no cases — this oracle would pass vacuously")
	}
	for _, tc := range cases {
		historic := tc.set.Validate()
		underBoth := tc.set.ValidateFor(ProcessPlaneBoth)
		if (historic == nil) != (underBoth == nil) {
			t.Fatalf("%s: plane=both must behave EXACTLY like the pre-TG-153 check (historic=%v, both=%v) — "+
				"it is the DEFAULT, so any divergence means every existing single-worker deployment changes "+
				"behaviour, or refuses to boot, on upgrade", tc.name, historic, underBoth)
		}
		if historic != nil && historic.Error() != underBoth.Error() {
			t.Fatalf("%s: plane=both must return the historic refusal verbatim; historic=%q both=%q", tc.name, historic, underBoth)
		}
	}
}

func TestParseProcessPlane(t *testing.T) {
	// Unset ⇒ both. This single line is what makes the fix deployable: an operator who upgrades and changes
	// nothing keeps the posture they had.
	if p, err := ParseProcessPlane(""); err != nil || p != ProcessPlaneBoth {
		t.Fatalf("unset TG_CREDENTIAL_PLANE must default to both (got %q, %v)", p, err)
	}
	for _, in := range []string{"triage", "TRIAGE", " triage "} {
		if p, err := ParseProcessPlane(in); err != nil || p != ProcessPlaneTriage {
			t.Fatalf("%q must parse as triage (got %q, %v)", in, p, err)
		}
	}
	if p, err := ParseProcessPlane("actuation"); err != nil || p != ProcessPlaneActuation {
		t.Fatalf("actuation must parse (got %q, %v)", p, err)
	}
	// A typo must be an ERROR, never a silent fall back to `both`: the operator would believe they had split
	// the planes while the actuation key sat next to the agent — the one outcome nobody investigates.
	if _, err := ParseProcessPlane("triage-only"); err == nil {
		t.Fatal("an unrecognised TG_CREDENTIAL_PLANE must FAIL the boot, not silently default to `both` — a " +
			"deployment that believes it is split but is not is worse than one that knows it is not")
	}
}

func TestProcessPlaneHolds(t *testing.T) {
	for _, tc := range []struct {
		p                 ProcessPlane
		triage, actuation bool
	}{
		{ProcessPlaneBoth, true, true},
		{ProcessPlaneTriage, true, false},
		{ProcessPlaneActuation, false, true},
	} {
		if tc.p.HoldsTriage() != tc.triage || tc.p.HoldsActuation() != tc.actuation {
			t.Fatalf("plane %q: HoldsTriage=%v HoldsActuation=%v, want %v/%v", tc.p, tc.p.HoldsTriage(), tc.p.HoldsActuation(), tc.triage, tc.actuation)
		}
	}
}

// TestSummaryForNeverCallsBothASplit guards the LOG, because the log is what a human reads to decide whether
// the split is live. The pre-TG-153 worker printed "plane split OK: 2 read-triage ref(s) disjoint from 2
// actuation ref(s)" while holding both in one process — a true sentence that read as an all-clear, and one
// reason this gap survived from TG-157 to TG-153.
func TestSummaryForNeverCallsBothASplit(t *testing.T) {
	p := PlaneSet{
		ReadTriage: []config.SecretRef{"env:NETBOX_TOKEN"},
		Actuation:  []config.SecretRef{"file:/secrets/one_key"},
	}
	both := p.SummaryFor(ProcessPlaneBoth)
	if !strings.Contains(both, "This is not a split") {
		t.Fatalf("plane=both must say plainly that ONE process holds both planes; got %q", both)
	}
	if !strings.Contains(p.SummaryFor(ProcessPlaneTriage), "CANNOT reach an estate-mutating credential") {
		t.Fatalf("plane=triage summary must state what the compromise of this process does NOT yield; got %q", p.SummaryFor(ProcessPlaneTriage))
	}
	if !strings.Contains(p.SummaryFor(ProcessPlaneActuation), "no untrusted-content reader") {
		t.Fatalf("plane=actuation summary must state that it reads no untrusted content; got %q", p.SummaryFor(ProcessPlaneActuation))
	}
}

// spec/022 T-022-5 (REQ-2205): the plane-split validation errors name the crossing/misplaced reference —
// which is safe ONLY when it IS a reference. A pasted inline literal in the plane config must render as
// its shape, never its value, in every validation error.
func TestPlaneSplitValidationNeverEchoesALiteralSecret(t *testing.T) {
	const canary = "hunter2-canary"
	p := PlaneSet{
		ReadTriage: []config.SecretRef{config.SecretRef(canary)},
		Actuation:  []config.SecretRef{config.SecretRef(canary)},
	}
	err := p.Validate()
	if err == nil {
		t.Fatal("a crossing reference must fail closed")
	}
	if strings.Contains(err.Error(), canary) {
		t.Fatalf("the crossing-reference error echoed a possible inline secret: %v", err)
	}
	for _, plane := range []ProcessPlane{ProcessPlaneTriage, ProcessPlaneActuation} {
		q := PlaneSet{ReadTriage: []config.SecretRef{config.SecretRef(canary)}, Actuation: []config.SecretRef{config.SecretRef(canary + "2")}}
		if err := q.ValidateFor(plane); err != nil && strings.Contains(err.Error(), canary) {
			t.Fatalf("ValidateFor(%s) echoed a possible inline secret: %v", plane, err)
		}
	}
}
