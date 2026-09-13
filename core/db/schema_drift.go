package db

// schema_drift.go — ask the RUNNING database what it contains, not the fixture (TG-383).
//
// THE GAP THIS FILLS. Every schema guard in this repo runs against a test fixture built from the
// migrations in this repo, so all of them answer one question: "is the schema we DECLARE
// self-consistent?" Nobody answered "does the schema that EXISTS match the one we declare?" — and the gap
// between those two is exactly where an undeclared, unowned, unguarded table lives.
//
// It is not hypothetical. Measured on dc1tg01:
//
//	policy_ruleset_bak_handsoff | owner postgres | plane comment: (none) | created by no migration
//
// A hand-made backup. `TestEveryTableDeclaresItsPlane` is green and structurally cannot see it: its
// universe is the migration set, while the thing it protects against — a table nobody declared — arrives
// by precisely the route the fixture cannot reproduce, someone typing CREATE TABLE on the box.
//
// That table already cost a control. It aborted the whole plane-grant derivation (TG-368): the privilege
// loop took a REVOKE arm on a table tg_migration does not own, 42501, transaction rolled back, no
// privileges derived for EITHER role. Migration 0066 taught the derivation to skip unrevokable tables, so
// the split works again — but nothing was made AWARE of the table, which is the half this file adds.
//
// READ-ONLY BY CONSTRUCTION. Every statement here is a SELECT against the catalog. This runs at boot on a
// production database; it must be incapable of changing it.

import (
	"context"
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// SchemaDrift is what the running database has that the declared schema does not account for.
type SchemaDrift struct {
	// Total is every ordinary table in `public`. It is the DENOMINATOR, and it is reported even when the
	// drift lists are empty: "0 undeclared" against an unknown total is the vacuous reading this whole
	// finding is about.
	Total int
	// Undeclared are tables no embedded migration creates. These are the dangerous ones — nothing in this
	// repo knows they exist, so no guard, no grant rule and no review has ever considered them.
	Undeclared []string
	// UnplaneD are tables carrying no `plane:` comment. Migration 0060's own error text says what that
	// costs: until a table declares its plane it is granted to BOTH by default, so if it records or
	// authorises an actuation, a compromised triage worker can forge that record and nothing fails.
	UnplaneD []string
}

// Clean reports whether the running schema matches what this build declares.
func (d SchemaDrift) Clean() bool { return len(d.Undeclared) == 0 && len(d.UnplaneD) == 0 }

// String is the boot line. It always states the denominator, so a clean result cannot be confused with a
// check that examined nothing — the failure mode this file exists to end.
func (d SchemaDrift) String() string {
	if d.Total == 0 {
		return "NO TABLES SEEN in public — this check examined nothing and proves nothing about the " +
			"running schema (expected on a database that has not been migrated yet)"
	}
	if d.Clean() {
		return fmt.Sprintf("%d table(s) in public; all created by a migration in this build and all "+
			"declaring a plane", d.Total)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%d table(s) in public", d.Total)
	if len(d.Undeclared) > 0 {
		fmt.Fprintf(&b, "; %d created by NO migration in this build [%s] — nothing in this repo knows "+
			"these exist, so no schema guard, grant rule or review has considered them",
			len(d.Undeclared), strings.Join(d.Undeclared, " "))
	}
	if len(d.UnplaneD) > 0 {
		fmt.Fprintf(&b, "; %d declare NO plane [%s] — an undeclared table is granted to BOTH credential "+
			"planes by default, so a compromised triage worker could forge any actuation record it holds",
			len(d.UnplaneD), strings.Join(d.UnplaneD, " "))
	}
	return b.String()
}

// createTableRe finds the tables the embedded migrations create. Deliberately tolerant of
// `IF NOT EXISTS`, of a `public.` qualifier and of quoting, because a miss here manufactures a false
// "undeclared" and a security check that cries wolf is one that gets muted.
var createTableRe = regexp.MustCompile(`(?is)\bCREATE\s+TABLE\s+(?:IF\s+NOT\s+EXISTS\s+)?"?(?:public\.)?"?([a-zA-Z_][a-zA-Z0-9_]*)"?`)

// runnerOwnedTables are created by the migration RUNNER rather than by any migration file, so no
// `CREATE TABLE` exists for them anywhere and no plane COMMENT can be attached to them either.
//
// THIS IS NOT A NEW JUDGEMENT — it mirrors one migration 0060 already recorded, in its own words:
//
//	`schema_migrations` is deliberately ABSENT. It is the migrator's own bookkeeping table, created
//	by the migrator rather than by any .up.sql — and CI applies these files with raw `psql -f`, where
//	it does not exist yet. Commenting on it there fails the whole run. It holds no plane-relevant data
//	either, so the test below skips it for the same reason rather than as a workaround.
//
// So the sibling plane guard ALREADY skips this table. A drift check that flags it would be reporting
// a decision as a defect, and disagreeing with the guard it sits beside.
//
// AND IT WOULD HAVE BROKEN THE ALERT. On the first live boot the check reported 2 undeclared tables:
// `policy_ruleset_bak_handsoff` (the real finding — a hand-made backup) and this one. The second is
// true and PERMANENTLY true, putting a floor under both gauges — while TG-383's own requested remedy
// is "a gauge alerted on non-zero". A gauge that can never reach zero cannot carry that alert, and an
// always-firing alert is the one operators learn to ignore: the exact pathology this check exists to
// end, reproduced inside the fix.
//
// IT IS A NAME, NOT A RULE. A pattern like "anything ending in _migrations" is how a genuinely
// undeclared table walks back in behind a plausible suffix — the same mistake as repointing a spec's
// files_owned by filename instead of by content (TG-416). One table, named, with this reason.
var runnerOwnedTables = map[string]bool{
	"schema_migrations": true,
}

// declaredTables is the set of tables the migrations embedded in THIS BINARY create. That is the honest
// denominator for "declared": not what the repo's files say today, but what the running build believes.
func declaredTables() map[string]bool {
	out := map[string]bool{}
	for t := range runnerOwnedTables {
		out[t] = true
	}
	entries, err := migrationsFS.ReadDir("migrations")
	if err != nil {
		return out
	}
	for _, e := range entries {
		b, err := migrationsFS.ReadFile("migrations/" + e.Name())
		if err != nil {
			continue
		}
		for _, m := range createTableRe.FindAllStringSubmatch(string(b), -1) {
			out[strings.ToLower(m[1])] = true
		}
	}
	return out
}

// DetectSchemaDrift compares the running database against what this build declares.
//
// It takes a Pool rather than a DSN because it is a read: it rides the connection the process already
// has, and cannot be handed migration credentials by accident.
func DetectSchemaDrift(ctx context.Context, p *Pool) (SchemaDrift, error) {
	var d SchemaDrift
	if p == nil {
		return d, fmt.Errorf("schema drift: no pool")
	}
	rows, err := p.Query(ctx, `
		SELECT c.relname,
		       COALESCE(obj_description(c.oid, 'pg_class'), '') AS cmt
		  FROM pg_class c
		  JOIN pg_namespace n ON n.oid = c.relnamespace
		 WHERE n.nspname = 'public' AND c.relkind = 'r'
		 ORDER BY c.relname`)
	if err != nil {
		return d, fmt.Errorf("schema drift: list tables: %w", err)
	}
	defer rows.Close()

	declared := declaredTables()
	for rows.Next() {
		var name, cmt string
		if err := rows.Scan(&name, &cmt); err != nil {
			return d, fmt.Errorf("schema drift: scan: %w", err)
		}
		d.Total++
		if !declared[strings.ToLower(name)] {
			d.Undeclared = append(d.Undeclared, name)
		}
		// The convention migration 0060 established is a `plane:` prefix in the table comment. The
		// runner-owned exemption applies HERE TOO, and for 0060's own stated reason: a plane comment
		// cannot be attached to schema_migrations under raw `psql -f`, and it holds no plane-relevant
		// data. The sibling plane guard skips it; flagging it here would make the two disagree.
		if !runnerOwnedTables[strings.ToLower(name)] && !strings.Contains(strings.ToLower(cmt), "plane:") {
			d.UnplaneD = append(d.UnplaneD, name)
		}
	}
	if err := rows.Err(); err != nil {
		return d, fmt.Errorf("schema drift: rows: %w", err)
	}
	sort.Strings(d.Undeclared)
	sort.Strings(d.UnplaneD)
	return d, nil
}
