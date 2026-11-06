package db

// EVERY TABLE DECLARES ITS CREDENTIAL PLANE, SO THE WITHHELD LISTS CANNOT NARROW SILENTLY (TG-323).
//
// TG-164's two withheld lists are hand-maintained Go slices, and a table in neither is granted to BOTH
// planes. That fail-OPEN default is deliberate and stays: deny-by-default would make a table added next
// month unwritable by the plane that needs it, surfacing as a permission error deep inside a Temporal
// activity rather than at boot — the worst failure mode TG-164's own risk note names.
//
// What was ungated is the OMISSION. TestPlaneWithheldTablesAreRealAndDisjoint catches a STALE entry and an
// entry in both lists; it cannot catch a MISSING one. So the control narrowed every time the schema grew,
// while the boot report kept printing a count that looked the same. A control that quietly covers less of
// the schema each month, and reports the same number, is the shape this repository keeps rediscovering.
//
// The classification lives in a table COMMENT (migration 0060) rather than a second Go list, because the
// person adding a table is then the person who classifies it — at the CREATE TABLE, where they are already
// thinking about what it holds. A second list in Go would be one more thing to forget, which is the defect.

import (
	"strings"
	"testing"
)

// planeOfComment extracts the declared plane from a table comment of the form `plane: <name>`.
func planeOfComment(c string) string {
	i := strings.Index(c, "plane:")
	if i < 0 {
		return ""
	}
	return strings.Fields(strings.TrimSpace(c[i+len("plane:"):]))[0]
}

func TestEveryTableDeclaresItsPlane(t *testing.T) {
	ctx, p, _ := planeRoleFixture(t)

	rows, err := p.Pool.Query(ctx, `
		SELECT c.relname, COALESCE(obj_description(c.oid, 'pg_class'), '')
		FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE n.nspname = 'public' AND c.relkind IN ('r','p')
		ORDER BY 1`)
	if err != nil {
		t.Fatalf("read table comments: %v", err)
	}
	defer rows.Close()

	act := map[string]bool{}
	for _, n := range ActuationAuthorityTables {
		act[n] = true
	}
	tri := map[string]bool{}
	for _, n := range TriageContentTables {
		tri[n] = true
	}

	var scanned int
	var undeclared, mismatched []string
	for rows.Next() {
		var name, comment string
		if err := rows.Scan(&name, &comment); err != nil {
			t.Fatalf("scan: %v", err)
		}
		// The migrator's own bookkeeping table is not part of the domain schema: it is created by the
		// migrator rather than by a migration, it does not exist when CI applies the .up.sql files with
		// raw psql, and it holds nothing either plane reads or writes as evidence.
		if name == "schema_migrations" {
			continue
		}
		scanned++
		plane := planeOfComment(comment)
		switch plane {
		case "":
			undeclared = append(undeclared, name)
			continue
		case "actuation", "triage", "both":
		default:
			mismatched = append(mismatched, name+": unknown plane "+plane)
			continue
		}
		// The declaration and the operative Go lists must agree, or the comment is documentation that
		// contradicts the control it claims to describe — worse than no comment.
		switch {
		case plane == "actuation" && !act[name]:
			mismatched = append(mismatched, name+": declares actuation but is not in ActuationAuthorityTables")
		case plane == "triage" && !tri[name]:
			mismatched = append(mismatched, name+": declares triage but is not in TriageContentTables")
		case plane == "both" && (act[name] || tri[name]):
			mismatched = append(mismatched, name+": declares both but is withheld from a plane")
		}
	}
	// Vacuity floor: a fixture with no tables would pass every check above by having nothing to check.
	if scanned == 0 {
		t.Fatal("no tables found in public — this guard scanned NOTHING and would have passed")
	}
	for _, n := range undeclared {
		t.Errorf("table %q declares no credential plane. Add `COMMENT ON TABLE %s IS 'plane: "+
			"actuation|triage|both';` to its migration.\n"+
			"Until it does, it is granted to BOTH planes by default — so if it records or authorises an "+
			"actuation, a compromised TRIAGE worker can forge that record and nothing fails.", n, n)
	}
	for _, m := range mismatched {
		t.Errorf("plane declaration contradicts the operative list — %s", m)
	}
}
