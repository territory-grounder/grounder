package groundnet

import (
	"context"
	"errors"
	"fmt"
)

// audit.go — the DURABLE emit/ingest trail of the groundnet seam (spec/021 T-021-8, REQ-2103/2104/2115,
// INV-19/INV-13). It fixes the row SHAPES and the append port; the pgx implementation that writes them lives
// in core/db (groundnet_audit_write.go) so this package stays pure, exactly as core/regime keeps its AuditSink
// pure and core/db owns the pgx writer. The MemAuditSink here backs the unit tests and the in-process e2e;
// the durable sink backs an armed node.
//
// Every recorded field is a KIND, a content-address, or a pseudonym — NEVER an estate identifier, a governance
// field, or a secret (REQ-2101). The producer is always a gnpub pseudonym (REQ-2103). The append-only DB
// guarantee (tg_runtime stripped of UPDATE/DELETE, migration 0113) makes the trail tamper-resistant (INV-19).

// EmitRecord is one immutable groundnet_emit row: a published wisdom unit's de-identified provenance.
type EmitRecord struct {
	Subject     string // sub — the statement content-address (a hash)
	ContentType string // the payload media type (a KIND)
	Issuer      string // iss — the producer pseudonym (gnpub:)
	KeyID       string // kid — the COSE key id binding iss
	Receipt     []byte // the Transparency Service Receipt (the provenance anchor)
	Retention   string // the declared retention class of the governed record
}

// IngestRecord is one immutable groundnet_ingest row: a foreign statement's verify + re-graduation outcome.
type IngestRecord struct {
	Subject      string // sub — the ingested statement's content-address
	Issuer       string // iss — the producer pseudonym (gnpub:)
	VerifyResult string // the COSE signature/envelope verify: VerifyVerified or VerifyRejected
	Disposition  string // the re-graduation disposition (a Disposition* constant)
}

// Verify results recorded for an ingest (REQ-2104). Kept coarse: the fine-grained rejection reason is the
// Disposition; VerifyResult only distinguishes a statement that verified from one that did not.
const (
	VerifyVerified = "verified"
	VerifyRejected = "rejected"
)

// AuditSink persists the emit/ingest records. Implemented durably by core/db (pgx, INSERT-only into the
// append-only 0113 tables); MemAuditSink is the in-memory fake. A sink NEVER updates or deletes — the trail
// is append-only by construction on both sides.
type AuditSink interface {
	AppendEmit(ctx context.Context, r EmitRecord) error
	AppendIngest(ctx context.Context, r IngestRecord) error
}

// ErrAuditRecordInvalid is returned when a record fails the same shape rules the 0113 CHECK constraints
// enforce at the database boundary — so a caller cannot persist an estate-bearing or malformed row even
// against a sink that does not itself re-check.
var ErrAuditRecordInvalid = errors.New("groundnet: audit record violates the de-identified append-only shape")

// Validate enforces the emit row invariants that mirror the 0113 CHECKs: a non-empty content-address and
// media type, a gnpub pseudonym issuer (REQ-2103), and a present Receipt (the provenance anchor, REQ-2105).
func (r EmitRecord) Validate() error {
	switch {
	case r.Subject == "":
		return fmt.Errorf("%w: empty subject", ErrAuditRecordInvalid)
	case r.ContentType == "":
		return fmt.Errorf("%w: empty content_type", ErrAuditRecordInvalid)
	case !hasPseudonymScheme(r.Issuer):
		return fmt.Errorf("%w: issuer %q is not a gnpub pseudonym (REQ-2103)", ErrAuditRecordInvalid, r.Issuer)
	case len(r.Receipt) == 0:
		return fmt.Errorf("%w: empty receipt (the provenance anchor is required, REQ-2105)", ErrAuditRecordInvalid)
	}
	return nil
}

// Validate enforces the ingest row invariants mirroring the 0113 CHECKs: a non-empty subject, a gnpub issuer,
// a known verify result, and the integrity tie — only a VERIFIED statement lands as a candidate, and every
// rejection carries a rejected-* disposition (REQ-2104/2109).
func (r IngestRecord) Validate() error {
	switch {
	case r.Subject == "":
		return fmt.Errorf("%w: empty subject", ErrAuditRecordInvalid)
	case !hasPseudonymScheme(r.Issuer):
		return fmt.Errorf("%w: issuer %q is not a gnpub pseudonym (REQ-2103)", ErrAuditRecordInvalid, r.Issuer)
	case r.VerifyResult != VerifyVerified && r.VerifyResult != VerifyRejected:
		return fmt.Errorf("%w: verify_result %q not in {verified,rejected}", ErrAuditRecordInvalid, r.VerifyResult)
	}
	// Integrity (mirrors the 0113 CHECK): landing as a candidate REQUIRES a verified statement. The converse
	// does NOT hold — a statement can verify yet be rejected downstream (a bad/absent receipt, a replay, an
	// unknown payload), so verified + a rejected-* disposition is legitimate.
	if r.Disposition == DispositionCandidate && r.VerifyResult != VerifyVerified {
		return fmt.Errorf("%w: a candidate landing requires a verified statement (verify=%q disposition=%q)",
			ErrAuditRecordInvalid, r.VerifyResult, r.Disposition)
	}
	return nil
}

func hasPseudonymScheme(s string) bool {
	return len(s) > len(PseudonymScheme) && s[:len(PseudonymScheme)] == PseudonymScheme
}

// Audit records emit/ingest outcomes to a sink after validating their shape. It anchors nothing itself — the
// Receipt IS the transparency-log anchor (translog.go) — it records the de-identified row and refuses a
// malformed one before it reaches the durable trail. It grants no authority and reaches no actuator.
type Audit struct {
	sink AuditSink
}

// NewAudit builds the recorder over a sink (MemAuditSink for tests/e2e, the durable pgx sink for an armed
// node). A nil sink yields a recorder whose Record* calls fail closed with ErrAuditNotConfigured — a dormant
// node records nothing rather than dropping a row silently.
func NewAudit(sink AuditSink) *Audit { return &Audit{sink: sink} }

// ErrAuditNotConfigured is returned when the recorder has no sink — the dormant, default-off posture.
var ErrAuditNotConfigured = errors.New("groundnet: audit sink not configured (dormant)")

// RecordEmit validates and appends one groundnet_emit row.
func (a *Audit) RecordEmit(ctx context.Context, r EmitRecord) error {
	if a == nil || a.sink == nil {
		return ErrAuditNotConfigured
	}
	if err := r.Validate(); err != nil {
		return err
	}
	return a.sink.AppendEmit(ctx, r)
}

// RecordIngest validates and appends one groundnet_ingest row.
func (a *Audit) RecordIngest(ctx context.Context, r IngestRecord) error {
	if a == nil || a.sink == nil {
		return ErrAuditNotConfigured
	}
	if err := r.Validate(); err != nil {
		return err
	}
	return a.sink.AppendIngest(ctx, r)
}

// MemAuditSink is the in-memory AuditSink fake: it retains the appended rows for assertion and is append-only
// (no update/delete API), mirroring the durable sink's shape.
type MemAuditSink struct {
	Emits   []EmitRecord
	Ingests []IngestRecord
}

// NewMemAuditSink returns an empty in-memory sink.
func NewMemAuditSink() *MemAuditSink { return &MemAuditSink{} }

// AppendEmit records an emit row.
func (m *MemAuditSink) AppendEmit(_ context.Context, r EmitRecord) error {
	m.Emits = append(m.Emits, r)
	return nil
}

// AppendIngest records an ingest row.
func (m *MemAuditSink) AppendIngest(_ context.Context, r IngestRecord) error {
	m.Ingests = append(m.Ingests, r)
	return nil
}

var _ AuditSink = (*MemAuditSink)(nil)
