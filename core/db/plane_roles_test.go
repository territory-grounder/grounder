package db

// ORACLES FOR THE DATABASE HALF OF THE CREDENTIAL-PLANE SPLIT (TG-164).
//
// These run against a REAL PostgreSQL and nothing else can substitute: the property under test is a Postgres
// GRANT, so a fake would be asserting that this file agrees with itself. They are gated on
// TG_TEST_POSTGRES_DSN and — this is the part that has bitten this repository before — a skip is INVISIBLE
// without -v (see dsn_gate_test.go). Without the DSN these prove exactly nothing.
//
// THE FIXTURE BUILDS ITS OWN DATABASE, and that is load-bearing rather than fastidious. The privilege posture
// under test is DERIVED from tg_runtime's, and tg_runtime's posture is itself built by the interaction of
// deploy/postgres-init/00-roles.sh (ALTER DEFAULT PRIVILEGES, applied at CREATE TABLE time) with fourteen
// migrations that REVOKE UPDATE/DELETE afterwards. Reusing a shared fixture database — whose tables were
// created by the superuser with no default privileges — would leave tg_runtime holding nothing, every
// derived grant empty, and every "permission denied" below passing for the wrong reason. So the fixture
// reproduces the deploy order exactly: create the roles, install the default privileges, THEN migrate.
//
// WHAT EACH ORACLE IS FOR:
//   - TestTriageRoleCannotForgeTheRecordOfAnActuation — the security property. RED is the finding.
//   - TestTriageRoleCanStillDoEverythingTriageNeeds  — the CONTROL. Without it the property above is
//     satisfied by a role with no privileges at all, i.e. by an outage.
//   - TestActuationRoleCannotForgeTheEvidenceItActsOn — the symmetric half.
//   - TestUnsplitRuntimeRoleIsUnchanged — `both` on tg_runtime must be byte-identical after this change.
//   - TestPlaneGrantsMutationControl — EXECUTES the killing mutation and asserts the oracle goes RED.

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// planeFixtureDSN is the DSN of the purpose-built fixture database, resolved once per test binary.
var (
	planeFixtureOnce sync.Once
	planeFixtureDSN  string
	planeFixtureErr  error
)

// planeRoleFixture builds (once) a database whose privilege posture matches a real deployment's, applies the
// plane grants, and returns a superuser pool over it plus the DSN.
func planeRoleFixture(t *testing.T) (context.Context, *Pool, string) {
	t.Helper()
	base := os.Getenv("TG_TEST_POSTGRES_DSN")
	if base == "" {
		t.Skip("set TG_TEST_POSTGRES_DSN to a Postgres superuser DSN to run the credential-plane role oracles " +
			"(they CREATE a dedicated database; a shared one cannot reproduce the deploy's default-privilege order)")
	}
	ctx := context.Background()
	planeFixtureOnce.Do(func() { planeFixtureDSN, planeFixtureErr = buildPlaneFixture(ctx, base) })
	if planeFixtureErr != nil {
		t.Fatalf("plane-role fixture: %v", planeFixtureErr)
	}
	p, err := Connect(ctx, planeFixtureDSN)
	if err != nil {
		t.Fatalf("plane-role fixture: connect: %v", err)
	}
	t.Cleanup(p.Close)
	return ctx, p, planeFixtureDSN
}

// planeFixtureDB is the fixture database name. Fixed rather than random so a failed run leaves ONE inspectable
// database behind instead of accumulating them, and so a rerun starts from a known state.
const planeFixtureDB = "tg_plane_roles_fixture"

// buildPlaneFixture reproduces a real deployment's privilege construction order, which is the only order in
// which the derived grants mean anything:
//
//	00-roles.sh:  CREATE ROLE tg_runtime; GRANT USAGE ON SCHEMA; ALTER DEFAULT PRIVILEGES ... TO tg_runtime
//	Migrate():    CREATE TABLE (picks up the default privileges) ... then REVOKE UPDATE,DELETE on the
//	              append-only tables (0015/0018/0019/0020/0022/0029/...)
//	ApplyPlaneGrants(): derive tg_triage / tg_actuate FROM whatever tg_runtime ended up holding
//
// Doing this in any other order silently produces an empty tg_runtime and a vacuous suite.
func buildPlaneFixture(ctx context.Context, baseDSN string) (string, error) {
	adminDSN, err := withDatabase(baseDSN, "postgres")
	if err != nil {
		return "", err
	}
	admin, err := pgxpool.New(ctx, adminDSN)
	if err != nil {
		return "", fmt.Errorf("connect maintenance db: %w", err)
	}
	defer admin.Close()

	// Roles are CLUSTER-wide, so create them on the maintenance connection and leave them alone afterwards.
	// NOLOGIN is deliberate: every assertion below reaches them through SET ROLE, and a fixture that minted
	// password-bearing login roles on a shared box would be a worse thing than the test it enables.
	for _, role := range []string{PlaneRoleRuntime, PlaneRoleTriage, PlaneRoleActuate} {
		if _, err := admin.Exec(ctx,
			"DO $do$ BEGIN CREATE ROLE "+pgIdent(role)+" NOLOGIN; EXCEPTION WHEN duplicate_object THEN NULL; END $do$;"); err != nil {
			return "", fmt.Errorf("create role %s: %w", role, err)
		}
	}
	if _, err := admin.Exec(ctx, "DROP DATABASE IF EXISTS "+pgIdent(planeFixtureDB)+" WITH (FORCE)"); err != nil {
		return "", fmt.Errorf("drop fixture db: %w", err)
	}
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+pgIdent(planeFixtureDB)); err != nil {
		return "", fmt.Errorf("create fixture db: %w", err)
	}

	dsn, err := withDatabase(baseDSN, planeFixtureDB)
	if err != nil {
		return "", err
	}
	setup, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return "", fmt.Errorf("connect fixture db: %w", err)
	}
	// pgvector: migration 0013 needs it and deploy/postgres-init/00-roles.sh installs it as the superuser for
	// exactly the reason it does in production — CREATE EXTENSION needs privileges the migration role lacks.
	for _, stmt := range []string{
		"CREATE EXTENSION IF NOT EXISTS vector",
		"GRANT USAGE ON SCHEMA public TO " + pgIdent(PlaneRoleRuntime),
		"ALTER DEFAULT PRIVILEGES IN SCHEMA public GRANT SELECT, INSERT, UPDATE, DELETE ON TABLES TO " + pgIdent(PlaneRoleRuntime),
		"ALTER DEFAULT PRIVILEGES IN SCHEMA public GRANT USAGE, SELECT ON SEQUENCES TO " + pgIdent(PlaneRoleRuntime),
	} {
		if _, err := setup.Exec(ctx, stmt); err != nil {
			setup.Close()
			return "", fmt.Errorf("fixture setup %q: %w", stmt, err)
		}
	}
	setup.Close()

	if err := Migrate(ctx, dsn); err != nil {
		return "", fmt.Errorf("migrate fixture: %w", err)
	}
	rep, err := ApplyPlaneGrants(ctx, dsn)
	if err != nil {
		return "", fmt.Errorf("apply plane grants: %w", err)
	}
	if !rep.Applied() {
		return "", fmt.Errorf("apply plane grants reported nothing applied (%s) — the roles were created above, "+
			"so every assertion in this file would be measuring a database the function never touched", rep)
	}
	return dsn, nil
}

// withDatabase rewrites a DSN's database name. Used to reach the maintenance database (to CREATE the fixture)
// and the fixture itself.
func withDatabase(dsn, name string) (string, error) {
	u, err := url.Parse(dsn)
	if err != nil {
		return "", fmt.Errorf("parse TG_TEST_POSTGRES_DSN: %w", err)
	}
	u.Path = "/" + name
	return u.String(), nil
}

// pgIdent double-quotes a fixture-internal identifier. Every value it is given in this file is a compile-time
// constant from this package, never operator or row data; the runtime grant DDL is quoted by format(%I)
// inside migration 0059, which is why no production Go path builds a name.
func pgIdent(s string) string { return `"` + strings.ReplaceAll(s, `"`, `""`) + `"` }

// asRole takes ONE session and pins it to role. Through the pool the statements could land on different
// connections and SET ROLE would not be in effect for the one under test — which would make every refusal
// below a superuser's success and every assertion a lie.
func asRole(t *testing.T, ctx context.Context, p *Pool, role string) *pgxpool.Conn {
	t.Helper()
	conn, err := p.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	t.Cleanup(conn.Release)
	if _, err := conn.Exec(ctx, "SET ROLE "+pgIdent(role)); err != nil {
		t.Fatalf("SET ROLE %s: %v — without the role this oracle asserts the SUPERUSER's privileges and proves nothing", role, err)
	}
	return conn
}

// denied fails unless err is Postgres' insufficient_privilege (42501). The SQLSTATE, not the message: a
// constraint violation, a missing column or a typo'd table name would all "fail" too, and a privilege oracle
// that accepts any error is an oracle that passes when the grant is wide open and the INSERT is malformed.
// It delegates to assertPermissionDenied (agent_step_evidence_reap_test.go) so both privilege suites in this
// package judge a refusal by the same rule.
func denied(t *testing.T, err error, what string) {
	t.Helper()
	assertPermissionDenied(t, err, what)
}

// forgeryAttempts are the writes a compromised TRIAGE worker would make to manufacture the record of an
// actuation it could not perform. Each is a REAL statement against the REAL column list, not a
// has_table_privilege lookup: the catalogue can say "no INSERT" while a column default, a trigger or a rule
// leaves a write path open, and the thing that matters is whether the row lands.
//
// Every one of these is the record of an actuation. The named consequence is what a reader of that row —
// PriorVerdictsActivity, the graduation evidence, the console audit — would be told by it.
func forgeryAttempts() []struct{ sql, what string } {
	return []struct{ sql, what string }{
		{`INSERT INTO action_verdict (action_id, plan_hash, verdict, target_host, site, schema_version)
		  VALUES ('tg164-forged', 'tg164-forged', 'match', 'forged-host', 'forged-site', 1)`,
			"a compromised TRIAGE worker WROTE action_verdict: it can declare that an action it never ran " +
				"verified CLEAN, and PriorVerdictsActivity reads that table back as this host's actuation history"},
		{`INSERT INTO action_execution (action_id, external_ref, verdict, target_host, site)
		  VALUES ('tg164-forged', 'tg164-forged', 'match', 'forged-host', 'forged-site')`,
			"a compromised TRIAGE worker WROTE action_execution: it can manufacture a run history of clean " +
				"hands-off heals for an op-class, which is the evidence the graduation ladder is argued from"},
		{`INSERT INTO interceptor_gate_verdict (action_id, external_ref, ordinal, gate, verdict, reason)
		  VALUES ('tg164-forged', 'tg164-forged', 1, 'evidence', 'pass', 'forged')`,
			"a compromised TRIAGE worker WROTE interceptor_gate_verdict: the per-gate trail an operator " +
				"reads to reconstruct WHY a mutation was admitted can be authored by the process that never ran a gate"},
		{`INSERT INTO policy_decision (rule_id, verdict, composed_band, action_id, mode)
		  VALUES ('tg164-forged', 'auto', 'AUTO', 'tg164-forged', 'Full-auto')`,
			"a compromised TRIAGE worker WROTE policy_decision: it can forge the AUTHORISATION record for an " +
				"action — a durable 'policy said auto' with no policy engine anywhere near it"},
		{`UPDATE action_verdict SET verdict = 'match' WHERE action_id = 'tg164-seeded'`,
			"a compromised TRIAGE worker REWROTE an existing action_verdict: a real deviation can be turned " +
				"into a match after the fact, which un-trips the mutation breaker's evidence"},
		{`DELETE FROM action_execution WHERE action_id = 'tg164-seeded'`,
			"a compromised TRIAGE worker DELETED an execution record: an actuation that happened can be made " +
				"never to have happened"},
	}
}

// KILLING MUTATION: grant the triage role the write it is denied here —
//
//	GRANT INSERT, UPDATE, DELETE ON action_verdict, action_execution, interceptor_gate_verdict,
//	                                 policy_decision TO tg_triage;
//
// (equivalently: drop those four names from ActuationAuthorityTables). This test then goes RED naming what a
// compromised triage worker could forge. TestPlaneGrantsMutationControl below EXECUTES that mutation in-process
// and asserts the RED, so the claim is not a comment.
func TestTriageRoleCannotForgeTheRecordOfAnActuation(t *testing.T) {
	ctx, p, _ := planeRoleFixture(t)
	conn := asRole(t, ctx, p, PlaneRoleTriage)

	// VACUITY FLOOR. In a database where tg_triage holds no privileges at all, every refusal below passes for
	// the wrong reason. Prove the role can write a table triage OWNS before believing a single denial.
	if _, err := conn.Exec(ctx, `
		INSERT INTO session_triage (external_ref, host, alert_rule, band, conclusion)
		VALUES ('tg164-vacuity-floor', 'floor-host', 'Device-Down', 'AUTO', 'floor')
		ON CONFLICT (external_ref) DO NOTHING`); err != nil {
		t.Fatalf("tg_triage could not write session_triage (%v) — this role holds nothing, so every "+
			"'permission denied' below would be vacuous", err)
	}
	// And prove it can still READ the withheld tables: withholding a WRITE must not blind the triage
	// workflow, which reads back the verdict the actuation plane wrote (VerifyActivity, PriorVerdicts).
	for _, tbl := range ActuationAuthorityTables {
		var n int
		if err := conn.QueryRow(ctx, "SELECT count(*) FROM "+pgIdent(tbl)).Scan(&n); err != nil {
			t.Fatalf("tg_triage cannot READ %s (%v) — the split withheld a read it needs; VerifyActivity and "+
				"PriorVerdictsActivity both read the actuation record back", tbl, err)
		}
	}

	for _, a := range forgeryAttempts() {
		_, err := conn.Exec(ctx, a.sql)
		denied(t, err, a.what)
	}
}

// THE CONTROL, and the half that matters most operationally: a split that breaks triage is an outage wearing
// a security change's clothes. Every statement here is one a triage-plane worker makes today.
func TestTriageRoleCanStillDoEverythingTriageNeeds(t *testing.T) {
	ctx, p, _ := planeRoleFixture(t)
	conn := asRole(t, ctx, p, PlaneRoleTriage)
	ref := fmt.Sprintf("tg164-control-%d", time.Now().UnixNano())

	for _, w := range []struct{ sql, why string }{
		{`INSERT INTO session_triage (external_ref, host, alert_rule, band, conclusion)
		  VALUES ('` + ref + `', 'h', 'Device-Down', 'AUTO', 'c')`,
			"RecordTriageActivity — every completed session"},
		{`INSERT INTO agent_step (external_ref, cycle, thought, tool, observation, outcome)
		  VALUES ('` + ref + `', 1, 't', 'check-host-services', 'o', 'ok')`,
			"InvestigateActivity's per-cycle transcript"},
		{`INSERT INTO agent_step_evidence (external_ref, cycle, evidence_id, tool, payload)
		  VALUES ('` + ref + `', 1, '` + ref + `-e1', 'check-host-services', 'p')`,
			"the screened tool payload behind each step (TG-272)"},
		{`INSERT INTO ingest_alert (external_ref, source_type, alert_rule, host)
		  VALUES ('` + ref + `', 'pve-liveness', 'Device-Down', 'h')`,
			"the PVE-liveness poller's accepted-envelope record"},
		{`INSERT INTO governance_ledger (seq, decision, reason, action_id, hash)
		  VALUES (` + fmt.Sprint(time.Now().UnixNano()) + `, 'approve', 'r', 'a', 'h')`,
			"RecordVoteActivity — the human approval must be durable BEFORE an action may proceed (INV-12/19). " +
				"Withholding this from triage would fail every governed session closed"},
		{`INSERT INTO policy_graduation (op_class, clean_run_count, level)
		  VALUES ('` + ref + `', 1, 'approve')`,
			"GraduationActivity — the ladder's earn path runs at session terminus, on the TRIAGE queue"},
	} {
		if _, err := conn.Exec(ctx, w.sql); err != nil {
			t.Errorf("tg_triage could NOT perform a write triage makes today (%s): %v — this is an OUTAGE, "+
				"not a hardening: the failure would surface as a permission error deep inside an activity", w.why, err)
		}
	}
	// The reap path (0055): the retention loop runs in BOTH worker processes and deletes only through the
	// SECURITY DEFINER function. EXECUTE must have been mirrored across, or the evidence corpus grows forever
	// on a split deployment and nothing says so.
	var deleted int64
	if err := conn.QueryRow(ctx, `SELECT reap_agent_step_evidence($1, $2)`,
		time.Now().UTC().Add(-3650*24*time.Hour), DefaultEvidenceReapBatch).Scan(&deleted); err != nil {
		t.Errorf("tg_triage cannot call reap_agent_step_evidence (%v) — the retention bound is wired to a path "+
			"the split worker cannot walk, i.e. a reaper that exists and never runs", err)
	}
}

// THE SYMMETRIC HALF. TG-153 argues that withholding untrusted content from the actuation process is what
// stops that process becoming the thing that gets popped; the same argument applies to its database identity.
// The evidence gate binds a proposal's cited tool-result ids to captured observations, so a process that can
// FABRICATE those observations can ground a mutation in a reading that never happened.
//
// KILLING MUTATION: drop agent_step_evidence (or the whole list) from TriageContentTables. RED here.
func TestActuationRoleCannotForgeTheEvidenceItActsOn(t *testing.T) {
	ctx, p, _ := planeRoleFixture(t)
	conn := asRole(t, ctx, p, PlaneRoleActuate)
	ref := fmt.Sprintf("tg164-actuate-%d", time.Now().UnixNano())

	// VACUITY FLOOR + the control in one: the actuation role must still be able to record an actuation.
	// If it cannot, this whole ticket has shipped an actuation outage and the refusals below are noise.
	for _, w := range []struct{ sql, why string }{
		{`INSERT INTO action_verdict (action_id, plan_hash, verdict, schema_version)
		  VALUES ('` + ref + `', '` + ref + `', 'match', 1)`, "the verifier is the sole writer (INV-10)"},
		{`INSERT INTO action_execution (action_id, external_ref, verdict)
		  VALUES ('` + ref + `', '` + ref + `', 'match')`, "one row per execution (P2-1)"},
		{`INSERT INTO interceptor_gate_verdict (action_id, external_ref, ordinal, gate, verdict)
		  VALUES ('` + ref + `', '` + ref + `', 1, 'evidence', 'pass')`, "the per-gate trail (spec/020 T-020-7)"},
		{`INSERT INTO policy_decision (rule_id, verdict, composed_band, mode)
		  VALUES ('` + ref + `', 'auto', 'AUTO', 'Shadow')`, "the policy authorisation audit (spec/015)"},
	} {
		if _, err := conn.Exec(ctx, w.sql); err != nil {
			t.Fatalf("tg_actuate could NOT record an actuation (%s): %v — the actuation plane cannot do its "+
				"one job, and every refusal below would be vacuous", w.why, err)
		}
	}

	for _, a := range []struct{ sql, what string }{
		{`INSERT INTO agent_step_evidence (external_ref, cycle, evidence_id, tool, payload)
		  VALUES ('` + ref + `', 1, '` + ref + `-forged', 'check-host-services', 'forged')`,
			"the ACTUATION worker WROTE agent_step_evidence: it can author the observation a mutation is " +
				"grounded in, and the evidence gate binds cited tool-result ids to exactly that table"},
		{`INSERT INTO agent_step (external_ref, cycle, thought, tool, observation, outcome)
		  VALUES ('` + ref + `', 1, 't', 'x', 'o', 'ok')`,
			"the ACTUATION worker WROTE agent_step: it can fabricate the investigation transcript an operator " +
				"reads to judge whether the mutation was reasonable"},
		{`INSERT INTO ingest_alert (external_ref, source_type, alert_rule, host)
		  VALUES ('` + ref + `-forged', 'forged', 'Device-Down', 'h')`,
			"the ACTUATION worker WROTE ingest_alert: it can invent the alert that justifies its own mutation"},
		{`INSERT INTO session_triage (external_ref, host, alert_rule, band, conclusion)
		  VALUES ('` + ref + `-forged', 'h', 'r', 'AUTO', 'c')`,
			"the ACTUATION worker WROTE session_triage: it can author the triage conclusion the whole record hangs from"},
	} {
		_, err := conn.Exec(ctx, a.sql)
		denied(t, err, a.what)
	}

	// It must still READ them — the interceptor's evidence gate binds against captured observations, and a
	// gate reasoning over rows it cannot see is a gate that cannot refuse.
	for _, tbl := range TriageContentTables {
		var n int
		if err := conn.QueryRow(ctx, "SELECT count(*) FROM "+pgIdent(tbl)).Scan(&n); err != nil {
			t.Errorf("tg_actuate cannot READ %s (%v) — the evidence gate binds against this corpus; blinding "+
				"the gate is not the same as bounding the writer", tbl, err)
		}
	}
}

// THE THIRD ORACLE THE TICKET ASKS FOR: an un-split (`TG_CREDENTIAL_PLANE=both`) deployment must be UNCHANGED
// on upgrade. `both` connects as tg_runtime, so the property is exactly "ApplyPlaneGrants does not touch
// tg_runtime's privileges" — asserted over the whole (table × privilege) grid, before and after, rather than
// spot-checked, because a single re-granted UPDATE on an append-only table is the failure that matters.
func TestUnsplitRuntimeRoleIsUnchanged(t *testing.T) {
	ctx, p, dsn := planeRoleFixture(t)
	before := privilegeGrid(t, ctx, p, PlaneRoleRuntime)
	if len(before) == 0 {
		t.Fatal("tg_runtime holds no privileges in the fixture — the comparison below would be trivially equal " +
			"and would prove nothing about an un-split deployment")
	}
	if _, err := ApplyPlaneGrants(ctx, dsn); err != nil {
		t.Fatalf("re-apply: %v", err)
	}
	after := privilegeGrid(t, ctx, p, PlaneRoleRuntime)
	if strings.Join(before, "\n") != strings.Join(after, "\n") {
		t.Errorf("ApplyPlaneGrants CHANGED tg_runtime's privileges — an existing `both` deployment would "+
			"behave differently after upgrade, which is the one thing this change promised not to do.\nbefore: %d entries\nafter:  %d entries\ndiff: %v",
			len(before), len(after), gridDiff(before, after))
	}
	// And the derived roles must never EXCEED the source. A plane role holding a privilege tg_runtime lacks
	// would mean the split re-granted something fourteen migrations deliberately revoked.
	src := map[string]bool{}
	for _, e := range after {
		src[e] = true
	}
	for _, role := range []string{PlaneRoleTriage, PlaneRoleActuate} {
		for _, e := range privilegeGrid(t, ctx, p, role) {
			// entries are "table:PRIV"; compare on the privilege, not the role
			if !src[e] {
				t.Errorf("%s holds %q which %s does NOT — the plane split ESCALATED a privilege that a "+
					"previous migration revoked (append-only posture, 0015/0018/0019/0020/...)", role, e, PlaneRoleRuntime)
			}
		}
	}
}

// privilegeGrid returns the sorted "table:PRIV" set a role holds over every table in public.
func privilegeGrid(t *testing.T, ctx context.Context, p *Pool, role string) []string {
	t.Helper()
	rows, err := p.Query(ctx, `
		SELECT c.relname, priv
		FROM pg_class c
		JOIN pg_namespace n ON n.oid = c.relnamespace,
		     unnest(ARRAY['SELECT','INSERT','UPDATE','DELETE']) AS priv
		WHERE n.nspname = 'public' AND c.relkind IN ('r','p')
		  AND has_table_privilege($1, c.oid, priv)
		ORDER BY c.relname, priv`, role)
	if err != nil {
		t.Fatalf("privilege grid for %s: %v", role, err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var tbl, priv string
		if err := rows.Scan(&tbl, &priv); err != nil {
			t.Fatalf("scan grid: %v", err)
		}
		out = append(out, tbl+":"+priv)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("grid rows: %v", err)
	}
	sort.Strings(out)
	return out
}

func gridDiff(before, after []string) []string {
	in := map[string]int{}
	for _, e := range before {
		in[e]++
	}
	for _, e := range after {
		in[e]--
	}
	var out []string
	for e, n := range in {
		if n > 0 {
			out = append(out, "-"+e)
		} else if n < 0 {
			out = append(out, "+"+e)
		}
	}
	sort.Strings(out)
	return out
}

// THE MUTATION CONTROL. It EXECUTES the killing mutation — grant tg_triage the actuation write — and asserts
// that the oracle above goes RED, then restores. A privilege test that stays green under a deliberately
// widened grant proves nothing, and this repository has shipped a gate that ran on every commit while
// examining nothing.
func TestPlaneGrantsMutationControl(t *testing.T) {
	ctx, p, dsn := planeRoleFixture(t)
	const target = "action_verdict"

	// Restore FIRST in the defer chain, and via ApplyPlaneGrants rather than a hand-written REVOKE, so the
	// fixture returns to the state the deriver actually produces even if an assertion below panics.
	t.Cleanup(func() {
		if _, err := ApplyPlaneGrants(ctx, dsn); err != nil {
			t.Fatalf("RESTORE FAILED (%v) — the fixture database is left with tg_triage able to write %s; "+
				"every later oracle in this binary would pass or fail for the wrong reason", err, target)
		}
	})

	if _, err := p.Exec(ctx, "GRANT INSERT ON "+pgIdent(target)+" TO "+pgIdent(PlaneRoleTriage)); err != nil {
		t.Fatalf("apply killing mutation: %v", err)
	}

	// Re-run the exact statement the security oracle refuses, under the widened grant. It must now SUCCEED —
	// which is what proves the refusal in TestTriageRoleCannotForgeTheRecordOfAnActuation is caused by the
	// withheld grant and not by a malformed INSERT, a missing table, or a constraint.
	conn := asRole(t, ctx, p, PlaneRoleTriage)
	if _, err := conn.Exec(ctx, `
		INSERT INTO action_verdict (action_id, plan_hash, verdict, target_host, site, schema_version)
		VALUES ('tg164-mutation-control', 'tg164-mutation-control', 'match', 'h', 's', 1)`); err != nil {
		t.Fatalf("under the killing mutation the forged INSERT was STILL refused (%v) — so the oracle's "+
			"refusal is not measuring the grant, and it would stay green with the control removed", err)
	}
	if _, err := p.Exec(ctx, `DELETE FROM action_verdict WHERE action_id = 'tg164-mutation-control'`); err != nil {
		t.Errorf("cleanup forged row: %v", err)
	}

	// And the boot self-check must SEE it. This is the seam the worker prints at boot; if it cannot detect a
	// widened grant it is decoration.
	audit, err := p.AuditPlaneWrites(ctx, ActuationAuthorityTables)
	if err != nil {
		t.Fatalf("audit: %v", err)
	}
	_ = audit // the superuser pool always passes; the role-scoped assertion is below
	tri, err := roleAudit(ctx, dsn, PlaneRoleTriage, ActuationAuthorityTables)
	if err != nil {
		t.Fatalf("role audit: %v", err)
	}
	if tri.Split() {
		t.Errorf("AuditPlaneWrites reported the triage role SPLIT while it held INSERT on %s — the worker's "+
			"boot self-check would print 'DENIED write on all off-plane tables' over a live exposure", target)
	}
	if len(tri.Checked) == 0 {
		t.Error("AuditPlaneWrites examined ZERO tables — Split() must never be answered off an empty check")
	}
}

// roleAudit runs AuditPlaneWrites as role (via SET ROLE on a dedicated session), which is what the worker
// does implicitly by authenticating as that role.
func roleAudit(ctx context.Context, dsn, role string, withheld []string) (PlaneWriteAudit, error) {
	p, err := Connect(ctx, dsn)
	if err != nil {
		return PlaneWriteAudit{}, err
	}
	defer p.Close()
	conn, err := p.Acquire(ctx)
	if err != nil {
		return PlaneWriteAudit{}, err
	}
	defer conn.Release()
	if _, err := conn.Exec(ctx, "SET ROLE "+pgIdent(role)); err != nil {
		return PlaneWriteAudit{}, err
	}
	var a PlaneWriteAudit
	if err := conn.QueryRow(ctx, "SELECT current_user").Scan(&a.Role); err != nil {
		return a, err
	}
	rows, err := conn.Query(ctx, `
		SELECT c.relname,
		       has_table_privilege(c.oid, 'INSERT')
		         OR has_table_privilege(c.oid, 'UPDATE')
		         OR has_table_privilege(c.oid, 'DELETE')
		FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE n.nspname='public' AND c.relkind IN ('r','p') AND c.relname = ANY($1)
		ORDER BY c.relname`, withheld)
	if err != nil {
		return a, err
	}
	defer rows.Close()
	for rows.Next() {
		var tbl string
		var w bool
		if err := rows.Scan(&tbl, &w); err != nil {
			return a, err
		}
		a.Checked = append(a.Checked, tbl)
		if w {
			a.Writable = append(a.Writable, tbl)
		}
	}
	return a, rows.Err()
}

// A NAME THAT DOES NOT EXIST PROTECTS NOTHING. Both lists are consumed by name; a typo, a renamed table or a
// table dropped by a later migration turns a withheld entry into a silent no-op while the boot log keeps
// counting it. This is the vacuity floor for the LISTS themselves.
func TestPlaneWithheldTablesAreRealAndDisjoint(t *testing.T) {
	ctx, p, _ := planeRoleFixture(t)
	live := map[string]bool{}
	rows, err := p.Query(ctx, "SELECT tablename FROM pg_tables WHERE schemaname='public'")
	if err != nil {
		t.Fatalf("list tables: %v", err)
	}
	for rows.Next() {
		var tbl string
		if err := rows.Scan(&tbl); err != nil {
			t.Fatalf("scan: %v", err)
		}
		live[tbl] = true
	}
	rows.Close()
	if len(live) == 0 {
		t.Fatal("the fixture schema is empty — every membership check below would be vacuous")
	}

	seen := map[string]string{}
	for _, list := range []struct {
		name   string
		tables []string
	}{
		{"ActuationAuthorityTables", ActuationAuthorityTables},
		{"TriageContentTables", TriageContentTables},
	} {
		if len(list.tables) == 0 {
			t.Errorf("%s is EMPTY — the plane it belongs to withholds nothing and the split is a log line", list.name)
		}
		for _, tbl := range list.tables {
			if !live[tbl] {
				t.Errorf("%s names %q, which does not exist in the migrated schema — the grant deriver skips it "+
					"silently and the boot report counts a protection that is not in force", list.name, tbl)
			}
			if other, dup := seen[tbl]; dup {
				t.Errorf("%q appears in both %s and %s — neither plane could write it, so the table would be "+
					"unreachable by the application entirely", tbl, other, list.name)
			}
			seen[tbl] = list.name
		}
	}
}

// The OPT-IN arm: a database without the plane roles must be left exactly alone. Asserted through the
// function's own contract (-1 for an absent role) rather than by dropping the real roles, which are
// cluster-wide and shared with every other test in this binary.
func TestPlaneGrantsAreANoOpForAnAbsentRole(t *testing.T) {
	ctx, p, _ := planeRoleFixture(t)
	var got int
	if err := p.QueryRow(ctx, "SELECT tg_apply_plane_grants($1, $2, $3)",
		"tg_role_that_does_not_exist", PlaneRoleRuntime, []string{}).Scan(&got); err != nil {
		t.Fatalf("call deriver for an absent role: %v", err)
	}
	if got != -1 {
		t.Errorf("tg_apply_plane_grants returned %d for a role that does not exist, want -1 — a deployment "+
			"that has not opted in must be changed in no way at all", got)
	}
}
