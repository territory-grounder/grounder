package groundnet

import (
	"context"
	"fmt"
	"time"
)

// Adapter is the groundnet Emit/Ingest seam (REQ-2108). It is DORMANT by default and reaches no
// actuator: Emit publishes de-identified wisdom sourced only from the generalizable layer, and
// Ingest lands a foreign chunk as a SUBORDINATE hint that re-graduates locally before it earns any
// standing (REQ-2109/2110) — it never lifts the never-auto floor, the actuation interceptor, or the
// mode chokepoint. The local decision tracer has NO dependency on this seam (REQ-2113).
type Adapter struct {
	pseudonym  *Pseudonym
	translog   TransparencyLog
	replay     ReplayGuard
	regraduate ReGraduator
}

// NewAdapter constructs the seam from its producer pseudonym and the injected downstream concerns
// (transparency log, replay guard, re-graduator). A nil dependency leaves the seam dormant for the
// direction that needs it: Emit needs pseudonym+translog, Ingest needs replay+regraduate.
func NewAdapter(p *Pseudonym, tl TransparencyLog, rg ReplayGuard, rgr ReGraduator) *Adapter {
	return &Adapter{pseudonym: p, translog: tl, replay: rg, regraduate: rgr}
}

// Emit assembles a wisdom Chunk into a signed, receipted SCITT Transparent Statement and returns it
// (REQ-2108). The Chunk is sourced ONLY from the generalizable projection, so Emit cannot read the
// estate-specific layer (REQ-2101). Emit signs with the producer pseudonym (REQ-2104), registers the
// statement with the transparency log for a Receipt (REQ-2105), and attaches it. The returned
// statement is the publishable Transparent Statement; its Receipt is available via stmt.Receipt().
func (a *Adapter) Emit(ctx context.Context, c Chunk) (*Statement, error) {
	if a.pseudonym == nil || a.translog == nil {
		return nil, ErrSeamNotConfigured
	}
	if err := c.Wisdom.Validate(); err != nil {
		return nil, err
	}
	payload, err := c.Wisdom.Marshal()
	if err != nil {
		return nil, err
	}
	stmt, err := a.pseudonym.Sign(StatementHeader{
		Subject:     ComputeSubject(WisdomMediaTypeV1, payload),
		IssuedAt:    time.Now().UTC().Truncate(time.Hour).Unix(), // coarsened to the hour (timing correlation)
		ContentType: WisdomMediaTypeV1,
	}, payload)
	if err != nil {
		return nil, err
	}
	receipt, err := a.translog.Register(ctx, stmt)
	if err != nil {
		return nil, fmt.Errorf("groundnet: registering statement with the transparency log: %w", err)
	}
	stmt.AttachReceipt(receipt)
	return stmt, nil
}

// Ingest lands a foreign signed statement into the LOCAL re-graduation path as a subordinate hint
// (REQ-2108/2109/2110). It runs the two anti-poisoning guards BEFORE anything else — verify the COSE
// signature and envelope (REQ-2104), then reject a replay by sub+Receipt (REQ-2115) — then decodes
// the de-identified payload and hands it to local re-graduation. It confers NO authority, reaches no
// actuator, and lifts no gate; a foreign chunk earns standing only by re-graduating locally. A
// rejected chunk returns a disposition, not an error — rejection is a normal outcome, not a fault.
func (a *Adapter) Ingest(ctx context.Context, statement []byte) (IngestOutcome, error) {
	if a.replay == nil || a.regraduate == nil {
		return IngestOutcome{}, ErrSeamNotConfigured
	}
	stmt, err := ParseStatement(statement)
	if err != nil {
		return IngestOutcome{Disposition: DispositionRejectedMalformed}, nil
	}
	// Guard 1: verify signature + envelope BEFORE ingest (REQ-2104). An unsigned or tampered
	// statement never reaches re-graduation.
	if err := Verify(stmt); err != nil {
		return IngestOutcome{Disposition: DispositionRejectedUnverified}, nil
	}
	h, err := stmt.Header()
	if err != nil {
		return IngestOutcome{Disposition: DispositionRejectedMalformed}, nil
	}
	receipt, ok := stmt.Receipt()
	if !ok {
		return IngestOutcome{Disposition: DispositionRejectedNoReceipt, Subject: h.Subject}, nil
	}
	// The Receipt must be a well-formed transparency-log Receipt BOUND to this statement's
	// content-address (REQ-2105/2106): a garbage Receipt, or one lifted from another statement, is
	// refused here. Full cryptographic inclusion verification against the producing node's witnessed
	// chain is far-future (the multi-witness VDS); this is the structural + binding guard a consumer
	// can run on a foreign chunk without holding the producer's chain.
	if _, err := ValidateReceiptShape(receipt, h.Subject); err != nil {
		return IngestOutcome{Disposition: DispositionRejectedBadReceipt, Subject: h.Subject}, nil
	}
	// Decode the de-identified payload; an unknown media type is rejected without authority (REQ-2102).
	// Decode BEFORE claiming so the replay log only ever records verified, decodable statements.
	w, err := DecodeWisdom(stmt)
	if err != nil {
		return IngestOutcome{Disposition: DispositionRejectedPayload, Subject: h.Subject}, nil
	}
	// Guard 2: reject a replay by content-address (REQ-2115) — one statement earns standing at most
	// once per node, so a re-emit cannot inflate reputation or re-trigger ingest. RecordIfNew claims
	// the subject ATOMICALLY, so two concurrent ingests of the same statement cannot both land.
	newlyRecorded, err := a.replay.RecordIfNew(ctx, h.Subject, receipt)
	if err != nil {
		return IngestOutcome{}, err
	}
	if !newlyRecorded {
		return IngestOutcome{Disposition: DispositionRejectedReplay, Subject: h.Subject}, nil
	}
	// Land as a SUBORDINATE candidate — it re-graduates locally before it earns standing (REQ-2110).
	// This is the only path forward, and it grants no authority. (The subject is content-addressed,
	// so a transient land failure is retried at the re-graduation layer, not by re-ingesting: the
	// same content always hashes to the same, now-claimed, subject.)
	if err := a.regraduate.LandCandidate(ctx, *w); err != nil {
		return IngestOutcome{}, err
	}
	return IngestOutcome{Accepted: true, Disposition: DispositionCandidate, Subject: h.Subject}, nil
}
