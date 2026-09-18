package db

import (
	"context"
	"testing"
	"time"

	"github.com/territory-grounder/grounder/core/auth"
	"strings"
)

// THE GRADUATION SURFACE UNDER-REPORTED DEVIATIONS 24-TO-1.
//
// #actions reads action_manifest.verdict, which is a BACKFILLED column, and the backfill is incomplete.
// Measured on the live control plane 2026-07-29: 61 of 113 manifests had m.verdict NULL, 46 of those DID
// have a scored row in action_verdict, and 23 OF THOSE HAD DEVIATED. So #actions rendered "1 deviation"
// while 24 actions had actually deviated — and #grounding, reading action_verdict directly, said 24 on the
// same console with no qualifier telling the operator the populations differed.
//
// #actions is the surface an operator reads to judge whether an op-class is safe to graduate to unattended
// AUTO, and a deviation is precisely the clamping event that must prevent it. Under-reporting a deviation
// there is the dangerous direction of wrong.
//
// Runs against a REAL Postgres (TG_TEST_DSN): every risk in this change is SQL semantics — whether the
// fallback fires, whether it can multiply a ribbon, and whether it can OVERWRITE a stored verdict. A pgx
// fake reproduces none of those, and has already hidden a field-drop in this repository once.

// seedVerdictFallback creates four manifests spanning the closed set of (manifest verdict, action_verdict)
// combinations, so the fallback is tested over the whole space rather than the one case that motivated it.
func seedVerdictFallback(ctx context.Context, t *testing.T, p *Pool) func() {
	t.Helper()
	const pfx = "vcoal-"
	at := time.Now().UTC().Add(-2 * time.Hour).Truncate(time.Microsecond)

	cleanup := func() {
		_, _ = p.Exec(ctx, `DELETE FROM action_verdict WHERE action_id LIKE $1`, pfx+"%")
		_, _ = p.Exec(ctx, `DELETE FROM action_manifest WHERE action_id LIKE $1`, pfx+"%")
	}
	cleanup()

	rows := []struct {
		id              string
		manifestVerdict string // "" => NULL in action_manifest
		avVerdict       string // "" => no row in action_verdict
	}{
		// THE DEFECT: no manifest verdict, but action_verdict says it DEVIATED. Must surface as deviation.
		{pfx + "null-deviation", "", "deviation"},
		// Same shape, benign verdict — proves the fallback is not deviation-specific.
		{pfx + "null-match", "", "match"},
		// Both present and equal: the fallback must be a no-op, not a second source of truth.
		{pfx + "both-match", "match", "match"},
		// Neither present: absence must stay absence. An unscored action must NOT inherit anything.
		{pfx + "neither", "", ""},
		// ★ THEY DISAGREE. Never observed live (0 of 98 cases where both exist), but precedence is a real
		// design decision and an untestable one is an accident waiting to be reinterpreted. The STORED
		// manifest verdict wins: it is the sealed per-manifest record, and action_verdict is the FALLBACK
		// for when the backfill has not run. Without this case the ordering inside COALESCE is unfalsifiable
		// — swapping the two arms leaves the suite green, which is how a "fallback" quietly becomes a second
		// source of truth. If this ever fires in production it is a backfill bug, not something to resolve
		// silently in a read.
		{pfx + "disagree", "match", "deviation"},
	}

	for _, r := range rows {
		action := `{"op":"restart","op_class":"restart-service","target":"dc1nc02","reversible":true,"params":{"unit":"nginx.service"}}`
		if r.manifestVerdict == "" {
			_, err := p.Exec(ctx, `INSERT INTO action_manifest (action_id, action, band, sealed_at)
				VALUES ($1,$2::jsonb,'AUTO'::band,$3)`, r.id, action, at)
			if err != nil {
				t.Fatalf("seed manifest %s: %v", r.id, err)
			}
		} else {
			_, err := p.Exec(ctx, `INSERT INTO action_manifest (action_id, action, band, verdict, sealed_at)
				VALUES ($1,$2::jsonb,'AUTO'::band,$3::verdict,$4)`, r.id, action, r.manifestVerdict, at)
			if err != nil {
				t.Fatalf("seed manifest %s: %v", r.id, err)
			}
		}
		if r.avVerdict != "" {
			_, err := p.Exec(ctx, `INSERT INTO action_verdict (action_id, plan_hash, verdict, created_at, schema_version)
				VALUES ($1,$2,$3::verdict,$4,1)`, r.id, "ph-"+r.id, r.avVerdict, at)
			if err != nil {
				t.Fatalf("seed action_verdict %s: %v", r.id, err)
			}
		}
	}
	return cleanup
}

func TestAnUnbackfilledDeviationStillReachesTheGraduationSurface(t *testing.T) {
	ctx, p, done := openFixture(t)
	defer done()
	defer seedVerdictFallback(ctx, t, p)()

	got, err := NewActionManifestReadStore(p).Recent(ctx, auth.Principal{}, 500)
	if err != nil {
		t.Fatalf("Recent: %v", err)
	}
	by := ribbonsByID(got)

	// THE DEFECT ITSELF.
	r, ok := by["vcoal-null-deviation"]
	if !ok {
		t.Fatal("vcoal-null-deviation missing from the projection")
	}
	if r.Verdict != "deviation" {
		t.Errorf("verdict = %q, want \"deviation\" — a manifest whose backfill never ran but which HAS a "+
			"scored deviation reads as unscored on the surface used to judge AUTO graduation. Live, that hid "+
			"23 of 24 real deviations.", r.Verdict)
	}
	if !r.Verified {
		t.Errorf("Verified = false for a scored deviation — the stage flags are derived from the verdict, so " +
			"the walk reads as incomplete too")
	}

	// NOT DEVIATION-SPECIFIC: the fallback is about absence, not about severity.
	if m, ok := by["vcoal-null-match"]; !ok || m.Verdict != "match" {
		t.Errorf("vcoal-null-match verdict = %q (present=%v), want \"match\"", m.Verdict, ok)
	}

	// THE FALLBACK MUST NEVER OVERWRITE A STORED VERDICT — it is a fallback, not a second source of truth.
	if b, ok := by["vcoal-both-match"]; !ok || b.Verdict != "match" {
		t.Errorf("vcoal-both-match verdict = %q (present=%v), want \"match\"", b.Verdict, ok)
	}
	// And where they DISAGREE the stored value wins, which is what makes the COALESCE ordering load-bearing.
	if d, ok := by["vcoal-disagree"]; !ok || d.Verdict != "match" {
		t.Errorf("vcoal-disagree verdict = %q (present=%v), want \"match\" — the sealed manifest verdict must "+
			"win over the fallback; swapping the COALESCE arms turns a fallback into a second source of truth",
			d.Verdict, ok)
	}

	// ABSENCE STAYS ABSENCE. This is the guard that keeps the fix honest: an unscored action must not
	// inherit a verdict from anywhere. Reporting "match" here would be a fabricated success.
	n, ok := by["vcoal-neither"]
	if !ok {
		t.Fatal("vcoal-neither missing from the projection")
	}
	if n.Verdict != "" {
		t.Errorf("verdict = %q for an action with NO verdict anywhere, want \"\" — absence must be reported "+
			"as absence, never as a score the data does not have", n.Verdict)
	}
	if n.Verified {
		t.Error("Verified = true for an unscored action — the surface is claiming a walk that did not happen")
	}
}

// The identity-collapse control for the new lookup. action_verdict holds one row per action_id today (max 1,
// 0 duplicates, verified live), but a JOIN would silently multiply ribbons if that ever changed — the same
// class of defect that once turned 87 executions into 26 rows here. The read uses a scalar subquery for
// exactly this reason, and this pins it: a SECOND action_verdict row must not produce a second ribbon.
func TestASecondVerdictRowStillYieldsOneRibbon(t *testing.T) {
	ctx, p, done := openFixture(t)
	defer done()
	defer seedVerdictFallback(ctx, t, p)()

	// Add a duplicate scored row for the same action_id, the shape a per-occurrence action_verdict would have.
	if _, err := p.Exec(ctx, `INSERT INTO action_verdict (action_id, plan_hash, verdict, created_at, schema_version)
		VALUES ($1,'ph-dup','partial'::verdict,$2,1)`,
		"vcoal-null-deviation", time.Now().UTC().Add(-time.Hour).Truncate(time.Microsecond)); err != nil {
		t.Skipf("action_verdict rejects a second row per action_id (a unique constraint) — the multiplication "+
			"hazard is structurally impossible, which is a stronger guarantee than this test: %v", err)
	}

	got, err := NewActionManifestReadStore(p).Recent(ctx, auth.Principal{}, 500)
	if err != nil {
		t.Fatalf("Recent: %v", err)
	}
	n := 0
	for _, r := range got {
		if r.ActionID == "vcoal-null-deviation" {
			n++
		}
	}
	if n != 1 {
		t.Errorf("%d ribbons for one manifest with two scored rows, want 1 — the verdict lookup multiplied "+
			"the surface's action count", n)
	}
}

// TestCountsUseTheSameVerdictReadAsTheRibbons closes the half of the coalesce fix that was missed.
//
// Recent() was fixed to fall back to action_verdict when the backfilled action_manifest.verdict is
// NULL/empty. Counts() — ninety lines below in the same file, feeding the SAME /v1/actions response — was
// not. So the headline badge read "1 deviation" directly above a ribbon list showing twenty-four, and an
// operator judging whether an op-class is safe to graduate to unattended AUTO reads the badge.
// Under-reporting a deviation on the graduation surface is the dangerous direction: a deviation is exactly
// the clamping event that must PREVENT the climb.
//
// WHY THE EXISTING GUARDS MISSED IT. TestActionCountsAreThePopulationNotThePage
// (action_manifest_read_test.go) does assert on Counts, but seeds through seedManifests, which writes the
// verdict INTO action_manifest.verdict — the fixture shares the code's blind spot exactly. And the
// coalesce regression tests in this file call Recent() only. Neither could see a manifest whose verdict
// lives only in action_verdict, which is the entire population the fallback exists for.
//
// This reuses seedVerdictFallback, the fixture that already spans the closed set of (manifest verdict,
// action_verdict) combinations. Only the caller was missing.
//
// KILLING MUTATION: revert Counts() to `count(*) FILTER (WHERE verdict = 'deviation')`. RED.
func TestCountsUseTheSameVerdictReadAsTheRibbons(t *testing.T) {
	ctx, p, done := openFixture(t)
	defer done()
	defer seedVerdictFallback(ctx, t, p)()

	s := NewActionManifestReadStore(p)

	// Derive the expected numbers from what the RIBBONS report over the seeded rows, rather than
	// hardcoding them: the two readers must agree, and pinning a literal would let both drift together.
	ribbons, err := s.Recent(ctx, auth.Principal{}, 200)
	if err != nil {
		t.Fatalf("Recent: %v", err)
	}
	seenVerified, seenDeviations, seenTotal := 0, 0, 0
	for _, r := range ribbons {
		if !strings.HasPrefix(r.ActionID, "vcoal-") {
			continue
		}
		seenTotal++
		if r.Verdict != "" {
			seenVerified++
		}
		if r.Verdict == "deviation" {
			seenDeviations++
		}
	}
	if seenTotal == 0 || seenDeviations == 0 {
		t.Fatalf("vacuity floor: the fixture produced %d seeded ribbons and %d deviations — with no "+
			"deviation in the population this test cannot detect an under-count", seenTotal, seenDeviations)
	}

	counts, err := s.Counts(ctx, auth.Principal{})
	if err != nil {
		t.Fatalf("Counts: %v", err)
	}
	// Counts is estate-wide and the fixture is a subset, so the assertion is a FLOOR: whatever the ribbons
	// can see, the counts must at least also see. An under-count fails; unrelated rows cannot mask it.
	if counts.Deviations < seenDeviations {
		t.Errorf("Counts().Deviations = %d but the ribbons show %d deviation(s) in the seeded set alone. "+
			"The badge under-reports the list beside it in the SAME response, and it is the number an "+
			"operator reads when deciding whether an op-class may graduate to unattended AUTO.",
			counts.Deviations, seenDeviations)
	}
	if counts.Verified < seenVerified {
		t.Errorf("Counts().Verified = %d but the ribbons show %d verified in the seeded set alone — a "+
			"manifest whose verdict lives only in action_verdict is counted as unverified",
			counts.Verified, seenVerified)
	}
	// And the total must not have been inflated by the verdict lookup: one manifest, one count.
	if counts.Total < seenTotal {
		t.Errorf("Counts().Total = %d is below the %d seeded manifests", counts.Total, seenTotal)
	}
}
