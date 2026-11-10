package groundnet

import (
	"context"
	"errors"

	"github.com/territory-grounder/grounder/core/trace"
)

// Chunk is the de-identified content of a groundnet wisdom unit — the wisdom payload assembled
// ONLY from the spec/020 generalizable projection (trace.GeneralizableLayer, the A1 schema-split).
// It is the Emit boundary: because a Chunk is built from the generalizable layer, the
// estate-specific layer has NO path into it (REQ-2101/2108). core/groundnet imports core/trace for
// the projection type; core/trace does NOT import core/groundnet (the Go compiler forbids the
// cycle), which mechanically enforces the ordering invariant — the local tracer never depends on
// this seam (REQ-2113).
type Chunk struct {
	Wisdom WisdomV0
}

// NewChunk maps a GENERALIZABLE-layer projection to a wisdom Chunk. It reads only the generalizable
// KINDS and the verified outcome; there is no estate-specific field to read. The outcome verifier
// is fixed to "mechanical" (INV-5) — a groundnet unit only ever carries a mechanically verified
// outcome. The diagnosis is left empty: the generalizable layer carries no free-text diagnosis, so
// none can leak (a structured diagnosis is a future payload version).
func NewChunk(l trace.GeneralizableLayer) Chunk {
	w := WisdomV0{
		AlertClass: l.AlertClass,
		OpClass:    l.OpClass,
		Reversible: l.Reversible,
		BlastClass: l.BlastClass,
		Outcome: WisdomOutcome{
			Verifier: VerifierMechanical,
			Verdict:  l.Verdict,
		},
	}
	// The projection (A1) already content-address-sanitizes the artifact refs. A groundnet wisdom
	// unit carries at most one graduated-artifact ref; take the first, if any. FUTURE hardening
	// (reviewer note): verify the ref RESOLVES to a real graduated artifact via the local registry,
	// not just that it is hash-shaped — a T-021-6/T-021-7 concern, not this projection's type guarantee.
	if len(l.Artifacts) > 0 {
		w.Artifact = &WisdomArtifact{Kind: l.Artifacts[0].Kind, Ref: l.Artifacts[0].Ref}
	}
	return Chunk{Wisdom: w}
}

// IngestOutcome is the result of ingesting a foreign chunk. It confers NO authority and gates
// nothing: it records only whether the chunk verified, was novel, and entered LOCAL re-graduation
// as a subordinate candidate — trust is earned solely by re-graduating against local traffic
// (REQ-2109/2110). No field carries estate data (Subject is the content-address).
type IngestOutcome struct {
	Accepted    bool   // true only if the chunk verified, was not a replay, and entered re-graduation
	Disposition string // one of the Disposition* constants
	Subject     string // the statement sub (content-address), for audit — never estate data
}

// Ingest dispositions.
const (
	DispositionCandidate          = "candidate"                // verified, novel → landed for local re-graduation
	DispositionRejectedMalformed  = "rejected-malformed"       // not a parseable COSE_Sign1 statement
	DispositionRejectedUnverified = "rejected-unverified"      // signature/envelope did not verify (REQ-2104)
	DispositionRejectedNoReceipt  = "rejected-no-receipt"      // no Transparency Service Receipt (REQ-2105)
	DispositionRejectedBadReceipt = "rejected-bad-receipt"     // Receipt garbage or not bound to this statement (REQ-2105/2106)
	DispositionRejectedReplay     = "rejected-replay"          // a duplicate content-address subject (REQ-2115)
	DispositionRejectedPayload    = "rejected-unknown-payload" // an unreadable/unknown payload media type (REQ-2102)
)

// TransparencyLog registers a Signed Statement and returns its Receipt (the SCITT inclusion proof
// over the append-only VDS). Implemented by the local transparency log (T-021-3); injected so the
// seam does not depend on the log's internals.
type TransparencyLog interface {
	Register(ctx context.Context, s *Statement) (receipt []byte, err error)
}

// ReplayGuard is the REQ-2115 de-duplication frontier. RecordIfNew ATOMICALLY records a statement
// (by its content-address subject) if it has not already been seen, returning whether it was newly
// recorded. It is a SINGLE op deliberately: a separate has-it-been-seen / record-it pair would open
// a check-then-act (TOCTOU) window under which two concurrent Ingest calls for the same statement
// could both proceed and double-land. Implemented by the local transparency log (T-021-3).
type ReplayGuard interface {
	RecordIfNew(ctx context.Context, subject string, receipt []byte) (newlyRecorded bool, err error)
}

// ReGraduator lands a verified, novel foreign chunk into the LOCAL graduation path as a subordinate
// CANDIDATE — it earns standing only by re-graduating against local traffic and local verified
// outcomes (REQ-2110). Implemented by T-021-6. It returns no authority and NEVER reaches an
// actuator, lifts a floor, or changes the mutation posture (INV-22).
type ReGraduator interface {
	LandCandidate(ctx context.Context, w WisdomV0) error
}

// ErrSeamNotConfigured is returned when the seam is used without the injected dependencies its
// direction needs — the DORMANT, default-off posture (REQ-2111): an unconfigured seam refuses
// rather than half-acting.
var ErrSeamNotConfigured = errors.New("groundnet: adapter seam not configured (dormant)")
