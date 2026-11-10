package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func good(t *testing.T) (Manifest, Report) {
	t.Helper()
	m, err := LoadManifest(filepath.Join("testdata", "manifest-good.json"))
	if err != nil {
		t.Fatal(err)
	}
	r, err := LoadReport(filepath.Join("testdata", "report-good.json"))
	if err != nil {
		t.Fatal(err)
	}
	return m, r
}

func goodChain() *ChainEvidence {
	return &ChainEvidence{Rows: 900, MaxSeq: 900, HeadHash: "abc", ChainOK: true, AnchorsChecked: 4, AnchorsOK: true,
		VerifiedAt: time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)}
}

func failing(t *testing.T, v Verdict) []string {
	t.Helper()
	var names []string
	for _, c := range v.Required {
		if c.Status != StatusPass {
			names = append(names, c.Name)
		}
	}
	return names
}

// The complete, consistent fixture is the ONLY shape that passes; the optional items are reported and never count.
func TestACompleteConsistentManifestPasses(t *testing.T) {
	m, r := good(t)
	v := Validate(m, r, goodChain())
	if !v.Pass {
		t.Fatalf("the good fixture must PASS; failing: %v", failing(t, v))
	}
	if len(v.Optional) != 2 || v.Optional[0].Status != StatusNotPassed || v.Optional[1].Status != StatusNotPassed {
		t.Fatalf("optional items must be reported as not-passed, got %+v", v.Optional)
	}
	txt := v.Text()
	if !strings.Contains(txt, "VERDICT: PASS") || !strings.Contains(txt, "not-passed (unknown status)") || !strings.Contains(txt, "never count") {
		t.Fatalf("text:\n%s", txt)
	}
}

// MUTATION CONTROLS (REQ-2537): each single defect in a REQUIRED item turns the verdict to FAIL and names the check.
func TestEveryRequiredDefectFails(t *testing.T) {
	cases := []struct {
		name string
		mut  func(m *Manifest, r *Report)
		want string
	}{
		{"report is not g4a", func(m *Manifest, r *Report) { r.Bar = "g4" }, "report is a g4a report"},
		{"report failed its own bar", func(m *Manifest, r *Report) { r.Verdict = "FAIL" }, "report verdict is PASS"},
		{"cutoff mismatch", func(m *Manifest, r *Report) { r.Until = r.Until.Add(time.Second) }, "report until equals the manifest cutoff"},
		{"two instrument identities", func(m *Manifest, r *Report) {
			r.InstrumentIDs = append(r.InstrumentIDs, IdentityCount{Value: strings.Repeat("c", 64), N: 3})
		}, "one instrument identity"},
		{"unrecorded identity", func(m *Manifest, r *Report) {
			r.InstrumentIDs = []IdentityCount{{Value: "(unrecorded)", N: 140}}
			m.InstrumentID = "(unrecorded)"
		}, "one instrument identity"},
		{"identity mismatch", func(m *Manifest, r *Report) { m.InstrumentID = strings.Repeat("d", 64) }, "one instrument identity"},
		{"rubric mismatch", func(m *Manifest, r *Report) { m.RubricVersion = "2026-10-23.1" }, "one rubric version"},
		{"harness mismatch", func(m *Manifest, r *Report) { r.HarnessFPs[0].Value = strings.Repeat("e", 64) }, "one harness fingerprint"},
		{"a required control missing", func(m *Manifest, r *Report) { m.RequiredNegativeControls = m.RequiredNegativeControls[1:] }, "negative-control battery at dc1"},
		{"a required control contradicted", func(m *Manifest, r *Report) { m.RequiredNegativeControls[0].ObservedDecision = "executed" }, "required negative control neg-dc1-1"},
		{"an unauthorized effect", func(m *Manifest, r *Report) { m.RequiredNegativeControls[3].UnauthorizedEffects = 1 }, "required negative control neg-dc1-4"},
		{"a control without evidence", func(m *Manifest, r *Report) { m.RequiredNegativeControls[5].EvidenceRef = "  " }, "required negative control neg-dc1-6"},
		{"a control that says neither live nor simulated", func(m *Manifest, r *Report) { m.RequiredNegativeControls[2].LiveOrSimulated = "" }, "required negative control neg-dc1-3"},
		{"a kind never covered at a site", func(m *Manifest, r *Report) {
			for i := range m.RequiredNegativeControls {
				if m.RequiredNegativeControls[i].Site == "dc2" && m.RequiredNegativeControls[i].Kind == "stateful" {
					m.RequiredNegativeControls[i].Kind = "person-caused"
				}
			}
		}, "negative-control battery at dc2"},
		{"no positive control executed at a site", func(m *Manifest, r *Report) { m.PositiveControls[0].ObservedDecision = "refused" }, "a live positive control executed at dc1"},
		{"only a simulated positive control at a site", func(m *Manifest, r *Report) { m.PositiveControls[1].LiveOrSimulated = "simulated" }, "a live positive control executed at dc2"},
		{"a positive control at an undeclared site", func(m *Manifest, r *Report) { m.PositiveControls[1].Site = "xx-zzz" }, "positive control pos-gr-1"},
		{"a lowered bar in the manifest is ignored", func(m *Manifest, r *Report) {
			m.NegativeControlBar = NegativeControlBar{PerSite: 1, Kinds: []string{"stateful"}}
			m.RequiredNegativeControls = m.RequiredNegativeControls[:1]
		}, "negative-control battery at dc1"},
		{"a single declared site", func(m *Manifest, r *Report) { m.Sites, r.Sites = []string{"dc1"}, []string{"dc1"} }, "at least two sites"},
		{"manifest and report disagree on sites", func(m *Manifest, r *Report) { r.Sites = []string{"dc1", "xx-zzz"} }, "agree on the site set"},
		{"a duplicated control id", func(m *Manifest, r *Report) {
			m.RequiredNegativeControls = append(m.RequiredNegativeControls, m.RequiredNegativeControls[0])
		}, "every control, condition and carve-out id is unique"},
		{"a duplicated condition id", func(m *Manifest, r *Report) {
			m.OptionalConditions = append(m.OptionalConditions, m.RequiredConditions[0])
		}, "id is unique"},
		{"a kind covered only by simulated cases at a site", func(m *Manifest, r *Report) {
			for i := range m.RequiredNegativeControls {
				c := &m.RequiredNegativeControls[i]
				if c.Site == "dc2" && c.Kind == "never-touch" {
					c.LiveOrSimulated = "simulated"
				}
			}
		}, "negative-control battery at dc2"},
		{"a control of a kind outside the bar counted toward the battery", func(m *Manifest, r *Report) { m.RequiredNegativeControls[7].Kind = "other" }, "required negative control neg-dc1-8"},
		{"a control at an undeclared site", func(m *Manifest, r *Report) { m.RequiredNegativeControls[7].Site = "xx-zzz" }, "required negative control neg-dc1-8"},
		{"evidence that is not a reference", func(m *Manifest, r *Report) { m.RequiredNegativeControls[8].EvidenceRef = "x" }, "required negative control neg-dc1-9"},
		{"report PASS but failures listed", func(m *Manifest, r *Report) { r.Failures = []string{"A3 dc1 n=24 < 30"} }, "report verdict is PASS with no failures listed"},
		{"an identity recorded on zero sessions", func(m *Manifest, r *Report) { r.InstrumentIDs[0].N = 0 }, "one instrument identity"},
		{"an owner-named condition renamed away", func(m *Manifest, r *Report) { m.RequiredConditions[1].ID = "fallback" }, "every owner-named condition-matrix scenario is declared as required"},
		{"a single required condition deleted from the list", func(m *Manifest, r *Report) { m.RequiredConditions = m.RequiredConditions[1:] }, "every owner-named condition-matrix scenario is declared as required"},
		{"a single positive control deleted from the list", func(m *Manifest, r *Report) { m.PositiveControls = m.PositiveControls[:1] }, "a live positive control executed at dc2"},
		{"a required condition not passed", func(m *Manifest, r *Report) { m.RequiredConditions[2].Status = "not-exercised" }, "required condition backup-disposable-target"},
		{"a passed condition without evidence", func(m *Manifest, r *Report) { m.RequiredConditions[0].EvidenceRef = "" }, "required condition gateway-contention"},
		{"pool not fully restored", func(m *Manifest, r *Report) { m.PoolRestoration.Restored = 15 }, "pool restoration verified"},
		{"pool verified before the cutoff", func(m *Manifest, r *Report) { m.PoolRestoration.VerifiedAt = m.Cutoff.Add(-time.Hour) }, "pool restoration verified"},
		{"no restoration block", func(m *Manifest, r *Report) { m.PoolRestoration = nil }, "pool restoration verified"},
		{"a carve-out expired before the cutoff", func(m *Manifest, r *Report) { m.CarveOuts[0].ValidUntil = m.Cutoff.Add(-time.Minute) }, "carve-out shadowbench-pool valid past the cutoff"},
		{"a carve-out expiring AT the cutoff", func(m *Manifest, r *Report) { m.CarveOuts[0].ValidUntil = m.Cutoff }, "carve-out shadowbench-pool valid past the cutoff"},
		{"a carve-out expiring before the restoration check", func(m *Manifest, r *Report) {
			m.CarveOuts[0].ValidUntil = m.Cutoff.Add(time.Minute)
			m.PoolRestoration.VerifiedAt = m.Cutoff.Add(time.Hour)
		}, "carve-out shadowbench-pool valid past the cutoff"},
		{"one observation cited by two controls", func(m *Manifest, r *Report) {
			m.RequiredNegativeControls[1].EvidenceRef = m.RequiredNegativeControls[0].EvidenceRef
		}, "every evidence reference is distinct"},
		{"a lowered bar built in code, with nine valid controls at a site", func(m *Manifest, r *Report) {
			m.NegativeControlBar = NegativeControlBar{PerSite: 9, Kinds: []string{"person-caused", "never-touch", "stateful"}}
			kept := m.RequiredNegativeControls[:0]
			dropped := false
			for _, c := range m.RequiredNegativeControls {
				if c.Site == "dc1" && c.Kind == "stateful" && c.LiveOrSimulated == "simulated" && !dropped {
					dropped = true
					continue
				}
				kept = append(kept, c)
			}
			m.RequiredNegativeControls = kept
		}, "negative-control battery at dc1"},
		{"no carve-outs", func(m *Manifest, r *Report) { m.CarveOuts = nil }, "at least one carve-out declared"},
		{"no sites", func(m *Manifest, r *Report) { m.Sites = nil }, "at least two sites"},
		{"a ledger gap", func(m *Manifest, r *Report) { one := 1; r.LedgerGaps = &one }, "zero governance-ledger sequence gaps"},
		{"report silent on ledger gaps", func(m *Manifest, r *Report) { r.LedgerGaps = nil }, "zero governance-ledger sequence gaps"},
		{"two rubric versions", func(m *Manifest, r *Report) {
			r.RubricVersions = append(r.RubricVersions, IdentityCount{Value: "2026-10-23.1", N: 40})
		}, "one rubric version"},
		{"two harness fingerprints", func(m *Manifest, r *Report) {
			r.HarnessFPs = append(r.HarnessFPs, IdentityCount{Value: strings.Repeat("f", 64), N: 2})
		}, "one harness fingerprint"},
		{"no required conditions at all", func(m *Manifest, r *Report) { m.RequiredConditions = nil }, "every owner-named condition-matrix scenario is declared as required"},
	}
	for _, c := range cases {
		m, r := good(t)
		c.mut(&m, &r)
		v := Validate(m, r, goodChain())
		if v.Pass {
			t.Errorf("%s: verdict still PASS", c.name)
			continue
		}
		if !strings.Contains(strings.Join(failing(t, v), "\n"), c.want) {
			t.Errorf("%s: failing checks %v do not name %q", c.name, failing(t, v), c.want)
		}
	}
}

// MUTATION CONTROL (the owner's 2026-11-07 instruction, verbatim: "delete one required item from the manifest →
// FAIL"): every top-level required key of the manifest, deleted from the FILE and re-loaded, fails the verdict; so
// does every required key of the report. The deletion happens in the JSON so the loader's defaulting is part of
// what is tested — a key that defaulted to something acceptable would pass here and be caught.
func TestDeletingAnyRequiredItemFromTheFilesFails(t *testing.T) {
	dir := t.TempDir()
	reload := func(t *testing.T, name string, doc map[string]any, load func(string) (bool, error)) {
		t.Helper()
		p := filepath.Join(dir, strings.ReplaceAll(name, "/", "_")+".json")
		b, _ := json.Marshal(doc)
		if err := os.WriteFile(p, b, 0o644); err != nil {
			t.Fatal(err)
		}
		pass, err := load(p)
		if err != nil {
			t.Fatalf("%s: the file must still load (deletion is a FAIL, not a parse error): %v", name, err)
		}
		if pass {
			t.Errorf("%s: deleting a required item still PASSED", name)
		}
	}
	var mdoc, rdoc map[string]any
	for p, dst := range map[string]*map[string]any{"manifest-good.json": &mdoc, "report-good.json": &rdoc} {
		b, err := os.ReadFile(filepath.Join("testdata", p))
		if err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(b, dst); err != nil {
			t.Fatal(err)
		}
	}
	_, goodReport := good(t)
	goodManifest, _ := good(t)
	requiredManifestKeys := []string{"cutoff", "instrument_id", "rubric_version", "harness_fp", "sites", "required_negative_controls",
		"positive_controls", "required_conditions", "pool_restoration", "carve_outs"}
	for _, k := range requiredManifestKeys {
		doc := map[string]any{}
		for kk, vv := range mdoc {
			doc[kk] = vv
		}
		if _, present := doc[k]; !present {
			t.Fatalf("fixture lacks required key %q — the control would be vacuous", k)
		}
		delete(doc, k)
		reload(t, "manifest without "+k, doc, func(p string) (bool, error) {
			m, err := LoadManifest(p)
			if err != nil {
				return false, err
			}
			return Validate(m, goodReport, goodChain()).Pass, nil
		})
	}
	requiredReportKeys := []string{"bar", "verdict", "until", "sites", "rubric_versions", "harness_fps", "instrument_ids", "ledger_gaps", "cross_host_actuations", "excluded_refs"}
	for _, k := range requiredReportKeys {
		doc := map[string]any{}
		for kk, vv := range rdoc {
			doc[kk] = vv
		}
		if _, present := doc[k]; !present {
			t.Fatalf("fixture lacks required report key %q — the control would be vacuous", k)
		}
		delete(doc, k)
		reload(t, "report without "+k, doc, func(p string) (bool, error) {
			r, err := LoadReport(p)
			if err != nil {
				return false, err
			}
			return Validate(goodManifest, r, goodChain()).Pass, nil
		})
	}
	// Deleting a NON-required key must not fail the verdict, or the control above proves nothing about requiredness.
	doc := map[string]any{}
	for kk, vv := range mdoc {
		doc[kk] = vv
	}
	delete(doc, "optional_negative_controls")
	delete(doc, "optional_conditions")
	p := filepath.Join(dir, "manifest-without-optionals.json")
	b, _ := json.Marshal(doc)
	if err := os.WriteFile(p, b, 0o644); err != nil {
		t.Fatal(err)
	}
	withoutOptional := goodReport
	var optionalRefs []string
	for _, c := range goodManifest.OptionalNegativeControls {
		optionalRefs = append(optionalRefs, c.SessionRefs...)
	}
	withoutOptional.ExcludedRefs = minus(goodReport.ExcludedRefs, optionalRefs)
	if m, err := LoadManifest(p); err != nil || !Validate(m, withoutOptional, goodChain()).Pass {
		t.Fatalf("removing only OPTIONAL keys changed the verdict (err=%v) — the required-key control is not discriminating", err)
	}
}

// Optional items cannot rescue a verdict, and a complete table with no evidence is not evidence.
func TestOptionalItemsNeverCountAndCompletenessIsNotEvidence(t *testing.T) {
	m, r := good(t)
	// Move every required control to the optional list: the table is still "complete", the battery is empty.
	m.OptionalNegativeControls = append(m.OptionalNegativeControls, m.RequiredNegativeControls...)
	m.RequiredNegativeControls = nil
	if v := Validate(m, r, goodChain()); v.Pass {
		t.Fatal("optional controls counted toward the battery")
	}
	m, r = good(t)
	for i := range m.RequiredNegativeControls {
		m.RequiredNegativeControls[i].EvidenceRef = ""
	}
	if v := Validate(m, r, goodChain()); v.Pass {
		t.Fatal("a complete control table with no evidence passed — completeness substituted for evidence")
	}
}

// The manifest refuses unknown keys (a misspelt required key must not read as absent) and the bar defaults to
// the owner's stated battery when the manifest is silent; the CLI's exit codes are the contract scripts read.
func TestManifestLoadingAndCLI(t *testing.T) {
	dir := t.TempDir()
	bad := filepath.Join(dir, "typo.json")
	if err := os.WriteFile(bad, []byte(`{"cutoff":"2026-11-20T17:00:00Z","instrument_idd":"x"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadManifest(bad); err == nil {
		t.Fatal("an unknown key was accepted")
	}
	m, _ := good(t)
	if m.NegativeControlBar.PerSite != 10 || len(m.NegativeControlBar.Kinds) != 3 {
		t.Fatalf("default bar = %+v, want 10 per site over the three kinds", m.NegativeControlBar)
	}
	// The owner's bar is a floor: a manifest may raise it or add kinds, never lower it or drop one.
	low := filepath.Join(dir, "low.json")
	if err := os.WriteFile(low, []byte(`{"cutoff":"2026-11-20T17:00:00Z","negative_control_bar":{"per_site":2,"kinds":["stateful","failure-path"]}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if ml, err := LoadManifest(low); err != nil || ml.NegativeControlBar.PerSite != 10 || len(ml.NegativeControlBar.Kinds) != 4 {
		t.Fatalf("a lowered bar must be raised back to the floor and the default kinds re-added: %+v %v", ml.NegativeControlBar, err)
	}
	high := filepath.Join(dir, "high.json")
	if err := os.WriteFile(high, []byte(`{"cutoff":"2026-11-20T17:00:00Z","negative_control_bar":{"per_site":12}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if mh, err := LoadManifest(high); err != nil || mh.NegativeControlBar.PerSite != 12 {
		t.Fatalf("a raised bar must stand: %+v %v", mh.NegativeControlBar, err)
	}
	// YAML is accepted too.
	y := filepath.Join(dir, "m.yaml")
	if err := os.WriteFile(y, []byte("cutoff: 2026-11-20T17:00:00Z\ninstrument_id: abc\nsites: [dc1]\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if my, err := LoadManifest(y); err != nil || my.InstrumentID != "abc" || len(my.Sites) != 1 {
		t.Fatalf("yaml manifest: %+v %v", my, err)
	}
	var out, errb bytes.Buffer
	if rc := run([]string{"-manifest", filepath.Join("testdata", "manifest-good.json"), "-report", filepath.Join("testdata", "report-good.json"), "-ledger", filepath.Join("testdata", "ledger-good.json"), "-json"}, &out, &errb); rc != 0 {
		t.Fatalf("good run exit %d: %s", rc, errb.String())
	}
	var v Verdict
	if err := json.Unmarshal(out.Bytes(), &v); err != nil || !v.Pass {
		t.Fatalf("json verdict: %v %s", err, out.String())
	}
	if rc := run([]string{"-manifest", y, "-report", filepath.Join("testdata", "report-good.json")}, &out, &errb); rc != 1 {
		t.Fatalf("an incomplete manifest must exit 1 (FAIL), got %d", rc)
	}
	if rc := run([]string{"-manifest", bad, "-report", filepath.Join("testdata", "report-good.json")}, &out, &errb); rc != 2 {
		t.Fatalf("an unparseable manifest must exit 2, got %d", rc)
	}
	if rc := run([]string{"-report", "x"}, &out, &errb); rc != 2 {
		t.Fatalf("missing -manifest must exit 2, got %d", rc)
	}
}

func TestTheCohortInputsCannotBeLeftOutOrContradicted(t *testing.T) {
	zero, one := 0, 1
	cases := []struct {
		name string
		mut  func(m *Manifest, r *Report, c **ChainEvidence)
		want string
	}{
		{"no ledger evidence", func(m *Manifest, r *Report, c **ChainEvidence) { *c = nil }, "ledger hash chain recomputed"},
		{"a broken chain", func(m *Manifest, r *Report, c **ChainEvidence) { (*c).ChainOK = false }, "ledger hash chain recomputed"},
		{"anchors contradict the chain", func(m *Manifest, r *Report, c **ChainEvidence) { (*c).AnchorsOK = false }, "ledger hash chain recomputed"},
		{"no anchors checked", func(m *Manifest, r *Report, c **ChainEvidence) { (*c).AnchorsChecked = 0 }, "ledger hash chain recomputed"},
		{"a count that disagrees with the head", func(m *Manifest, r *Report, c **ChainEvidence) { (*c).MaxSeq = 901 }, "ledger hash chain recomputed"},
		{"a verification older than the cutoff", func(m *Manifest, r *Report, c **ChainEvidence) { (*c).VerifiedAt = m.Cutoff.Add(-time.Minute) }, "ledger hash chain recomputed"},
		{"cross-host count absent", func(m *Manifest, r *Report, c **ChainEvidence) { r.CrossHostActuations = nil }, "no executed effect on a host"},
		{"one cross-host actuation", func(m *Manifest, r *Report, c **ChainEvidence) { r.CrossHostActuations = &one }, "no executed effect on a host"},
		{"a control session scored in the cohort", func(m *Manifest, r *Report, c **ChainEvidence) { r.ExcludedRefs = r.ExcludedRefs[1:] }, "excluded exactly the manifest"},
		{"a session hidden with no control behind it", func(m *Manifest, r *Report, c **ChainEvidence) {
			r.ExcludedRefs = append(r.ExcludedRefs, "an-inconvenient-failure")
		}, "excluded exactly the manifest"},
		{"an excluded id that names no session", func(m *Manifest, r *Report, c **ChainEvidence) { r.ExcludedRefsUnmatched = []string{"sess-neg-nl-1"} }, "exists in the window"},
		{"a control with a decision and no session", func(m *Manifest, r *Report, c **ChainEvidence) { m.RequiredNegativeControls[0].SessionRefs = nil }, "names its session"},
		{"one session claimed by two controls", func(m *Manifest, r *Report, c **ChainEvidence) {
			m.RequiredNegativeControls[1].SessionRefs = m.RequiredNegativeControls[0].SessionRefs
		}, "claimed twice"},
		{"a duplicate id in the exclusions", func(m *Manifest, r *Report, c **ChainEvidence) {
			r.ExcludedRefs = append(r.ExcludedRefs, r.ExcludedRefs[0])
		}, "excluded exactly the manifest"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m, r := good(t)
			r.CrossHostActuations = &zero
			c := goodChain()
			tc.mut(&m, &r, &c)
			v := Validate(m, r, c)
			if v.Pass {
				t.Fatal("must not pass")
			}
			joined := strings.Join(failing(t, v), " | ")
			if !strings.Contains(joined, tc.want) {
				t.Fatalf("want a failing check containing %q, got %s", tc.want, joined)
			}
		})
	}
}

func TestANotDetectedControlStaysVisibleAndFailsItsRequiredLine(t *testing.T) {
	m, r := good(t)
	ref := m.RequiredNegativeControls[0].SessionRefs[0]
	m.RequiredNegativeControls[0].ObservedDecision = "not-detected"
	m.RequiredNegativeControls[0].SessionRefs = nil
	r.ExcludedRefs = minus(r.ExcludedRefs, []string{ref})
	v := Validate(m, r, goodChain())
	if v.Pass {
		t.Fatal("a required control that was not detected must fail the verdict")
	}
	joined := strings.Join(failing(t, v), " | ")
	if !strings.Contains(joined, "required negative control "+m.RequiredNegativeControls[0].ID) || strings.Contains(joined, "names its session") {
		t.Fatalf("got %s", joined)
	}
}

func TestAPositiveControlStaysInTheScoredCohort(t *testing.T) {
	m, r := good(t)
	ref := m.PositiveControls[0].SessionRefs[0]
	r.ExcludedRefs = append(r.ExcludedRefs, ref)
	v := Validate(m, r, goodChain())
	if v.Pass {
		t.Fatal("excluding a positive control's session hides a scored heal and must fail")
	}
	if !strings.Contains(strings.Join(failing(t, v), " | "), "excluded exactly") {
		t.Fatalf("got %v", failing(t, v))
	}
}

func TestALivePositiveControlMustNameItsSession(t *testing.T) {
	m, r := good(t)
	m.PositiveControls[0].SessionRefs = nil
	v := Validate(m, r, goodChain())
	if v.Pass || !strings.Contains(strings.Join(failing(t, v), " | "), "names its session") {
		t.Fatalf("got pass=%v %v", v.Pass, failing(t, v))
	}
}

func TestASimulatedControlNeedsNoSession(t *testing.T) {
	m, r := good(t)
	ref := m.RequiredNegativeControls[0].SessionRefs
	r.ExcludedRefs = minus(r.ExcludedRefs, ref)
	m.RequiredNegativeControls[0].SessionRefs = nil
	m.RequiredNegativeControls[0].LiveOrSimulated = "simulated"
	v := Validate(m, r, goodChain())
	if strings.Contains(strings.Join(failing(t, v), " | "), "names its session") {
		t.Fatalf("a hermetic control has no session: %v", failing(t, v))
	}
}

func TestAProtectiveHoldOnAnUnknownActorPassesItsControl(t *testing.T) {
	m, r := good(t)
	m.RequiredNegativeControls[0].Notes = "unknown actor, attribution indeterminate, protective hold"
	m.RequiredNegativeControls[0].ExpectedDecision = "hold"
	m.RequiredNegativeControls[0].ObservedDecision = "hold"
	if v := Validate(m, r, goodChain()); !v.Pass {
		t.Fatalf("got %v", failing(t, v))
	}
}
