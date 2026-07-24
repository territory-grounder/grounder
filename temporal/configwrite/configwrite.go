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
type Ledger interface {
	Append(d audit.GovDecision) (audit.LedgerEntry, error)
}

// ConfigStore persists the committed override row (the pgx CPConfigStore satisfies it).
type ConfigStore interface {
	Upsert(ctx context.Context, key, value, rationale, updatedBy string, ledgerSeq int64, schemaVersion int) error
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
}

// Activities carries Deps for Temporal registration.
type Activities struct{ D Deps }

// ApplyConfigActivity is the single-writer config override: registry legality (the LAW clamp,
// re-checked HERE — the surface can never be the only line), ledger append, THEN the row.
func (a *Activities) ApplyConfigActivity(ctx context.Context, req ConfigRequest) (ConfigResult, error) {
	if strings.TrimSpace(req.Rationale) == "" {
		return ConfigResult{}, ErrRationaleRequired
	}
	k, err := cpconfig.ValidateWrite(req.Key, req.Value)
	if err != nil {
		return ConfigResult{}, err
	}
	sum := sha256.Sum256([]byte(req.Value))
	entry, err := a.D.Ledger.Append(audit.GovDecision{
		Decision: "config:set",
		Reason:   strings.TrimSpace(req.Rationale) + " [by " + req.Operator + "]",
		ActionID: "config:" + k.Name + ":" + hex.EncodeToString(sum[:6]),
		Withheld: false,
	})
	if err != nil {
		return ConfigResult{}, fmt.Errorf("ledger append: %w", err)
	}
	sv, err := schema.Current(schema.TableControlPlaneConfig)
	if err != nil {
		return ConfigResult{}, err
	}
	if err := a.D.Config.Upsert(ctx, k.Name, req.Value, strings.TrimSpace(req.Rationale), req.Operator, entry.Seq, int(sv)); err != nil {
		return ConfigResult{}, err
	}
	return ConfigResult{Key: k.Name, Value: req.Value, LedgerSeq: entry.Seq}, nil
}

// PutSecretActivity is the single-writer sealed-secret commit: ledger append (name + ciphertext
// digest — never material), THEN the row. The blob arrives already sealed by the grounder.
func (a *Activities) PutSecretActivity(ctx context.Context, req SecretRequest) (SecretResult, error) {
	if strings.TrimSpace(req.Rationale) == "" {
		return SecretResult{}, ErrRationaleRequired
	}
	if req.Name == "" || len(req.Ciphertext) == 0 || len(req.WrappedDEK) == 0 {
		return SecretResult{}, errors.New("configwrite: sealed secret name and ciphertext required")
	}
	sum := sha256.Sum256(req.Ciphertext)
	entry, err := a.D.Ledger.Append(audit.GovDecision{
		Decision: "secret:put",
		Reason:   strings.TrimSpace(req.Rationale) + " [by " + req.Operator + "]",
		ActionID: "secret:" + req.Name + ":" + hex.EncodeToString(sum[:6]),
		Withheld: false,
	})
	if err != nil {
		return SecretResult{}, fmt.Errorf("ledger append: %w", err)
	}
	sv, err := schema.Current(schema.TableSealedSecret)
	if err != nil {
		return SecretResult{}, err
	}
	blob := seal.Sealed{Ciphertext: req.Ciphertext, Nonce: req.Nonce, WrappedDEK: req.WrappedDEK, DEKNonce: req.DEKNonce}
	if err := a.D.Secrets.Put(ctx, req.Name, blob, req.Purpose, req.Operator, entry.Seq, int(sv)); err != nil {
		return SecretResult{}, err
	}
	return SecretResult{Name: req.Name, Ref: "store:" + req.Name, LedgerSeq: entry.Seq}, nil
}

// activityOpts: no retries — a refused write (LAW key, bad value, missing rationale) is a DECISION,
// not a transient; it surfaces verbatim (mirrors temporal/skillwrite).
func activityOpts() workflow.ActivityOptions {
	return workflow.ActivityOptions{
		StartToCloseTimeout: 15 * time.Second,
		RetryPolicy:         &temporal.RetryPolicy{MaximumAttempts: 1},
	}
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
