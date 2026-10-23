package db

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/territory-grounder/grounder/core/knowledge"
	"github.com/territory-grounder/grounder/core/schema"
)

// readMigration reads a migration file from disk (CI has no Postgres, so migrations cannot be executed —
// these are pure-Go structural guards over the DDL text, per the plan's compose-only integration model).
func readMigration(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("migrations", name))
	if err != nil {
		t.Fatalf("read migration %s: %v", name, err)
	}
	return string(b)
}

// Every .up.sql migration must have a matching .down.sql so a migration is always reversible.
func TestMigrationsHaveUpDownPairs(t *testing.T) {
	entries, err := os.ReadDir("migrations")
	if err != nil {
		t.Fatal(err)
	}
	downs := map[string]bool{}
	var ups []string
	for _, e := range entries {
		switch {
		case strings.HasSuffix(e.Name(), ".up.sql"):
			ups = append(ups, e.Name())
		case strings.HasSuffix(e.Name(), ".down.sql"):
			downs[e.Name()] = true
		}
	}
	if len(ups) == 0 {
		t.Fatal("no .up.sql migrations found")
	}
	for _, up := range ups {
		down := strings.TrimSuffix(up, ".up.sql") + ".down.sql"
		if !downs[down] {
			t.Errorf("migration %s has no matching %s (a migration must be reversible)", up, down)
		}
	}
}

// The registered infragraph_prediction table must be created by a migration with the falsifiability control
// column and the schema-version guard — the P2-26 fix (the table existed only in-memory before).
func TestInfragraphPredictionMigration(t *testing.T) {
	sql := readMigration(t, "0002_infragraph_prediction.up.sql")
	table := string(schema.TableInfragraphPrediction) // the registry is the source of truth for the name
	if !strings.Contains(sql, "CREATE TABLE "+table) {
		t.Fatalf("migration must CREATE TABLE %s (the registered governed table)", table)
	}
	for _, want := range []string{
		"control_hosts",   // INV-22: the negative control persists on every row
		"control_tp",      // verify-time control score
		"prediction_hash", // bound into the ActionManifest
		"schema_version  int NOT NULL CHECK (schema_version > 0)", // reader-guard invariant
		"infragraph_cascade_stats",                                // the cascade over-prediction gating table
	} {
		if !strings.Contains(sql, want) {
			t.Errorf("infragraph_prediction migration missing %q", want)
		}
	}
	// the down migration must drop what the up creates
	down := readMigration(t, "0002_infragraph_prediction.down.sql")
	for _, want := range []string{"DROP TABLE IF EXISTS infragraph_prediction", "DROP TABLE IF EXISTS infragraph_cascade_stats"} {
		if !strings.Contains(down, want) {
			t.Errorf("down migration missing %q", want)
		}
	}
}

// The two audit-spine tables must be created with their invariant CHECKs — the hash-chain columns on the
// ledger and the auto_proceed_on_timeout=false integrity constraint on the risk audit (0003).
func TestAuditSpineMigration(t *testing.T) {
	sql := readMigration(t, "0003_audit_spine.up.sql")
	for _, tbl := range []schema.Table{schema.TableGovernanceLedger, schema.TableSessionRiskAudit} {
		if !strings.Contains(sql, "CREATE TABLE "+string(tbl)) {
			t.Errorf("migration must CREATE TABLE %s", tbl)
		}
	}
	for _, want := range []string{
		"prev_hash", "hash", // the ledger hash chain (INV-19)
		"CHECK (auto_proceed_on_timeout = false)", // the poll-never-proceeds invariant, structural
		"schema_version         int NOT NULL CHECK (schema_version > 0)",
	} {
		if !strings.Contains(sql, want) {
			t.Errorf("audit-spine migration missing %q", want)
		}
	}
	down := readMigration(t, "0003_audit_spine.down.sql")
	for _, want := range []string{"DROP TABLE IF EXISTS governance_ledger", "DROP TABLE IF EXISTS session_risk_audit"} {
		if !strings.Contains(down, want) {
			t.Errorf("down migration missing %q", want)
		}
	}
}

// 0015 makes the append-only accountability spine tamper-RESISTANT: tg_runtime (the DML runtime role that
// performs mutations) must LOSE UPDATE+DELETE on the three genuinely append-only tables, so the app cannot
// rewrite its own audit trail (readiness-review G4, INV-19). infragraph_prediction is deliberately NOT
// revoked — falsify.go legitimately UPDATEs a prediction row with its scored outcome.
func TestAppendOnlySpineRevokeMigration(t *testing.T) {
	up := strings.Join(strings.Fields(readMigration(t, "0015_append_only_spine_revoke.up.sql")), " ")
	for _, tbl := range []schema.Table{schema.TableGovernanceLedger, schema.TableSessionRiskAudit, schema.TableActionVerdict} {
		if !strings.Contains(up, "REVOKE UPDATE, DELETE ON "+string(tbl)+" FROM tg_runtime") {
			t.Errorf("0015 must REVOKE UPDATE,DELETE on append-only %s from tg_runtime", tbl)
		}
	}
	// The mutable working-set must NOT be revoked — that would break the falsify scorer's legitimate UPDATE.
	// Check for a REVOKE/GRANT *statement* ("ON infragraph_prediction"), not a mention in the rationale comment.
	if strings.Contains(up, "ON infragraph_prediction") {
		t.Error("0015 must NOT touch infragraph_prediction (it is legitimately UPDATEd by the scorer)")
	}
	down := strings.Join(strings.Fields(readMigration(t, "0015_append_only_spine_revoke.down.sql")), " ")
	for _, tbl := range []schema.Table{schema.TableGovernanceLedger, schema.TableSessionRiskAudit, schema.TableActionVerdict} {
		if !strings.Contains(down, "GRANT UPDATE, DELETE ON "+string(tbl)+" TO tg_runtime") {
			t.Errorf("0015 down migration must restore the grant on %s (reversible)", tbl)
		}
	}
}

// 0004 creates the remaining four registered tables with their integrity constraints (INV-12 idempotent chat,
// bi-temporal registry, verdict enum).
func TestLedgersRegistriesMigration(t *testing.T) {
	sql := readMigration(t, "0004_ledgers_registries.up.sql")
	for _, tbl := range []schema.Table{schema.TableActionVerdict, schema.TableDiscoveredScheduledReboots, schema.TableEscalationQueue, schema.TableChatEvents} {
		if !strings.Contains(sql, "CREATE TABLE "+string(tbl)) {
			t.Errorf("migration must CREATE TABLE %s", tbl)
		}
	}
	for _, want := range []string{
		"UNIQUE (source_id, event_id)",     // INV-12 idempotent chat insert
		"PRIMARY KEY (host, kind, cron)",   // registry keyed incl. cron (P1-10)
		"PRIMARY KEY (source_id, room_id)", // per-room cursor, no global cursor (INV-12)
		"verdict        verdict NOT NULL",  // the mechanical verdict enum
	} {
		if !strings.Contains(sql, want) {
			t.Errorf("0004 migration missing %q", want)
		}
	}
}

// 0013 provisions the semantic-retrieval sidecar (spec/012 REQ-1110/REQ-1111): the pgvector extension, a
// NULLABLE embedding column whose dimension matches the compiled default (TG_EMBED_DIM's boot check reads
// the column back), the HNSW cosine index, and the schema-version guard. The down migration removes the
// table/index but deliberately leaves the extension (superuser bootstrap's decision).
func TestSemanticRetrievalMigration(t *testing.T) {
	sql := readMigration(t, "0013_semantic_retrieval.up.sql")
	if !strings.Contains(sql, "CREATE TABLE "+string(schema.TableKnowledgeEmbedding)) {
		t.Fatalf("migration must CREATE TABLE %s (the registered governed table)", schema.TableKnowledgeEmbedding)
	}
	for _, want := range []string{
		"CREATE EXTENSION IF NOT EXISTS vector",
		fmt.Sprintf("embedding      vector(%d)", knowledge.DefaultEmbedDim), // NULLABLE by design (no NOT NULL)
		"content_hash   text NOT NULL",                                      // the re-embed idempotency key
		"USING hnsw (embedding vector_cosine_ops)",                          // approximate cosine top-K
		"schema_version int NOT NULL DEFAULT 1 CHECK (schema_version > 0)",  // reader-guard invariant
	} {
		if !strings.Contains(sql, want) {
			t.Errorf("semantic-retrieval migration missing %q", want)
		}
	}
	if strings.Contains(sql, fmt.Sprintf("vector(%d) NOT NULL", knowledge.DefaultEmbedDim)) {
		t.Error("embedding must stay NULLABLE — rows without vectors are legal (lexical still serves them)")
	}
	down := readMigration(t, "0013_semantic_retrieval.down.sql")
	for _, want := range []string{"DROP INDEX IF EXISTS knowledge_embedding_cosine_hnsw", "DROP TABLE IF EXISTS knowledge_embedding"} {
		if !strings.Contains(down, want) {
			t.Errorf("down migration missing %q", want)
		}
	}
	if strings.Contains(down, "DROP EXTENSION") {
		t.Error("the down migration must not drop the vector extension (database-scoped, superuser-owned)")
	}
}

// 0018 creates the append-only credential_resolution audit table (spec/016 REQ-1617): a NON-SECRET row per
// per-target credential resolution, with the outcome CHECK, the schema_version guard, and — like the
// accountability spine (0015) — the runtime role stripped of UPDATE/DELETE so the app cannot rewrite its own
// resolution audit. The down migration drops the table.
func TestCredentialResolutionMigration(t *testing.T) {
	up := readMigration(t, "0018_credential_resolution.up.sql")
	if !strings.Contains(up, "CREATE TABLE credential_resolution") {
		t.Fatal("0018 must CREATE TABLE credential_resolution")
	}
	for _, want := range []string{
		"outcome        text NOT NULL CHECK (outcome IN ('resolved', 'unresolved', 'ambiguous'))",
		"key_ref_scheme text NOT NULL DEFAULT ''", // the ref SCHEME only — never the ref value or key material
		"schema_version int NOT NULL DEFAULT 1 CHECK (schema_version > 0)",
	} {
		if !strings.Contains(up, want) {
			t.Errorf("0018 up migration missing %q", want)
		}
	}
	// Append-only / tamper-resistant: the runtime DML role loses UPDATE+DELETE (readiness-review G4, INV-19).
	upFlat := strings.Join(strings.Fields(up), " ")
	if !strings.Contains(upFlat, "REVOKE UPDATE, DELETE ON credential_resolution FROM tg_runtime") {
		t.Error("0018 must REVOKE UPDATE,DELETE on the append-only credential_resolution from tg_runtime")
	}
	// No column may carry a full secret reference or value — assert the table stores only the ref SCHEME.
	if strings.Contains(up, "key_ref ") && !strings.Contains(up, "key_ref_scheme") {
		t.Error("0018 must store the key reference SCHEME only, never a full key_ref value")
	}
	down := readMigration(t, "0018_credential_resolution.down.sql")
	if !strings.Contains(down, "DROP TABLE IF EXISTS credential_resolution") {
		t.Error("0018 down migration must drop credential_resolution")
	}
}

// 0019 creates the four Policy Engine tables (spec/015 T-015-12, REQ-1518, INV-19). policy_decision is the
// append-only, NON-SECRET per-decision audit — like the accountability spine (0015) and credential_resolution
// (0018) the runtime DML role is stripped of UPDATE/DELETE so the app cannot rewrite its own decision trail;
// it carries the schema_version guard, the verdict/mode/band CHECKs, and NO argv/host/secret column. The
// three latest-wins durable tables (policy_mode, policy_graduation, policy_ruleset) carry their integrity
// CHECKs. The down migration drops all four.
func TestPolicyEngineMigration(t *testing.T) {
	up := readMigration(t, "0019_policy_engine.up.sql")
	for _, tbl := range []string{"policy_decision", "policy_mode", "policy_graduation", "policy_ruleset"} {
		if !strings.Contains(up, "CREATE TABLE "+tbl) {
			t.Errorf("0019 must CREATE TABLE %s", tbl)
		}
	}
	for _, want := range []string{
		"verdict        text NOT NULL CHECK (verdict IN ('auto', 'approve', 'deny'))",
		"composed_band  text NOT NULL CHECK (composed_band IN ('POLL_PAUSE', 'AUTO_NOTICE', 'AUTO'))",
		"mode           text NOT NULL CHECK (mode IN ('Shadow', 'HITL', 'Semi-auto', 'Full-auto'))",
		"schema_version int NOT NULL DEFAULT 1 CHECK (schema_version > 0)",
		"level           text NOT NULL CHECK (level IN ('approve', 'auto'))",
		"clean_run_count int NOT NULL DEFAULT 0 CHECK (clean_run_count >= 0)",
	} {
		if !strings.Contains(up, want) {
			t.Errorf("0019 up migration missing %q", want)
		}
	}
	// Append-only / tamper-resistant: the runtime DML role loses UPDATE+DELETE on policy_decision (G4, INV-19).
	upFlat := strings.Join(strings.Fields(up), " ")
	if !strings.Contains(upFlat, "REVOKE UPDATE, DELETE ON policy_decision FROM tg_runtime") {
		t.Error("0019 must REVOKE UPDATE,DELETE on the append-only policy_decision from tg_runtime")
	}
	// The durable latest-wins tables are NOT append-only (they are current-state, upserted) — they must NOT be
	// revoked, or the worker could never update the active mode / ladder / ruleset.
	for _, mutable := range []string{"policy_mode", "policy_graduation", "policy_ruleset"} {
		if strings.Contains(upFlat, "REVOKE UPDATE, DELETE ON "+mutable) {
			t.Errorf("0019 must NOT revoke UPDATE/DELETE on the latest-wins %s (it is upserted current-state)", mutable)
		}
	}
	// NON-SECRET: no policy table may carry an argv / host / credential / secret column. Scan the DDL with the
	// `--` rationale comments stripped (the comments legitimately DESCRIBE what is excluded).
	ddlOnly := stripSQLComments(up)
	for _, forbidden := range []string{"argv", "host", "credential", "secret", "password", "private_key"} {
		if strings.Contains(strings.ToLower(ddlOnly), forbidden) {
			t.Errorf("0019 must store NO secret/argv/host column — found %q in the DDL", forbidden)
		}
	}
	down := readMigration(t, "0019_policy_engine.down.sql")
	for _, tbl := range []string{"policy_decision", "policy_mode", "policy_graduation", "policy_ruleset"} {
		if !strings.Contains(down, "DROP TABLE IF EXISTS "+tbl) {
			t.Errorf("0019 down migration must drop %s", tbl)
		}
	}
}

// 0021 creates the CROSS-PROCESS mutation_breaker_state table (design-wisdom #3): the single durable source of
// truth for each named breaker's three-state position, so a deviation trip in one worker force-Shadows every
// sibling. It is CURRENT-STATE (latest-wins upsert by name), NOT append-only — the tamper-evident record of a
// trip is the governance_ledger 'safety:breaker-trip' entry — so unlike the accountability spine (0015) /
// policy_decision (0019) the runtime role KEEPS UPDATE (the upsert needs it) and the migration must NOT revoke
// it. It carries the schema_version guard and the state/counter CHECKs, and stores NO secret/argv/host column.
func TestMutationBreakerStateMigration(t *testing.T) {
	up := readMigration(t, "0021_mutation_breaker_state.up.sql")
	if !strings.Contains(up, "CREATE TABLE mutation_breaker_state") {
		t.Fatal("0021 must CREATE TABLE mutation_breaker_state")
	}
	for _, want := range []string{
		"state               text NOT NULL CHECK (state IN ('closed', 'open', 'half_open'))",
		"failure_count       int NOT NULL DEFAULT 0 CHECK (failure_count >= 0)",
		"half_open_successes int NOT NULL DEFAULT 0 CHECK (half_open_successes >= 0)",
		"schema_version      int NOT NULL DEFAULT 1 CHECK (schema_version > 0)",
		"name                text PRIMARY KEY CHECK (length(btrim(name)) > 0)",
	} {
		if !strings.Contains(up, want) {
			t.Errorf("0021 up migration missing %q", want)
		}
	}
	// Current-state / latest-wins, NOT append-only: the runtime role must KEEP UPDATE — a REVOKE here would break
	// the breaker's own upsert (it could never record a state change). So the migration must NOT revoke it.
	upFlat := strings.Join(strings.Fields(up), " ")
	if strings.Contains(upFlat, "REVOKE UPDATE") || strings.Contains(upFlat, "REVOKE DELETE") {
		t.Error("0021 must NOT revoke UPDATE/DELETE on mutation_breaker_state (it is a latest-wins upserted current-state row)")
	}
	// NON-SECRET: only breaker coordination fields — no argv / host / credential / secret column may land here.
	ddlOnly := stripSQLComments(up)
	for _, forbidden := range []string{"argv", "host", "credential", "secret", "password", "private_key"} {
		if strings.Contains(strings.ToLower(ddlOnly), forbidden) {
			t.Errorf("0021 must store NO secret/argv/host column — found %q in the DDL", forbidden)
		}
	}
	down := readMigration(t, "0021_mutation_breaker_state.down.sql")
	if !strings.Contains(down, "DROP TABLE IF EXISTS mutation_breaker_state") {
		t.Error("0021 down migration must drop mutation_breaker_state")
	}
}

// 0020 creates the three APPEND-ONLY Actuation Regime Engine audit tables (spec/017 T-017-6, REQ-1715,
// INV-19/INV-13): regime_resolution (one per lane selection), regime_actuation (one per launch), and
// deferred_verdict (one per completed deferred verify). Like the accountability spine (0015) /
// credential_resolution (0018) / policy_decision (0019), the runtime DML role is STRIPPED of UPDATE/DELETE on
// all three so the app cannot rewrite its own regime audit trail. Each carries the schema_version guard and
// its enum/integrity CHECKs, is single-org (no tenant_id), and stores NO secret — the only credential
// material is the token as a SecretRef REFERENCE (token_ref), never a value. The down migration drops all three.
func TestActuationRegimeMigration(t *testing.T) {
	up := readMigration(t, "0020_actuation_regime.up.sql")
	for _, tbl := range []string{"regime_resolution", "regime_actuation", "deferred_verdict"} {
		if !strings.Contains(up, "CREATE TABLE "+tbl) {
			t.Errorf("0020 must CREATE TABLE %s", tbl)
		}
	}
	for _, want := range []string{
		"outcome        text NOT NULL CHECK (outcome IN ('resolved', 'refused'))",
		"status         text NOT NULL CHECK (status IN ('successful', 'failed', 'error', 'canceled'))",
		"verdict        text NOT NULL CHECK (verdict IN ('match', 'deviation', 'unverified'))",
		"graduation     text NOT NULL CHECK (graduation IN ('verified_clean', 'deviated', 'no_credit'))",
		"schema_version int NOT NULL DEFAULT 1 CHECK (schema_version > 0)",
		// INV-13: the token is a SecretRef REFERENCE only — the CHECK forbids a raw plaintext value (no scheme).
		"token_ref       text NOT NULL DEFAULT '' CHECK (token_ref = '' OR token_ref ~ '^[a-z][a-z0-9]*:.+')",
	} {
		if !strings.Contains(up, want) {
			t.Errorf("0020 up migration missing %q", want)
		}
	}
	// Append-only / tamper-resistant: the runtime DML role loses UPDATE+DELETE on all three tables (G4, INV-19).
	upFlat := strings.Join(strings.Fields(up), " ")
	for _, tbl := range []string{"regime_resolution", "regime_actuation", "deferred_verdict"} {
		if !strings.Contains(upFlat, "REVOKE UPDATE, DELETE ON "+tbl+" FROM tg_runtime") {
			t.Errorf("0020 must REVOKE UPDATE,DELETE on the append-only %s from tg_runtime", tbl)
		}
	}
	// Single-org (ADR-0010): no tenant_id column may be reintroduced.
	if strings.Contains(strings.ToLower(stripSQLComments(up)), "tenant") {
		t.Error("0020 must NOT introduce a tenant_id (single-org, ADR-0010)")
	}
	// NON-SECRET: no argv/host/credential/password/private_key column — the only credential material is a
	// SecretRef reference (token_ref, which does not match the forbidden substrings). Scan the DDL, not comments.
	ddlOnly := strings.ToLower(stripSQLComments(up))
	for _, forbidden := range []string{"argv", "host", "credential", "secret", "password", "private_key"} {
		if strings.Contains(ddlOnly, forbidden) {
			t.Errorf("0020 must store NO secret/argv/host column — found %q in the DDL", forbidden)
		}
	}
	down := readMigration(t, "0020_actuation_regime.down.sql")
	for _, tbl := range []string{"regime_resolution", "regime_actuation", "deferred_verdict"} {
		if !strings.Contains(down, "DROP TABLE IF EXISTS "+tbl) {
			t.Errorf("0020 down migration must drop %s", tbl)
		}
	}
}

// 0023 creates the two CROSS-PROCESS cost/budget spend-guard tables (the $-ceiling breaker, the spend-guard
// sibling of the mutation breaker): cost_accrual (the additive day/session spend accumulators) and
// cost_breaker_state (the latest-wins breaker position). Both are CURRENT-STATE (additive / latest-wins
// upsert), NOT append-only — the tamper-evident record of a cost trip is the governance_ledger
// 'cost:breaker-trip' entry — so, like mutation_breaker_state (0021) and UNLIKE the accountability spine
// (0015) / policy_decision (0019), the runtime role KEEPS UPDATE (the upserts need it) and the migration
// must NOT revoke it. Both carry the schema_version guard and their integrity CHECKs, are single-org (no
// tenant_id), and store NO secret/argv/host column.
func TestCostBreakerMigration(t *testing.T) {
	up := readMigration(t, "0023_cost_breaker.up.sql")
	for _, tbl := range []string{"cost_accrual", "cost_breaker_state"} {
		if !strings.Contains(up, "CREATE TABLE "+tbl) {
			t.Errorf("0023 must CREATE TABLE %s", tbl)
		}
	}
	for _, want := range []string{
		"bucket_kind     text NOT NULL CHECK (bucket_kind IN ('day', 'session'))",
		"usd_accrued     double precision NOT NULL DEFAULT 0 CHECK (usd_accrued >= 0)",
		"state           text NOT NULL CHECK (state IN ('closed', 'open'))",
		"usd_at_trip     double precision NOT NULL DEFAULT 0 CHECK (usd_at_trip >= 0)",
		"PRIMARY KEY (bucket_kind, bucket_key)",
		"name            text PRIMARY KEY CHECK (length(btrim(name)) > 0)",
		"schema_version  int NOT NULL DEFAULT 1 CHECK (schema_version > 0)",
	} {
		if !strings.Contains(up, want) {
			t.Errorf("0023 up migration missing %q", want)
		}
	}
	// Current-state / latest-wins + additive, NOT append-only: the runtime role must KEEP UPDATE — a REVOKE
	// here would break the guard's own upserts (it could never accrue or record a trip). Must NOT revoke.
	upFlat := strings.Join(strings.Fields(up), " ")
	if strings.Contains(upFlat, "REVOKE UPDATE") || strings.Contains(upFlat, "REVOKE DELETE") {
		t.Error("0023 must NOT revoke UPDATE/DELETE on the cost tables (they are additive/latest-wins upserted current-state rows)")
	}
	// Single-org (ADR-0010): no tenant_id column.
	if strings.Contains(strings.ToLower(stripSQLComments(up)), "tenant") {
		t.Error("0023 must NOT introduce a tenant_id (single-org, ADR-0010)")
	}
	// NON-SECRET: only spend coordination fields — no argv / host / credential / secret column may land here.
	ddlOnly := strings.ToLower(stripSQLComments(up))
	for _, forbidden := range []string{"argv", "host", "credential", "secret", "password", "private_key"} {
		if strings.Contains(ddlOnly, forbidden) {
			t.Errorf("0023 must store NO secret/argv/host column — found %q in the DDL", forbidden)
		}
	}
	down := readMigration(t, "0023_cost_breaker.down.sql")
	for _, tbl := range []string{"cost_accrual", "cost_breaker_state"} {
		if !strings.Contains(down, "DROP TABLE IF EXISTS "+tbl) {
			t.Errorf("0023 down migration must drop %s", tbl)
		}
	}
}

// 0055 bounds agent_step_evidence and reconciles that bound with 0053's REVOKE (TG-295). The DDL properties
// asserted here are the ones the whole design rests on, and each is invisible in a behavioural test run
// against a database where someone has already granted something by hand:
//
//   - deletion is a SECURITY DEFINER FUNCTION, not a DELETE grant — a grant is a privilege over every row,
//     including the one an attacker wants gone, and it puts the audit write on the honest path;
//   - the runtime role keeps INSERT/SELECT and never regains UPDATE/DELETE on the evidence table;
//   - the journal is not writable by the role that triggers purges, so a purge cannot be forged or buried;
//   - EXECUTE is not left on PUBLIC.
//
// KILLING MUTATION (executed): replace the function grant with `GRANT DELETE ON agent_step_evidence TO
// tg_runtime`. RED — "0055 must NOT grant DELETE on agent_step_evidence to tg_runtime".
func TestAgentStepEvidenceRetentionMigration(t *testing.T) {
	up := readMigration(t, "0055_agent_step_evidence_retention.up.sql")
	upFlat := strings.Join(strings.Fields(up), " ")

	for _, want := range []string{
		"CREATE FUNCTION reap_agent_step_evidence(cutoff timestamptz, max_rows integer DEFAULT 50000)",
		"SECURITY DEFINER",                                      // the one privileged path
		"SET search_path = pg_catalog, public",                  // an owner-privileged body must not resolve names via the caller
		"CREATE TABLE agent_step_evidence_reap",                 // the audit journal
		"rows_deleted bigint NOT NULL CHECK (rows_deleted > 0)", // a journal row means a real deletion
		"CREATE INDEX agent_step_evidence_created ON agent_step_evidence (created_at)",
		"REVOKE ALL ON FUNCTION reap_agent_step_evidence(timestamptz, integer) FROM PUBLIC",
		"GRANT EXECUTE ON FUNCTION reap_agent_step_evidence(timestamptz, integer) TO tg_runtime",
		"GRANT SELECT, INSERT ON agent_step_evidence TO tg_runtime",                 // the append path, stated not inherited
		"GRANT USAGE, SELECT ON SEQUENCE agent_step_evidence_id_seq TO tg_runtime",  // bigserial: nextval is its own privilege
		"REVOKE UPDATE, DELETE ON agent_step_evidence FROM tg_runtime",              // 0053's guarantee, restated
		"GRANT SELECT ON agent_step_evidence_reap TO tg_runtime",                    // an operator can read what was purged
		"REVOKE INSERT, UPDATE, DELETE ON agent_step_evidence_reap FROM tg_runtime", // and can never author it
		"interval '24 hours'", // the floor: this path can never erase the most recent window
	} {
		if !strings.Contains(upFlat, strings.Join(strings.Fields(want), " ")) {
			t.Errorf("0055 up migration missing %q", want)
		}
	}

	// THE DESIGN DECISION, ASSERTED. The reaper must not be reconciled with 0053 by handing the runtime role
	// a DELETE it can aim anywhere. Scan the DDL with the rationale comments stripped — the header
	// legitimately DISCUSSES the DELETE-grant alternative it rejects.
	ddl := strings.Join(strings.Fields(stripSQLComments(up)), " ")
	if strings.Contains(ddl, "GRANT DELETE ON agent_step_evidence") ||
		strings.Contains(ddl, "GRANT UPDATE, DELETE ON agent_step_evidence") {
		t.Error("0055 must NOT grant DELETE on agent_step_evidence to any role: a table-level DELETE covers " +
			"every row (including the evidence of whatever made someone want it gone) and makes the journal " +
			"write a promise in application code rather than the same transaction as the delete")
	}
	// Vacuity floor for the scan above: it is only meaningful if stripSQLComments left the statements behind.
	if !strings.Contains(ddl, "GRANT EXECUTE ON FUNCTION reap_agent_step_evidence") {
		t.Fatal("the comment-stripped DDL no longer contains the function grant — the forbidden-grant scan " +
			"above is matching against nothing and would pass over any migration at all")
	}

	down := readMigration(t, "0055_agent_step_evidence_retention.down.sql")
	for _, want := range []string{
		"DROP FUNCTION IF EXISTS reap_agent_step_evidence(timestamptz, integer)",
		"DROP TABLE IF EXISTS agent_step_evidence_reap",
		"DROP INDEX IF EXISTS agent_step_evidence_created",
	} {
		if !strings.Contains(down, want) {
			t.Errorf("0055 down migration missing %q", want)
		}
	}
}

// 0056 stores the typed CLAIM behind a session's proposal (TG-201) as a NULLABLE COLUMN on the terminal
// triage record — not as a table of its own, and this test is the guard on that.
//
// ★ THE FEATURE SHIPPED TWICE, IN PARALLEL, AND NEARLY KEPT BOTH. One change added the judge dimension and
// this column; another added the operator surface and a separate `session_diagnosis` TABLE under the SAME
// migration number, with its own writer beside the investigate activity. Two stores for one fact is this
// codebase's documented pathology: the judge would have graded the column while the console rendered the
// table, both would have looked correct, and nothing would have said which was stale. The surface was
// rebased onto the column and the table deleted. If a `CREATE TABLE session_diagnosis` ever reappears, that
// merge has been re-made in the other direction.
//
// The NULLABILITY is the other load-bearing property, and it is invisible to a behavioural test run against
// an already-migrated database: NULL means "this session predates the column" and the judge scores that
// dimension N/A, while an explicit '{}' means "the agent bound no claim" and IS graded. A NOT NULL DEFAULT
// would collapse the two and retro-grade ~3,200 historical sessions against a rule they were never offered —
// the TG-61 global-floor failure, which fired the flywheel's Regressed trigger for every skill at once.
//
// KILLING MUTATION (executed): give the column `NOT NULL DEFAULT '{}'::jsonb`. RED — "0056 must add
// `diagnosis` to session_triage as a NULLABLE column".
func TestSessionDiagnosisIsAColumnOnTheTriageRowNotASecondStore(t *testing.T) {
	up := strings.Join(strings.Fields(stripSQLComments(readMigration(t, "0056_session_diagnosis.up.sql"))), " ")

	// VACUITY FLOOR. Every assertion below is a substring scan, and a migration renamed out from under this
	// test would satisfy the negative checks by holding no DDL at all.
	if !strings.Contains(up, "ALTER TABLE session_triage") {
		t.Fatalf("VACUITY FLOOR: 0056 no longer alters session_triage — the checks below would pass by matching "+
			"nothing. DDL read: %q", up)
	}
	if !strings.Contains(up, "ADD COLUMN IF NOT EXISTS diagnosis jsonb") {
		t.Errorf("0056 must add `diagnosis` to session_triage as a NULLABLE column (DDL: %q)", up)
	}
	if strings.Contains(up, "NOT NULL") || strings.Contains(up, "DEFAULT") {
		t.Errorf("0056 constrained or defaulted `diagnosis` (DDL: %q) — NULL is the load-bearing value: it means "+
			"the session predates the column and the judge must score it N/A. A default collapses that into "+
			"\"the agent bound no claim\" and retroactively grades every historical session", up)
	}
	if strings.Contains(up, "CREATE TABLE") {
		t.Errorf("0056 creates a TABLE (DDL: %q) — the typed claim has exactly one store, the column on "+
			"session_triage that the judge scores and the console reads. A second store for the same fact is "+
			"how the judge and the operator end up reading different claims about the same session", up)
	}
	down := strings.Join(strings.Fields(stripSQLComments(readMigration(t, "0056_session_diagnosis.down.sql"))), " ")
	if !strings.Contains(down, "DROP COLUMN IF EXISTS diagnosis") {
		t.Errorf("0056 down migration must drop the column the up adds (DDL: %q)", down)
	}
}

// 0057 records WHICH MODEL DECIDED as its OWN column (TG-198). model_tier (0027) carries the tier the
// read-only INVESTIGATION ran on; the TG-60 decide-nudge hands the terminal cycle to a different tier, and
// that switch was recorded nowhere — so all 537 recorded incidents attribute their decision to "fast".
//
// THE STRUCTURAL POINT THIS GUARDS is the tempting one-line "fix": re-pointing model_tier at the decision
// tier. That would make one column mean the investigation tier for every pre-migration row and the decision
// tier for every row after, with nothing on the row saying which — every aggregate over the corpus would
// silently mix two populations, and the investigate/decide PAIR (the TG-60 effect itself) would be lost.
// So the up must ADD a column and must not touch model_tier.
//
// KILLING MUTATION (executed): change the up to `ALTER TABLE session_triage RENAME COLUMN model_tier TO
// decision_model_tier`. RED — "0057 must ADD decision_model_tier to session_triage" and "0057 touches
// model_tier".
func TestSessionDecisionTierMigration(t *testing.T) {
	up := strings.Join(strings.Fields(stripSQLComments(readMigration(t, "0057_session_decision_tier.up.sql"))), " ")
	// VACUITY FLOOR: if the migration stops altering session_triage at all, every Contains check below
	// would pass by matching nothing. Anchor first.
	if !strings.Contains(up, "ALTER TABLE session_triage") {
		t.Fatalf("VACUITY FLOOR: 0057 no longer alters session_triage — the checks below would pass by matching "+
			"an empty/renamed migration (DDL: %q)", up)
	}
	if !strings.Contains(up, "ADD COLUMN IF NOT EXISTS decision_model_tier text NOT NULL DEFAULT ''") {
		t.Errorf("0057 must ADD decision_model_tier to session_triage, additive+defaulted so every existing "+
			"row and every pre-field writer stays valid (DDL: %q)", up)
	}
	// The DDL must leave model_tier alone. Checked against the comment-stripped text so the rationale prose
	// above — which legitimately names model_tier to explain why it is NOT touched — cannot trip it.
	if strings.Contains(up, "model_tier TO") || strings.Contains(up, "DROP COLUMN model_tier") ||
		strings.Contains(up, "ALTER COLUMN model_tier") {
		t.Errorf("0057 touches model_tier (DDL: %q) — the investigation tier and the decision tier are two "+
			"facts. Overloading one column makes it mean different things by row date, and destroys the "+
			"investigate/decide pair that is the whole TG-60 measurement", up)
	}
	// '' is load-bearing: a pre-0057 session genuinely did not record who decided, and defaulting it to a
	// tier NAME would manufacture the exact false attribution TG-198 exists to end.
	if strings.Contains(up, "DEFAULT 'fast'") || strings.Contains(up, "DEFAULT 'primary'") {
		t.Errorf("0057 defaults decision_model_tier to a tier NAME (DDL: %q) — an unattributable historical "+
			"session must read back empty, not be retroactively credited to a model that may not have decided it", up)
	}
	down := strings.Join(strings.Fields(stripSQLComments(readMigration(t, "0057_session_decision_tier.down.sql"))), " ")
	if !strings.Contains(down, "DROP COLUMN IF EXISTS decision_model_tier") {
		t.Errorf("0057 down migration must drop the column the up adds (DDL: %q)", down)
	}
	if strings.Contains(down, "model_tier TO") || strings.Contains(down, "DROP COLUMN model_tier") {
		t.Errorf("0057 down migration touches model_tier (DDL: %q) — reverting 0057 must leave 0027's column "+
			"exactly as it found it", down)
	}
}

// 0058 records HOW LONG a decision took as its OWN column (TG-205). step_count (0037) counts investigation
// CYCLES; docs/BENCHMARK-AXES.md defines A6 as MTTR, and every implementation measured the cycles — so the
// axis name and the code had drifted apart and no axis surface reported time (only the cumulative
// tg_agent_run_seconds_total counter, which no scorecard reads and which cannot be attributed to an incident).
//
// THE STRUCTURAL POINT THIS GUARDS is the tempting shortcut of making the column NULLABLE or defaulting it to
// something other than 0, and the worse one of re-purposing step_count. 0 is the load-bearing value: it means
// UNMEASURED (a pre-0058 row, or a session that never ran the loop) and the A6b query excludes it. A NULL
// would work equally well in SQL but would break every pre-field writer that omits the column, and 0 is what
// this codebase already uses for exactly this meaning on step_count.
//
// KILLING MUTATION (executed): change the up to `ALTER TABLE session_triage RENAME COLUMN step_count TO
// decision_ms`. RED — "0058 must ADD decision_ms to session_triage" and "0058 touches step_count".
func TestTriageDecisionLatencyMigration(t *testing.T) {
	up := strings.Join(strings.Fields(stripSQLComments(readMigration(t, "0058_triage_decision_latency.up.sql"))), " ")
	// VACUITY FLOOR: with the migration renamed or emptied, every Contains check below would pass by
	// matching nothing at all.
	if !strings.Contains(up, "ALTER TABLE session_triage") {
		t.Fatalf("VACUITY FLOOR: 0058 no longer alters session_triage — the checks below would pass by matching "+
			"an empty/renamed migration (DDL: %q)", up)
	}
	if !strings.Contains(up, "ADD COLUMN IF NOT EXISTS decision_ms bigint NOT NULL DEFAULT 0") {
		t.Errorf("0058 must ADD decision_ms to session_triage as bigint NOT NULL DEFAULT 0 — additive and "+
			"defaulted so every existing row and every pre-field writer stays valid, and 0 carries the "+
			"UNMEASURED meaning the A6b query filters on (DDL: %q)", up)
	}
	// A6a and A6b are two different measurements. Overloading the steps column would destroy the pair and
	// make one column mean different things by row date — the exact error 0057 exists to have avoided.
	if strings.Contains(up, "step_count TO") || strings.Contains(up, "DROP COLUMN step_count") ||
		strings.Contains(up, "ALTER COLUMN step_count") {
		t.Errorf("0058 touches step_count (DDL: %q) — how MANY cycles a decision took and how LONG it took are "+
			"two facts, and the whole point of splitting A6 is that neither implies the other", up)
	}
	down := strings.Join(strings.Fields(stripSQLComments(readMigration(t, "0058_triage_decision_latency.down.sql"))), " ")
	if !strings.Contains(down, "DROP COLUMN IF EXISTS decision_ms") {
		t.Errorf("0058 down migration must drop the column the up adds (DDL: %q)", down)
	}
	if strings.Contains(down, "step_count") {
		t.Errorf("0058 down migration touches step_count (DDL: %q) — reverting the wall-clock half must leave "+
			"0037's steps column exactly as it found it", down)
	}
}

// stripSQLComments removes `--` line comments so a forbidden-column scan tests the DDL, not the rationale
// prose (which legitimately names the excluded argv/host/secret columns to document their absence).
func stripSQLComments(sql string) string {
	var b strings.Builder
	for _, line := range strings.Split(sql, "\n") {
		if i := strings.Index(line, "--"); i >= 0 {
			line = line[:i]
		}
		b.WriteString(line)
		b.WriteByte('\n')
	}
	return b.String()
}

// Completeness: EVERY registered governed table must be created by SOME migration — no schema.Table const
// without a backing table (which would fail the reader guard / stamp at runtime).
func TestEveryRegisteredTableHasAMigration(t *testing.T) {
	entries, err := os.ReadDir("migrations")
	if err != nil {
		t.Fatal(err)
	}
	var allDDL strings.Builder
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".up.sql") {
			allDDL.WriteString(readMigration(t, e.Name()))
			allDDL.WriteByte('\n')
		}
	}
	ddl := allDDL.String()
	for _, tbl := range schema.Tables() {
		if !strings.Contains(ddl, "CREATE TABLE "+string(tbl)) {
			t.Errorf("registered table %q has no CREATE TABLE in any migration", tbl)
		}
	}
}
