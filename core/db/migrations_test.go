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
