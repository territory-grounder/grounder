package main

import (
	"context"
	"fmt"
	"strings"
	"time"

	enumspb "go.temporal.io/api/enums/v1"
	"go.temporal.io/sdk/client"

	"github.com/territory-grounder/grounder/core/cpconfig"
	"github.com/territory-grounder/grounder/core/db"
	"github.com/territory-grounder/grounder/core/httpapi"
	"github.com/territory-grounder/grounder/core/seal"
	tg "github.com/territory-grounder/grounder/temporal"
	"github.com/territory-grounder/grounder/temporal/configwrite"
)

// configWriteBackend implements httpapi.ConfigWriter (task #27 Phase C, REQ-523): the grounder never
// appends to the hash chain itself — every override executes in the WORKER via the distinctly-named
// configwrite.ConfigWriteWorkflow (ledger append BEFORE the row commits, single writer).
type configWriteBackend struct {
	tc client.Client
}

func (b configWriteBackend) WriteConfig(ctx context.Context, key, value, rationale, operator string) (httpapi.ConfigWriteOutcome, error) {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	run, err := b.tc.ExecuteWorkflow(ctx, client.StartWorkflowOptions{
		ID:        fmt.Sprintf("tg/configwrite/%s", key),
		TaskQueue: tg.TaskQueueRunner,
		// A completed same-id run may repeat (the same key is legitimately re-written later); an
		// IN-FLIGHT duplicate is a double console click, rejected by Temporal's running-dedup.
		WorkflowIDReusePolicy: enumspb.WORKFLOW_ID_REUSE_POLICY_ALLOW_DUPLICATE,
	}, configwrite.ConfigWriteWorkflow, configwrite.ConfigRequest{Key: key, Value: value, Rationale: rationale, Operator: operator})
	if err != nil {
		return httpapi.ConfigWriteOutcome{}, err
	}
	var res configwrite.ConfigResult
	if err := run.Get(ctx, &res); err != nil {
		return httpapi.ConfigWriteOutcome{}, unwrapConfigErr(err)
	}
	return httpapi.ConfigWriteOutcome{Key: res.Key, Value: res.Value, Source: "console", LedgerSeq: res.LedgerSeq}, nil
}

// ClearConfig removes an override through the SAME worker lane (TG-261). The response reports Source
// "env", because that is what the key resolves from once the override is gone — the honest answer to
// "where does this value come from now?", not merely "the row is deleted".
func (b configWriteBackend) ClearConfig(ctx context.Context, key, rationale, operator string) (httpapi.ConfigWriteOutcome, error) {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	run, err := b.tc.ExecuteWorkflow(ctx, client.StartWorkflowOptions{
		ID:                    fmt.Sprintf("tg/configclear/%s", key),
		TaskQueue:             tg.TaskQueueRunner,
		WorkflowIDReusePolicy: enumspb.WORKFLOW_ID_REUSE_POLICY_ALLOW_DUPLICATE,
	}, configwrite.ConfigWriteWorkflow, configwrite.ConfigRequest{Key: key, Rationale: rationale, Operator: operator, Clear: true})
	if err != nil {
		return httpapi.ConfigWriteOutcome{}, err
	}
	var res configwrite.ConfigResult
	if err := run.Get(ctx, &res); err != nil {
		return httpapi.ConfigWriteOutcome{}, unwrapConfigErr(err)
	}
	return httpapi.ConfigWriteOutcome{Key: res.Key, Source: "env", LedgerSeq: res.LedgerSeq}, nil
}

// unwrapConfigErr maps a workflow-wrapped refusal back onto the typed cpconfig errors so the surface
// returns the honest status (a Temporal ApplicationError carries only the message) — the same
// longest-message-first discipline as the skill-write backend.
func unwrapConfigErr(err error) error {
	msg := err.Error()
	for _, known := range []error{
		configwrite.ErrRationaleRequired, cpconfig.ErrLawPinned, cpconfig.ErrNotWritable,
		cpconfig.ErrValueBounds, cpconfig.ErrUnknownKey,
	} {
		if strings.Contains(msg, known.Error()) {
			return fmt.Errorf("%w (worker refused)", known)
		}
	}
	return err
}

// secretsWriteBackend implements httpapi.SealedSecretWriter (task #27 Phase D, REQ-524): the value is
// SEALED HERE — envelope-encrypted with the master key resolved per write and discarded — so only
// ciphertext enters the workflow (Temporal history holds no plaintext, INV-13), and the worker
// ledgers name+digest before the row commits.
type secretsWriteBackend struct {
	tc     client.Client
	sealer *seal.Sealer // built from config: OpenBao Transit (master key off the worker) or the in-process master key
}

func (b secretsWriteBackend) PutSecret(ctx context.Context, name, value, purpose, rationale, operator string) (httpapi.SecretPutOutcome, error) {
	if b.sealer == nil {
		return httpapi.SecretPutOutcome{}, fmt.Errorf("sealing is not configured (fail closed)")
	}
	blob, err := b.sealer.Seal(name, []byte(value))
	if err != nil {
		return httpapi.SecretPutOutcome{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	run, err := b.tc.ExecuteWorkflow(ctx, client.StartWorkflowOptions{
		ID:                    fmt.Sprintf("tg/secretput/%s", name),
		TaskQueue:             tg.TaskQueueRunner,
		WorkflowIDReusePolicy: enumspb.WORKFLOW_ID_REUSE_POLICY_ALLOW_DUPLICATE,
	}, configwrite.SecretPutWorkflow, configwrite.SecretRequest{
		Name: name, Ciphertext: blob.Ciphertext, Nonce: blob.Nonce,
		WrappedDEK: blob.WrappedDEK, DEKNonce: blob.DEKNonce,
		Purpose: purpose, Rationale: rationale, Operator: operator,
	})
	if err != nil {
		return httpapi.SecretPutOutcome{}, err
	}
	var res configwrite.SecretResult
	if err := run.Get(ctx, &res); err != nil {
		return httpapi.SecretPutOutcome{}, err
	}
	return httpapi.SecretPutOutcome{Name: res.Name, Ref: res.Ref, LedgerSeq: res.LedgerSeq}, nil
}

// sealRewrapBackend implements httpapi.SealRewrapper (TG-163): the operator-driven re-key of the sealed
// store onto the CURRENT master-key version.
//
// It carries NO sealer, unlike secretsWriteBackend above, and that asymmetry is the point. A secret write
// seals in the grounder so plaintext never enters Temporal history. A rewrap has no plaintext to protect —
// it moves key-side ciphertext only — and it MUST run where the sealed store's single writer lives, or it
// becomes a second writer racing PutSecretActivity over the same rows. So the grounder starts the workflow
// and waits; the worker does all of it.
type sealRewrapBackend struct{ tc client.Client }

func (b sealRewrapBackend) RewrapSeals(ctx context.Context, rationale, after string, limit int, operator string) (httpapi.SealRewrapOutcome, error) {
	// Longer than the other write lanes' 20s: this walks every sealed row with a key-service round trip
	// each. It matches the activity's own StartToCloseTimeout, so the operator's request does not give up
	// on work that is still running and about to be recorded.
	ctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	run, err := b.tc.ExecuteWorkflow(ctx, client.StartWorkflowOptions{
		ID:        "tg/secretrewrap",
		TaskQueue: tg.TaskQueueRunner,
		// REJECT_DUPLICATE would block a legitimate resume after an interrupted run. ALLOW_DUPLICATE lets
		// the operator re-post; concurrent runs are safe by construction anyway, because every row update
		// is conditional on the exact bytes that run read (db.SealedSecretStore.RewrapDEK).
		WorkflowIDReusePolicy: enumspb.WORKFLOW_ID_REUSE_POLICY_ALLOW_DUPLICATE,
	}, configwrite.SecretRewrapWorkflow, configwrite.RewrapRequest{
		Rationale: rationale, Operator: operator, AfterName: after, Limit: limit,
	})
	if err != nil {
		return httpapi.SealRewrapOutcome{}, err
	}
	var res configwrite.RewrapOutcome
	if err := run.Get(ctx, &res); err != nil {
		return httpapi.SealRewrapOutcome{}, err
	}
	return httpapi.SealRewrapOutcome{
		Scanned: res.Scanned, Rewrapped: res.Rewrapped, Skipped: res.Skipped,
		LastName: res.LastName, LedgerSeq: res.LedgerSeq, Partial: res.Partial,
		Versions: res.Versions, Note: res.Note,
	}, nil
}

// sealedReadStore adapts the sealed store's value-less inventory to the /v1/secrets read surface
// (REQ-524): names + metadata + the store:<name> reference — the DTO has no value field at all.
type sealedReadStore struct {
	s *db.SealedSecretStore
}

func (r sealedReadStore) SealedSecrets(ctx context.Context) ([]httpapi.SealedSecretInfo, error) {
	rows, err := r.s.List(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]httpapi.SealedSecretInfo, 0, len(rows))
	for _, row := range rows {
		out = append(out, httpapi.SealedSecretInfo{
			Name: row.Name, Ref: "store:" + row.Name, Purpose: row.Purpose,
			CreatedAt: row.CreatedAt, UpdatedAt: row.UpdatedAt,
		})
	}
	return out, nil
}
