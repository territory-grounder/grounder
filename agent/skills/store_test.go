package skills

import (
	"strings"
	"testing"

	"github.com/territory-grounder/grounder/core/execclass"
	"github.com/territory-grounder/grounder/core/skillstore"
)

func row(name, version, body string, aw skillstore.AppliesWhen, pinned bool, pos int) skillstore.ProductionRow {
	return skillstore.ProductionRow{SkillName: name, Version: version, Body: body, AppliesWhen: aw,
		ContentHash: skillstore.ContentHash(body, aw), Pinned: pinned, Position: pos}
}

func deepCtx() Context {
	return Context{Phase: PhaseInvestigate, ExecClass: execclass.DeepInvestigation}
}

// REQ-1303: a valid store row overrides the compiled body by name; composition stays deterministic
// (two composes of the same context are identical) and the provenance names the origin per skill.
func TestStoreRowOverridesCompiledDeterministically(t *testing.T) {
	rows := []skillstore.ProductionRow{
		row("triage-protocol", "9.0.0", "## Triage protocol v9 (store-graduated)", skillstore.AppliesWhen{}, false, 5),
	}
	rows[0].VersionID = 42
	reg, prov := NewFromStore(rows, Default())
	if prov.Fallback != "" {
		t.Fatalf("valid rows must not fall back: %s", prov.Fallback)
	}
	b1, l1 := reg.Compose(deepCtx())
	b2, _ := reg.Compose(deepCtx())
	if b1 != b2 {
		t.Fatal("composition must be deterministic")
	}
	if !strings.Contains(b1, "v9 (store-graduated)") || strings.Contains(b1, "## Triage protocol\nAlways:") {
		t.Fatal("the store body must replace the compiled body")
	}
	if got := prov.Skills["triage-protocol"]; got.Origin != OriginStore || got.Version != "9.0.0" || got.VersionID != 42 {
		t.Fatalf("provenance must name the store origin and version id, got %+v", got)
	}
	if len(l1) == 0 {
		t.Fatal("loaded names must be reported for the skill_load record")
	}
}

// REQ-1304: ANY invalid row fails the WHOLE store path back to the compiled registry, with the reason
// recorded — a partially-applied store never composes.
func TestInvalidRowFallsBackToCompiledInFull(t *testing.T) {
	good := row("triage-protocol", "9.0.0", "store body", skillstore.AppliesWhen{}, false, 5)
	bad := good
	bad.SkillName = "loop-red-flags"
	bad.ContentHash = "tampered"
	reg, prov := NewFromStore([]skillstore.ProductionRow{good, bad}, Default())
	if prov.Fallback == "" || !strings.Contains(prov.Fallback, "hash mismatch") {
		t.Fatalf("a tampered row must fall back with the reason, got %q", prov.Fallback)
	}
	body, _ := reg.Compose(deepCtx())
	if strings.Contains(body, "store body") {
		t.Fatal("no store body may compose after a fallback")
	}
	for _, name := range []string{"proving-your-work", "triage-protocol", "conservative-remediation"} {
		if prov.Skills[name].Origin != OriginCompiled {
			t.Fatalf("%s must be compiled-origin after fallback, got %v", name, prov.Skills[name].Origin)
		}
	}
}

// REQ-1305: a pinned skill composes its COMPILED body even when a store row targets it; the shadowed
// row is reported (visible, never silent).
func TestPinnedSkillComposesCompiledBody(t *testing.T) {
	rows := []skillstore.ProductionRow{
		row("conservative-remediation", "9.9.9", "weakened floor text", skillstore.AppliesWhen{}, true, 4),
	}
	reg, prov := NewFromStore(rows, Default())
	body, _ := reg.Compose(deepCtx())
	if strings.Contains(body, "weakened floor") {
		t.Fatal("a pinned skill's store row must never compose")
	}
	if !strings.Contains(body, "HARD FLOOR") {
		t.Fatal("the compiled floor body must compose")
	}
	if prov.Skills["conservative-remediation"].Origin != OriginPinned {
		t.Fatalf("the shadowed pinned row must be reported, got %v", prov.Skills["conservative-remediation"].Origin)
	}
}

// REQ-1303: a store-only skill (flywheel-authored new competence) composes after the compiled set, and
// its declarative predicate scopes it — including the fail-toward-more-guidance empty-class rule.
func TestStoreOnlySkillScopedByPredicate(t *testing.T) {
	rows := []skillstore.ProductionRow{
		row("bgp-cascade-triage", "1.0.0", "## BGP cascade triage (new)", skillstore.AppliesWhen{
			ExecClasses: []string{string(execclass.DeepInvestigation)},
		}, false, 99),
	}
	reg, _ := NewFromStore(rows, Default())

	deep, _ := reg.Compose(deepCtx())
	if !strings.Contains(deep, "BGP cascade triage") {
		t.Fatal("the store-only skill must compose for its scoped class")
	}
	if !strings.HasSuffix(strings.TrimSpace(deep), "## BGP cascade triage (new)") {
		t.Fatal("a store-only skill composes AFTER the compiled set")
	}
	fast, _ := reg.Compose(Context{Phase: PhaseInvestigate, ExecClass: execclass.FastAgent})
	if strings.Contains(fast, "BGP cascade triage") {
		t.Fatal("a deep-scoped skill must not compose for a fast agent")
	}
	unclassified, _ := reg.Compose(Context{Phase: PhaseInvestigate})
	if !strings.Contains(unclassified, "BGP cascade triage") {
		t.Fatal("an unclassified context fails toward MORE guidance (empty class matches any predicate)")
	}
}

// REQ-1304 (review blocker): a row written AROUND the API — oversized body with a correctly computed
// hash — must not flood the seed; the whole store path falls back.
func TestOversizedBodyFallsBack(t *testing.T) {
	huge := strings.Repeat("x", 20000)
	rows := []skillstore.ProductionRow{row("triage-protocol", "9.0.0", huge, skillstore.AppliesWhen{}, false, 5)}
	reg, prov := NewFromStore(rows, Default())
	if prov.Fallback == "" || !strings.Contains(prov.Fallback, "out of bounds") {
		t.Fatalf("an oversized validly-hashed body must fall back, got %q", prov.Fallback)
	}
	body, _ := reg.Compose(deepCtx())
	if len(body) > 20000 {
		t.Fatal("the oversized body must not compose")
	}
}

// REQ-1315/1316 + TG-476 (ADR-0017): ONLY skill-class rows are seed material. Before this filter the
// composer admitted EVERY class under its per-class cap — this very test asserted a 9000-byte
// runbook-class body "must reach the composed seed", which is exactly the leak the class model exists
// to close (a wiki page, the judge rubric, or the prompt half entering <behavioral_guidance>). The
// non-skill KNOWN classes are now excluded silently — no compose, no fallback, no provenance load —
// even when the row is corrupt, so a broken wiki page can never thin the agent's skill library. The
// skill-class 8 KiB compose re-check stays, and an UNKNOWN class still fails the whole path closed.
func TestNonSkillClassesNeverCompose(t *testing.T) {
	nineK := strings.Repeat("y", 9000)

	skillRow := row("triage-protocol", "9.0.0", nineK, skillstore.AppliesWhen{}, false, 5) // class absent = skill
	_, prov := NewFromStore([]skillstore.ProductionRow{skillRow}, Default())
	if prov.Fallback == "" || !strings.Contains(prov.Fallback, "out of bounds") {
		t.Fatalf("9000 bytes on a skill-class row must fall back (8 KiB class cap), got %q", prov.Fallback)
	}

	for _, tc := range []struct {
		class skillstore.ArtifactClass
		name  string
	}{
		{skillstore.ClassRunbook, "disk-full-runbook"},
		{skillstore.ClassRubric, "judge-rubric"},
		{skillstore.ClassPrompt, "base-prompt-trialable"},
	} {
		r := row(tc.name, "1.0.0", nineK, skillstore.AppliesWhen{}, tc.class == skillstore.ClassRubric, 99)
		r.Class = tc.class
		reg, prov := NewFromStore([]skillstore.ProductionRow{r}, Default())
		if prov.Fallback != "" {
			t.Fatalf("a %s-class row must be excluded WITHOUT failing the store path, got fallback %q", tc.class, prov.Fallback)
		}
		if body, _ := reg.Compose(deepCtx()); strings.Contains(body, nineK) {
			t.Fatalf("a %s-class body must never reach the composed seed", tc.class)
		}
		if _, loaded := prov.Skills[tc.name]; loaded {
			t.Fatalf("an excluded %s-class row must not be booked as a provenance load", tc.class)
		}
	}

	// A CORRUPT non-skill row (tampered hash — the exact shape that fails the whole path for a skill
	// row) is still just excluded: the wiki's brokenness must not strip the graduated skill library.
	corrupt := row("disk-full-runbook", "1.0.0", "wiki page body", skillstore.AppliesWhen{}, false, 99)
	corrupt.Class = skillstore.ClassRunbook
	corrupt.ContentHash = "tampered"
	good := row("triage-protocol", "9.0.0", "store body v9", skillstore.AppliesWhen{}, false, 5)
	reg, prov := NewFromStore([]skillstore.ProductionRow{corrupt, good}, Default())
	if prov.Fallback != "" {
		t.Fatalf("a corrupt runbook row must not knock the skill path back to compiled, got %q", prov.Fallback)
	}
	if body, _ := reg.Compose(deepCtx()); !strings.Contains(body, "store body v9") {
		t.Fatal("the graduated skill row must keep composing beside an excluded corrupt runbook row")
	}

	unknownRow := row("mystery", "1.0.0", "tiny body", skillstore.AppliesWhen{}, false, 98)
	unknownRow.Class = "wiki"
	_, prov = NewFromStore([]skillstore.ProductionRow{unknownRow}, Default())
	if prov.Fallback == "" || !strings.Contains(prov.Fallback, "unknown artifact class") {
		t.Fatalf("an unknown-class row must fail the store path closed, got %q", prov.Fallback)
	}
}
