package opcover

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// withExemptions writes an exemptions file into a temp repo root and returns that root.
func withExemptions(t *testing.T, body string) string {
	t.Helper()
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "spec"), 0o755); err != nil {
		t.Fatal(err)
	}
	if body != "" {
		if err := os.WriteFile(ExemptionsPath(root), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func kinds(r Report) map[string]int {
	m := map[string]int{}
	for _, f := range r.Findings {
		m[f.Kind]++
	}
	return m
}

// TestUncoveredOpClassIsAFinding is the whole point: an op-class nothing can provoke is registered, tested,
// renders in the catalog, holds a ladder row — and can never earn autonomy. Nothing else in the lattice says so.
func TestUncoveredOpClassIsAFinding(t *testing.T) {
	t.Parallel()
	root := withExemptions(t, "")
	rep, err := Check(root,
		map[string]bool{"start-guest": true, "reload-service": true},
		map[string][]string{"device-down": {"start-guest"}})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Covered100() {
		t.Fatal("an op-class with no fault source and no exemption must be a finding")
	}
	if kinds(rep)["uncovered-op-class"] != 1 {
		t.Fatalf("want exactly 1 uncovered-op-class finding, got %v", kinds(rep))
	}
	if !strings.Contains(rep.Findings[0].Detail, "reload-service") {
		t.Fatalf("finding must NAME the uncovered class, got %q", rep.Findings[0].Detail)
	}
}

// TestDeclaredExemptionSatisfiesCoverage — a declared gap with a reason is a legitimate engineering position
// (the ratify contract). Silence is the failure, not the gap itself.
func TestDeclaredExemptionSatisfiesCoverage(t *testing.T) {
	t.Parallel()
	root := withExemptions(t, `{"exemptions":[{"op_class":"reload-service","why":"no config-drift fault class exists"}]}`)
	rep, err := Check(root,
		map[string]bool{"start-guest": true, "reload-service": true},
		map[string][]string{"device-down": {"start-guest"}})
	if err != nil {
		t.Fatal(err)
	}
	if !rep.Covered100() {
		t.Fatalf("a declared exemption with a reason must satisfy coverage, got %v", rep.Findings)
	}
	if rep.Exempted != 1 || rep.Covered != 1 {
		t.Fatalf("covered=%d exempted=%d, want 1 and 1", rep.Covered, rep.Exempted)
	}
}

// TestEmptyExemptionReasonIsAFinding — "declaring" a gap without saying why is silence with extra steps, and
// would let anyone mute this checker by listing every class.
func TestEmptyExemptionReasonIsAFinding(t *testing.T) {
	t.Parallel()
	root := withExemptions(t, `{"exemptions":[{"op_class":"reload-service","why":"   "}]}`)
	rep, err := Check(root, map[string]bool{"reload-service": true}, map[string][]string{})
	if err != nil {
		t.Fatal(err)
	}
	k := kinds(rep)
	if k["empty-exemption-reason"] != 1 {
		t.Fatalf("want an empty-exemption-reason finding, got %v", k)
	}
	// and it must NOT then count as covered
	if k["uncovered-op-class"] != 1 {
		t.Fatalf("a reasonless exemption must not satisfy coverage either, got %v", k)
	}
}

// TestStaleExemptionIsAFinding — once a fault class DOES provoke the op-class, the exemption must go. Left in
// place, resolved gaps accumulate and the next real gap hides among them.
func TestStaleExemptionIsAFinding(t *testing.T) {
	t.Parallel()
	root := withExemptions(t, `{"exemptions":[{"op_class":"restart-service","why":"was uncovered once"}]}`)
	rep, err := Check(root,
		map[string]bool{"restart-service": true},
		map[string][]string{"service-down": {"restart-service"}})
	if err != nil {
		t.Fatal(err)
	}
	if kinds(rep)["stale-exemption"] != 1 {
		t.Fatalf("an exemption for a NOW-covered class must be flagged stale, got %v", kinds(rep))
	}
}

// TestExemptionForAnUnregisteredClassIsAFinding — an exemption that outlived its op-class hides nothing and
// misleads the next reader into thinking a gap is managed.
func TestExemptionForAnUnregisteredClassIsAFinding(t *testing.T) {
	t.Parallel()
	root := withExemptions(t, `{"exemptions":[{"op_class":"ghost-verb","why":"removed long ago"}]}`)
	rep, err := Check(root, map[string]bool{"start-guest": true},
		map[string][]string{"device-down": {"start-guest"}})
	if err != nil {
		t.Fatal(err)
	}
	if kinds(rep)["stale-exemption"] != 1 {
		t.Fatalf("want a stale-exemption finding for an unregistered class, got %v", kinds(rep))
	}
}

// TestPhantomProvokesIsAFinding — a fault class naming an op-class that does not exist injects forever and is
// credited to nothing. Worse, it would otherwise read as coverage for a class that is not there.
func TestPhantomProvokesIsAFinding(t *testing.T) {
	t.Parallel()
	root := withExemptions(t, "")
	rep, err := Check(root, map[string]bool{"start-guest": true},
		map[string][]string{"device-down": {"start-guest"}, "weird-fault": {"verb-that-was-renamed"}})
	if err != nil {
		t.Fatal(err)
	}
	if kinds(rep)["phantom-provokes"] != 1 {
		t.Fatalf("want a phantom-provokes finding, got %v", kinds(rep))
	}
}

// TestMissingExemptionsFileFailsClosed — no file must mean NO exemptions (every gap is a finding), never
// "everything is fine". A checker that passes when its input is absent is the vacuous-pass failure mode.
func TestMissingExemptionsFileFailsClosed(t *testing.T) {
	t.Parallel()
	root := t.TempDir() // no spec/ dir at all
	rep, err := Check(root, map[string]bool{"reload-service": true}, map[string][]string{})
	if err != nil {
		t.Fatalf("an absent exemptions file is not an error, it is zero exemptions: %v", err)
	}
	if rep.Covered100() {
		t.Fatal("with no exemptions file, an uncovered class must still be a finding (fail closed)")
	}
}

// TestNormalizationCannotDodgeTheCheck — a case/whitespace variant must not create a phantom gap or a phantom
// coverage, mirroring how the registry normalizes op-class slugs.
func TestNormalizationCannotDodgeTheCheck(t *testing.T) {
	t.Parallel()
	root := withExemptions(t, `{"exemptions":[{"op_class":"  Reload-Service  ","why":"declared with odd casing"}]}`)
	rep, err := Check(root,
		map[string]bool{"start-guest": true, "reload-service": true},
		map[string][]string{"device-down": {"  Start-Guest  "}})
	if err != nil {
		t.Fatal(err)
	}
	if !rep.Covered100() {
		t.Fatalf("normalization must make both the provokes and the exemption match, got %v", rep.Findings)
	}
}
