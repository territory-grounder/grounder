package db

// TG-383 — the drift check's own oracles.
//
// THE IRONY IS THE POINT: this file must not repeat the mistake it exists to fix. The finding is that
// every schema guard here runs against a fixture built from this repo's migrations, so none of them can
// see a table created by hand on the box. A purely in-memory test of DetectSchemaDrift would be exactly
// that mistake again — so the SQL is exercised against a real Postgres, and the part that can be pure
// (the declared-set parse) is tested for the failure modes that would make the check lie.

import (
	"context"
	"os"
	"strings"
	"testing"
)

// A FALSE "UNDECLARED" IS THE DANGEROUS FAILURE, not a missed one. If the parse misses a CREATE TABLE
// form the migrations actually use, every table of that form is reported as an unknown object and the
// operator learns to ignore the signal — which leaves the real one invisible again, one level up.
func TestTheDeclaredSetParsesTheFormsTheMigrationsActuallyUse(t *testing.T) {
	got := declaredTables()
	if len(got) == 0 {
		t.Fatal("parsed ZERO declared tables from the embedded migrations — every table in the running " +
			"database would be reported as undeclared, which is a check that cries wolf and gets muted")
	}
	// Tables this build certainly creates. Named individually rather than counted, because a count
	// passes while the parse silently drops one FORM (quoted, IF NOT EXISTS, schema-qualified).
	for _, want := range []string{"session_triage", "governance_ledger", "manifest_entry", "action_execution"} {
		if !got[want] {
			t.Errorf("declaredTables() does not contain %q, which this repo's migrations definitely create "+
				"— the CREATE TABLE parse is missing a form, so real tables will be reported as undeclared", want)
		}
	}
	// ...and it must not invent members. `policy_ruleset_bak_handsoff` is the hand-made table this whole
	// ticket is about: it must NOT appear as declared, or the check reports clean over the one object it
	// was built to surface.
	if got["policy_ruleset_bak_handsoff"] {
		t.Error("the hand-made backup table is being treated as DECLARED — the drift check would then " +
			"report clean over precisely the object TG-383 was filed about")
	}
}

// The parse must handle the shapes SQL actually appears in, or a migration written slightly differently
// silently shrinks the declared set.
func TestTheDeclaredParseHandlesQuotingAndIfNotExists(t *testing.T) {
	cases := map[string]string{
		"plain":            "CREATE TABLE foo_one (id int);",
		"if not exists":    "create table if not exists foo_two (id int);",
		"schema qualified": `CREATE TABLE public.foo_three (id int);`,
		"quoted":           `CREATE TABLE "foo_four" (id int);`,
		"newline split":    "CREATE\n  TABLE\n  foo_five (id int);",
	}
	for name, sql := range cases {
		t.Run(name, func(t *testing.T) {
			m := createTableRe.FindStringSubmatch(sql)
			if m == nil {
				t.Fatalf("no match for the %s form: %q — a migration written this way would leave its "+
					"tables looking undeclared forever", name, sql)
			}
			if !strings.HasPrefix(m[1], "foo_") {
				t.Errorf("captured %q, want the table name — a bad capture pollutes the declared set with "+
					"keywords and makes real drift invisible", m[1])
			}
		})
	}
}

// AN EMPTY RESULT MUST ANNOUNCE ITSELF. This is the vacuity floor: the whole finding is that a check
// reported clean over a population that excluded the interesting members, so "0 tables" must never render
// as a pass.
func TestZeroTablesReadsAsExaminedNothingNotAsClean(t *testing.T) {
	var d SchemaDrift
	s := d.String()
	if !strings.Contains(s, "examined nothing") {
		t.Errorf("a zero-table drift renders as %q — it must say it examined nothing, or an unmigrated "+
			"or unreachable database reads exactly like a verified-clean one", s)
	}
	if d.Clean() != true {
		t.Error("Clean() is false on an empty drift; the distinction this test pins is in the RENDERING, " +
			"and the message is what an operator reads")
	}
}

// The clean line must state the denominator. "All good" without "out of how many" is the shape that hid
// this finding for weeks.
func TestTheCleanLineStatesItsDenominator(t *testing.T) {
	d := SchemaDrift{Total: 61}
	s := d.String()
	if !strings.Contains(s, "61") {
		t.Errorf("the clean line %q omits the table count — a reader cannot tell a verified schema from a "+
			"check that looked at one table", s)
	}
}

// THE ONE THAT MATTERS: the SQL must be accepted by a real Postgres and must actually classify.
//
// DSN-gated rather than faked, for the reason this whole file exists: the defect being fixed is that a
// fixture cannot see the running schema. A Go-level fake would re-create that blind spot inside the fix.
func TestDetectSchemaDriftRunsAgainstARealPostgresAndClassifies(t *testing.T) {
	dsn := os.Getenv("TG_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("set TG_TEST_POSTGRES_DSN to a migrated database to run the schema-drift reader")
	}
	ctx := context.Background()
	if err := Migrate(ctx, dsn); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	p, err := Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	defer p.Close()

	base, err := DetectSchemaDrift(ctx, p)
	if err != nil {
		t.Fatalf("drift: %v", err)
	}
	if base.Total == 0 {
		t.Fatal("a migrated database reported ZERO tables — the catalog query is wrong, and every " +
			"assertion below would be vacuous")
	}

	// THE REAL CASE, REPRODUCED: a table created by hand that no migration knows about. This is the exact
	// route the fixture-based guards cannot reproduce, which is why it is done here against live DDL.
	const hand = "tg383_hand_made_probe"
	// DROP FIRST. `CREATE TABLE IF NOT EXISTS` is a silent no-op when a previous run left the probe
	// behind, and then `base` already counts it — which is exactly how this test failed on its first full
	// run (total 61 -> 61 "after adding one table"). A fixture-state-dependent oracle is the same class of
	// defect as the one under test, one level up.
	if _, err := p.Exec(ctx, "DROP TABLE IF EXISTS "+hand); err != nil {
		t.Fatalf("cannot drop the probe table (%v) — this fixture cannot support the assertion, and "+
			"skipping here would be a PERMANENT skip that no environment lifts", err)
	}
	base, err = DetectSchemaDrift(ctx, p) // re-measure AFTER the drop, so base is the true pre-state
	if err != nil {
		t.Fatalf("drift rebase: %v", err)
	}
	if _, err := p.Exec(ctx, "CREATE TABLE "+hand+" (id int)"); err != nil {
		t.Fatalf("cannot create the probe table (%v) — the DSN-gated fixture is expected to grant DDL; "+
			"a skip here would never be lifted by any environment", err)
	}
	t.Cleanup(func() { _, _ = p.Exec(ctx, "DROP TABLE IF EXISTS "+hand) })

	after, err := DetectSchemaDrift(ctx, p)
	if err != nil {
		t.Fatalf("drift after: %v", err)
	}
	var sawIt bool
	for _, n := range after.Undeclared {
		if n == hand {
			sawIt = true
		}
	}
	if !sawIt {
		t.Fatalf("a hand-made table was NOT reported as undeclared (undeclared=%v) — this is the precise "+
			"blind spot TG-383 names: the guard is green and the object is invisible", after.Undeclared)
	}
	if after.Clean() {
		t.Error("the drift reports Clean() with an undeclared table present")
	}
	if !strings.Contains(after.String(), hand) {
		t.Errorf("the boot line does not name the offending table: %q — an operator told 'drift detected' "+
			"with no name cannot act on it", after.String())
	}
	if after.Total != base.Total+1 {
		t.Errorf("total went %d -> %d after adding one table; the denominator must track the real schema",
			base.Total, after.Total)
	}

	// THE LOOKALIKE PROBE — and it exists because a mutation SURVIVED without it.
	//
	// Replacing the named exemption with `strings.HasSuffix(name, "_migrations")` at the call site passed
	// every test in this file: the map-shaped guards above assert on `runnerOwnedTables`, and a pattern
	// mutation never touches the map. The guard was testing the LIST, not the RULE, which is precisely the
	// defect class this repo keeps re-finding one level up.
	//
	// So this creates a table a plausible pattern WOULD swallow and asserts it is still reported.
	const lookalike = "tg383_fake_migrations"
	if _, err := p.Exec(ctx, "DROP TABLE IF EXISTS "+lookalike); err != nil {
		t.Fatalf("cannot drop the lookalike probe: %v", err)
	}
	if _, err := p.Exec(ctx, "CREATE TABLE "+lookalike+" (id int)"); err != nil {
		t.Fatalf("cannot create the lookalike probe: %v", err)
	}
	t.Cleanup(func() { _, _ = p.Exec(ctx, "DROP TABLE IF EXISTS "+lookalike) })

	look, err := DetectSchemaDrift(ctx, p)
	if err != nil {
		t.Fatalf("drift lookalike: %v", err)
	}
	var sawLookUndeclared, sawLookUnplaned bool
	for _, n := range look.Undeclared {
		if n == lookalike {
			sawLookUndeclared = true
		}
	}
	for _, n := range look.UnplaneD {
		if n == lookalike {
			sawLookUnplaned = true
		}
	}
	if !sawLookUndeclared {
		t.Errorf("%q was NOT reported as undeclared (undeclared=%v) — the exemption has become name-SHAPED "+
			"rather than a name, so any hand-made table ending in _migrations is now invisible",
			lookalike, look.Undeclared)
	}
	if !sawLookUnplaned {
		t.Errorf("%q was NOT reported as unplaned (unplaned=%v) — same defect on the plane half, which is "+
			"where the exemption is applied at the call site and where the surviving mutation landed",
			lookalike, look.UnplaneD)
	}
}

// THE EXEMPTION MUST STAY A HIDING PLACE FOR EXACTLY ONE TABLE.
//
// `runnerOwnedTables` suppresses a real security signal for its members, so its danger is growth: every
// name added is a table the drift check will never report again. It exists because migration 0060 already
// recorded the same decision for the same table, and because leaving it in put a permanent floor under a
// gauge whose whole purpose is to be alerted on at non-zero.
//
// KILLING MUTATION: add any second entry. RED.
func TestTheRunnerOwnedExemptionCoversExactlyOneNamedTable(t *testing.T) {
	if len(runnerOwnedTables) != 1 || !runnerOwnedTables["schema_migrations"] {
		t.Fatalf("runnerOwnedTables = %v, want exactly {schema_migrations}. Every name here is a table "+
			"the drift check will NEVER report again — the exemption is a hiding place by construction, so "+
			"it stays one table with a recorded reason (migration 0060) rather than a growing list.",
			runnerOwnedTables)
	}
}

// AND IT MUST NOT SWALLOW A LOOKALIKE. The reason this is a name and not a pattern is that a rule like
// "ends in _migrations" is how a genuinely undeclared table walks back in behind a plausible suffix.
func TestTheExemptionIsANameNotAPattern(t *testing.T) {
	for _, lookalike := range []string{"tg_schema_migrations", "schema_migrations_bak", "SCHEMA_MIGRATIONS_old"} {
		if runnerOwnedTables[strings.ToLower(lookalike)] {
			t.Errorf("%q is exempted — the exemption has become suffix- or prefix-shaped, which is exactly "+
				"how a hand-made table hides behind a plausible name", lookalike)
		}
	}
	// The exact name, case-folded the way DetectSchemaDrift folds it, must still hit.
	if !runnerOwnedTables[strings.ToLower("Schema_Migrations")] {
		t.Error("the exemption misses its own table under the case-folding DetectSchemaDrift applies, so " +
			"the permanent-floor problem would return on any database that cased it differently")
	}
}

// THE REAL FINDING MUST SURVIVE THE EXEMPTION. The whole point is that one table stops being reported
// and the OTHER one — the hand-made backup this ticket was filed about — still is.
func TestTheExemptionDoesNotHideTheHandMadeTable(t *testing.T) {
	declared := declaredTables()
	if declared["policy_ruleset_bak_handsoff"] {
		t.Fatal("the hand-made backup is being treated as declared — the exemption has swallowed the " +
			"exact object TG-383 was filed about, which would make the whole check report clean")
	}
	if !declared["schema_migrations"] {
		t.Error("schema_migrations is not exempted, so tg_schema_undeclared_tables keeps a permanent " +
			"floor and the non-zero alert TG-383 asks for cannot be wired")
	}
}
