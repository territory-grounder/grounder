package db

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/territory-grounder/grounder/core/manifest"
	"github.com/territory-grounder/grounder/core/safety"
)

// ManifestStore is the pgx-backed, APPEND-ONLY writer for action_manifest (migration 0001) — the immutable
// content-hashed action binding (INV-07). A sealed manifest is written once (PK action_id, first-wins) and
// never mutated; on read, Assert re-derives the action id so a tampered persisted row is rejected. The
// staged lifecycle chain (Provenance/Stages) is an in-process INV-07 verification structure; the core sealed
// manifest is persisted here for cross-session audit.
type ManifestStore struct{ p *Pool }

// NewManifestStore returns a Postgres-backed action-manifest writer.
func NewManifestStore(p *Pool) *ManifestStore { return &ManifestStore{p: p} }

// bandText / bandOf map the typed band to the DB enum text and back. An unknown stored value fails SAFE to
// POLL_PAUSE (the most restrictive band), never silently to an auto band.
func bandText(b safety.Band) string { return b.String() }

func bandOf(text string) safety.Band {
	switch text {
	case "AUTO":
		return safety.BandAuto
	case "AUTO_NOTICE":
		return safety.BandAutoNotice
	default:
		return safety.BandPollPause
	}
}

// Seal persists a sealed manifest. A duplicate action_id is ignored (append-only, first-wins) — the sealed
// action binding is immutable.
func (s *ManifestStore) Seal(ctx context.Context, m *manifest.ActionManifest) error {
	if m == nil || m.ActionID == "" {
		return errors.New("db: cannot seal a nil/unbound manifest")
	}
	actionJSON, err := json.Marshal(m.Action)
	if err != nil {
		return fmt.Errorf("db: marshal action: %w", err)
	}
	_, err = s.p.Exec(ctx, `
		INSERT INTO action_manifest (action_id, action, band, plan_hash, prediction_hash)
		VALUES ($1, $2::jsonb, $3, $4, $5)
		ON CONFLICT (action_id) DO NOTHING`,
		m.ActionID, string(actionJSON), bandText(m.Band), m.PlanHash, m.PredictionHash)
	if err != nil {
		return fmt.Errorf("db: seal manifest %s: %w", m.ActionID, err)
	}
	return nil
}

// Get reads back a sealed manifest and RE-ASSERTS its content hash — a persisted row whose action was
// tampered fails the Assert, so a corrupted binding never reads back as authorized (INV-07).
func (s *ManifestStore) Get(ctx context.Context, actionID string) (*manifest.ActionManifest, bool, error) {
	var (
		actionJSON               []byte
		band, planHash, predHash string
	)
	err := s.p.QueryRow(ctx,
		"SELECT action, band, COALESCE(plan_hash,''), COALESCE(prediction_hash,'') FROM action_manifest WHERE action_id = $1", actionID).
		Scan(&actionJSON, &band, &planHash, &predHash)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, false, nil
		}
		return nil, false, fmt.Errorf("db: get manifest %s: %w", actionID, err)
	}
	var action manifest.Action
	if err := json.Unmarshal(actionJSON, &action); err != nil {
		return nil, false, fmt.Errorf("db: unmarshal action %s: %w", actionID, err)
	}
	// Rehydrate re-derives and verifies the content hash and returns a SEALED manifest; a hand-built struct
	// literal here cannot set the unexported `sealed` flag, so Assert would refuse every persisted row.
	m, err := manifest.Rehydrate(actionID, action, bandOf(band), planHash, predHash)
	if err != nil {
		return nil, false, fmt.Errorf("db: persisted manifest failed re-assert: %w", err)
	}
	return m, true, nil
}
