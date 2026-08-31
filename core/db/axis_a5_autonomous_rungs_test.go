package db

// AXIS A5 CAPABILITY BREADTH MUST COUNT BOTH AUTONOMOUS RUNGS (TG-249 item 3).
//
// GraduatedOpClasses answers "what can TG heal autonomously right now". It filtered
// policy_graduation.level = 'auto' alone, so every class sitting at auto_notice was omitted — while acting
// without a vote the entire time.
//
// core/policy.Level.Verdict states the design directly:
//
//	"auto_notice sharing the `auto` verdict with auto is the point of the rung, not a leak: the class acts
//	 without a vote at BOTH rungs. The notice is applied downstream as a band floor."
//
// And the undercount is systematic, not occasional: auto_notice is a MANDATORY intermediate rung
// (spec/028 REQ-2808) that every class holds before it may reach silent auto. So the classes missed are
// precisely the newly-autonomous ones — the interesting half of a capability metric.
//
// This is a SOURCE-level guard because the query is one line of SQL in a package whose database tests
// cannot run without TG_TEST_DSN; a reader must not mistake a green package run for evidence about this.

import (
	"os"
	"strings"
	"testing"
	"time"
)

func TestGraduatedOpClassesQueryCoversBothAutonomousRungs(t *testing.T) {
	raw, err := os.ReadFile("axis_read.go")
	if err != nil {
		t.Fatalf("read axis_read.go: %v", err)
	}
	src := string(raw)

	// Located by literal anchors rather than a windowed regex: this repo's lint hooks refuse bounded
	// quantifiers over character classes, and a fixed-window match would silently stop matching if the
	// query grew a line anyway.
	const head = "SELECT op_class FROM policy_graduation WHERE "
	const tail = "ORDER BY op_class"
	i := strings.Index(src, head)
	if i < 0 {
		t.Fatal("the GraduatedOpClasses query was not found by its SELECT prefix. Either it moved or it " +
			"was rewritten — this guard is asserting nothing about a query it cannot see, and must fail " +
			"rather than pass.")
	}
	rest := src[i+len(head):]
	j := strings.Index(rest, tail)
	if j < 0 {
		t.Fatal("found the query prefix but not its ORDER BY — the parse is wrong and the assertions " +
			"below would run against the rest of the file")
	}
	where := strings.TrimSpace(rest[:j])

	if !strings.Contains(where, "auto_notice") {
		t.Errorf("the capability-breadth query does not include the auto_notice rung: WHERE %s\n"+
			"A class at auto_notice ACTS WITHOUT A VOTE (core/policy.Level.Verdict grants both rungs the "+
			"auto verdict; the notice is a downstream band floor). Omitting it understates what TG can "+
			"heal — and because auto_notice is a mandatory rung on the way to silent auto, it understates "+
			"it exactly for newly-autonomous classes.", where)
	}
	if !strings.Contains(where, "'auto'") {
		t.Errorf("the query no longer includes the silent auto rung: WHERE %s — that swaps one undercount "+
			"for another", where)
	}
	// A rung that permits at most `approve` must NOT be counted as capability. Overstating what TG can do
	// without a human is a worse error than the undercount this test was written to fix.
	for _, notAutonomous := range []string{"'approve'", "'shadow'", "'observe'"} {
		if strings.Contains(where, notAutonomous) {
			t.Errorf("the capability-breadth query counts %s, which is NOT an autonomous rung: WHERE %s",
				notAutonomous, where)
		}
	}
}

// THE RUNG SPLIT, AGAINST A REAL LADDER (TG-249 item 3, second half).
//
// Counting both rungs stopped A5 undercounting. It also made the number ambiguous in a new way: a class at
// `auto` heals silently, a class at `auto_notice` heals and tells someone, and one figure cannot say which.
// The ticket asked for them "returned separately so silent autonomy is not conflated with acts-and-pages",
// and that half had not been delivered.
//
// This one runs against a real Postgres rather than the source text, because a subset relationship between
// two queries is exactly the kind of thing that reads correctly and behaves wrongly.
//
// KILLING MUTATION: point the NoticeOpClasses query at level='auto', or drop it and derive Notice from
// Graduated. RED.
func TestNoticeOpClassesIsTheAutoNoticeSubset(t *testing.T) {
	ctx, p, done := openFixture(t)
	defer done()

	if _, err := p.Exec(ctx, `DELETE FROM policy_graduation`); err != nil {
		t.Fatalf("clear ladder: %v", err)
	}
	for _, r := range []struct{ opClass, level string }{
		{"restart-service", "auto"},      // silent
		{"start-guest", "auto_notice"},   // acts AND pages
		{"start-service", "auto_notice"}, // acts AND pages
		// `approve` is the negative case, and the ONLY one available: migration 0050's CHECK constraint
		// permits exactly (approve | auto_notice | auto), so seeding anything else fails the INSERT and
		// would test the constraint rather than the query.
		{"grow-disk", "approve"}, // not autonomous at all
	} {
		// GROUNDED SEED (TG-321, migration 0067). Reaching an autonomy-granting rung now requires a
		// graduation_credit row, which requires an action_execution (0064). This test is about the axis
		// QUERY, not the ladder's admission rules, so it supplies the grounding rather than being exempted
		// — an exemption would let the fixture seed a state production can no longer reach, and the axis
		// would then be verified against an impossible estate.
		ref := "a5-" + r.opClass
		if _, err := p.Exec(ctx,
			`INSERT INTO action_execution (action_id, external_ref, verdict, unverifiable, target_host, site, executed_at, schema_version)
			 VALUES ($1,$2,'match',false,'a5-host','nl',now(),1) ON CONFLICT DO NOTHING`, "a5-act-"+r.opClass, ref); err != nil {
			t.Fatalf("seed execution for %s: %v", r.opClass, err)
		}
		if _, err := p.Exec(ctx,
			`INSERT INTO graduation_credit (op_class, external_ref) VALUES ($1,$2) ON CONFLICT DO NOTHING`,
			r.opClass, ref); err != nil {
			t.Fatalf("seed credit for %s: %v", r.opClass, err)
		}
		if _, err := p.Exec(ctx,
			`INSERT INTO policy_graduation (op_class, level) VALUES ($1, $2)
			 ON CONFLICT (op_class) DO UPDATE SET level = EXCLUDED.level`, r.opClass, r.level); err != nil {
			t.Fatalf("seed %s: %v", r.opClass, err)
		}
	}

	agg, err := NewAxisReadStore(p).Aggregate(ctx, time.Now().UTC().Add(-2*time.Hour))
	if err != nil {
		t.Fatalf("Aggregate: %v", err)
	}

	wantGraduated := []string{"restart-service", "start-guest", "start-service"}
	if got := append([]string(nil), agg.GraduatedOpClasses...); !rungEqual(got, wantGraduated) {
		t.Errorf("GraduatedOpClasses = %v, want %v — the capability breadth must still be BOTH rungs", got, wantGraduated)
	}

	wantNotice := []string{"start-guest", "start-service"}
	got := append([]string(nil), agg.NoticeOpClasses...)
	if !rungEqual(got, wantNotice) {
		t.Errorf("NoticeOpClasses = %v, want %v — this is the acts-AND-pages subset, and a reader uses it to "+
			"tell how much autonomy is still audible", got, wantNotice)
	}
	// The relationship the console and axisscore both derive silent-autonomy from.
	if silent := len(agg.GraduatedOpClasses) - len(agg.NoticeOpClasses); silent != 1 {
		t.Errorf("silent autonomy = %d, want 1 — Graduated minus Notice is how every consumer computes the "+
			"classes that act with nobody hearing", silent)
	}
	// Notice must be a SUBSET, never a disjoint set: every consumer treats Graduated as the total.
	for _, n := range agg.NoticeOpClasses {
		if !rungContains(agg.GraduatedOpClasses, n) {
			t.Errorf("%q is in NoticeOpClasses but not in GraduatedOpClasses — the subtraction above would "+
				"then report a negative silent count", n)
		}
	}
	// And the non-autonomous rungs must appear in NEITHER.
	for _, absent := range []string{"grow-disk"} {
		if rungContains(agg.GraduatedOpClasses, absent) || rungContains(agg.NoticeOpClasses, absent) {
			t.Errorf("%q sits below the autonomous rungs and must not be counted as capability", absent)
		}
	}
}

func rungEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func rungContains(hay []string, needle string) bool {
	for _, h := range hay {
		if h == needle {
			return true
		}
	}
	return false
}
