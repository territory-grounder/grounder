package db

import (
	"context"
	"strings"
	"testing"
)

// TG-368. tg_apply_plane_grants() walked every table in `public` and took a REVOKE arm whenever the source
// role did not hold the privilege. The function is SECURITY INVOKER, so that REVOKE runs as the CALLER --
// and on any table the caller does not own it raises 42501 and rolls the whole derivation back.
//
// Production had exactly one such table (`policy_ruleset_bak_handsoff`, owner postgres, in no migration in
// this repo, the only one of 60 that tg_runtime cannot SELECT). One foreign table denied the database plane
// split to the entire schema, and the failure was invisible until an operator created the plane roles and
// read the grounder's boot log:
//
//	credential-plane DB roles: DERIVATION FAILED (... permission denied for table
//	policy_ruleset_bak_handsoff (SQLSTATE 42501)) -- any tg_triage/tg_actuate worker is running on
//	whatever privileges it already had, NOT the ones this build declares (TG-164)
//
// These oracles fix BOTH halves of the behaviour, because the naive fix (drop the REVOKE arm) makes the
// first test pass and silently destroys the convergence guarantee the arm exists for.

// planeGrantScratch builds the shape that reproduces the production failure: a non-superuser CALLER that
// owns one table, and a SECOND table it does not own and holds nothing on. Returns the caller, source,
// plane and owned-table names. Everything is dropped by t.Cleanup.
func planeGrantScratch(ctx context.Context, t *testing.T, p *Pool) (caller, source, plane, owned, foreign string) {
	t.Helper()
	caller, source, plane = "tg_gr_caller", "tg_gr_src", "tg_gr_plane"
	owned, foreign = "tg_gr_owned", "tg_gr_foreign"

	exec := func(sql string) {
		t.Helper()
		if _, err := p.Exec(ctx, sql); err != nil {
			t.Fatalf("scratch setup %q: %v", sql, err)
		}
	}
	// Idempotent teardown first: a previous failed run must not make this one fail for the wrong reason.
	drop := func() {
		for _, s := range []string{
			`DROP TABLE IF EXISTS ` + owned,
			`DROP TABLE IF EXISTS ` + foreign,
			`REASSIGN OWNED BY ` + caller + ` TO CURRENT_USER`,
			`DROP OWNED BY ` + caller,
			`DROP OWNED BY ` + source,
			`DROP OWNED BY ` + plane,
			`DROP ROLE IF EXISTS ` + caller,
			`DROP ROLE IF EXISTS ` + source,
			`DROP ROLE IF EXISTS ` + plane,
		} {
			_, _ = p.Exec(ctx, s)
		}
	}
	drop()
	t.Cleanup(drop)

	for _, r := range []string{caller, source, plane} {
		exec(`CREATE ROLE ` + r + ` NOSUPERUSER`)
	}
	exec(`GRANT USAGE ON SCHEMA public TO ` + caller + `, ` + source + `, ` + plane)

	// FAITHFULNESS. In production the caller is tg_migration, which OWNS the schema it created: measured on
	// dc1tg01, all 27 sequences in `public` are owned by tg_migration, so the function's blanket
	// `GRANT ... ON ALL SEQUENCES` succeeds there. A scratch caller that owns nothing fails on the first
	// TG-schema sequence instead of on the table under test, which would make this oracle report the
	// production defect for the wrong reason -- and would tempt a fix to the sequence path that production
	// does not need. Granting the caller membership in the schema owner reproduces tg_migration's real
	// authority; the alien table below is then the ONLY object it cannot act on, which is the actual
	// production shape.
	// Granted as a privilege rather than by role membership on purpose: making the caller a MEMBER of the
	// schema owner would, in this fixture, make it a member of a superuser role, and the alien table below
	// would stop being alien -- the vacuity check would then (correctly) reject the whole oracle.
	exec(`GRANT ALL ON ALL SEQUENCES IN SCHEMA public TO ` + caller + ` WITH GRANT OPTION`)

	// The table the caller owns and the source role can write: this is what the derivation must GRANT.
	exec(`CREATE TABLE ` + owned + ` (id bigserial PRIMARY KEY, v text)`)
	exec(`ALTER TABLE ` + owned + ` OWNER TO ` + caller)
	exec(`GRANT SELECT, INSERT, UPDATE, DELETE ON ` + owned + ` TO ` + source)

	// The foreign table: owned by the superuser running this test, NOT by the caller, and the source role
	// holds nothing on it. This is `policy_ruleset_bak_handsoff` in miniature.
	//
	// Deliberately a plain bigint, not a bigserial: an owned sequence would be alien too, and production's
	// alien object contributes none (all 27 sequences in `public` on dc1tg01 are owned by tg_migration).
	// Giving it one would test a shape production does not have.
	exec(`CREATE TABLE ` + foreign + ` (id bigint PRIMARY KEY)`)
	exec(`REVOKE ALL ON ` + foreign + ` FROM PUBLIC`)
	exec(`REVOKE ALL ON ` + foreign + ` FROM ` + source)
	exec(`REVOKE ALL ON ` + foreign + ` FROM ` + caller)

	// Prove the precondition rather than assuming it: if the caller could revoke here, the whole oracle
	// would be vacuous and would pass against the unfixed function.
	var canRevoke bool
	if err := p.QueryRow(ctx,
		`SELECT pg_has_role($1, (SELECT relowner FROM pg_class WHERE relname = $2), 'USAGE')`,
		caller, foreign).Scan(&canRevoke); err != nil {
		t.Fatalf("probing caller ownership of the foreign table: %v", err)
	}
	if canRevoke {
		t.Fatalf("VACUOUS ORACLE: %s is a member of the owner of %s, so REVOKE would succeed and the "+
			"defect under test cannot occur", caller, foreign)
	}
	return
}

// deriveAs runs the derivation as the non-owner caller, the way the grounder does (SECURITY INVOKER).
func deriveAs(ctx context.Context, t *testing.T, p *Pool, caller, plane, source string, withhold []string) (int, error) {
	t.Helper()
	conn, err := p.Pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	defer conn.Release()
	if _, err := conn.Exec(ctx, `SET ROLE `+caller); err != nil {
		t.Fatalf("SET ROLE %s: %v", caller, err)
	}
	defer conn.Exec(ctx, `RESET ROLE`)

	var granted int
	err = conn.QueryRow(ctx, `SELECT tg_apply_plane_grants($1, $2, $3)`, plane, source, withhold).Scan(&granted)
	return granted, err
}

// TestDerivationSurvivesATableTheCallerCannotRevokeOn is the production failure, reproduced.
func TestDerivationSurvivesATableTheCallerCannotRevokeOn(t *testing.T) {
	ctx, p, _ := planeRoleFixture(t)
	caller, source, plane, owned, foreign := planeGrantScratch(ctx, t, p)

	granted, err := deriveAs(ctx, t, p, caller, plane, source, []string{})
	if err != nil {
		if strings.Contains(err.Error(), "42501") || strings.Contains(err.Error(), "permission denied") {
			t.Fatalf("TG-368 REGRESSION: the derivation aborted on a table the caller cannot REVOKE on "+
				"(%s, owned by someone else, source role holds nothing). One foreign table must not deny the "+
				"plane split to the whole schema.\n%v", foreign, err)
		}
		t.Fatalf("derivation failed: %v", err)
	}
	if granted <= 0 {
		t.Fatalf("derivation reported %d granted privileges — it must have mirrored the source role's rights "+
			"on %s; a zero here would let ApplyPlaneGrants' floor refuse a role that can read and not write",
			granted, owned)
	}

	// It must have granted on the table it COULD act on, not merely survived.
	var canWrite bool
	if err := p.QueryRow(ctx, `SELECT has_table_privilege($1, $2, 'INSERT')`, plane, owned).Scan(&canWrite); err != nil {
		t.Fatalf("probe: %v", err)
	}
	if !canWrite {
		t.Fatalf("the derivation survived but granted nothing: %s cannot INSERT on %s, which %s can. "+
			"Surviving by doing nothing is the failure this test exists to catch.", plane, owned, source)
	}
}

// TestDerivationStillConvergesDownward pins the property the REVOKE arm exists for. The cheap fix for the
// test above -- delete the REVOKE arm -- passes it and silently turns the withheld-table lists into
// documentation of a control that is not in force.
func TestDerivationStillConvergesDownward(t *testing.T) {
	ctx, p, _ := planeRoleFixture(t)
	caller, source, plane, owned, _ := planeGrantScratch(ctx, t, p)

	// Round 1: nothing withheld, so the plane role earns the write.
	if _, err := deriveAs(ctx, t, p, caller, plane, source, []string{}); err != nil {
		t.Fatalf("first derivation: %v", err)
	}
	var before bool
	if err := p.QueryRow(ctx, `SELECT has_table_privilege($1, $2, 'INSERT')`, plane, owned).Scan(&before); err != nil {
		t.Fatalf("probe: %v", err)
	}
	if !before {
		t.Fatalf("precondition failed: %s should hold INSERT on %s after an unwithheld derivation", plane, owned)
	}

	// Round 2: the table moves ONTO the withheld list, exactly as adding a new actuation-record table to
	// ActuationAuthorityTables would do. The privilege must be taken away.
	if _, err := deriveAs(ctx, t, p, caller, plane, source, []string{owned}); err != nil {
		t.Fatalf("second derivation: %v", err)
	}
	var after bool
	if err := p.QueryRow(ctx, `SELECT has_table_privilege($1, $2, 'INSERT')`, plane, owned).Scan(&after); err != nil {
		t.Fatalf("probe: %v", err)
	}
	if after {
		t.Fatalf("CONVERGENCE LOST: %s still holds INSERT on %s after it was withheld. The REVOKE arm must "+
			"still fire where the role actually HAS the privilege — skipping it entirely makes the withheld "+
			"lists describe a control that is not applied.", plane, owned)
	}

	// SELECT is never withheld: both planes read each other's records.
	var canRead bool
	if err := p.QueryRow(ctx, `SELECT has_table_privilege($1, $2, 'SELECT')`, plane, owned).Scan(&canRead); err != nil {
		t.Fatalf("probe: %v", err)
	}
	if !canRead {
		t.Fatalf("withholding writes on %s also removed SELECT from %s — reads are never withheld, or "+
			"verification breaks rather than hardens", owned, plane)
	}
}

// TestNullWithholdListIsNotAnEmptyOne pins the second defect, which the oracle above found by accident: a
// nil Go slice reaches SQL as NULL, `relname = ANY (NULL)` is NULL rather than false, and the IF treats
// NULL as false — so every INSERT/UPDATE/DELETE is skipped and the plane role comes out READ-ONLY.
//
// The reason this needs its own oracle rather than a comment: ApplyPlaneGrants' floor refuses `granted <= 0`,
// and this failure returns a healthy-looking positive count because the SELECTs are still granted. The
// deployment would report a successful derivation and the split worker would die inside an activity.
func TestNullWithholdListIsNotAnEmptyOne(t *testing.T) {
	ctx, p, _ := planeRoleFixture(t)
	caller, source, plane, owned, _ := planeGrantScratch(ctx, t, p)

	granted, err := deriveAs(ctx, t, p, caller, plane, source, nil) // nil -> SQL NULL, deliberately
	if err != nil {
		t.Fatalf("derivation with a NULL withhold list: %v", err)
	}
	var canWrite, canRead bool
	if err := p.QueryRow(ctx, `SELECT has_table_privilege($1,$2,'INSERT'), has_table_privilege($1,$2,'SELECT')`,
		plane, owned).Scan(&canWrite, &canRead); err != nil {
		t.Fatalf("probe: %v", err)
	}
	if canRead && !canWrite {
		t.Fatalf("A NULL withhold list produced a READ-ONLY %s (granted=%d, SELECT=%v INSERT=%v on %s). "+
			"`relname = ANY (NULL)` is NULL, not false, so every write privilege is skipped — and the "+
			"positive grant count means ApplyPlaneGrants' floor scores this as success. COALESCE the array.",
			plane, granted, canRead, canWrite, owned)
	}
	if !canWrite {
		t.Fatalf("%s cannot INSERT on %s after an unwithheld derivation (granted=%d)", plane, owned, granted)
	}
}
