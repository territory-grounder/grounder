package db

import (
	"context"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// TG-80 P1-3: the mutable working set is PINNED. Migration 0105 revokes UPDATE/DELETE across the public
// schema and grants back exactly the tables the code writes (a whole-tree trace, 2026-08-22). This test
// parses the `GRANT UPDATE/DELETE ... TO tg_runtime` lines across ALL migrations — a table created AFTER
// the inversion (transaction_plan in 0111, granted its UPDATE in 0112, TG-551) cannot be granted in 0105
// because it does not exist yet, so its write grant lands in a later migration — and compares the union to
// the golden set, so the allowlist changes only on purpose: a table added here without a writer is a
// privilege nobody uses; a writer added without a grant fails loudly at its first UPDATE (the fail-closed
// direction the inversion buys).
var tg80MutableWorkingSet = map[string]string{ // table → privileges granted back
	"action_manifest": "UPDATE", "action_prestate": "UPDATE", "actuation_target_state": "UPDATE", // TG-554: target-admission claim/cooldown (0114)
	"alert_cluster": "UPDATE", "chat_cursor": "UPDATE",
	"commit_confirm": "UPDATE", "cost_accrual": "UPDATE", "cost_breaker_state": "UPDATE", "credential_coverage": "UPDATE",
	"credential_sync_run": "UPDATE", "discovered_scheduled_reboots": "UPDATE", "discovery_deviation": "UPDATE",
	"escalation_queue": "UPDATE", "guest_config_baseline": "UPDATE", "guest_liveness": "UPDATE",
	"infragraph_prediction": "UPDATE", "injected_fault": "UPDATE", "manifest_entry": "UPDATE",
	"module_capability_projection": "UPDATE", "mutation_breaker_state": "UPDATE", "observation_probe": "UPDATE",
	"opclass_candidate": "UPDATE", "pending_decision": "UPDATE", "pending_verification": "UPDATE",
	"policy_engine_toggle": "UPDATE", "policy_graduation": "UPDATE", "policy_mode": "UPDATE", "policy_ruleset": "UPDATE",
	"runtime_posture": "UPDATE", "sealed_secret": "UPDATE", "session_judgment": "UPDATE", "session_triage": "UPDATE",
	"skill": "UPDATE", "skill_chain_head": "UPDATE", "skill_trial": "UPDATE", "skill_version": "UPDATE",
	"skill_watch": "UPDATE", "sources": "UPDATE", "tracker_entry": "UPDATE",
	"transaction_plan": "UPDATE", "transaction_plan_step": "UPDATE", // TG-551: forward-only CAS state column (0112)
	"co_occurrence": "DELETE", "co_occurrence_host": "DELETE", "credential_native_rule": "DELETE", "estate_object_group": "DELETE",
	"control_plane_config": "UPDATE, DELETE", "credential_binding_projection": "UPDATE, DELETE",
	"knowledge_embedding": "UPDATE, DELETE", "operator_sessions": "UPDATE, DELETE",
}

var grantRe = regexp.MustCompile(`(?s)GRANT (UPDATE, DELETE|UPDATE|DELETE) ON (.*?)\s+TO tg_runtime;`)

func TestTG80MigrationGrantsBackExactlyTheTracedMutableSet(t *testing.T) {
	// The append-only inversion itself lives in 0105 — assert it still REVOKEs the wildcard, or every
	// grant-back below is additive noise.
	inv, err := os.ReadFile("migrations/0105_append_only_by_default.up.sql")
	if err != nil {
		t.Fatalf("read 0105: %v", err)
	}
	if !strings.Contains(string(inv), "REVOKE UPDATE, DELETE ON ALL TABLES IN SCHEMA public FROM tg_runtime;") {
		t.Fatal("0105 no longer revokes the wildcard — the inversion is gone and the grants below are additive noise")
	}
	// Union the tg_runtime UPDATE/DELETE grant-backs across EVERY migration — a table created after the
	// inversion is granted in a later migration, not 0105 (TG-551).
	files, err := filepath.Glob("migrations/*.up.sql")
	if err != nil {
		t.Fatalf("glob migrations: %v", err)
	}
	if len(files) < 40 {
		t.Fatalf("globbed only %d migration files — the glob or the path is broken (vacuity floor)", len(files))
	}
	got := map[string]string{}
	for _, f := range files {
		raw, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("read %s: %v", f, err)
		}
		for _, m := range grantRe.FindAllStringSubmatch(string(raw), -1) {
			for _, tbl := range strings.Split(m[2], ",") {
				tbl = strings.TrimSpace(strings.ReplaceAll(tbl, "\n", " "))
				tbl = strings.Fields(tbl)[0]
				got[tbl] = m[1]
			}
		}
	}
	if len(got) < 40 {
		t.Fatalf("parsed only %d grant-back tables across the migrations — the parser or the files are broken (vacuity floor 40)", len(got))
	}
	var names []string
	for k := range tg80MutableWorkingSet {
		names = append(names, k)
	}
	sort.Strings(names)
	for _, n := range names {
		if got[n] != tg80MutableWorkingSet[n] {
			t.Errorf("%s: 0105 grants %q, traced working set needs %q", n, got[n], tg80MutableWorkingSet[n])
		}
	}
	for n := range got {
		if _, ok := tg80MutableWorkingSet[n]; !ok {
			t.Errorf("%s: 0105 grants a privilege the traced working set does not list — a writer must be named here first", n)
		}
	}
}

// Against a live fixture: after 0105, tg_runtime holds UPDATE/DELETE on exactly the working set and on
// NOTHING else in public — the property the opt-out model could never state. Gated like every live test.
func TestTG80RuntimeRoleHoldsOnlyTheTracedWriteGrants(t *testing.T) {
	dsn := os.Getenv("TG_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("set TG_TEST_POSTGRES_DSN to a migrated database to run the append-only-default privilege audit")
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
	rows, err := p.Query(ctx, `
		SELECT c.relname,
		       has_table_privilege('tg_runtime', c.oid, 'UPDATE'),
		       has_table_privilege('tg_runtime', c.oid, 'DELETE')
		FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE n.nspname = 'public' AND c.relkind = 'r'`)
	if err != nil {
		t.Fatalf("privilege query: %v", err)
	}
	defer rows.Close()
	seen := 0
	for rows.Next() {
		var name string
		var upd, del bool
		if err := rows.Scan(&name, &upd, &del); err != nil {
			t.Fatalf("scan: %v", err)
		}
		seen++
		want := tg80MutableWorkingSet[name]
		wantUpd, wantDel := strings.Contains(want, "UPDATE"), strings.Contains(want, "DELETE")
		if upd != wantUpd || del != wantDel {
			t.Errorf("%s: tg_runtime update=%v delete=%v, traced working set says update=%v delete=%v", name, upd, del, wantUpd, wantDel)
		}
	}
	if seen < 60 {
		t.Fatalf("audited only %d tables — the fixture is not the migrated schema (vacuity floor 60)", seen)
	}
}
