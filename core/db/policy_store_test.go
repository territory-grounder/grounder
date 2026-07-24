package db

import (
	"context"
	"encoding/json"
	"os"
	"reflect"
	"testing"

	"github.com/territory-grounder/grounder/core/policy"
	"github.com/territory-grounder/grounder/core/safety"
)

// ORACLE (no DB): the in-memory policy AuditSink records each decision, and the pgx stores satisfy the policy
// interfaces (compile-time proof lives beside each impl). CI has no Postgres, so the durable round-trip is
// gated on TG_TEST_POSTGRES_DSN below.
func TestMemPolicyDecision_Records(t *testing.T) {
	sink := NewMemPolicyDecisionStore()
	dec := policy.NewPolicyDecision(policy.VerdictAuto, "svc-restart", safety.BandAuto, nil, policy.ModeFullAuto,
		"composed", policy.DecisionAudit{})
	if err := sink.AppendPolicyDecision(context.Background(), dec); err != nil {
		t.Fatalf("append: %v", err)
	}
	if len(sink.Records) != 1 || sink.Records[0].Verdict() != policy.VerdictAuto {
		t.Fatalf("expected one recorded auto decision, got %+v", sink.Records)
	}
}

// The pgx impls satisfy the policy engine's persistence interfaces (the leaves left them as TODO/interface).
func TestPolicyPgxStoresSatisfyInterfaces(t *testing.T) {
	var (
		_ policy.AuditSink       = (*PolicyDecisionWriteStore)(nil)
		_ policy.ModeStore       = (*PolicyModeStore)(nil)
		_ policy.GraduationStore = (*PolicyGraduationStore)(nil)
		_ policy.RulesetStore    = (*PolicyRulesetStore)(nil)
	)
}

// TestPolicyStoresRoundTrip drives the REAL pgx path for all four policy tables — the append-only
// policy_decision sink + read, the mode store save/load, the graduation store save/load, and the ruleset
// store save/load — so a struct field the SQL forgets to carry fails HERE instead of silently dropping in
// prod. Gated on TG_TEST_POSTGRES_DSN (CI has no Postgres).
func TestPolicyStoresRoundTrip(t *testing.T) {
	dsn := os.Getenv("TG_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("set TG_TEST_POSTGRES_DSN to a migrated database to run the policy store round-trip test")
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
	defer func() {
		_, _ = p.Exec(ctx, "DELETE FROM policy_decision")
		_, _ = p.Exec(ctx, "DELETE FROM policy_graduation WHERE op_class = 'rt-svc'")
		_, _ = p.Exec(ctx, "DELETE FROM policy_mode")
		_, _ = p.Exec(ctx, "DELETE FROM policy_ruleset")
	}()

	// policy_decision: append one decision, read it back with every non-secret field carried.
	sink := NewPolicyDecisionWriteStore(p)
	dec := policy.NewPolicyDecision(policy.VerdictAuto, "rt-rule", safety.BandAuto, nil, policy.ModeSemiAuto,
		"composed-reason", policy.DecisionAudit{
			Compose:       policy.ComposeRecord{BandMode: policy.BandRespect},
			Refine:        policy.RefineRecord{MinConfidence: 0.7},
			BundleVersion: "bundle-abc123",
			MatchedRules:  []policy.MatchedRule{{ID: "rt-rule", Verdict: policy.VerdictAuto}, {ID: "floor", Verdict: policy.VerdictDeny}},
			// spec/020 T-020-3 (REQ-2005): the correlation/attribution keys the interceptor threads. A real
			// action_id here means the writer persists it verbatim (NOT the synthetic policy-decision:<rule>:<v>
			// fallback), and external_ref/principal must round-trip so the decision joins the tracer walk.
			ActionID:    "act-rt",
			ExternalRef: "ext-rt",
			Principal:   "runner:ext-rt",
		})
	if err := sink.AppendPolicyDecision(ctx, dec); err != nil {
		t.Fatalf("append decision: %v", err)
	}
	read := NewPolicyReadStore(p)
	rows, err := read.Decisions(ctx, 10)
	if err != nil {
		t.Fatalf("read decisions: %v", err)
	}
	if len(rows) != 1 || rows[0].RuleID != "rt-rule" || rows[0].Verdict != "auto" ||
		rows[0].BandMode != "respect" || rows[0].ComposedBand != "AUTO" || rows[0].MinConfidence != 0.7 ||
		rows[0].Mode != "Semi-auto" || rows[0].ActionID != "act-rt" ||
		rows[0].BundleVersion != "bundle-abc123" || rows[0].Reason != "composed-reason" ||
		len(rows[0].MatchedRules) != 2 || rows[0].MatchedRules[0].ID != "rt-rule" ||
		rows[0].MatchedRules[1].Verdict != "deny" {
		t.Fatalf("policy_decision round-trip dropped a field: %+v", rows)
	}
	// external_ref + principal are new (migration 0028 / T-020-3) and not (yet) surfaced by the read store, so
	// assert them straight from the row — proving the pgx INSERT actually persisted both correlation keys.
	var gotExtRef, gotPrincipal string
	if err := p.QueryRow(ctx, "SELECT external_ref, principal FROM policy_decision WHERE action_id = $1", "act-rt").
		Scan(&gotExtRef, &gotPrincipal); err != nil {
		t.Fatalf("read external_ref/principal: %v", err)
	}
	if gotExtRef != "ext-rt" || gotPrincipal != "runner:ext-rt" {
		t.Fatalf("policy_decision correlation keys dropped: external_ref=%q principal=%q, want ext-rt / runner:ext-rt", gotExtRef, gotPrincipal)
	}

	// policy_mode: save + load through the pgx ModeStore, and read via the read store.
	modeStore := NewPolicyModeStore(p)
	if err := modeStore.Save(ctx, policy.ModeSemiAuto); err != nil {
		t.Fatalf("save mode: %v", err)
	}
	if m, err := modeStore.Load(ctx); err != nil || m != policy.ModeSemiAuto {
		t.Fatalf("load mode = (%v, %v), want Semi-auto", m, err)
	}
	if name, present, err := read.Mode(ctx); err != nil || !present || name != "Semi-auto" {
		t.Fatalf("read mode = (%q, %v, %v), want Semi-auto/true", name, present, err)
	}

	// policy_graduation: save + load a class state through the pgx GraduationStore.
	gradStore := NewPolicyGraduationStore(p)
	st := policy.ClassState{OpClass: "rt-svc", Level: policy.LevelAuto, CleanRunCount: 2, LastOutcome: policy.OutcomeVerifiedClean}
	if err := gradStore.Save(ctx, st); err != nil {
		t.Fatalf("save graduation: %v", err)
	}
	got, err := gradStore.Load(ctx, "rt-svc")
	if err != nil || got.Level != policy.LevelAuto || got.CleanRunCount != 2 || got.LastOutcome != policy.OutcomeVerifiedClean {
		t.Fatalf("load graduation = (%+v, %v), want auto/2/verified_clean", got, err)
	}

	// policy_ruleset: save + load the operator rules-as-data document.
	rulesetStore := NewPolicyRulesetStore(p)
	doc := []byte(`{"rules":[{"id":"rt","verdict":"deny","match":{"argv_pattern":"rm -rf"}}]}`)
	savedRS, err := rulesetStore.Save(ctx, doc, "rt-admin")
	if err != nil {
		t.Fatalf("save ruleset: %v", err)
	}
	// PolicyRulesetStore.Save archives the version IMMUTABLY in the SAME transaction (spec/020 T-020-6). Prove
	// the INLINED production archive actually persisted, keyed by the exact bundle_version the engine stamps —
	// reading it back via the standalone version store guards against the inlined tx SQL and RulesetVersionStore
	// drifting apart undetected (T-020-6 review follow-up). Cleaned up with the rest below.
	bv := policy.BundleVersion(savedRS)
	defer func() { _, _ = p.Exec(ctx, "DELETE FROM policy_ruleset_version WHERE bundle_version = $1", bv) }()
	archived, archOK, aerr := NewRulesetVersionStore(p).Get(ctx, bv)
	if aerr != nil || !archOK {
		t.Fatalf("inlined ruleset-version archive read: ok=%v err=%v — PolicyRulesetStore.Save did not archive the version", archOK, aerr)
	}
	// The archive round-trips through a jsonb column, so Postgres normalizes whitespace + key order — compare the
	// PARSED documents, not the raw bytes (the archive must be the SAME ruleset, not byte-identical to the input).
	var gotDoc, wantDoc any
	if err := json.Unmarshal(archived, &gotDoc); err != nil {
		t.Fatalf("archived version doc is not valid JSON: %v (%q)", err, archived)
	}
	if err := json.Unmarshal(doc, &wantDoc); err != nil {
		t.Fatalf("input doc unmarshal: %v", err)
	}
	if !reflect.DeepEqual(gotDoc, wantDoc) {
		t.Fatalf("archived version doc mismatch: got %q, want-equivalent %q", archived, doc)
	}
	rs, raw, err := rulesetStore.Load(ctx)
	if err != nil || len(rs.Rules) != 1 || rs.Rules[0].ID != "rt" || len(raw) == 0 {
		t.Fatalf("load ruleset = (%+v, %v), want one rule 'rt'", rs, err)
	}
	if rr, rerr := read.Ruleset(ctx); rerr != nil || !rr.Present || rr.RuleCount != 1 || rr.UpdatedBy != "rt-admin" {
		t.Fatalf("read ruleset = (%+v, %v), want present/1/rt-admin", rr, rerr)
	}
}
