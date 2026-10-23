package db

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/jackc/pgx/v5"

	"github.com/territory-grounder/grounder/core/manifest"
	"github.com/territory-grounder/grounder/core/safety"
)

// BackfillLifecycle updates a SEALED action_manifest's non-hashed LIFECYCLE columns — approval_choice (the
// human vote outcome) and verdict (the post-execution mechanical verdict) — for the decision tracer (spec/020
// T-020-4, REQ-2006). The Runner seals the manifest at propose time (action/band/plan_hash/prediction_hash),
// but the approval choice and the verdict are only known LATER (after the vote and after verify), so they were
// left NULL and the tracer could not show how an action resolved.
//
// SAFE against the append-only binding: the content-hash Assert (INV-07) re-derives from action + band +
// plan_hash + prediction_hash ONLY — approval_choice and verdict are OUTSIDE that hash, so updating them cannot
// tamper the sealed identity (a reloaded manifest still re-asserts). action_manifest is NOT on the 0015
// append-only REVOKE set (unlike the governance_ledger/session_risk_audit/action_verdict spine), so the DML
// runtime role holds UPDATE here by design. COALESCE(NULLIF) semantics: an empty input leaves the column
// UNCHANGED, so a later verdict backfill never erases an earlier approval_choice backfill and vice-versa; a
// non-`match/partial/deviation` verdict writes nothing (still unverified). NON-SECRET: both are governance
// labels, never argv/host/credential.
// externalRef (TG-532) is the SESSION whose resolution each written label describes. It is recorded beside
// the label because action_id is per-SHAPE and up to 198 sessions share one row here: a bare "approved"
// answers "which session?" with silence, and a reader fills that silence with the session in front of them.
func (s *ManifestStore) BackfillLifecycle(ctx context.Context, actionID, externalRef, approvalChoice string, verdict safety.Verdict) error {
	if actionID == "" {
		return errors.New("db: manifest lifecycle backfill with empty action_id refused")
	}
	var aptr *string
	if approvalChoice != "" {
		aptr = &approvalChoice
	}
	var vptr *string
	if v := string(verdict); v == "match" || v == "partial" || v == "deviation" {
		vptr = &v
	}
	if aptr == nil && vptr == nil {
		return nil // nothing to backfill (no vote outcome, no verified verdict yet)
	}
	// Each *_ref moves with — and only with — its own label: the CASE keys on the same NULL the COALESCE
	// does, so a verdict backfill never restamps the approval's owner and vice-versa. An unknown session
	// writes '' (honest absence) rather than borrowing the row's previous owner.
	_, err := s.p.Exec(ctx, `
		UPDATE action_manifest
		SET approval_choice = COALESCE($2, approval_choice),
		    approval_ref    = CASE WHEN $2::text IS NULL THEN approval_ref ELSE $4::text END,
		    verdict         = COALESCE($3::verdict, verdict),
		    verdict_ref     = CASE WHEN $3::text IS NULL THEN verdict_ref  ELSE $4::text END
		WHERE action_id = $1`,
		actionID, aptr, vptr, strings.TrimSpace(externalRef))
	if err != nil {
		return fmt.Errorf("db: backfill manifest lifecycle %s: %w", actionID, err)
	}
	return nil
}

// ManifestLifecycle is the pair of non-hashed lifecycle labels backfilled onto a sealed manifest (REQ-2006):
// the human approval choice and the post-execution mechanical verdict. Empty strings = not-yet-known.
type ManifestLifecycle struct {
	ApprovalChoice string
	Verdict        string
	// ApprovalRef / VerdictRef name the SESSION each label describes (TG-532). '' means the label predates
	// migration 0110 or is unset — its owner is unrecoverable, which the surface must say rather than
	// imply the reader's own session.
	ApprovalRef string
	VerdictRef  string
}

// Lifecycle reads back a sealed manifest's approval_choice + verdict (the tracer surface / round-trip check).
// Read-only, parameter-bound; ok=false when the action_id has no manifest row. NULL columns read as "".
func (s *ManifestStore) Lifecycle(ctx context.Context, actionID string) (ManifestLifecycle, bool, error) {
	var lc ManifestLifecycle
	err := s.p.QueryRow(ctx,
		`SELECT COALESCE(approval_choice,''), COALESCE(verdict::text,''), approval_ref, verdict_ref
		   FROM action_manifest WHERE action_id = $1`, actionID).
		Scan(&lc.ApprovalChoice, &lc.Verdict, &lc.ApprovalRef, &lc.VerdictRef)
	switch {
	case err == nil:
		return lc, true, nil
	case errors.Is(err, pgx.ErrNoRows):
		return ManifestLifecycle{}, false, nil
	default:
		return ManifestLifecycle{}, false, fmt.Errorf("db: manifest lifecycle read %s: %w", actionID, err)
	}
}

// MemManifestStore is the in-memory ManifestSink + backfill twin for the CI oracles (no Postgres): it seals a
// manifest, backfills its lifecycle labels, and reads them back — so an acceptance test proves the seal→
// backfill→read seam carries approval_choice + verdict WITHOUT a database. The pgx round-trip
// (manifest_backfill_test.go, DSN-gated) proves the real SQL. Concurrency-safe. Only lifecycle labels are
// stored per action_id; the sealed manifest identity is recorded by action_id.
type MemManifestStore struct {
	mu     sync.Mutex
	sealed map[string]bool
	lc     map[string]ManifestLifecycle
}

// NewMemManifestStore returns an empty in-memory manifest twin.
func NewMemManifestStore() *MemManifestStore {
	return &MemManifestStore{sealed: map[string]bool{}, lc: map[string]ManifestLifecycle{}}
}

// Seal records the sealed action id (first-wins, mirroring the pgx ON CONFLICT DO NOTHING).
func (m *MemManifestStore) Seal(_ context.Context, mf *manifest.ActionManifest) error {
	if mf == nil || mf.ActionID == "" {
		return errors.New("db: cannot seal a nil/unbound manifest")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.sealed[mf.ActionID] {
		m.sealed[mf.ActionID] = true
	}
	return nil
}

// BackfillLifecycle applies the same COALESCE semantics as the pgx writer: empty inputs leave the column
// unchanged, a non-match/partial/deviation verdict writes nothing.
func (m *MemManifestStore) BackfillLifecycle(_ context.Context, actionID, externalRef, approvalChoice string, verdict safety.Verdict) error {
	if actionID == "" {
		return errors.New("db: manifest lifecycle backfill with empty action_id refused")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	cur := m.lc[actionID]
	ref := strings.TrimSpace(externalRef)
	if approvalChoice != "" {
		cur.ApprovalChoice, cur.ApprovalRef = approvalChoice, ref
	}
	if v := string(verdict); v == "match" || v == "partial" || v == "deviation" {
		cur.Verdict, cur.VerdictRef = v, ref
	}
	m.lc[actionID] = cur
	return nil
}

// Lifecycle reads back the recorded labels (ok=false when the action_id was never sealed).
func (m *MemManifestStore) Lifecycle(_ context.Context, actionID string) (ManifestLifecycle, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.sealed[actionID] {
		return ManifestLifecycle{}, false, nil
	}
	return m.lc[actionID], true, nil
}
