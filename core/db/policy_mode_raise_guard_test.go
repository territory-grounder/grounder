package db

import (
	"context"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// TG-661 — THE DIRECTION OF A MODE WRITE IS A DATABASE INVARIANT (deep-review F2 / pack item 11), proven against
// a REAL PostgreSQL with the real roles: migration 0135's triggers let every runtime role LOWER the autonomy mode
// and TRIP the safety breaker, and refuse a RAISE to an actuating mode or a breaker RE-ARM from any role that is
// not tg_mode_admin / tg_migration / a superuser. The tests run as the fixture superuser and SET LOCAL ROLE
// inside a transaction that is always rolled back, so the fixture's mode and breaker rows are never changed.
//
// Gated on TG_TEST_POSTGRES_DSN (the harness job's empty, self-migrated database — the same fixture the TG-80
// grant audit runs on, so `make test` runs these wherever it runs that). A pgx fake could not prove any of this:
// the subject IS the trigger, the role membership and the SQLSTATE.
//
// KILLING MUTATIONS (executed 2026-11-07, recorded on TG-661): `DROP TRIGGER policy_mode_raise_guard ON
// policy_mode` in the fixture turned TestPolicyModeRaiseGuardRefusesEveryRuntimeRole RED (tg_runtime raised to
// Full-auto); `DROP TRIGGER breaker_reset_guard ON mutation_breaker_state` turned
// TestBreakerResetGuardRefusesEveryRuntimeRole RED (tg_runtime closed the tripped breaker; after the review's
// two bypasses were closed the same test also covers open -> half_open and a rename of the row).

const tg661SQLInsufficientPrivilege = "42501"

func tg661Pool(t *testing.T) (*Pool, context.Context) {
	t.Helper()
	dsn := os.Getenv("TG_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("set TG_TEST_POSTGRES_DSN to a migratable database to run the TG-661 raise-guard tests")
	}
	ctx := context.Background()
	if err := Migrate(ctx, dsn); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	p, err := Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	t.Cleanup(p.Close)
	return p, ctx
}

// tg661RuntimeRoles are the roles the deployment's planes connect as THAT HOLD UPDATE on policy_mode — the
// population the guard bounds (a role without the grant is refused by the grant itself). The fixture creates at
// least tg_runtime with the 0105 grant-back; in production tg_triage and tg_actuate hold the same grants.
func tg661RuntimeRoles(t *testing.T, ctx context.Context, p *Pool) []string {
	t.Helper()
	rows, err := p.Query(ctx, `SELECT rolname FROM pg_roles WHERE rolname IN ('tg_runtime','tg_triage','tg_actuate')
		AND has_table_privilege(rolname, 'policy_mode', 'UPDATE') AND has_table_privilege(rolname, 'mutation_breaker_state', 'UPDATE') ORDER BY 1`)
	if err != nil {
		t.Fatalf("roles: %v", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var r string
		if err := rows.Scan(&r); err != nil {
			t.Fatalf("scan: %v", err)
		}
		out = append(out, r)
	}
	if len(out) == 0 {
		t.Fatal("the fixture has none of the runtime roles — this is not the harness database")
	}
	return out
}

// tg661AsRole runs stmt inside a transaction as role and ROLLS BACK, returning the statement's error (nil = admitted).
// The role is given SELECT on the two tables for the transaction — production's runtime roles hold `arw` on both
// (measured live 2026-11-07), the CI fixture's 0105 grant-back leaves only `w`; the GRANT is transactional and
// rolled back with everything else, and the guard's subject (the UPDATE direction) is untouched by it.
func tg661AsRole(ctx context.Context, p *Pool, role, setup, stmt string) error {
	tx, err := p.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if setup != "" {
		if _, err := tx.Exec(ctx, setup); err != nil {
			return fmt.Errorf("setup: %w", err)
		}
	}
	if _, err := tx.Exec(ctx, "GRANT SELECT ON policy_mode, mutation_breaker_state TO "+pgx.Identifier{role}.Sanitize()); err != nil {
		return fmt.Errorf("grant select to %s: %w", role, err)
	}
	if _, err := tx.Exec(ctx, "SET LOCAL ROLE "+pgx.Identifier{role}.Sanitize()); err != nil {
		return fmt.Errorf("set role %s: %w", role, err)
	}
	_, err = tx.Exec(ctx, stmt)
	return err
}

func tg661IsInsufficientPrivilege(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == tg661SQLInsufficientPrivilege
}

// the fixture's current mode row is untouched: every statement runs under a rolled-back transaction, but the
// guard needs an OLD row to compare against, so the setup inserts/pins one INSIDE the transaction as superuser.
const tg661PinShadow = `INSERT INTO policy_mode (singleton, mode) VALUES (true, 'Shadow')
	ON CONFLICT (singleton) DO UPDATE SET mode = 'Shadow', updated_at = now()`

func TestPolicyModeRaiseGuardIsInstalled(t *testing.T) {
	p, ctx := tg661Pool(t)
	var n int
	if err := p.QueryRow(ctx, `SELECT count(*) FROM pg_trigger WHERE tgrelid = 'policy_mode'::regclass AND tgname = 'policy_mode_raise_guard' AND tgenabled <> 'D'`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("policy_mode_raise_guard must be installed and enabled: n=%d err=%v", n, err)
	}
	if err := p.QueryRow(ctx, `SELECT count(*) FROM pg_trigger WHERE tgrelid = 'mutation_breaker_state'::regclass AND tgname = 'breaker_reset_guard' AND tgenabled <> 'D'`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("breaker_reset_guard must be installed and enabled: n=%d err=%v", n, err)
	}
}

// THE GUARD: a runtime role may lower, may not raise.
func TestPolicyModeRaiseGuardRefusesEveryRuntimeRole(t *testing.T) {
	p, ctx := tg661Pool(t)
	for _, role := range tg661RuntimeRoles(t, ctx, p) {
		for _, to := range []string{"Semi-auto", "Full-auto"} {
			err := tg661AsRole(ctx, p, role, tg661PinShadow, fmt.Sprintf(`UPDATE policy_mode SET mode = '%s', updated_at = now() WHERE singleton`, to))
			if !tg661IsInsufficientPrivilege(err) {
				t.Fatalf("%s raising Shadow -> %s must fail with SQLSTATE %s, got %v", role, to, tg661SQLInsufficientPrivilege, err)
			}
		}
		// the kill path: lowering from an actuating mode is open to every plane, with no credential
		if err := tg661AsRole(ctx, p, role, `INSERT INTO policy_mode (singleton, mode) VALUES (true, 'Full-auto')
			ON CONFLICT (singleton) DO UPDATE SET mode = 'Full-auto', updated_at = now()`,
			`UPDATE policy_mode SET mode = 'Shadow', updated_at = now() WHERE singleton`); err != nil {
			t.Fatalf("%s must be able to LOWER Full-auto -> Shadow (the safety kill): %v", role, err)
		}
		if err := tg661AsRole(ctx, p, role, `INSERT INTO policy_mode (singleton, mode) VALUES (true, 'Full-auto')
			ON CONFLICT (singleton) DO UPDATE SET mode = 'Full-auto', updated_at = now()`,
			`UPDATE policy_mode SET mode = 'HITL', updated_at = now() WHERE singleton`); err != nil {
			t.Fatalf("%s must be able to lower to HITL: %v", role, err)
		}
		// the 10 s resync re-persist: a same-mode rewrite is a no-op, not a raise
		if err := tg661AsRole(ctx, p, role, `INSERT INTO policy_mode (singleton, mode) VALUES (true, 'Semi-auto')
			ON CONFLICT (singleton) DO UPDATE SET mode = 'Semi-auto', updated_at = now()`,
			`UPDATE policy_mode SET mode = 'Semi-auto', updated_at = now() WHERE singleton`); err != nil {
			t.Fatalf("%s rewriting Semi-auto -> Semi-auto must be admitted (resync): %v", role, err)
		}
		// an INSERT of an actuating mode into an empty table is a raise too
		if err := tg661AsRole(ctx, p, role, `DELETE FROM policy_mode`, `INSERT INTO policy_mode (singleton, mode) VALUES (true, 'Semi-auto')`); !tg661IsInsufficientPrivilege(err) {
			t.Fatalf("%s inserting Semi-auto into an empty policy_mode must be refused, got %v", role, err)
		}
	}
}

// THE ADMIT PATH: a member of tg_mode_admin raises; tg_migration raises (it is the migration identity).
func TestPolicyModeRaiseGuardAdmitsTheModeAdminRole(t *testing.T) {
	p, ctx := tg661Pool(t)
	member := tg661ModeAdminMember(t, ctx, p)
	for _, to := range []string{"Semi-auto", "Full-auto"} {
		if err := tg661AsRole(ctx, p, member, tg661PinShadow, fmt.Sprintf(`UPDATE policy_mode SET mode = '%s', updated_at = now() WHERE singleton`, to)); err != nil {
			t.Fatalf("a tg_mode_admin member must raise Shadow -> %s: %v", to, err)
		}
	}
	// the migration identity raises too (the fixture may lack the role: CI creates only tg_runtime — provision it
	// for the assertion and remove it again)
	var hasMigration bool
	if err := p.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'tg_migration')`).Scan(&hasMigration); err != nil {
		t.Fatalf("role probe: %v", err)
	}
	if !hasMigration {
		if _, err := p.Exec(ctx, `CREATE ROLE tg_migration NOLOGIN`); err != nil {
			t.Fatalf("create tg_migration: %v", err)
		}
		if _, err := p.Exec(ctx, `GRANT USAGE ON SCHEMA public TO tg_migration`); err != nil {
			t.Fatalf("grant schema to tg_migration: %v", err)
		}
		if _, err := p.Exec(ctx, `GRANT SELECT, INSERT, UPDATE ON policy_mode TO tg_migration`); err != nil {
			t.Fatalf("grant tg_migration: %v", err)
		}
		t.Cleanup(func() {
			_, _ = p.Exec(ctx, `DROP OWNED BY tg_migration`)
			_, _ = p.Exec(ctx, `DROP ROLE IF EXISTS tg_migration`)
		})
	}
	if err := tg661AsRole(ctx, p, "tg_migration", tg661PinShadow, `UPDATE policy_mode SET mode = 'Semi-auto', updated_at = now() WHERE singleton`); err != nil {
		t.Fatalf("tg_migration must raise: %v", err)
	}
}

// THE BREAKER: any plane trips it and accrues failures; closing a tripped breaker or zeroing its failures needs the
// admin role. Other breakers in the table (model-*, judge-death) recover under the runtime role, untouched.
func TestBreakerResetGuardRefusesEveryRuntimeRole(t *testing.T) {
	p, ctx := tg661Pool(t)
	tripped := `INSERT INTO mutation_breaker_state (name, state, failure_count, opened_at, last_transition_at)
		VALUES ('mutation', 'open', 3, now(), now())
		ON CONFLICT (name) DO UPDATE SET state = 'open', failure_count = 3, opened_at = now(), last_transition_at = now()`
	closed := `INSERT INTO mutation_breaker_state (name, state, failure_count)
		VALUES ('mutation', 'closed', 1)
		ON CONFLICT (name) DO UPDATE SET state = 'closed', failure_count = 1, opened_at = NULL`
	for _, role := range tg661RuntimeRoles(t, ctx, p) {
		if err := tg661AsRole(ctx, p, role, closed, `UPDATE mutation_breaker_state SET state = 'open', opened_at = now(), last_transition_at = now() WHERE name = 'mutation'`); err != nil {
			t.Fatalf("%s must be able to TRIP the safety breaker: %v", role, err)
		}
		if err := tg661AsRole(ctx, p, role, closed, `UPDATE mutation_breaker_state SET failure_count = failure_count + 1 WHERE name = 'mutation'`); err != nil {
			t.Fatalf("%s must be able to accrue a failure: %v", role, err)
		}
		if err := tg661AsRole(ctx, p, role, tripped, `UPDATE mutation_breaker_state SET state = 'closed', failure_count = 0, opened_at = NULL WHERE name = 'mutation'`); !tg661IsInsufficientPrivilege(err) {
			t.Fatalf("%s re-arming the tripped safety breaker must fail with SQLSTATE %s, got %v", role, tg661SQLInsufficientPrivilege, err)
		}
		if err := tg661AsRole(ctx, p, role, closed, `UPDATE mutation_breaker_state SET failure_count = 0 WHERE name = 'mutation'`); !tg661IsInsufficientPrivilege(err) {
			t.Fatalf("%s zeroing the safety breaker's failures must be refused, got %v", role, err)
		}
		// the review's two bypasses: open -> half_open reads as "not tripped" at gate 4e, and a RENAME would move
		// the row out from under the WHEN filter — both are re-arms and both need the admin role
		if err := tg661AsRole(ctx, p, role, tripped, `UPDATE mutation_breaker_state SET state = 'half_open', last_transition_at = now() WHERE name = 'mutation'`); !tg661IsInsufficientPrivilege(err) {
			t.Fatalf("%s moving the tripped safety breaker open -> half_open must be refused (it reads as not tripped), got %v", role, err)
		}
		if err := tg661AsRole(ctx, p, role, tripped, `UPDATE mutation_breaker_state SET name = 'mutation-parked' WHERE name = 'mutation'`); !tg661IsInsufficientPrivilege(err) {
			t.Fatalf("%s renaming the safety breaker row away must be refused, got %v", role, err)
		}
		if err := tg661AsRole(ctx, p, role, `INSERT INTO mutation_breaker_state (name, state, failure_count) VALUES ('model-tg661-rename', 'closed', 0)
			ON CONFLICT (name) DO UPDATE SET state = 'closed', failure_count = 0`+"; DELETE FROM mutation_breaker_state WHERE name = 'mutation'",
			`UPDATE mutation_breaker_state SET name = 'mutation' WHERE name = 'model-tg661-rename'`); !tg661IsInsufficientPrivilege(err) {
			t.Fatalf("%s renaming another row INTO 'mutation' must be refused, got %v", role, err)
		}
		// a half-open probe that fails trips again — that direction stays open
		if err := tg661AsRole(ctx, p, role, `INSERT INTO mutation_breaker_state (name, state, failure_count) VALUES ('mutation', 'half_open', 3)
			ON CONFLICT (name) DO UPDATE SET state = 'half_open', failure_count = 3`,
			`UPDATE mutation_breaker_state SET state = 'open', opened_at = now() WHERE name = 'mutation'`); err != nil {
			t.Fatalf("%s must be able to re-trip a half-open safety breaker: %v", role, err)
		}
		// the preconditions the guard rests on: no runtime role may DELETE the row (so it cannot vanish and be
		// re-inserted closed)
		var canDelete bool
		if err := p.QueryRow(ctx, `SELECT has_table_privilege($1, 'mutation_breaker_state', 'DELETE') OR has_table_privilege($1, 'policy_mode', 'DELETE')`, role).Scan(&canDelete); err != nil || canDelete {
			t.Fatalf("%s must hold no DELETE on policy_mode / mutation_breaker_state: can=%v err=%v", role, canDelete, err)
		}
		// a model breaker closes under the runtime role — the guard is scoped to the safety breaker
		if err := tg661AsRole(ctx, p, role, `INSERT INTO mutation_breaker_state (name, state, failure_count) VALUES ('model-tg661-probe', 'open', 2)
			ON CONFLICT (name) DO UPDATE SET state = 'open', failure_count = 2`,
			`UPDATE mutation_breaker_state SET state = 'closed', failure_count = 0 WHERE name = 'model-tg661-probe'`); err != nil {
			t.Fatalf("%s must still recover a MODEL breaker: %v", role, err)
		}
	}
	member := tg661ModeAdminMember(t, ctx, p)
	if err := tg661AsRole(ctx, p, member, tripped, `UPDATE mutation_breaker_state SET state = 'closed', failure_count = 0, opened_at = NULL WHERE name = 'mutation'`); err != nil {
		t.Fatalf("a tg_mode_admin member must re-arm the safety breaker: %v", err)
	}
	if err := tg661AsRole(ctx, p, member, tripped, `UPDATE mutation_breaker_state SET state = 'half_open' WHERE name = 'mutation'`); err != nil {
		t.Fatalf("a tg_mode_admin member may half-open the safety breaker: %v", err)
	}
}

// tg661ModeAdminMember provisions, for this test only, the NOLOGIN group role tg_mode_admin (if the fixture lacks
// it, as CI's does) with the grants migration 0135 gives it, plus a throwaway member role — and removes both at
// cleanup so the fixture is left as it was (the TG-80 grant audit keys on tg_runtime, which is untouched).
func tg661ModeAdminMember(t *testing.T, ctx context.Context, p *Pool) string {
	t.Helper()
	member := fmt.Sprintf("tg661_member_%d", time.Now().UnixNano())
	var created bool
	if err := p.QueryRow(ctx, `SELECT NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'tg_mode_admin')`).Scan(&created); err != nil {
		t.Fatalf("role probe: %v", err)
	}
	stmts := []string{}
	if created {
		stmts = append(stmts, `CREATE ROLE tg_mode_admin NOLOGIN`,
			`GRANT USAGE ON SCHEMA public TO tg_mode_admin`,
			`GRANT SELECT, INSERT, UPDATE ON policy_mode TO tg_mode_admin`,
			`GRANT SELECT, INSERT, UPDATE ON mutation_breaker_state TO tg_mode_admin`)
	}
	stmts = append(stmts, `CREATE ROLE `+pgx.Identifier{member}.Sanitize()+` NOLOGIN`, `GRANT tg_mode_admin TO `+pgx.Identifier{member}.Sanitize())
	for _, s := range stmts {
		if _, err := p.Exec(ctx, s); err != nil {
			t.Fatalf("%s: %v", s, err)
		}
	}
	t.Cleanup(func() {
		_, _ = p.Exec(ctx, `DROP ROLE IF EXISTS `+pgx.Identifier{member}.Sanitize())
		if created {
			_, _ = p.Exec(ctx, `DROP OWNED BY tg_mode_admin`)
			_, _ = p.Exec(ctx, `DROP ROLE IF EXISTS tg_mode_admin`)
		}
	})
	return member
}
