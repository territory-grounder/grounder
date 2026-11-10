package db

// THE TOTAL MUST COUNT THE POPULATION THE LIST PAGES (TG-249 item 2).
//
// Count() read session_risk_audit alone while Recent() draws from the UNION of session_risk_audit and
// session_triage. Measured on the live estate 2026-08-05:
//
//	total_as_reported  2040
//	true_population    3225
//
// 1,185 sessions — 37% — that a client could page through while `total` said they did not exist. This is
// the same defect class as the !852 badge that counted a different population than the list beside it in
// the same response, failing in the roomier direction: a client trusting `total` for pagination stops
// early and never learns that it stopped early.
//
// A source-level guard, because core/db's database tests cannot run without TG_TEST_DSN and a green
// package run must not be mistaken for evidence about this query.

import (
	"os"
	"strings"
	"testing"
)

// sqlBetween returns the text of the function body starting at a named func, up to the next top-level
// func. Literal anchors rather than a windowed regex: this repo's hooks refuse bounded quantifiers over
// character classes, and a fixed window silently stops matching when the body grows.
func sqlBetween(t *testing.T, src, funcDecl string) string {
	t.Helper()
	i := strings.Index(src, funcDecl)
	if i < 0 {
		t.Fatalf("%s not found — this guard is reading a file that no longer defines it, and would "+
			"otherwise pass by inspecting nothing", funcDecl)
	}
	rest := src[i+len(funcDecl):]
	if j := strings.Index(rest, "\nfunc "); j > 0 {
		rest = rest[:j]
	}
	// COMMENT LINES ARE NOT CODE. The first version of this guard failed on its own subject: the comment
	// above the query explains why UNION ALL would be wrong, and the guard read that prose as the query
	// using UNION ALL. That is the exact defect this repo keeps finding (TG-326, TG-143) reproduced inside
	// the check written to catch it, which is worth the four lines to prevent.
	var code []string
	for _, ln := range strings.Split(rest, "\n") {
		if t := strings.TrimSpace(ln); t == "" || strings.HasPrefix(t, "//") {
			continue
		}
		code = append(code, ln)
	}
	return strings.Join(code, "\n")
}

func TestSessionCountAndRecentReadTheSamePopulation(t *testing.T) {
	raw, err := os.ReadFile("sessions_read.go")
	if err != nil {
		t.Fatalf("read sessions_read.go: %v", err)
	}
	src := string(raw)

	countBody := sqlBetween(t, src, "func (s *SessionReadStore) Count(")

	// Both tables must appear in the count, because both appear in the list.
	for _, tbl := range []string{"session_risk_audit", "session_triage"} {
		if !strings.Contains(countBody, tbl) {
			t.Errorf("Count() does not read %s, but Recent() pages a UNION that includes it.\n"+
				"A total drawn from a narrower population than the list is worse than no total: a client "+
				"paging on it stops early and never learns it stopped early. Measured live, the gap was "+
				"2040 reported against 3225 real.", tbl)
		}
	}

	// UNION, not UNION ALL: external_ref is the join key and a session in both tables must count once.
	// UNION ALL would trade the undercount for an overcount of exactly the overlap.
	if strings.Contains(countBody, "UNION ALL") {
		t.Error("Count() uses UNION ALL, which double-counts every session present in BOTH tables — " +
			"swapping a 37% undercount for an overcount of exactly the overlap. external_ref is the join " +
			"key; plain UNION is what makes the count match the list.")
	}
	if !strings.Contains(countBody, "UNION") {
		t.Error("Count() no longer unions the two session tables at all")
	}
}
