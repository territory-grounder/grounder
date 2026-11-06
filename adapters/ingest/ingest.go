// Package ingest is the stable interface for the ingest module surface: a connector that authenticates to
// an event source, validates a raw payload against an explicit grammar, and normalizes it to the one
// canonical triage shape (core/ingest.IncidentEnvelope) before any model dispatch.
//
// Provenance: [O] INV-04 (validate against an explicit grammar), INV-05 (re-read the entity from its
// system of record), INV-18 (one implementation per source type), spec/008. Implementations live under
// modules/ingest/<source>/; each is a thin normalizer, never a place where payload content becomes control
// flow.
package ingest

import (
	"context"

	coreingest "github.com/territory-grounder/grounder/core/ingest"
)

// Ingester normalizes a raw source payload into the canonical triage envelope. The payload is a claim
// validated against an explicit grammar and mapped by field — it never drives control flow.
type Ingester interface {
	// SourceType is the source/vendor slug (e.g. "librenms", "prometheus-alertmanager", "crowdsec"). One
	// implementation serves every deployment of that source; per-site variance is configuration (INV-18).
	SourceType() string
	// Normalize validates a raw payload against the source's explicit grammar and maps it to the canonical
	// triage envelope. A payload that does not satisfy the grammar is rejected (INV-04) — never coerced.
	Normalize(ctx context.Context, raw []byte) (coreingest.IncidentEnvelope, error)
}

// BatchIngester is the OPTIONAL batch extension of Ingester for sources whose transport groups multiple
// alerts into one payload (Alertmanager webhooks group by alertname by default). The front door type-asserts
// it and fans each returned envelope out to its own idempotent triage session; sources without it keep the
// one-envelope path. Grammar discipline is per alert: a malformed webhook is rejected whole, while a single
// alert failing the grammar is rejected individually without discarding its well-formed siblings.
type BatchIngester interface {
	Ingester
	NormalizeBatch(ctx context.Context, raw []byte) ([]coreingest.IncidentEnvelope, error)
}
