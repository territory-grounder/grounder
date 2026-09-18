package db

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// EVERY SCHEDULABLE FAULT CLASS MUST HAVE A DETECTION MAPPING.
//
// The mapping fails CLOSED — an unmapped class counts as a miss — which is the right default and is exactly
// why the gap was invisible: container-down shipped 2026-07-27 with no entry, so every injection added +1 to
// the A1 denominator and could never reach the numerator. It detects 17/18 in reality and was published as
// 0/18, understating pooled A1 from 83.3% to 78.5%.
//
// Nothing failed. The number was just quietly wrong, in the direction that makes TG look worse — which is the
// direction nobody investigates. A closed-enumeration assertion is the only thing that catches it, because a
// missing OR-clause is not a bug in any code path, it is an absence.
func TestEverySchedulableFaultClassHasADetectionMapping(t *testing.T) {
	// ★ THE ENUMERATION IS READ FROM THE INJECTOR'S SOURCE, NOT MIRRORED HERE.
	//
	// It used to be a hand-kept map in this file, with a comment saying "if the two ever diverge the
	// divergence is itself the finding". They diverged and nothing found it: log-fill shipped 2026-07-29 with
	// no mapping AND no entry in the mirror, so this test — written precisely to catch a class shipped
	// without a mapping — passed. A mirror is maintained by the same person who forgot the thing it guards.
	//
	// core/db still must not IMPORT tools/, so the classes are extracted from the source text of the closed
	// enumeration. That keeps the dependency direction intact while removing the copy: a class added to
	// plan.go is checked here the moment it is declared.
	src, err := os.ReadFile(filepath.Join("..", "..", "tools", "faultinjector", "plan.go"))
	if err != nil {
		t.Fatalf("cannot read the injector's class enumeration: %v", err)
	}
	// Class<Name> Class = "<slug>"
	re := regexp.MustCompile(`Class\w+\s+Class\s*=\s*"([a-z0-9-]+)"`)
	found := re.FindAllStringSubmatch(string(src), -1)
	if len(found) < 5 {
		t.Fatalf("extracted only %d fault classes from plan.go — the pattern no longer matches the "+
			"declaration form, so this oracle would pass vacuously", len(found))
	}
	for _, m := range found {
		class := m[1]
		if !strings.Contains(detectRuleMatch, "'"+class+"'") {
			t.Errorf("fault class %q has NO detection mapping.\n"+
				"The mapping fails closed, so every injection of it is +1 denominator and 0 numerator — A1 is "+
				"silently understated rather than failing. Add an OR-clause to detectRuleMatch.", class)
		}
	}
}

// The predicate is interpolated into TWO queries (pooled and per-class). They must be the SAME text, or the
// headline and the breakdown can disagree while both look authoritative.
func TestPooledAndPerClassUseTheSamePredicate(t *testing.T) {
	// ★ THIS ASSERTED THAT A CONSTANT EQUALS ITSELF. The helper it called was
	// `func axisDetectionSQLSources() string { return detectRuleMatch + detectRuleMatch }` — it returned the
	// constant twice, so counting two occurrences proved nothing whatsoever about the two real queries. A
	// query that had been rewritten with a hand-copied predicate would have satisfied it forever.
	//
	// Read the implementation's SOURCE and count the interpolation sites instead.
	src, err := os.ReadFile("axis_read.go")
	if err != nil {
		t.Fatalf("cannot read axis_read.go: %v", err)
	}
	body := string(src)

	n := strings.Count(body, "+detectRuleMatch+")
	if n < 2 {
		t.Fatalf("found %d interpolation(s) of detectRuleMatch in axis_read.go, want at least 2 (the pooled "+
			"recall query and the per-class breakdown). Two hand-written copies drift, and a reader cannot "+
			"tell which one produced a number", n)
	}

	// ★ THE STRONGER HALF: the mapping text must exist in exactly ONE place. Counting interpolation SITES is
	// not enough — a third query can hand-copy the predicate and the count still passes. (It nearly did: a
	// detection-latency query added a third site, so an earlier version of this assertion, `n >= 2`, stayed
	// green while one site was replaced by a hand-copy in a mutation control.)
	//
	// Every `f.fault_type = '` in the file must sit INSIDE the constant's declaration. If one appears outside
	// it, some query is matching fault classes by its own copy of the rules, and the headline and the
	// breakdown can disagree while both look authoritative.
	const marker = "f.fault_type = '"
	declStart := strings.Index(body, "const detectRuleMatch = `")
	if declStart < 0 {
		t.Fatal("cannot locate the detectRuleMatch declaration — this oracle can no longer tell inside from outside")
	}
	declEnd := strings.Index(body[declStart+len("const detectRuleMatch = `"):], "`")
	if declEnd < 0 {
		t.Fatal("detectRuleMatch declaration is unterminated")
	}
	decl := body[declStart : declStart+len("const detectRuleMatch = `")+declEnd]

	inDecl := strings.Count(decl, marker)
	inFile := strings.Count(body, marker)
	if inDecl == 0 {
		t.Fatal("the constant contains no fault_type predicate at all — it would match nothing")
	}
	if inFile != inDecl {
		t.Errorf("axis_read.go contains %d %q predicates but only %d are inside the detectRuleMatch "+
			"declaration — %d live in a hand-copied predicate somewhere else. The mapping must exist in ONE "+
			"place; a copy drifts silently and fails closed, understating A1 in the direction nobody "+
			"investigates", inFile, marker, inDecl, inFile-inDecl)
	}
}

// MUTATION CONTROL surface: the constant must actually be non-trivial. A fix that emptied it would make every
// class "match" the substring test above while matching nothing in SQL.
func TestDetectMappingIsNotVacuous(t *testing.T) {
	if !strings.Contains(detectRuleMatch, "alert_rule ILIKE") {
		t.Fatal("the mapping must actually constrain alert_rule — an empty or always-true predicate would " +
			"count every alert on the host as a detection and inflate A1 to ~100%")
	}
	if !strings.Contains(detectRuleMatch, "f.fault_type =") {
		t.Fatal("the mapping must key on fault_type — without it any alert matches any fault")
	}
}
