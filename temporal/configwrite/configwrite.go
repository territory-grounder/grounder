// Package configwrite executes control-plane configuration and sealed-secret writes in the WORKER —
// the governance ledger's single writer (task #27 Phases C+D, spec/006 REQ-523/REQ-524). The
// grounder's admin-session surface never appends to the hash chain itself: it starts one of these
// workflows and waits, so every write is ledgered BEFORE its row commits (a crash leaves an
// over-recorded ledger, never an unrecorded override) and a concurrent grounder can never fork the
// chain. Secret MATERIAL never transits here: the grounder seals the value FIRST (core/seal) and the
// workflow carries only envelope ciphertext — Temporal history holds no plaintext (INV-13).
package configwrite

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/workflow"

	"github.com/territory-grounder/grounder/core/audit"
	"github.com/territory-grounder/grounder/core/cpconfig"
	"github.com/territory-grounder/grounder/core/schema"
	"github.com/territory-grounder/grounder/core/seal"
)

// ErrRationaleRequired refuses a write with no stated reason — enforced at the surface AND here (the
// authority), like every governed write lane.
var ErrRationaleRequired = errors.New("configwrite: rationale required — every override states why it exists")

// ConfigRequest is the typed config-override order (operator identity server-derived at the surface).
type ConfigRequest struct {
	Key       string
	Value     string
	Rationale string
	Operator  string
	// Clear removes the override instead of setting one (TG-261). A field rather than a second workflow:
	// the lane, the ledger discipline and the single-writer guarantee are identical, and only the verb
	// differs. Old requests deserialize with Clear=false, so in-flight histories keep their meaning.
	Clear bool
}

// ConfigResult is the committed override's essentials for the console response.
type ConfigResult struct {
	Key       string
	Value     string
	LedgerSeq int64
}

// SecretRequest is the typed sealed-secret order. It carries ONLY envelope ciphertext (core/seal) —
// the grounder seals before starting the workflow, so no plaintext enters Temporal history.
type SecretRequest struct {
	Name       string
	Ciphertext []byte
	Nonce      []byte
	WrappedDEK []byte
	DEKNonce   []byte
	Purpose    string
	Rationale  string
	Operator   string
}

// SecretResult is the committed secret's value-less essentials (the reference, never the material).
type SecretResult struct {
	Name      string
	Ref       string // "store:<name>" — how the config plane consumes it
	LedgerSeq int64
}

// Ledger is the slice of audit.Ledger these writes need — append-only governance decisions (INV-19).
//
// AppendContext, not Append (TG-277): the activity's deadline MUST reach the durable chain write. Without
// it a stalled substrate held this step until the database answered, which spent the whole
// StartToCloseTimeout in step one and produced a refusal that named nothing.
type Ledger interface {
	AppendContext(ctx context.Context, d audit.GovDecision) (audit.LedgerEntry, error)
}

// Step names for the per-step latency record. Constants rather than literals so the oracle asserting that
// every step is measured cannot drift away from the code that measures them.
const (
	StepLedgerAppend = "ledger.append"
	StepSchemaStamp  = "schema.current"
	StepStoreWrite   = "store.write"
)

// DefaultStepBudget bounds EACH step of a governed write.
//
// Sized from the MEASURED distribution, not from a guess (TG-277 asked for exactly this). Live on
// dc1tg01 2026-08-04 against a ledger at seq ~8800: six consecutive SecretPutWorkflow probes ran the
// whole workflow in 40-43ms, with PutSecretActivity itself at 11-13ms; the slowest activity ever observed
// on this path, during a substrate stall, was 1.26s. 4s is ~330x the median and ~3x the worst case, and
// three of them (12s) still fit inside the 15s StartToCloseTimeout — so a step that blows its budget
// ALWAYS reports itself before Temporal's opaque timeout fires. That ordering is the whole point: under
// MaximumAttempts 1 the operator gets one message, and it has to name the cause.
const DefaultStepBudget = 4 * time.Second

// StepLatency is what one step of a governed write cost.
type StepLatency struct {
	Step string
	Took time.Duration
}

// LatencyObserver receives the per-step timing of one governed write, on success and on failure alike.
//
// It exists because this path had NO latency observability at all: a 15s activity timeout on the
// credential-onboarding lane was unattributable, so TG-277 was filed against the hash-chain append — a
// step that measures ~12ms. The worker wires this to its log; a nil observer is the safe default and
// changes nothing for a deployment that does not set one.
type LatencyObserver func(op string, steps []StepLatency, total time.Duration, err error)

// ConfigStore persists the committed override row (the pgx CPConfigStore satisfies it).
type ConfigStore interface {
	Upsert(ctx context.Context, key, value, rationale, updatedBy string, ledgerSeq int64, schemaVersion int) error
	// Delete removes an override, reporting whether a row existed (TG-261). Same single-writer lane as
	// Upsert: the grounder never touches this table directly.
	Delete(ctx context.Context, key string) (bool, error)
}

// SecretStore persists the sealed blob (the pgx SealedSecretStore satisfies it).
type SecretStore interface {
	Put(ctx context.Context, name string, blob seal.Sealed, purpose, createdBy string, ledgerSeq int64, schemaVersion int) error
}

// Deps are the worker-side collaborators.
type Deps struct {
	Ledger  Ledger
	Config  ConfigStore
	Secrets SecretStore
	// Rewrap + Sealer drive the operator-triggered DEK rewrap lane (TG-163, rewrap.go). BOTH optional and
	// BOTH required together: a worker with no seal backend has nothing to rewrap with, and the lane
	// refuses rather than reporting a successful no-op over an untouched store.
	Rewrap SecretRewrapper
	Sealer *seal.Sealer
	// Observe reports each write's per-step latency (TG-277). Optional: nil means "measure nothing", which
	// is what every deployment did before this change.
	Observe LatencyObserver
	// StepBudget overrides DefaultStepBudget. Zero means the default; tests set a small one so the
	// latency-bound oracle runs in milliseconds instead of seconds.
	StepBudget time.Duration
}

// Activities carries Deps for Temporal registration.
type Activities struct{ D Deps }

// budget is the per-step allowance, defaulting when unset.
func (a *Activities) budget() time.Duration {
	if a.D.StepBudget > 0 {
		return a.D.StepBudget
	}
	return DefaultStepBudget
}

// timed runs one step of a governed write under its own slice of the activity budget and records what it
// cost (TG-277).
//
// The sub-deadline is the fix, not the timing: it converts "the activity hung somewhere for 15 seconds"
// into "step X exceeded its 4s budget after 4.001s", which is the difference between an operator
// concluding the secret store is unreliable and an operator seeing that their database stalled. A step
// that blows its own budget while the ACTIVITY context is still live is reported as a budget overrun; if
// the activity itself was cancelled, that is the honest error and it passes through unchanged.
func (a *Activities) timed(ctx context.Context, rec *[]StepLatency, step string, fn func(context.Context) error) error {
	budget := a.budget()
	// start is captured BEFORE the budget context exists (2026-08-25, main #50584): the timeout clock
	// begins inside WithTimeout, so with start captured after it, any scheduling gap between the two
	// lines is SUBTRACTED from the measured step time — on a contended runner a step that consumed its
	// whole budget reports Took just under it, which both mis-states the number an operator reads and
	// flaked the ≥budget assertion in TestFailedWriteStillReportsItsStepLatencies. Ordered this way,
	// Took can only be ≥ the span the deadline actually governed.
	start := time.Now()
	sctx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()
	err := fn(sctx)
	took := time.Since(start)
	*rec = append(*rec, StepLatency{Step: step, Took: took})
	if err != nil && sctx.Err() != nil && ctx.Err() == nil {
		return fmt.Errorf("configwrite: step %s exceeded its %s budget after %s: %w",
			step, budget, took.Round(time.Millisecond), err)
	}
	return err
}

// observe hands the completed measurement to the wired observer, if any.
func (a *Activities) observe(op string, steps []StepLatency, total time.Duration, err error) {
	if a.D.Observe == nil {
		return
	}
	a.D.Observe(op, steps, total, err)
}

// ApplyConfigActivity is the single-writer config override: registry legality (the LAW clamp,
// re-checked HERE — the surface can never be the only line), ledger append, THEN the row.
func (a *Activities) ApplyConfigActivity(ctx context.Context, req ConfigRequest) (ConfigResult, error) {
	if strings.TrimSpace(req.Rationale) == "" {
		return ConfigResult{}, ErrRationaleRequired
	}
	if req.Clear {
		return a.clearConfig(ctx, req)
	}
	k, err := cpconfig.ValidateWrite(req.Key, req.Value)
	if err != nil {
		return ConfigResult{}, err
	}
	var (
		steps []StepLatency
		entry audit.LedgerEntry
		sv    schema.Version
	)
	start := time.Now()
	err = func() error {
		sum := sha256.Sum256([]byte(req.Value))
		if lerr := a.timed(ctx, &steps, StepLedgerAppend, func(c context.Context) error {
			var e error
			entry, e = a.D.Ledger.AppendContext(c, audit.GovDecision{
				Decision: "config:set",
				Reason:   strings.TrimSpace(req.Rationale) + " [by " + req.Operator + "]",
				ActionID: "config:" + k.Name + ":" + hex.EncodeToString(sum[:6]),
				Withheld: false,
			})
			if e != nil {
				return fmt.Errorf("ledger append: %w", e)
			}
			return nil
		}); lerr != nil {
			return lerr
		}
		if serr := a.timed(ctx, &steps, StepSchemaStamp, func(context.Context) error {
			var e error
			sv, e = schema.Current(schema.TableControlPlaneConfig)
			return e
		}); serr != nil {
			return serr
		}
		return a.timed(ctx, &steps, StepStoreWrite, func(c context.Context) error {
			return a.D.Config.Upsert(c, k.Name, req.Value, strings.TrimSpace(req.Rationale), req.Operator, entry.Seq, int(sv))
		})
	}()
	a.observe("config:set", steps, time.Since(start), err)
	if err != nil {
		return ConfigResult{}, err
	}
	return ConfigResult{Key: k.Name, Value: req.Value, LedgerSeq: entry.Seq}, nil
}

// PutSecretActivity is the single-writer sealed-secret commit: ledger append (name + ciphertext
// digest — never material), THEN the row. The blob arrives already sealed by the grounder.
//
// Every step is measured and separately bounded (TG-277). This is the path an administrator uses to
// onboard EVERY credential, it runs with MaximumAttempts 1, and until this change it emitted nothing at
// all — so when a write timed out there was no way to tell which of the three steps had stalled, and the
// investigation blamed the one that turns out to cost ~12ms.
func (a *Activities) PutSecretActivity(ctx context.Context, req SecretRequest) (SecretResult, error) {
	if strings.TrimSpace(req.Rationale) == "" {
		return SecretResult{}, ErrRationaleRequired
	}
	if req.Name == "" || len(req.Ciphertext) == 0 || len(req.WrappedDEK) == 0 {
		return SecretResult{}, errors.New("configwrite: sealed secret name and ciphertext required")
	}
	var (
		steps []StepLatency
		entry audit.LedgerEntry
		sv    schema.Version
	)
	start := time.Now()
	err := func() error {
		sum := sha256.Sum256(req.Ciphertext)
		if lerr := a.timed(ctx, &steps, StepLedgerAppend, func(c context.Context) error {
			var e error
			entry, e = a.D.Ledger.AppendContext(c, audit.GovDecision{
				Decision: "secret:put",
				Reason:   strings.TrimSpace(req.Rationale) + " [by " + req.Operator + "]",
				ActionID: "secret:" + req.Name + ":" + hex.EncodeToString(sum[:6]),
				Withheld: false,
			})
			if e != nil {
				return fmt.Errorf("ledger append: %w", e)
			}
			return nil
		}); lerr != nil {
			return lerr
		}
		if serr := a.timed(ctx, &steps, StepSchemaStamp, func(context.Context) error {
			var e error
			sv, e = schema.Current(schema.TableSealedSecret)
			return e
		}); serr != nil {
			return serr
		}
		blob := seal.Sealed{Ciphertext: req.Ciphertext, Nonce: req.Nonce, WrappedDEK: req.WrappedDEK, DEKNonce: req.DEKNonce}
		return a.timed(ctx, &steps, StepStoreWrite, func(c context.Context) error {
			return a.D.Secrets.Put(c, req.Name, blob, req.Purpose, req.Operator, entry.Seq, int(sv))
		})
	}()
	a.observe("secret:put", steps, time.Since(start), err)
	if err != nil {
		return SecretResult{}, err
	}
	return SecretResult{Name: req.Name, Ref: "store:" + req.Name, LedgerSeq: entry.Seq}, nil
}

// activityOpts: no retries — a refused write (LAW key, bad value, missing rationale) is a DECISION,
// not a transient; it surfaces verbatim (mirrors temporal/skillwrite).
//
// The 15s StartToCloseTimeout STAYS, and TG-277's suspicion that it was mis-sized does not survive
// measurement: live on dc1tg01 2026-08-04, six consecutive secret writes completed the whole workflow
// in 40-43ms with the activity at 11-13ms. The ~10.5s the ticket measured was a Temporal WORKFLOW-task
// timeout (10.005s, twice) while the server's own Postgres was returning "Failed to start transaction:
// context deadline exceeded" — a substrate stall, not this activity's work. Raising the timeout would
// have bought nothing and hidden the next one; the per-step budgets above are what make the failure
// legible, and they fire at 4s, well before this backstop.
func activityOpts() workflow.ActivityOptions {
	return workflow.ActivityOptions{
		StartToCloseTimeout: 15 * time.Second,
		RetryPolicy:         &temporal.RetryPolicy{MaximumAttempts: 1},
	}
}

// clearConfig removes an override through the SAME single-writer lane, ledger-first (TG-261). The ledger
// entry is appended BEFORE the row goes, in the same direction as every other governed decision: a clear
// that is recorded but not applied is recoverable, a clear that is applied but unrecorded is a capability
// change nobody can audit.
func (a *Activities) clearConfig(ctx context.Context, req ConfigRequest) (ConfigResult, error) {
	k, err := cpconfig.ValidateClear(req.Key)
	if err != nil {
		return ConfigResult{}, err
	}
	var (
		steps   []StepLatency
		entry   audit.LedgerEntry
		existed bool
	)
	start := time.Now()
	err = func() error {
		if lerr := a.timed(ctx, &steps, StepLedgerAppend, func(c context.Context) error {
			var e error
			entry, e = a.D.Ledger.AppendContext(c, audit.GovDecision{
				Decision: "config:clear",
				Reason:   strings.TrimSpace(req.Rationale) + " [by " + req.Operator + "]",
				ActionID: "config:" + k.Name + ":clear",
				Withheld: false,
			})
			if e != nil {
				return fmt.Errorf("ledger append: %w", e)
			}
			return nil
		}); lerr != nil {
			return lerr
		}
		return a.timed(ctx, &steps, StepStoreWrite, func(c context.Context) error {
			var e error
			existed, e = a.D.Config.Delete(c, k.Name)
			return e
		})
	}()
	a.observe("config:clear", steps, time.Since(start), err)
	if err != nil {
		return ConfigResult{}, err
	}
	if !existed {
		// Reported, not an error the operator must fix: the end state they asked for is the end state.
		// The ledger entry stands — an attempt to clear is a decision worth seeing even when it was a no-op.
		return ConfigResult{Key: k.Name, LedgerSeq: entry.Seq}, nil
	}
	return ConfigResult{Key: k.Name, LedgerSeq: entry.Seq}, nil
}

// ConfigWriteWorkflow is the one-activity config-override workflow. Named DISTINCTLY — Temporal
// registers by bare function name, and two packages both exporting `Workflow` collide at
// RegisterWorkflow (the 2026-07-17 worker boot-loop; guarded by the skilltrial names test).
func ConfigWriteWorkflow(ctx workflow.Context, req ConfigRequest) (ConfigResult, error) {
	var res ConfigResult
	err := workflow.ExecuteActivity(workflow.WithActivityOptions(ctx, activityOpts()),
		new(Activities).ApplyConfigActivity, req).Get(ctx, &res)
	return res, err
}

// SecretPutWorkflow is the one-activity sealed-secret workflow. Distinctly named (see above).
func SecretPutWorkflow(ctx workflow.Context, req SecretRequest) (SecretResult, error) {
	var res SecretResult
	err := workflow.ExecuteActivity(workflow.WithActivityOptions(ctx, activityOpts()),
		new(Activities).PutSecretActivity, req).Get(ctx, &res)
	return res, err
}
