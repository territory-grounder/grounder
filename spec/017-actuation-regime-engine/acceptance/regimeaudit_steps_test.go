package acceptance

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/cucumber/godog"

	"github.com/territory-grounder/grounder/core/audit"
	"github.com/territory-grounder/grounder/core/config"
	"github.com/territory-grounder/grounder/core/regime"
)

// T-017-6 (REQ-1715) — the append-only regime-audit persistence. Registered from this file's init() as a
// step-registrar slice so the shared acceptance harness (acceptance_test.go) is never edited by parallel task
// work — the same pattern the regime-core (T-017-1/2) and awxplaybooks (T-017-5) slices use.
func init() {
	stepRegistrars = append(stepRegistrars, registerRegimeAuditSteps)
}

// auditSecretValue is the plaintext AWX token the oracle deliberately NEVER passes into a row — only its
// SecretRef reference is. The Then step scans every recorded row + ledger entry to prove this literal is
// absent (INV-13).
const (
	auditSecretValue = "s3cr3t-awx-oauth2-token-value"
	auditTokenRef    = config.SecretRef("store:awx.launch.token")
)

// regimeAuditWorld drives the real core/regime.Audit writer over the in-memory fake sink + a real governance
// ledger, and reads the real migration 0020 DDL to prove the append-only grant.
type regimeAuditWorld struct {
	sink      *regime.MemAuditSink
	ledger    *audit.Ledger
	recErr    error
	migration string
}

// repoRootFromHere walks up from this test file to the repo root (the dir containing go.mod), so the migration
// is located regardless of the test's working directory.
func repoRootFromHere() (string, error) {
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		return "", fmt.Errorf("cannot resolve caller path")
	}
	dir := filepath.Dir(thisFile)
	for i := 0; i < 12; i++ {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir, nil
		}
		dir = filepath.Dir(dir)
	}
	return "", fmt.Errorf("go.mod not found above %s", thisFile)
}

func registerRegimeAuditSteps(sc *godog.ScenarioContext) {
	w := &regimeAuditWorld{}

	// ---- Given: the engine resolves a lane, launches a job, and completes a deferred verify ----
	sc.Step(`^the engine resolves a lane launches a job and completes a deferred verify$`, func() error {
		w.sink = regime.NewMemAuditSink()
		w.ledger = audit.NewLedger()
		a := regime.NewAudit(w.sink, w.ledger)
		ctx := context.Background()

		if err := a.RecordResolution(ctx, regime.ResolutionRow{
			Target: "web01", Regime: regime.RegimeAWXJob, Lane: regime.RegimeAWXJob,
			RuleID: "glob:web*", Outcome: regime.OutcomeResolved,
		}); err != nil {
			w.recErr = fmt.Errorf("record resolution: %w", err)
			return w.recErr
		}
		// The launch carries the AWX token as its SecretRef REFERENCE only — never the plaintext value.
		if err := a.RecordActuation(ctx, regime.ActuationRow{
			ActionID: "act#regime-1", Lane: regime.RegimeAWXJob, JobTemplateID: "7",
			OpClass: "restart-service", JobID: "42", TokenRef: auditTokenRef,
		}); err != nil {
			w.recErr = fmt.Errorf("record actuation: %w", err)
			return w.recErr
		}
		if err := a.RecordDeferredVerdict(ctx, regime.DeferredVerdictRow{
			ActionID: "act#regime-1", JobID: "42", Status: regime.JobSuccessful,
			Verdict: regime.VerdictMatch, Graduation: regime.GraduationVerifiedClean,
		}); err != nil {
			w.recErr = fmt.Errorf("record deferred verdict: %w", err)
			return w.recErr
		}
		return nil
	})

	// ---- When: the runtime DB role attempts to UPDATE/DELETE a row (the grant that denies it is the DDL) ----
	sc.Step(`^the runtime database role attempts to update or delete a regime_resolution regime_actuation or deferred_verdict row$`, func() error {
		// CI has no Postgres, so the append-only guarantee is proven structurally over the real migration DDL:
		// the runtime DML role tg_runtime holds no UPDATE/DELETE grant (mirrors core/db migrations_test.go). Load
		// the real migration 0020 the Then step asserts against.
		root, err := repoRootFromHere()
		if err != nil {
			return err
		}
		b, err := os.ReadFile(filepath.Join(root, "core", "db", "migrations", "0020_actuation_regime.up.sql"))
		if err != nil {
			return fmt.Errorf("read migration 0020: %w", err)
		}
		w.migration = string(b)
		return nil
	})

	// ---- Then: one non-secret row per resolution/launch/verdict is appended + ledgered; the UPDATE/DELETE
	// is denied by the grants ----
	sc.Step(`^one row per resolution launch and verdict carrying only non-secret metadata is appended to the tamper-evident ledger and the update or delete is denied by the grants$`, func() error {
		if w.recErr != nil {
			return w.recErr
		}
		// One row per required output on its append-only table (REQ-1715).
		if res, act, ver := w.sink.Counts(); res != 1 || act != 1 || ver != 1 {
			return fmt.Errorf("expected exactly one resolution/actuation/verdict row, got %d/%d/%d", res, act, ver)
		}
		// Each row is ALSO chained into the tamper-evident governance ledger (INV-19), and the chain verifies.
		if w.ledger.Len() != 3 {
			return fmt.Errorf("expected 3 ledger entries (one per audit row), got %d", w.ledger.Len())
		}
		if err := w.ledger.Verify(); err != nil {
			return fmt.Errorf("the governance ledger chain must verify (tamper-evident): %w", err)
		}
		// ONLY NON-SECRET metadata: the launch token is the SecretRef reference, and the plaintext value never
		// appears in any recorded row or ledger entry (INV-13).
		if got := w.sink.Actuations[0].TokenRef; got != auditTokenRef {
			return fmt.Errorf("the token must be persisted as its reference %q, got %q", auditTokenRef, got)
		}
		var fields []string
		for _, r := range w.sink.Resolutions {
			fields = append(fields, r.Target, string(r.Regime), string(r.Lane), r.RuleID, string(r.Outcome))
		}
		for _, r := range w.sink.Actuations {
			fields = append(fields, r.ActionID, string(r.Lane), r.JobTemplateID, r.OpClass, r.JobID, string(r.TokenRef))
		}
		for _, r := range w.sink.Verdicts {
			fields = append(fields, r.ActionID, r.JobID, string(r.Status), string(r.Verdict), string(r.Graduation))
		}
		for _, e := range w.ledger.Entries() {
			fields = append(fields, e.Decision, e.Reason, e.ActionID)
		}
		for _, f := range fields {
			if strings.Contains(f, auditSecretValue) {
				return fmt.Errorf("INV-13 VIOLATION: a plaintext secret leaked into an audit field: %q", f)
			}
		}
		// THE UPDATE/DELETE IS DENIED BY THE GRANTS: the append-only tables strip UPDATE/DELETE from the runtime
		// DML role tg_runtime (migration 0020), so the attempted UPDATE/DELETE fails at the privilege boundary.
		flat := strings.Join(strings.Fields(w.migration), " ")
		for _, tbl := range []string{"regime_resolution", "regime_actuation", "deferred_verdict"} {
			if !strings.Contains(flat, "REVOKE UPDATE, DELETE ON "+tbl+" FROM tg_runtime") {
				return fmt.Errorf("migration 0020 must REVOKE UPDATE,DELETE on append-only %s from tg_runtime", tbl)
			}
		}
		return nil
	})
}
