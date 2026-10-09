package db

import (
	"context"
	"fmt"

	"github.com/territory-grounder/grounder/core/auth"
	"github.com/territory-grounder/grounder/core/config"
)

// SourceResolver implements core/auth.SourceResolver against the sources table. The HMAC secret is
// stored as a REFERENCE (hmac_secret_ref) and resolved in memory here — the literal secret is never
// in the database, code, or logs [O] INV-13.
type SourceResolver struct{ p *Pool }

// NewSourceResolver returns a Postgres-backed source resolver.
func NewSourceResolver(p *Pool) *SourceResolver { return &SourceResolver{p: p} }

// LookupSource maps a source_id to its resolved HMAC secret.
func (s *SourceResolver) LookupSource(ctx context.Context, sourceID string) (auth.Source, error) {
	var hmacRef, tokenRef *string
	err := s.p.QueryRow(ctx,
		"SELECT hmac_secret_ref, ingest_token_ref FROM sources WHERE source_id = $1", sourceID).
		Scan(&hmacRef, &tokenRef)
	if err != nil {
		return auth.Source{}, fmt.Errorf("db: source %q: %w", sourceID, err)
	}
	// Each credential is optional (0008: a push source may be token-only, a signer secret-only). A missing
	// ref leaves that credential empty, so the corresponding auth path fails closed for this source.
	src := auth.Source{SourceID: sourceID}
	if hmacRef != nil && *hmacRef != "" {
		secret, err := config.SecretRef(*hmacRef).Resolve()
		if err != nil {
			return auth.Source{}, fmt.Errorf("db: source %q secret: %w", sourceID, err)
		}
		src.HMACSecret = []byte(secret)
	}
	if tokenRef != nil && *tokenRef != "" {
		// A token ref that fails to resolve leaves the token EMPTY — the bearer path fails closed for
		// this source — rather than erroring the whole lookup, which would take down the source's
		// HMAC/mTLS path too. The optional new credential must never break the stronger existing one.
		if token, terr := config.SecretRef(*tokenRef).Resolve(); terr == nil {
			src.IngestToken = []byte(token)
		}
	}
	return src, nil
}

// UpsertSource idempotently records a bearer-authenticated push-ingest source, keyed by source_id and
// storing ONLY the token REFERENCE (env:/file:/store:, INV-13) — never the literal token, which stays in
// the referenced secret store. A bearer-only source leaves hmac_secret_ref NULL; the 0008 CHECK
// (hmac_secret_ref IS NOT NULL OR ingest_token_ref IS NOT NULL) is satisfied by the token ref alone. On
// conflict the ref is refreshed so a rotated ref re-provisions in place. This lets a boot step provision a
// push source REPRODUCIBLY (config-not-code) instead of a hand-run INSERT. Parameterized SQL only.
func (s *SourceResolver) UpsertSource(ctx context.Context, sourceID, ingestTokenRef string) error {
	_, err := s.p.Exec(ctx,
		`INSERT INTO sources (source_id, ingest_token_ref) VALUES ($1, $2)
		 ON CONFLICT (source_id) DO UPDATE SET ingest_token_ref = EXCLUDED.ingest_token_ref`,
		sourceID, ingestTokenRef)
	if err != nil {
		return fmt.Errorf("db: upsert source %q: %w", sourceID, err)
	}
	return nil
}
