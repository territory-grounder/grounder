package db

import (
	"context"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/territory-grounder/grounder/core/audit"
	"github.com/territory-grounder/grounder/core/manifest"
	"github.com/territory-grounder/grounder/core/persist"
	"github.com/territory-grounder/grounder/core/predict"
	"github.com/territory-grounder/grounder/core/safety"
	"github.com/territory-grounder/grounder/core/schema"
	"github.com/territory-grounder/grounder/core/verify"
)

// The set↔jsonb marshaling is the fiddly part and is pure — it round-trips deterministically without a DB.
func TestPredictionSetRoundTrip(t *testing.T) {
	set := map[string]struct{}{"db01": {}, "cache01": {}, "app01": {}}
	raw, err := sortedKeys(set)
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != `["app01","cache01","db01"]` {
		t.Fatalf("keys must serialize sorted, got %s", raw)
	}
	back, err := keysToSet(raw)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(back, set) {
		t.Fatalf("round-trip mismatch: %v vs %v", back, set)
	}
	// empty / nil are handled
	empty, _ := sortedKeys(nil)
	if s, _ := keysToSet(empty); len(s) != 0 {
		t.Fatal("empty set must round-trip to empty")
	}
	if s, _ := keysToSet(nil); len(s) != 0 {
		t.Fatal("nil bytes must yield an empty set")
	}
}

// PredictedRules keys are verify.RuleKey = host+"\x00"+rule. Postgres jsonb rejects a NUL byte, so the raw
// keys cannot be stored as strings; ruleKeysToJSON must render them NUL-free and round-trip exactly.
func TestRuleKeysJSONIsNulFreeAndRoundTrips(t *testing.T) {
	set := map[string]struct{}{
		verify.RuleKey("n8n01", "HostDown"):       {},
		verify.RuleKey("db01", "Service up/down"): {}, // a rule with a space and a slash
	}
	raw, err := ruleKeysToJSON(set)
	if err != nil {
		t.Fatal(err)
	}
	// the stored jsonb text must contain no raw NUL byte and no literal \u0000 escape (the class jsonb rejects).
	if strings.ContainsRune(string(raw), 0) || strings.Contains(string(raw), `\u0000`) {
		t.Fatalf("stored rule keys must be NUL-free, got %q", raw)
	}
	// deterministic + structured as [host, rule] pairs
	if string(raw) != `[["db01","Service up/down"],["n8n01","HostDown"]]` {
		t.Fatalf("rule keys must serialize as sorted [host,rule] pairs, got %s", raw)
	}
	back, err := jsonToRuleKeys(raw)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(back, set) {
		t.Fatalf("rule-key round-trip mismatch: %v vs %v", back, set)
	}
	if s, _ := jsonToRuleKeys(nil); len(s) != 0 {
		t.Fatal("nil bytes must yield an empty rule set")
	}
}

// The pgx store round-trips against a real Postgres. Skipped in CI (no DB); runs under compose when
// TG_TEST_POSTGRES_DSN points at a migrated database.
func TestPredictionStoreIntegration(t *testing.T) {
	dsn := os.Getenv("TG_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("set TG_TEST_POSTGRES_DSN to a migrated database to run the pgx prediction-store integration test")
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
	store := NewPredictionStore(p)

	rec := predict.PredictionRecord{
		Prediction: verify.Prediction{
			ActionID: "act-int-1", PlanHash: "plan-int-1", TargetHost: "pve01", Site: "nl",
			PredictedHosts: map[string]struct{}{"n8n01": {}, "litellm01": {}},
			PredictedRules: map[string]struct{}{verify.RuleKey("n8n01", "HostDown"): {}},
		},
		ControlHosts:   map[string]struct{}{"web09": {}},
		SchemaVersion:  schema.Version(1),
		PredictionHash: "hash-int-1",
		ExternalRef:    "ext-int-1",
	}
	if err := store.Commit(ctx, rec); err != nil {
		t.Fatalf("commit: %v", err)
	}
	// idempotent-append: a second commit of the same plan_hash must not error or duplicate.
	if err := store.Commit(ctx, rec); err != nil {
		t.Fatalf("re-commit must be idempotent: %v", err)
	}
	if has, err := store.Has(ctx, "plan-int-1"); err != nil || !has {
		t.Fatalf("Has must find the committed plan_hash: %v %v", has, err)
	}
	got, ok, err := store.Get(ctx, "plan-int-1")
	if err != nil || !ok {
		t.Fatalf("Get must return the row: %v %v", ok, err)
	}
	if !reflect.DeepEqual(got.Prediction.PredictedHosts, rec.Prediction.PredictedHosts) ||
		!reflect.DeepEqual(got.ControlHosts, rec.ControlHosts) || got.PredictionHash != rec.PredictionHash ||
		got.ExternalRef != "ext-int-1" {
		t.Fatalf("round-trip mismatch (external_ref dropped?): %+v", got)
	}
}

// The scheduled-reboot state mapping is pure and round-trips (runs in CI); the store round-trip is a
// compose-only integration test.
func TestScheduledRebootStateMapping(t *testing.T) {
	if srStateText(persist.SRLive) != "live" || srStateText(persist.SRObserving) != "observing" {
		t.Fatal("state text mapping wrong")
	}
	if srStateOf("live") != persist.SRLive || srStateOf("observing") != persist.SRObserving {
		t.Fatal("state parse wrong")
	}
	// an unknown stored value must fail SAFE to observing (never silently live)
	if srStateOf("bogus") != persist.SRObserving {
		t.Fatal("unknown state must fail safe to observing")
	}
}

func TestScheduledRebootsStoreIntegration(t *testing.T) {
	dsn := os.Getenv("TG_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("set TG_TEST_POSTGRES_DSN to run the pgx scheduled-reboots integration test")
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
	store := NewScheduledReboots(p)

	from := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	until := from.AddDate(1, 0, 0)
	sr := persist.ScheduledReboot{Host: "pve01", Kind: "reboot", Cron: "0 3 * * 0", State: persist.SRLive, Observations: 3, ValidFrom: from, ValidUntil: until}
	if _, err := store.Register(ctx, sr); err != nil {
		t.Fatalf("register: %v", err)
	}
	got, ok, err := store.Get(ctx, "pve01", "reboot")
	if err != nil || !ok {
		t.Fatalf("get: %v %v", ok, err)
	}
	if got.State != persist.SRLive || got.Observations != 3 || got.Cron != "0 3 * * 0" {
		t.Fatalf("round-trip mismatch: %+v", got)
	}
	// an inverted window fails closed
	if _, err := store.Register(ctx, persist.ScheduledReboot{Host: "h", Kind: "reboot", Cron: "* * * * *", ValidFrom: until, ValidUntil: from}); err == nil {
		t.Fatal("an inverted validity window must be rejected")
	}
}

// The pgx ledger writer persists the chain and continues it from the tail; the read-back chain verifies.
func TestLedgerStoreIntegration(t *testing.T) {
	dsn := os.Getenv("TG_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("set TG_TEST_POSTGRES_DSN to run the pgx ledger integration test")
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
	store := NewLedgerStore(p)

	// continue from whatever tail exists, mirror two decisions.
	seq0, hash0, err := store.Tail(ctx)
	if err != nil {
		t.Fatalf("tail: %v", err)
	}
	l := audit.NewLedgerFromTail(seq0, hash0).WithSink(store)
	if _, err := l.Append(audit.GovDecision{Decision: "classify:AUTO", ActionID: "int-a1"}); err != nil {
		t.Fatalf("append1: %v", err)
	}
	if _, err := l.Append(audit.GovDecision{Decision: "gate:deny", ActionID: "int-a2"}); err != nil {
		t.Fatalf("append2: %v", err)
	}
	// the whole persisted chain must verify unbroken.
	all, err := store.All(ctx)
	if err != nil {
		t.Fatalf("all: %v", err)
	}
	if err := audit.VerifyChain(all); err != nil {
		t.Fatalf("the persisted chain must verify: %v", err)
	}
	// the tail advanced by two.
	seq1, _, _ := store.Tail(ctx)
	if seq1 != seq0+2 {
		t.Fatalf("tail must advance by 2, got %d from %d", seq1, seq0)
	}
}

// The pgx session_risk_audit writer round-trips EVERY column and the DB CHECK rejects
// auto_proceed_on_timeout=true. session_risk_audit is a write-only audit sink (no Go reader), so without a
// read-back a dropped column in the INSERT would be silently written as its default and never noticed — the
// field-drop class (see the pgx-fake-hides-field-drop lesson: a sink that is never read back gives false
// confidence). Every field here is set DISTINCT from its column default so a drop shows up as the default on
// read-back. Flip-relevant: this row is the de-identified classification audit the ledger's decision joins to.
func TestRiskAuditStoreIntegration(t *testing.T) {
	dsn := os.Getenv("TG_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("set TG_TEST_POSTGRES_DSN to run the pgx risk-audit integration test")
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
	store := NewRiskAuditStore(p)

	ref := "TG-ra-rt-1"
	// Non-default on every writable column: auto_approved/notify_required/operator_override all true (default
	// false), band AUTO_NOTICE (default POLL_PAUSE), a non-empty signals map (default '{}'), a distinct
	// schema_version, and set plan_hash/action_id. auto_proceed_on_timeout stays false (the CHECK pins it).
	ra := audit.RiskAudit{
		ExternalRef: ref, RiskLevel: "high", Band: safety.BandAutoNotice,
		AutoApproved: true, AutoProceedOnTimeout: false, NotifyRequired: true, OperatorOverride: true,
		Signals:  map[string]string{"blast_radius": "3", "known_pattern": "yes"},
		PlanHash: "ph-rt-1", ActionID: "act-rt-1", SchemaVersion: 2,
	}
	if err := store.PersistRiskAudit(ra); err != nil {
		t.Fatalf("persist: %v", err)
	}

	// Read the raw columns back and assert each survived — a dropped column would read as its default.
	var (
		riskLevel, band, planHash, actionID, signalsJSON string
		autoApproved, autoProceed, notifyReq, opOverride bool
		schemaVer                                        int
	)
	err = p.QueryRow(ctx, `
		SELECT risk_level, band, auto_approved, auto_proceed_on_timeout, notify_required,
		       operator_override, signals_json::text, plan_hash, action_id, schema_version
		FROM session_risk_audit WHERE external_ref = $1 ORDER BY id DESC LIMIT 1`, ref).Scan(
		&riskLevel, &band, &autoApproved, &autoProceed, &notifyReq, &opOverride, &signalsJSON, &planHash, &actionID, &schemaVer)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if riskLevel != "high" || band != ra.Band.String() || !autoApproved || autoProceed || !notifyReq ||
		!opOverride || planHash != "ph-rt-1" || actionID != "act-rt-1" || schemaVer != 2 {
		t.Fatalf("risk-audit round-trip dropped a field: level=%q band=%q auto=%v proceed=%v notify=%v override=%v plan=%q act=%q sv=%d",
			riskLevel, band, autoApproved, autoProceed, notifyReq, opOverride, planHash, actionID, schemaVer)
	}
	if !strings.Contains(signalsJSON, `"blast_radius"`) || !strings.Contains(signalsJSON, `"known_pattern"`) {
		t.Fatalf("signals_json dropped a key on round-trip: %s", signalsJSON)
	}

	// The structural CHECK rejects auto_proceed_on_timeout=true independent of the writer (a poll never
	// proceeds on timeout — TG's core safety invariant).
	if _, err := p.Exec(ctx, `
		INSERT INTO session_risk_audit (external_ref, risk_level, band, action_id, schema_version, auto_proceed_on_timeout)
		VALUES ($1, 'low', 'POLL_PAUSE', 'a', 1, true)`, ref+"-chk"); err == nil {
		t.Fatal("the DB CHECK must reject auto_proceed_on_timeout=true")
	}
}

// The pgx escalation lane enqueues, lists due pending, and marks fired — a durable requeue survives a restart.
func TestEscalationStoreIntegration(t *testing.T) {
	dsn := os.Getenv("TG_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("set TG_TEST_POSTGRES_DSN to run the pgx escalation integration test")
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
	store := NewEscalationStore(p)

	past := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	it, err := store.Enqueue(ctx, "TG-esc-1", 1, past)
	if err != nil || it.Seq == 0 {
		t.Fatalf("enqueue: %v %+v", err, it)
	}
	due, err := store.DuePending(ctx, time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("due: %v", err)
	}
	found := false
	for _, d := range due {
		if d.ExternalRef == "TG-esc-1" {
			found = true
		}
	}
	if !found {
		t.Fatal("the enqueued escalation must be due")
	}
	if err := store.MarkFired(ctx, it.Seq); err != nil {
		t.Fatalf("mark fired: %v", err)
	}
	// after firing it is no longer pending.
	due2, _ := store.DuePending(ctx, time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	for _, d := range due2 {
		if d.Seq == it.Seq {
			t.Fatal("a fired escalation must not be pending")
		}
	}
	// empty ref fails closed
	if _, err := store.Enqueue(ctx, "", 0, past); err == nil {
		t.Fatal("an empty ref must be rejected")
	}
}

// The pgx verdict store appends one verdict per action (first-wins), rejects an invalid verdict, and reads back.
func TestVerdictStoreIntegration(t *testing.T) {
	dsn := os.Getenv("TG_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("set TG_TEST_POSTGRES_DSN to run the pgx verdict integration test")
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
	store := NewVerdictStore(p)

	if err := store.Commit(ctx, "vact-1", "vp-1", "web01", "nl", safety.VerdictMatch); err != nil {
		t.Fatalf("commit: %v", err)
	}
	// first-wins: a second commit does not overwrite.
	if err := store.Commit(ctx, "vact-1", "vp-1", "web01", "nl", safety.VerdictDeviation); err != nil {
		t.Fatalf("re-commit: %v", err)
	}
	got, ok, err := store.Get(ctx, "vact-1")
	if err != nil || !ok || got != safety.VerdictMatch {
		t.Fatalf("first-wins verdict must be match, got %q ok=%v err=%v", got, ok, err)
	}
	// an invalid verdict is rejected at the boundary.
	if err := store.Commit(ctx, "vact-2", "vp-2", "h", "s", safety.Verdict("bogus")); err != ErrInvalidVerdict {
		t.Fatalf("an invalid verdict must be rejected, got %v", err)
	}
}

// The chat store records idempotently (a redelivery is a no-op) and keeps a per-room cursor (no global cursor).
func TestChatStoreIntegration(t *testing.T) {
	dsn := os.Getenv("TG_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("set TG_TEST_POSTGRES_DSN to run the pgx chat integration test")
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
	store := NewChatStore(p)

	ins, err := store.Record(ctx, "matrix", "evt-1", "!room-a", `{"vote":"approve"}`)
	if err != nil || !ins {
		t.Fatalf("first record must insert: %v ins=%v", err, ins)
	}
	// redelivery of the SAME (source, event) is an idempotent no-op.
	again, err := store.Record(ctx, "matrix", "evt-1", "!room-a", `{"vote":"approve"}`)
	if err != nil || again {
		t.Fatalf("a redelivered event must be a no-op, got ins=%v err=%v", again, err)
	}
	// per-room cursor: room-a advances independently of room-b.
	if err := store.AdvanceCursor(ctx, "matrix", "!room-a", "evt-1"); err != nil {
		t.Fatalf("advance: %v", err)
	}
	if c, _ := store.Cursor(ctx, "matrix", "!room-a"); c != "evt-1" {
		t.Fatalf("room-a cursor must be evt-1, got %q", c)
	}
	if c, _ := store.Cursor(ctx, "matrix", "!room-b"); c != "" {
		t.Fatalf("room-b must have no cursor (no global cursor), got %q", c)
	}
	// missing identity is rejected.
	if _, err := store.Record(ctx, "matrix", "", "!room-a", ""); err == nil {
		t.Fatal("an event with no event_id must be rejected")
	}
}

// bandText/bandOf round-trip and fail safe (pure — runs in CI).
func TestBandTextRoundTrip(t *testing.T) {
	for _, b := range []safety.Band{safety.BandAuto, safety.BandAutoNotice, safety.BandPollPause} {
		if bandOf(bandText(b)) != b {
			t.Fatalf("band %v did not round-trip", b)
		}
	}
	if bandOf("bogus") != safety.BandPollPause {
		t.Fatal("an unknown band must fail safe to POLL_PAUSE")
	}
}

// The pgx manifest store seals a manifest once (first-wins) and re-asserts its content hash on read-back.
func TestManifestStoreIntegration(t *testing.T) {
	dsn := os.Getenv("TG_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("set TG_TEST_POSTGRES_DSN to run the pgx manifest integration test")
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
	store := NewManifestStore(p)

	m, err := manifest.New(manifest.Action{Target: "web01", OpClass: "restart-service", Op: "restart", Reversible: true}, safety.BandAuto, "ph-1", "predh-1")
	if err != nil {
		t.Fatalf("new manifest: %v", err)
	}
	if err := store.Seal(ctx, m); err != nil {
		t.Fatalf("seal: %v", err)
	}
	got, ok, err := store.Get(ctx, m.ActionID)
	if err != nil || !ok {
		t.Fatalf("get: %v ok=%v", err, ok)
	}
	if got.Band != safety.BandAuto || got.Action.Target != "web01" || got.PlanHash != "ph-1" {
		t.Fatalf("round-trip mismatch: %+v", got)
	}
}
