package db

import (
	"context"
	"testing"
	"time"

	"github.com/territory-grounder/grounder/core/auth"
	"github.com/territory-grounder/grounder/core/httpapi"
)

// GOLDEN-FIXTURE TEST FOR THE ACTIONS READ PROJECTION.
//
// This surface replaces five INVENTED incidents that were rendered against REAL estate hostnames
// ("Repeated auth failures -> ASA dc1fw01") while 109 genuine manifests sat unread. The replacement is
// only an improvement if it reports what is stored — a projection that quietly guesses a stage is the same
// defect with better provenance.
//
// Runs against a REAL Postgres (TG_TEST_DSN) because every risk here is JOIN semantics: a manifest with
// several execution attempts must yield ONE ribbon, and an unscored manifest must not inherit a verdict. A
// pgx fake reproduces neither, and has already hidden a field-drop in this repository once.

func seedManifests(ctx context.Context, t *testing.T, p *Pool) func() {
	t.Helper()
	const pfx = "gold-act-"
	base := time.Now().UTC().Add(-3 * time.Hour).Truncate(time.Microsecond)

	cleanup := func() {
		_, _ = p.Exec(ctx, `DELETE FROM action_execution WHERE action_id LIKE $1`, pfx+"%")
		_, _ = p.Exec(ctx, `DELETE FROM action_manifest WHERE action_id LIKE $1`, pfx+"%")
	}
	cleanup()

	rows := []struct {
		id, opClass, target, band, verdict, approval string
		predicted                                    bool
		execs                                        int
	}{
		// A fully walked, auto-executed heal: every stage true.
		{pfx + "1", "start-service", "dc1mealie01", "AUTO", "match", "", true, 1},
		// Human-approved, executed, scored.
		{pfx + "2", "restart-service", "dc1reactive01", "POLL_PAUSE", "match", "approve", true, 1},
		// ★ THE IDENTITY-COLLAPSE CASE: three execution attempts for ONE manifest. Must yield ONE ribbon.
		{pfx + "3", "start-container", "dc1ghostfolio01", "AUTO_NOTICE", "deviation", "", true, 3},
		// Committed but never predicted, never approved, never executed, never scored: all absence.
		{pfx + "4", "restart-container", "dc1wallos01", "POLL_PAUSE", "", "", false, 0},
	}

	for i, r := range rows {
		var predHash any
		if r.predicted {
			predHash = "pred-" + r.id
		}
		var verdict any
		if r.verdict != "" {
			verdict = r.verdict
		}
		var approval any
		if r.approval != "" {
			approval = r.approval
		}
		action := `{"op":"start","op_class":"` + r.opClass + `","target":"` + r.target +
			`","reversible":true,"params":{"unit":"nginx.service"}}`
		if _, err := p.Exec(ctx, `
			INSERT INTO action_manifest (action_id, action, band, plan_hash, prediction_hash,
			                             approval_choice, verdict, sealed_at)
			VALUES ($1,$2::jsonb,$3::band,$4,$5,$6,$7::verdict,$8)`,
			r.id, action, r.band, "plan-"+r.id, predHash, approval, verdict,
			base.Add(time.Duration(i)*time.Minute)); err != nil {
			t.Fatalf("seed manifest %s: %v", r.id, err)
		}
		// action_execution enforces (unverifiable AND verdict IS NULL) OR (NOT unverifiable AND verdict IS
		// NOT NULL) — a withheld verdict can never be mistaken for a clean one (TG-182). So the retried
		// case is seeded the way it actually happens: the earlier attempts were UNVERIFIABLE (post-state
		// unreadable, hence the retry) and only the final one carries a verdict.
		for e := 0; e < r.execs; e++ {
			last := e == r.execs-1
			var execVerdict any
			unverifiable := true
			if last {
				execVerdict, unverifiable = r.verdict, false
				if r.verdict == "" { // a manifest with no verdict has no verified execution either
					execVerdict, unverifiable = nil, true
				}
			}
			if _, err := p.Exec(ctx, `
				INSERT INTO action_execution (action_id, external_ref, target_host, verdict, unverifiable, executed_at)
				VALUES ($1,$2,$3,$4::verdict,$5,$6)`,
				r.id, "ref-"+r.id, r.target, execVerdict, unverifiable,
				base.Add(time.Duration(e)*time.Second)); err != nil {
				t.Fatalf("seed execution for %s: %v", r.id, err)
			}
		}
	}
	return cleanup
}

func ribbonsByID(rs []httpapi.ActionRibbon) map[string]httpapi.ActionRibbon {
	m := map[string]httpapi.ActionRibbon{}
	for _, r := range rs {
		m[r.ActionID] = r
	}
	return m
}

// TestActionsProjectTheSealedManifest — the ribbon must carry what TG actually bound itself to.
func TestActionsProjectTheSealedManifest(t *testing.T) {
	ctx, p, done := openFixture(t)
	defer done()
	defer seedManifests(ctx, t, p)()

	got, err := NewActionManifestReadStore(p).Recent(ctx, auth.Principal{}, 200)
	if err != nil {
		t.Fatalf("Recent: %v", err)
	}
	by := ribbonsByID(got)

	r, ok := by["gold-act-1"]
	if !ok {
		t.Fatal("gold-act-1 missing from the projection")
	}
	if r.OpClass != "start-service" || r.Target != "dc1mealie01" {
		t.Errorf("op_class/target = %q/%q, want start-service/dc1mealie01 — the sealed manifest is not "+
			"being decoded, which is how a surface ends up inventing what the action was", r.OpClass, r.Target)
	}
	if !r.Reversible || r.Params["unit"] != "nginx.service" {
		t.Errorf("reversible=%v params=%v, want the sealed values", r.Reversible, r.Params)
	}
	if r.Band != "AUTO" || r.Verdict != "match" {
		t.Errorf("band/verdict = %q/%q, want AUTO/match", r.Band, r.Verdict)
	}
}

// TestOneManifestYieldsOneRibbonRegardlessOfExecutionAttempts is the identity-collapse control. A JOIN to
// action_execution would emit THREE ribbons for gold-act-3 and silently triple the surface's action count —
// the same class of defect that once turned 87 executions into 26 rows in this project's own reporting.
func TestOneManifestYieldsOneRibbonRegardlessOfExecutionAttempts(t *testing.T) {
	ctx, p, done := openFixture(t)
	defer done()
	defer seedManifests(ctx, t, p)()

	got, err := NewActionManifestReadStore(p).Recent(ctx, auth.Principal{}, 200)
	if err != nil {
		t.Fatalf("Recent: %v", err)
	}
	n := 0
	for _, r := range got {
		if r.ActionID == "gold-act-3" {
			n++
		}
	}
	if n != 1 {
		t.Errorf("gold-act-3 appears %d times; it has 3 execution attempts and MUST appear exactly once — "+
			"one manifest is one governed action, however many times the effect leaf was tried", n)
	}
	if r := ribbonsByID(got)["gold-act-3"]; !r.Executed {
		t.Error("gold-act-3 has execution rows but Executed is false")
	}
}

// TestAbsenceIsReportedAsAbsence — the stage flags must come from rows that exist. An unscored manifest that
// reports Verified, or an unapproved one that reports Approved, is a surface telling the operator a human
// signed off on something nobody saw.
func TestAbsenceIsReportedAsAbsence(t *testing.T) {
	ctx, p, done := openFixture(t)
	defer done()
	defer seedManifests(ctx, t, p)()

	got, err := NewActionManifestReadStore(p).Recent(ctx, auth.Principal{}, 200)
	if err != nil {
		t.Fatalf("Recent: %v", err)
	}
	r, ok := ribbonsByID(got)["gold-act-4"]
	if !ok {
		t.Fatal("gold-act-4 missing")
	}
	for _, c := range []struct {
		name string
		got  bool
	}{
		{"Predicted", r.Predicted}, {"Approved", r.Approved},
		{"Executed", r.Executed}, {"Verified", r.Verified},
	} {
		if c.got {
			t.Errorf("%s is true for a manifest that never reached that stage — the flag must be grounded "+
				"in a stored row, never assumed", c.name)
		}
	}
	if r.Verdict != "" {
		t.Errorf("verdict = %q for an unscored manifest, want empty — absence must never render as a score",
			r.Verdict)
	}
	if !r.Classified {
		t.Error("Classified is false though the manifest carries a band")
	}
	// The APPROVED case must still work, or the check above would pass by never being true at all.
	if a := ribbonsByID(got)["gold-act-2"]; !a.Approved || a.ApprovalChoice != "approve" {
		t.Errorf("gold-act-2 approved=%v choice=%q, want true/approve — if approval never registers, the "+
			"absence assertion above proves nothing", a.Approved, a.ApprovalChoice)
	}
}

// seedRiskAndConfidence attaches a risk audit + triage row to gold-act-1 ONLY, so the fixture contains both
// a manifest that has these values and manifests that do not. A fixture where every row has a confidence
// cannot tell "reports the real value" apart from "reports a constant".
func seedRiskAndConfidence(ctx context.Context, t *testing.T, p *Pool) func() {
	t.Helper()
	const ref = "gold-act-ref-1"
	cleanup := func() {
		_, _ = p.Exec(ctx, `DELETE FROM session_risk_audit WHERE external_ref = $1`, ref)
		_, _ = p.Exec(ctx, `DELETE FROM session_triage WHERE external_ref = $1`, ref)
	}
	cleanup()
	if _, err := p.Exec(ctx, `
		INSERT INTO session_risk_audit (external_ref, action_id, band, risk_level, plan_hash, schema_version)
		VALUES ($1,$2,'AUTO'::band,'high','plan-gold-act-1',1)`, ref, "gold-act-1"); err != nil {
		t.Fatalf("seed risk audit: %v", err)
	}
	if _, err := p.Exec(ctx, `
		INSERT INTO session_triage (external_ref, confidence, schema_version) VALUES ($1, 0.73, 1)`, ref); err != nil {
		t.Fatalf("seed triage: %v", err)
	}
	return cleanup
}

// TestRiskIsPublishedAsItsCategoryAndConfidenceAsItsNumber — the two are DIFFERENT KINDS of value and must
// be carried as such.
//
// Confidence is a genuine scalar (a prediction about one thing, which is why it is calibratable) and is
// published as a float. Risk is the label the classifier ladder produced; core/risk/classifier.go returns
// early on the FIRST matching rule, so no score exists to publish and rendering a decimal would mean
// inventing weights across incommensurable veto conditions.
func TestRiskIsPublishedAsItsCategoryAndConfidenceAsItsNumber(t *testing.T) {
	ctx, p, done := openFixture(t)
	defer done()
	defer seedManifests(ctx, t, p)()
	defer seedRiskAndConfidence(ctx, t, p)()

	got, err := NewActionManifestReadStore(p).Recent(ctx, auth.Principal{}, 200)
	if err != nil {
		t.Fatalf("Recent: %v", err)
	}
	by := ribbonsByID(got)

	r := by["gold-act-1"]
	if r.RiskLevel != "high" {
		t.Errorf("risk_level = %q, want the stored category %q", r.RiskLevel, "high")
	}
	if !r.HasConfidence || r.Confidence < 0.72 || r.Confidence > 0.74 {
		t.Errorf("confidence = %v (has=%v), want the stored 0.73 — a real scalar must be carried as one, "+
			"not rounded into a band", r.Confidence, r.HasConfidence)
	}
}

// TestAnUnrecordedConfidenceIsNotZero is the load-bearing half. 0.0 is a MEANINGFUL confidence value
// ("no confidence at all"), so a manifest with none recorded must be distinguishable from one scored zero.
// Collapsing the two is the same "absent is not zero" defect that once made a calibration score unreadable.
func TestAnUnrecordedConfidenceIsNotZero(t *testing.T) {
	ctx, p, done := openFixture(t)
	defer done()
	defer seedManifests(ctx, t, p)()
	defer seedRiskAndConfidence(ctx, t, p)()

	got, err := NewActionManifestReadStore(p).Recent(ctx, auth.Principal{}, 200)
	if err != nil {
		t.Fatalf("Recent: %v", err)
	}
	by := ribbonsByID(got)

	// gold-act-2 has no risk audit and no triage row at all.
	r, ok := by["gold-act-2"]
	if !ok {
		t.Fatal("gold-act-2 missing")
	}
	if r.HasConfidence {
		t.Errorf("gold-act-2 reports has_confidence=true (value %v) though nothing recorded one — an "+
			"unrecorded confidence rendered as a number is a claim the data does not make", r.Confidence)
	}
	if r.RiskLevel != "" {
		t.Errorf("gold-act-2 reports risk_level %q though no classification row exists", r.RiskLevel)
	}
	// ...and the positive case must still hold, or the assertions above pass by nothing ever being set.
	if a := by["gold-act-1"]; !a.HasConfidence {
		t.Error("gold-act-1 lost its confidence — if the value never arrives, the absence checks prove nothing")
	}
}

// TestActionCountsAreThePopulationNotThePage — same discipline that the alerts badge needed: the page is
// bounded by construction, so a surface fed len(page) reports its own fetch limit.
func TestActionCountsAreThePopulationNotThePage(t *testing.T) {
	ctx, p, done := openFixture(t)
	defer done()
	defer seedManifests(ctx, t, p)()

	s := NewActionManifestReadStore(p)
	page, err := s.Recent(ctx, auth.Principal{}, 2) // deliberately fewer than seeded
	if err != nil {
		t.Fatalf("Recent: %v", err)
	}
	if len(page) != 2 {
		t.Fatalf("page has %d rows, want the requested 2 — fixture does not exercise the defect", len(page))
	}
	c, err := s.Counts(ctx, auth.Principal{})
	if err != nil {
		t.Fatalf("Counts: %v", err)
	}
	if c.Total == len(page) {
		t.Errorf("counts.total = %d, exactly the page size — this is the fetch-limit-as-count defect", c.Total)
	}
	if c.Total < 4 {
		t.Errorf("counts.total = %d, want at least the 4 seeded manifests", c.Total)
	}
	if c.Deviations < 1 {
		t.Errorf("counts.deviations = %d, want at least the 1 seeded deviation", c.Deviations)
	}
}
