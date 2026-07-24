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
