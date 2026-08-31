package db

// TG-4 (Phase 3, anti-drift): EVERY table declares its retention posture, two-way-complete against the
// live schema. The epic's "per-data-class retention purge worker" exists piecemeal (evidence reaper,
// snapshot reaper, abandoned-decision reconciler, conversation TTL) — what was missing is the INVENTORY
// that proves the piecemeal set covers everything ON PURPOSE: before this test, a new table simply grew
// forever with nobody having decided that, and nothing could tell a deliberate append-only ledger from an
// accidental one ("a guard cannot catch what its list never names").
//
// The declaration is the POSTURE, not a mechanism claim: "reaped:"/"ttl:" entries name a mechanism that
// verifiably exists (each is exercised by its own test in this package); "retained:" states that NO
// scheduled reaper is declared and why that is the intended posture — it does not claim in-path expiry
// deletes do or don't exist. A new table fails this test until its author decides, in review, which it is.

import (
	"context"
	"sort"
	"testing"
	"time"
)

// retentionDeclarations: table -> declared posture. Keep entries sorted within their group.
var retentionDeclarations = map[string]string{
	// ---- reaped / TTL'd (the mechanism names a wiring or store exercised by its own test) ----
	"agent_step_evidence": "reaped: evidence reaper (cmd/worker/evidence_reap_wiring.go), purges journalled in agent_step_evidence_reap",
	"conversation_turn":   "ttl: DefaultConversationTTL (14d) bounds every turn — derived, purgeable memory",
	"estate_snapshot":     "reaped: snapshot reaper (keep newest per plane + first-of-day + last 24h), journalled in estate_snapshot_reap",
	"pending_decision":    "reaped: abandoned-decision reconciler (cmd/worker/abandoned_decision_reap_wiring.go) closes rows stranded by dead workflows",

	// ---- purge journals (the append-only record OF the purges; deleting them would unrecord the reap) ----
	"agent_step_evidence_reap": "retained: purge journal — the audit record of evidence reaps",
	"estate_snapshot_reap":     "retained: purge journal — the audit record of snapshot reaps",

	// ---- governance / audit spine (append-only BY LAW — INV-19 tamper-evidence; purging is forbidden, not missing) ----
	"action_execution":         "retained: actuation audit record (INV-19 surface)",
	"agent_step":               "retained: per-step agent reasoning trace — the session's audit spine (its heavy evidence blobs are the reaped part, agent_step_evidence)",
	"action_manifest":          "retained: sealed action identity — the manifest row IS the action's identity (INV-07)",
	"action_prestate":          "retained: pre-state evidence backing verify/rollback claims",
	"action_verdict":           "retained: post-actuation verdict record",
	"commit_confirm":           "retained: commit-confirmed actuation record",
	"deferred_verdict":         "retained: async-verify verdict record (fail-closed channel, TG-435)",
	"exec_class_decision":      "retained: execution-class decision audit",
	"governance_ledger":        "retained: THE hash-chained governance ledger (INV-19; no-UPDATE/DELETE privilege boundary)",
	"groundnet_emit":           "retained: append-only federation emit trail — de-identified provenance of published wisdom (spec/021 T-021-8; no-UPDATE/DELETE privilege boundary, INV-19)",
	"groundnet_ingest":         "retained: append-only federation ingest trail — verify + re-graduation outcome of foreign statements (spec/021 T-021-8; no-UPDATE/DELETE privilege boundary, INV-19)",
	"interceptor_gate_verdict": "retained: interceptor chain verdict audit",
	"ledger_anchor":            "retained: external anchor points of the governance chain (TG-489)",
	"manifest_entry":           "retained: manifest lifecycle entries (per-shape identity, TG-532 lesson)",
	"prediction_verdict":       "retained: prediction-gate verdict record — the falsifiability trail",
	"regime_actuation":         "retained: regime-lane actuation record (spec/017)",
	"regime_resolution":        "retained: regime resolution record (spec/017)",
	"session_judgment":         "retained: judged session scores — the eval/calibration corpus",
	"session_risk_audit":       "retained: full risk-classification detail behind ledger decisions",
	"transaction_plan":         "retained: multi-step plan record (spec/030) — plan identity and audit",
	"transaction_plan_step":    "retained: per-step record under a plan id (spec/030)",

	// ---- triage / incident corpus (the system's memory; TG reads it as precedent — no purge mandated) ----
	"alert_cluster":           "retained: correlated-burst cluster record",
	"co_occurrence":           "retained: learned rule co-occurrence statistics",
	"co_occurrence_host":      "retained: learned host co-occurrence statistics",
	"edge_disproof":           "retained: causal-edge disproof evidence (TG-206a)",
	"escalation_queue":        "retained: escalation lifecycle rows — drained by state transition, not deletion",
	"ingest_alert":            "retained: admitted-alert corpus — triage evidence and recovery joins",
	"ingest_alert_occurrence": "retained: per-occurrence dedup/flap evidence under an admitted alert",
	"ingest_transition":       "retained: source recovery-transition evidence (confirmed-clear chain)",
	"session_triage":          "retained: per-session triage record — the eval plane reads it (DB-backed Eval.Session)",
	"tracker_entry":           "retained: tracker filing record (entry-ticket identity, TG-490 lane)",

	// ---- estate model / observability (current-state projections and learned topology) ----
	"actuation_target_state":        "retained: per-target actuation state projection",
	"credential_binding_projection": "retained: boot-time credential binding projection (the yield register reads it)",
	"credential_coverage":           "retained: credential coverage register",
	"credential_native_rule":        "retained: native credential-source rules",
	"credential_resolution":         "retained: credential resolution audit (spec/016)",
	"credential_sync_run":           "retained: credential sync-run audit (spec/016)",
	"module_capability_projection":  "retained: boot-time module capability projection (the wiring register's read surface)",
	"sealed_secret":                 "retained: the sealed-secret store — deleting a row IS losing the secret (TG-283 class)",
	"discovered_scheduled_reboots":  "retained: learned scheduled-reboot suppression windows",
	"discovery_deviation":           "retained: estate discovery deviation record",
	"estate_object_group":           "retained: estate object groups (policy binding surface, TG-481)",
	"guest_config_baseline":         "retained: guest config baselines (confighash escalation reads them, TG-466)",
	"guest_liveness":                "retained: guest liveness projection (seal-time precondition source, TG-378)",
	"infragraph_cascade_stats":      "retained: learned cascade statistics",
	"infragraph_prediction":         "retained: infragraph prediction record (identity per action, INV-07)",
	"observation_coverage":          "retained: observation census register (TG-180)",
	"observation_probe":             "retained: observation-probe outcomes (TG-180 falsifiability record)",

	// ---- knowledge / skills (versioned content stores; history is the value) ----
	"graduation_credit":            "retained: autonomy-ladder graduation evidence",
	"knowledge_embedding":          "retained: embedded knowledge corpus (semantic retrieval)",
	"opclass_candidate":            "retained: op-class candidate lifecycle (clustering lane)",
	"opclass_candidate_occurrence": "retained: occurrences backing op-class candidates",
	"opclass_ratified":             "retained: ratified op-class catalog (prompt surface — registry data)",
	"policy_graduation":            "retained: policy graduation record (the ladder's memory)",
	"skill":                        "retained: skill registry",
	"skill_chain_head":             "retained: skill-store hash-chain head (tamper evidence)",
	"skill_trial":                  "retained: A/B trial record (Welch promotion evidence)",
	"skill_trial_assignment":       "retained: per-session trial assignments",
	"skill_version":                "retained: full skill version history",
	"skill_watch":                  "retained: skill watch/telemetry register",

	// ---- policy / config plane (small, versioned, singleton-ish; history is the audit trail) ----
	"control_plane_config":   "retained: control-plane config store (boot precedence, TG-260)",
	"cost_accrual":           "retained: cost accrual record (budget evidence)",
	"cost_breaker_state":     "retained: cost breaker state + trip history",
	"mutation_breaker_state": "retained: mutation breaker state + trip history",
	"policy_decision":        "retained: per-decision policy audit (spec/015)",
	"policy_engine_toggle":   "retained: engine-toggle audit",
	"policy_mode":            "retained: THE mode chokepoint's owner-set store — history is the mode audit",
	"policy_ruleset":         "retained: policy rulesets (versioned)",
	"policy_ruleset_version": "retained: ruleset version history",
	"runtime_posture":        "retained: runtime posture register",
	"sources":                "retained: ingest source registry",

	// ---- session/auth/campaign surfaces ----
	"auth_nonce":           "retained: no scheduled reaper declared — nonces are bounded by their in-path expiry check; unbounded growth here is a defect to fix, not a posture",
	"chat_cursor":          "retained: per-conversation cursor (bounded: one row per conversation)",
	"chat_events":          "retained: console chat event log — derived UI history, no purge mandated in this dev estate",
	"injected_fault":       "retained: THE injector ground-truth ledger — campaign evidence, never purged",
	"operator_sessions":    "retained: operator session records — bounded by expiry-at-auth; the audit of who held a session",
	"pending_verification": "retained: async-verify pending register (drained by state transition)",
}

// TestEveryTableDeclaresItsRetention is two-way complete: an undeclared live table fails (decide its
// posture in review before it ships), and a declared-but-absent table fails (a stale entry misdescribes
// the schema — the docs-vs-running-system drift TG-4 exists to forbid).
// The enumeration targets a MIGRATED FIXTURE (TG_TEST_DSN), not prod: a prod DB can legitimately carry
// operator-made stray tables outside the migrations (policy_ruleset_bak_handsoff is the recorded
// specimen — see schema_drift.go); pointing this test at such a DSN would honestly fail on them, which
// is correct for a fixture and noise for prod. Wire a prod-facing variant through schema_drift's
// stray-table handling first if that is ever wanted.
func TestEveryTableDeclaresItsRetention(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	dsn := skipWithoutDB(t)
	p, err := Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer p.Close()

	rows, err := p.Query(ctx, `
		SELECT table_name FROM information_schema.tables
		WHERE table_schema = 'public' AND table_type = 'BASE TABLE'
		  AND table_name <> 'schema_migrations'
		ORDER BY table_name`)
	if err != nil {
		t.Fatalf("enumerate tables: %v", err)
	}
	defer rows.Close()
	live := map[string]bool{}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatalf("scan: %v", err)
		}
		live[name] = true
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	// VACUITY FLOOR: an empty enumeration must never PASS as "everything declared" — a broken query, an
	// unmigrated fixture and a healthy schema are three different states and only one of them is green.
	if len(live) < 40 {
		t.Fatalf("only %d table(s) enumerated — the fixture looks unmigrated or the query is broken; "+
			"refusing a vacuous pass (declared: %d)", len(live), len(retentionDeclarations))
	}

	var undeclared, stale []string
	for name := range live {
		if _, ok := retentionDeclarations[name]; !ok {
			undeclared = append(undeclared, name)
		}
	}
	for name := range retentionDeclarations {
		if !live[name] {
			stale = append(stale, name)
		}
	}
	sort.Strings(undeclared)
	sort.Strings(stale)
	if len(undeclared) > 0 {
		t.Errorf("%d of %d live table(s) have NO retention declaration — decide each one's posture "+
			"(reaped:/ttl:/retained:+why) in core/db/retention_coverage_test.go before it ships: %v",
			len(undeclared), len(live), undeclared)
	}
	if len(stale) > 0 {
		t.Errorf("%d declaration(s) name tables that do not exist — a stale entry misdescribes the "+
			"schema; remove or rename: %v", len(stale), stale)
	}
	t.Logf("retention coverage: %d/%d live table(s) declared", len(live)-len(undeclared), len(live))
}
