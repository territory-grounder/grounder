package estate

import (
	"strconv"
	"strings"
	"testing"
)

// TestParseRelType pins the declared relation vocabulary that both the declared-config parser and the eval
// snapshot loader bind to. The member_of / routes_via cases are the regression: the old eval/discovery.go
// relOf recognised only runs_on and silently coerced these DECLARED types into depends_on (TG-179a, TG-175).
func TestParseRelType(t *testing.T) {
	cases := []struct {
		in   string
		want RelType
		ok   bool
	}{
		{"runs_on", RelRunsOn, true},
		{"RUNS_ON", RelRunsOn, true},          // case-insensitive, matching the old EqualFold behaviour
		{"member_of", RelMemberOf, true},      // regression: was silently coerced to depends_on
		{"routes_via", RelRoutesVia, true},    // regression: was silently coerced to depends_on
		{"depends_on", RelDependsOn, true},
		{"", RelDependsOn, true},              // empty is the legitimate generic default
		{"   ", RelDependsOn, true},           // whitespace trims to empty
		{"peers_with", RelDependsOn, false},   // boundary violation: unknown rel, ok=false
		{"member of", RelDependsOn, false},    // near-miss typo is NOT silently accepted
	}
	for _, c := range cases {
		got, ok := ParseRelType(c.in)
		if got != c.want || ok != c.ok {
			t.Errorf("ParseRelType(%q) = (%v, %v); want (%v, %v)", c.in, got, ok, c.want, c.ok)
		}
	}
}

// TestParseRelType_KnownSetComplete guards against a new RelType being added to the const block without
// being taught to the parser (and vice versa) — every knownRelTypes entry must round-trip through ParseRelType.
func TestParseRelType_KnownSetComplete(t *testing.T) {
	for k := range knownRelTypes {
		got, ok := ParseRelType(string(k))
		if !ok || got != k {
			t.Errorf("declared rel %q does not round-trip through ParseRelType: got (%v, %v)", k, got, ok)
		}
	}
}

// TestUnknownRelationCounterIncrementsAtTheFallback is the killing test for the TG-179 unknown_relation
// counter. BEFORE the counter was wired, a relation string outside the declared vocabulary vanished into the
// depends_on fallback with no signal at all — the ontology's own boundary violation was invisible. Now every
// unrecognised NON-EMPTY relation is COUNTED at the shared ParseRelType chokepoint (surfaced live as
// tg_estate_unknown_relation_total), whether the caller then rejects or coerces it. The assertions are
// DELTA-based because the counter is a process-wide monotonic total that other tests in this package also
// move. (TG-179, epic TG-175 — ontology losability.)
func TestUnknownRelationCounterIncrementsAtTheFallback(t *testing.T) {
	// CONTROL: legal relations (and the empty/whitespace generic default) must NOT move the counter —
	// otherwise the signal is noise and cannot discriminate a boundary violation from ordinary traffic.
	for _, legal := range []string{"runs_on", "member_of", "depends_on", "routes_via", "", "   "} {
		before := UnknownRelationCount()
		if _, ok := ParseRelType(legal); !ok {
			t.Fatalf("ParseRelType(%q) reported a declared relation as unknown", legal)
		}
		if got := UnknownRelationCount(); got != before {
			t.Fatalf("a legal relation %q moved the unknown_relation counter %d -> %d; the control must be flat", legal, before, got)
		}
	}

	// KILL: a non-empty relation outside the vocabulary must move the counter by EXACTLY one — the boundary
	// violation the ontology cannot represent is now visible instead of silently coerced to depends_on.
	before := UnknownRelationCount()
	got, ok := ParseRelType("peers_with")
	if ok {
		t.Fatal("ParseRelType accepted peers_with as a declared relation")
	}
	if got != RelDependsOn {
		t.Fatalf("ParseRelType(peers_with) coerced to %q, want the depends_on fallback the counter now observes", got)
	}
	if n := UnknownRelationCount(); n != before+1 {
		t.Fatalf("unknown_relation counter did not move for an out-of-vocabulary relation: %d -> %d (want +1). "+
			"Before the fix the coercion to depends_on left no signal at all.", before, n)
	}

	// LIVE worker path: an unknown rel in an operator declared-estate edge feeds the SAME counter — the worker
	// parses declared-estate config through ParseDeclared (cmd/worker/main.go), so the boundary violation is
	// visible on /metrics and not only in the skip log. The rel is still REJECTED (no phantom edge seeded).
	before = UnknownRelationCount()
	if _, err := ParseDeclared(strings.NewReader(`[{"from":"a","to":"b","rel":"peers_with"}]`)); err == nil {
		t.Fatal("ParseDeclared must still reject an unknown rel (no phantom edge seeded)")
	}
	if n := UnknownRelationCount(); n != before+1 {
		t.Fatalf("a declared-estate boundary violation did not reach the unknown_relation counter: %d -> %d (want +1)", before, n)
	}

	// CONTROL on the live path: a well-formed declared edge with a legal rel must NOT move the counter.
	before = UnknownRelationCount()
	if _, err := ParseDeclared(strings.NewReader(`[{"from":"a","to":"b","rel":"depends_on"}]`)); err != nil {
		t.Fatalf("a legal declared edge must parse cleanly, got: %v", err)
	}
	if n := UnknownRelationCount(); n != before {
		t.Fatalf("a legal declared edge moved the unknown_relation counter %d -> %d; the control must be flat", before, n)
	}
}

// TestParseDeclaredRejectSemanticsPreserved is the review-fix guard: the unknown_relation counter is
// OBSERVE-ONLY and must not WIDEN what the LIVE declared-estate ingest (cmd/worker/main.go via ParseDeclared)
// accepts. ParseDeclared keeps origin/main's STRICT, case-sensitive vocabulary check — a case variant of a
// legal relation ("RUNS_ON") and a whitespace-only relation ("   ") must STILL hard-REJECT (no phantom edge
// seeded), byte-equivalent to before the counter existed. The reject branch still COUNTS the violation, but
// counting never changes the accept/reject decision. (TG-179 review fix.)
func TestParseDeclaredRejectSemanticsPreserved(t *testing.T) {
	// Shapes origin/main HARD-rejected (case variants, whitespace-only, genuine unknowns) must still reject,
	// and each must leave a signal on the counter (the reject branch increments).
	for _, rel := range []string{"RUNS_ON", "Depends_On", "  ", "\t", "runs_on ", "peers_with"} {
		before := UnknownRelationCount()
		body := `[{"from":"a","to":"b","rel":` + strconv.Quote(rel) + `}]`
		edges, err := ParseDeclared(strings.NewReader(body))
		if err == nil {
			t.Fatalf("ParseDeclared ACCEPTED rel %q (%d edges) — origin/main hard-rejected it; the counter must not widen ingest acceptance", rel, len(edges))
		}
		if n := UnknownRelationCount(); n != before+1 {
			t.Fatalf("ParseDeclared rejected rel %q but did not count the live-path violation: %d -> %d (want +1)", rel, before, n)
		}
	}
	// The exact-lowercase legal forms and the empty default still ACCEPT, and must NOT move the counter.
	for _, rel := range []string{"runs_on", "member_of", "depends_on", "routes_via", ""} {
		before := UnknownRelationCount()
		body := `[{"from":"a","to":"b","rel":` + strconv.Quote(rel) + `}]`
		if _, err := ParseDeclared(strings.NewReader(body)); err != nil {
			t.Fatalf("ParseDeclared rejected legal rel %q: %v", rel, err)
		}
		if n := UnknownRelationCount(); n != before {
			t.Fatalf("ParseDeclared counted a legal rel %q: %d -> %d; the control must be flat", rel, before, n)
		}
	}
}
