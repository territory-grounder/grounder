package regime

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/territory-grounder/grounder/core/audit"
	"github.com/territory-grounder/grounder/core/config"
)

// The plaintext token value the tests inject NOWHERE into a row — only its SecretRef reference is passed.
// Every recorded row is scanned to prove this literal never lands in the audit trail (INV-13).
const (
	plaintextToken = "s3cr3t-awx-oauth2-token-value"
	tokenReference = config.SecretRef("store:awx.launch.token")
)

// TestAudit_RecordsThreeRowsAndChainsLedger proves the writer appends exactly one resolution, one actuation,
// and one deferred-verdict row to the append-only tables AND chains each into the tamper-evident ledger
// (REQ-1715, INV-19), with no plaintext secret anywhere (INV-13).
func TestAudit_RecordsThreeRowsAndChainsLedger(t *testing.T) {
	sink := NewMemAuditSink()
	ledger := audit.NewLedger()
	a := NewAudit(sink, ledger)
	ctx := context.Background()

	if err := a.RecordResolution(ctx, ResolutionRow{
		Target: "web01", Regime: RegimeAWXJob, Lane: RegimeAWXJob, RuleID: "glob:web*", Outcome: OutcomeResolved,
	}); err != nil {
		t.Fatalf("RecordResolution: %v", err)
	}
	if err := a.RecordActuation(ctx, ActuationRow{
		ActionID: "act#1", Lane: RegimeAWXJob, JobTemplateID: "7", OpClass: "restart-service",
		JobID: "42", TokenRef: tokenReference, // only the REFERENCE — never plaintextToken
	}); err != nil {
		t.Fatalf("RecordActuation: %v", err)
	}
	if err := a.RecordDeferredVerdict(ctx, DeferredVerdictRow{
		ActionID: "act#1", JobID: "42", Status: JobSuccessful, Verdict: VerdictMatch, Graduation: GraduationVerifiedClean,
	}); err != nil {
		t.Fatalf("RecordDeferredVerdict: %v", err)
	}

	// Exactly one row of each kind on its append-only table.
	if res, act, ver := sink.Counts(); res != 1 || act != 1 || ver != 1 {
		t.Fatalf("expected exactly 1 row per kind, got resolutions=%d actuations=%d verdicts=%d", res, act, ver)
	}
	// One ledger entry per row (three), and the tamper-evident chain verifies.
	if ledger.Len() != 3 {
		t.Fatalf("expected 3 ledger entries (one per audit row), got %d", ledger.Len())
	}
	if err := ledger.Verify(); err != nil {
		t.Fatalf("governance ledger chain must verify: %v", err)
	}

	// The actuation row carries the SecretRef REFERENCE, not the value.
	if got := sink.Actuations[0].TokenRef; got != tokenReference {
		t.Fatalf("token must be persisted as its reference %q, got %q", tokenReference, got)
	}
	// No plaintext secret leaked into ANY recorded field (rows or ledger entries) — INV-13.
	assertNoPlaintextSecret(t, sink, ledger)
}

// assertNoPlaintextSecret scans every recorded row and ledger entry for the plaintext token literal.
func assertNoPlaintextSecret(t *testing.T, sink *MemAuditSink, ledger *audit.Ledger) {
	t.Helper()
	var fields []string
	for _, r := range sink.Resolutions {
		fields = append(fields, r.Target, string(r.Regime), string(r.Lane), r.RuleID, string(r.Outcome))
	}
	for _, r := range sink.Actuations {
		fields = append(fields, r.ActionID, string(r.Lane), r.JobTemplateID, r.OpClass, r.JobID, string(r.TokenRef))
	}
	for _, r := range sink.Verdicts {
		fields = append(fields, r.ActionID, r.JobID, string(r.Status), string(r.Verdict), string(r.Graduation))
	}
	for _, e := range ledger.Entries() {
		fields = append(fields, e.Decision, e.Reason, e.ActionID)
	}
	for _, f := range fields {
		if strings.Contains(f, plaintextToken) {
			t.Fatalf("INV-13 VIOLATION: a plaintext secret leaked into an audit field: %q", f)
		}
	}
}

// TestAudit_FailsClosedOnIncompleteRows proves an incomplete row is never persisted (fail closed, REQ-1715).
func TestAudit_FailsClosedOnIncompleteRows(t *testing.T) {
	ctx := context.Background()
	a := NewAudit(NewMemAuditSink(), audit.NewLedger())

	cases := []struct {
		name string
		call func() error
		want error
	}{
		{"resolution missing target", func() error {
			return a.RecordResolution(ctx, ResolutionRow{Outcome: OutcomeResolved, Regime: RegimeAWXJob, Lane: RegimeAWXJob})
		}, ErrIncompleteAuditRow},
		{"resolved row missing regime/lane", func() error {
			return a.RecordResolution(ctx, ResolutionRow{Target: "web01", Outcome: OutcomeResolved})
		}, ErrIncompleteAuditRow},
		{"refused row carrying a lane", func() error {
			return a.RecordResolution(ctx, ResolutionRow{Target: "web01", Outcome: OutcomeRefused, Regime: RegimeAWXJob, Lane: RegimeAWXJob})
		}, ErrIncompleteAuditRow},
		{"actuation missing action_id", func() error {
			return a.RecordActuation(ctx, ActuationRow{Lane: RegimeAWXJob, TokenRef: tokenReference})
		}, ErrIncompleteAuditRow},
		{"actuation invalid lane", func() error {
			return a.RecordActuation(ctx, ActuationRow{ActionID: "act#1", Lane: Regime("bogus"), TokenRef: tokenReference})
		}, ErrIncompleteAuditRow},
		{"verdict invalid terminal status", func() error {
			return a.RecordDeferredVerdict(ctx, DeferredVerdictRow{ActionID: "act#1", Status: JobStatus("running"), Verdict: VerdictMatch, Graduation: GraduationVerifiedClean})
		}, ErrIncompleteAuditRow},
	}
	for _, c := range cases {
		if err := c.call(); !errors.Is(err, c.want) {
			t.Errorf("%s: expected %v, got %v", c.name, c.want, err)
		}
	}
}

// TestAudit_RefusesPlaintextTokenInActuation proves INV-13 at the writer boundary: an actuation whose token is
// a raw value (not a SecretRef reference) is refused before persistence.
func TestAudit_RefusesPlaintextTokenInActuation(t *testing.T) {
	sink := NewMemAuditSink()
	a := NewAudit(sink, audit.NewLedger())
	err := a.RecordActuation(context.Background(), ActuationRow{
		ActionID: "act#1", Lane: RegimeAWXJob, TokenRef: config.SecretRef(plaintextToken), // a RAW value, no scheme
	})
	if !errors.Is(err, ErrSecretInAuditRow) {
		t.Fatalf("a plaintext token must be refused with ErrSecretInAuditRow, got %v", err)
	}
	if res, act, ver := sink.Counts(); res != 0 || act != 0 || ver != 0 {
		t.Fatalf("nothing must persist when a row is refused, got resolutions=%d actuations=%d verdicts=%d", res, act, ver)
	}
}

// TestAudit_FailsClosedWhenUnwired proves an unwired writer refuses rather than silently dropping the audit.
func TestAudit_FailsClosedWhenUnwired(t *testing.T) {
	ctx := context.Background()
	// nil sink.
	if err := NewAudit(nil, audit.NewLedger()).RecordResolution(ctx, ResolutionRow{Target: "web01", Outcome: OutcomeRefused}); !errors.Is(err, ErrAuditNotWired) {
		t.Errorf("nil sink must fail closed with ErrAuditNotWired, got %v", err)
	}
	// nil ledger.
	if err := NewAudit(NewMemAuditSink(), nil).RecordActuation(ctx, ActuationRow{ActionID: "act#1", Lane: RegimeAWXJob}); !errors.Is(err, ErrAuditNotWired) {
		t.Errorf("nil ledger must fail closed with ErrAuditNotWired, got %v", err)
	}
	// nil writer.
	var a *Audit
	if err := a.RecordResolution(ctx, ResolutionRow{Target: "web01", Outcome: OutcomeRefused}); !errors.Is(err, ErrAuditNotWired) {
		t.Errorf("nil writer must fail closed with ErrAuditNotWired, got %v", err)
	}
}

// TestAudit_RefusalRowPersists proves a fail-closed refusal is itself a recorded audit row (no regime/lane).
func TestAudit_RefusalRowPersists(t *testing.T) {
	sink := NewMemAuditSink()
	a := NewAudit(sink, audit.NewLedger())
	if err := a.RecordResolution(context.Background(), ResolutionRow{Target: "unknown-x", Outcome: OutcomeRefused}); err != nil {
		t.Fatalf("a refusal must be recordable: %v", err)
	}
	if len(sink.Resolutions) != 1 || sink.Resolutions[0].Outcome != OutcomeRefused {
		t.Fatalf("the refusal must persist as a resolution row, got %+v", sink.Resolutions)
	}
}

// TestAudit_ConcurrentAppendsRaceSafe drives concurrent Record* calls so `go test -race` proves the writer +
// fake sink + ledger are safe under the worker's concurrent Temporal activities.
func TestAudit_ConcurrentAppendsRaceSafe(t *testing.T) {
	sink := NewMemAuditSink()
	a := NewAudit(sink, audit.NewLedger())
	ctx := context.Background()
	const n = 50
	var wg sync.WaitGroup
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func() {
			defer wg.Done()
			_ = a.RecordResolution(ctx, ResolutionRow{Target: "web01", Regime: RegimeNativeSSH, Lane: RegimeNativeSSH, Outcome: OutcomeResolved})
		}()
	}
	wg.Wait()
	if res, _, _ := sink.Counts(); res != n {
		t.Fatalf("expected %d concurrently-appended resolution rows, got %d", n, res)
	}
}
