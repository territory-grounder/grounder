package db

import (
	"context"
	"fmt"
	"strings"
	"sync"

	"github.com/territory-grounder/grounder/core/credential"
)

// CredentialResolutionWriteStore is the pgx-backed credential.ResolutionSink: it appends one immutable
// credential_resolution row per Resolve (spec/016 REQ-1617, migration 0018). Parameters are always bound
// ($1) — no string-built SQL. Only NON-SECRET identity metadata is written — the credential.ResolutionEvent
// type is secret-free by construction (INV-13): it can carry the resolved user, the connection scheme, and
// the key reference SCHEME (env/file/...), but never key material and never a full SecretRef value. The
// runtime role holds no UPDATE/DELETE on the table (0018 REVOKE), so an appended resolution is tamper-
// resistant like the rest of the accountability spine.
type CredentialResolutionWriteStore struct{ p *Pool }

// NewCredentialResolutionWriteStore returns the Postgres-backed resolution-audit writer.
func NewCredentialResolutionWriteStore(p *Pool) *CredentialResolutionWriteStore {
	return &CredentialResolutionWriteStore{p: p}
}

// AppendResolution inserts one credential_resolution row. It writes ONLY the non-secret ResolutionEvent
// fields; the shadowed-source list is joined into a comma-separated non-secret text. A write error is
// returned (the resolver treats the append as best-effort and never fails an authorized target on a
// projection error).
func (s *CredentialResolutionWriteStore) AppendResolution(ctx context.Context, ev credential.ResolutionEvent) error {
	_, err := s.p.Pool.Exec(ctx, `
		INSERT INTO credential_resolution
			(target, plane, outcome, source, native, rule_id, resolved_user, scheme, key_ref_scheme, shadowed, err, external_ref, schema_version)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, 1)`,
		ev.Target, string(ev.Plane), string(ev.Outcome), ev.Source, ev.Native, ev.RuleID,
		ev.User, string(ev.Scheme), ev.KeyRefScheme, strings.Join(ev.Shadowed, ","), ev.Err, ev.ExternalRef)
	if err != nil {
		return fmt.Errorf("db: credential_resolution insert (target %q): %w", ev.Target, err)
	}
	return nil
}

// MemCredentialResolutionStore is the in-memory credential.ResolutionSink twin for the CI oracles (no
// Postgres). It records every appended event so a test can assert exactly which non-secret fields were
// written — and that no secret (key material or a full ref value) ever leaks into a row. Concurrency-safe.
type MemCredentialResolutionStore struct {
	mu     sync.Mutex
	Events []credential.ResolutionEvent
}

// NewMemCredentialResolutionStore returns an empty in-memory resolution-audit twin.
func NewMemCredentialResolutionStore() *MemCredentialResolutionStore {
	return &MemCredentialResolutionStore{}
}

// AppendResolution records the event (append).
func (m *MemCredentialResolutionStore) AppendResolution(_ context.Context, ev credential.ResolutionEvent) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.Events = append(m.Events, ev)
	return nil
}

// compile-time proof both implementations satisfy the sink interface.
var (
	_ credential.ResolutionSink = (*CredentialResolutionWriteStore)(nil)
	_ credential.ResolutionSink = (*MemCredentialResolutionStore)(nil)
)
