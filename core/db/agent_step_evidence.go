package db

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/territory-grounder/grounder/core/safety"
	"github.com/territory-grounder/grounder/core/trace"
)

// ErrEvidenceNotFound is trace.ErrEvidenceNotFound, aliased so db-side callers need not reach across packages
// for it. It is a REAL and expected answer, not a fault: every session recorded before migration 0053 has a
// walk and no evidence behind it, and the console must be able to say "this walk predates evidence capture"
// rather than rendering an error or, worse, an empty body that reads as "the tool returned nothing".
var ErrEvidenceNotFound = trace.ErrEvidenceNotFound

// AgentStepEvidenceStore is the pgx-backed writer/reader for agent_step_evidence (migration 0053) — the
// SCREENED, SCRUBBED tool output behind each recorded reasoning step (TG-272). Append-only: the runtime role
// holds no UPDATE/DELETE, so evidence an operator audits cannot be rewritten after the fact.
type AgentStepEvidenceStore struct{ p *Pool }

// NewAgentStepEvidenceStore returns the Postgres-backed evidence store.
func NewAgentStepEvidenceStore(p *Pool) *AgentStepEvidenceStore { return &AgentStepEvidenceStore{p: p} }

var (
	_ trace.AgentStepEvidenceSink   = (*AgentStepEvidenceStore)(nil)
	_ trace.AgentStepEvidenceReader = (*AgentStepEvidenceStore)(nil)
	// The durable read record is also what the read-lane recon meter seeds its rolling hour from at boot
	// (TG-165) — so a worker restart does not hand whatever was mid-burst a brand-new hour.
	_ safety.ReconLedger = (*AgentStepEvidenceStore)(nil)
)

// maxReconSeedRows bounds ReadsSince. The recon window is compared against a bound in the hundreds, so any
// answer past this is "far over budget" and reading more rows would only make a hot boot slower at exactly
// the moment the estate is being enumerated. The seed is a floor either way (see safety.SeedFromLedger).
const maxReconSeedRows = 10000

// ReadsSince returns the timestamps of the recorded reads at or after `since`, oldest first — the seed for
// the read-lane recon budget (TG-165, satisfying safety.ReconLedger).
//
// Timestamps, not a count: placing every seeded read at one instant would either park an hour of history at
// `now` (over-binding a freshly booted worker for a full hour) or at `since` (draining instantly, which is
// no seed at all). The rows are cheap — created_at is indexed by migration 0055 for the reaper — and the
// window is bounded by both the interval and maxReconSeedRows.
func (s *AgentStepEvidenceStore) ReadsSince(ctx context.Context, since time.Time) ([]time.Time, error) {
	rows, err := s.p.Query(ctx, `
		SELECT created_at FROM agent_step_evidence
		WHERE created_at >= $1
		ORDER BY created_at ASC
		LIMIT $2`, since.UTC(), maxReconSeedRows)
	if err != nil {
		return nil, fmt.Errorf("db: agent_step_evidence reads since %s: %w", since.UTC().Format(time.RFC3339), err)
	}
	defer rows.Close()
	var out []time.Time
	for rows.Next() {
		var at time.Time
		if serr := rows.Scan(&at); serr != nil {
			return nil, fmt.Errorf("db: agent_step_evidence reads since %s: scan: %w", since.UTC().Format(time.RFC3339), serr)
		}
		out = append(out, at)
	}
	if rerr := rows.Err(); rerr != nil {
		return nil, fmt.Errorf("db: agent_step_evidence reads since %s: %w", since.UTC().Format(time.RFC3339), rerr)
	}
	return out, nil
}

// EmitEvidence appends one evidence row, bounding the payload and recording that it did so.
//
// ON CONFLICT DO NOTHING because Temporal RETRIES the investigate activity: the same cycle can legitimately
// emit the same observation twice, and a duplicate-key error on a best-effort side effect would produce a
// worrying log line about a completely correct situation. The first write wins, which is the right one — the
// table is append-only precisely so a later write cannot revise recorded evidence.
func (s *AgentStepEvidenceStore) EmitEvidence(ctx context.Context, e trace.AgentStepEvidence) error {
	if e.ExternalRef == "" {
		return fmt.Errorf("db: agent_step_evidence with empty external_ref refused")
	}
	if e.EvidenceID == "" {
		return fmt.Errorf("db: agent_step_evidence with empty evidence_id refused")
	}
	payload, truncated, full := trace.Truncate(e.Payload)
	_, err := s.p.Exec(ctx, `
		INSERT INTO agent_step_evidence
			(external_ref, cycle, evidence_id, tool, payload, truncated, full_bytes, schema_version)
		VALUES ($1, $2, $3, $4, $5, $6, $7, 1)
		ON CONFLICT (external_ref, cycle, evidence_id) DO NOTHING`,
		e.ExternalRef, e.Cycle, e.EvidenceID, e.Tool, payload, truncated, full)
	if err != nil {
		return fmt.Errorf("db: agent_step_evidence insert %s#%d/%s: %w", e.ExternalRef, e.Cycle, e.EvidenceID, err)
	}
	return nil
}

// Evidence reads one stored observation. A missing row is ErrEvidenceNotFound, never a zero-value record —
// the console renders "not recorded" and "the tool returned nothing" completely differently, and collapsing
// them here would make that distinction unrecoverable upstream.
func (s *AgentStepEvidenceStore) Evidence(ctx context.Context, externalRef, evidenceID string) (trace.AgentStepEvidence, error) {
	var e trace.AgentStepEvidence
	err := s.p.QueryRow(ctx, `
		SELECT external_ref, cycle, evidence_id, tool, payload, truncated, full_bytes
		FROM agent_step_evidence WHERE external_ref = $1 AND evidence_id = $2
		ORDER BY cycle ASC LIMIT 1`, externalRef, evidenceID).
		Scan(&e.ExternalRef, &e.Cycle, &e.EvidenceID, &e.Tool, &e.Payload, &e.Truncated, &e.FullBytes)
	if errors.Is(err, pgx.ErrNoRows) {
		return trace.AgentStepEvidence{}, ErrEvidenceNotFound
	}
	if err != nil {
		return trace.AgentStepEvidence{}, fmt.Errorf("db: agent_step_evidence read %s/%s: %w", externalRef, evidenceID, err)
	}
	return e, nil
}

// MemAgentStepEvidenceStore is the in-memory twin for oracles that need no Postgres. It applies the SAME
// truncation as the pgx store — a twin that stores what the real one would reject teaches a test to pass on
// behaviour production does not have.
type MemAgentStepEvidenceStore struct {
	mu   sync.Mutex
	rows map[string]trace.AgentStepEvidence
	// at is the append order's wall-clock, mirroring the pgx store's created_at DEFAULT now(). It exists so
	// the twin can answer ReadsSince: a twin that cannot answer a query the real store answers sends its
	// oracle to Postgres, and the recon seed (TG-165) is exactly the kind of thing that must be provable
	// without a database.
	at []time.Time
	// Now is the twin's clock, swappable so a seed oracle can place recorded reads in the past.
	Now func() time.Time
}

// NewMemAgentStepEvidenceStore returns an empty in-memory evidence twin.
func NewMemAgentStepEvidenceStore() *MemAgentStepEvidenceStore {
	return &MemAgentStepEvidenceStore{rows: map[string]trace.AgentStepEvidence{}, Now: time.Now}
}

var (
	_ trace.AgentStepEvidenceSink   = (*MemAgentStepEvidenceStore)(nil)
	_ trace.AgentStepEvidenceReader = (*MemAgentStepEvidenceStore)(nil)
	_ safety.ReconLedger            = (*MemAgentStepEvidenceStore)(nil)
)

// ReadsSince mirrors the pgx store's seed read: the timestamps of recorded reads at or after `since`,
// oldest first (the rows are appended in order, so they are already sorted).
func (m *MemAgentStepEvidenceStore) ReadsSince(_ context.Context, since time.Time) ([]time.Time, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []time.Time
	for _, at := range m.at {
		if !at.Before(since) {
			out = append(out, at)
		}
	}
	return out, nil
}

func memEvidenceKey(ref, id string) string { return ref + "\x00" + id }

// EmitEvidence records one bounded evidence row; first write wins, mirroring ON CONFLICT DO NOTHING.
func (m *MemAgentStepEvidenceStore) EmitEvidence(_ context.Context, e trace.AgentStepEvidence) error {
	if e.ExternalRef == "" || e.EvidenceID == "" {
		return fmt.Errorf("db: agent_step_evidence with empty external_ref or evidence_id refused")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	k := memEvidenceKey(e.ExternalRef, e.EvidenceID)
	if _, dup := m.rows[k]; dup {
		return nil
	}
	e.Payload, e.Truncated, e.FullBytes = trace.Truncate(e.Payload)
	m.rows[k] = e
	now := time.Now
	if m.Now != nil {
		now = m.Now
	}
	m.at = append(m.at, now())
	return nil
}

// Evidence reads one recorded observation, or ErrEvidenceNotFound.
func (m *MemAgentStepEvidenceStore) Evidence(_ context.Context, externalRef, evidenceID string) (trace.AgentStepEvidence, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	e, ok := m.rows[memEvidenceKey(externalRef, evidenceID)]
	if !ok {
		return trace.AgentStepEvidence{}, ErrEvidenceNotFound
	}
	return e, nil
}
