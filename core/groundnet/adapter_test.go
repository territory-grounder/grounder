package groundnet

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/territory-grounder/grounder/core/trace"
)

// --- test fakes for the injected downstream concerns (T-021-3 / T-021-6) ---

type fakeTranslog struct{ registered int }

func (f *fakeTranslog) Register(_ context.Context, s *Statement) ([]byte, error) {
	f.registered++
	h, _ := s.Header()
	// A well-formed Receipt BOUND to the statement's subject — Ingest validates the shape.
	return json.Marshal(Receipt{
		Domain: TranslogDomain, Seq: 1, EntryHash: "fake-entry",
		Subject: h.Subject, HeadSeq: 1, HeadHash: "fake-head", Digest: "fake-digest",
	})
}

type fakeReplay struct{ seen map[string]bool }

func newFakeReplay() *fakeReplay { return &fakeReplay{seen: map[string]bool{}} }

func (f *fakeReplay) RecordIfNew(_ context.Context, subject string, _ []byte) (bool, error) {
	if f.seen[subject] {
		return false, nil
	}
	f.seen[subject] = true
	return true, nil
}

type fakeRegraduator struct{ landed []WisdomV0 }

func (f *fakeRegraduator) LandCandidate(_ context.Context, w WisdomV0) error {
	f.landed = append(f.landed, w)
	return nil
}

func testLayer() trace.GeneralizableLayer {
	return trace.GeneralizableLayer{
		AlertClass: "service-down/http",
		OpClass:    "restart-service",
		Reversible: true,
		BlastClass: "single-host",
		Band:       "AUTO_NOTICE",
		Verdict:    "clean",
		Confidence: 0.9,
		Artifacts:  []trace.ArtifactRef{{Kind: "runbook", Ref: "sha256:" + strings.Repeat("ab", 32)}},
	}
}

func testAdapter(t *testing.T) (*Adapter, *fakeTranslog, *fakeReplay, *fakeRegraduator) {
	t.Helper()
	p := testPseudonym(t, testSeedHex)
	tl, rp, rg := &fakeTranslog{}, newFakeReplay(), &fakeRegraduator{}
	return NewAdapter(p, tl, rp, rg), tl, rp, rg
}

// REQ-2108: Emit sources its chunk ONLY from the generalizable layer and produces a verifiable,
// receipted Transparent Statement carrying the de-identified wisdom.
func TestEmitFromGeneralizableLayer(t *testing.T) {
	a, tl, _, _ := testAdapter(t)
	stmt, err := a.Emit(context.Background(), NewChunk(testLayer()))
	if err != nil {
		t.Fatalf("Emit: %v", err)
	}
	if tl.registered != 1 {
		t.Errorf("translog.Register called %d times, want 1", tl.registered)
	}
	if err := Verify(stmt); err != nil {
		t.Fatalf("emitted statement must verify: %v", err)
	}
	if _, ok := stmt.Receipt(); !ok {
		t.Error("emitted statement must carry a Receipt")
	}
	w, err := DecodeWisdom(stmt)
	if err != nil {
		t.Fatalf("DecodeWisdom: %v", err)
	}
	if w.OpClass != "restart-service" || w.AlertClass != "service-down/http" || w.Outcome.Verifier != VerifierMechanical {
		t.Errorf("wisdom content wrong: %+v", w)
	}
	if w.Diagnosis != "" {
		t.Errorf("no free-text diagnosis should be present (estate-leak surface), got %q", w.Diagnosis)
	}
}

// The in-process Emit -> Ingest round-trip (a preview of the Phase-D e2e): a chunk emitted on a node
// ingests on the same node, verifies, and lands as a subordinate candidate.
func TestEmitIngestRoundTrip(t *testing.T) {
	a, _, _, rg := testAdapter(t)
	stmt, err := a.Emit(context.Background(), NewChunk(testLayer()))
	if err != nil {
		t.Fatalf("Emit: %v", err)
	}
	wire, err := stmt.MarshalCBOR()
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	out, err := a.Ingest(context.Background(), wire)
	if err != nil {
		t.Fatalf("Ingest: %v", err)
	}
	if !out.Accepted || out.Disposition != DispositionCandidate {
		t.Fatalf("round-trip should land as candidate, got %+v", out)
	}
	if len(rg.landed) != 1 || rg.landed[0].OpClass != "restart-service" {
		t.Errorf("re-graduator should have received the wisdom once: %+v", rg.landed)
	}
}

// REQ-2104: a tampered statement is refused BEFORE re-graduation — it never reaches LandCandidate.
func TestIngestRejectsTampered(t *testing.T) {
	a, _, _, rg := testAdapter(t)
	stmt, _ := a.Emit(context.Background(), NewChunk(testLayer()))
	stmt.msg.Payload[0] ^= 0xFF // tamper the payload after signing
	wire, _ := stmt.MarshalCBOR()
	out, err := a.Ingest(context.Background(), wire)
	if err != nil {
		t.Fatalf("Ingest returned an error (rejection should be a disposition): %v", err)
	}
	if out.Disposition != DispositionRejectedUnverified {
		t.Fatalf("tampered chunk: got %q, want %q", out.Disposition, DispositionRejectedUnverified)
	}
	if len(rg.landed) != 0 {
		t.Error("a tampered chunk must NEVER reach re-graduation")
	}
}

// REQ-2115: a replayed statement is rejected — it lands at most once per node.
func TestIngestRejectsReplay(t *testing.T) {
	a, _, _, rg := testAdapter(t)
	stmt, _ := a.Emit(context.Background(), NewChunk(testLayer()))
	wire, _ := stmt.MarshalCBOR()
	if out := mustIngest(t, a, wire); out.Disposition != DispositionCandidate {
		t.Fatalf("first ingest: %+v", out)
	}
	if out := mustIngest(t, a, wire); out.Disposition != DispositionRejectedReplay {
		t.Fatalf("replay: got %q, want %q", out.Disposition, DispositionRejectedReplay)
	}
	if len(rg.landed) != 1 {
		t.Errorf("a replay must not re-land: landed %d times", len(rg.landed))
	}
}

// REQ-2105: a statement with no Transparency Service Receipt is rejected and never lands.
func TestIngestRejectsNoReceipt(t *testing.T) {
	a, _, _, rg := testAdapter(t)
	p := testPseudonym(t, testSeedHex)
	w := NewChunk(testLayer()).Wisdom
	payload, _ := w.Marshal()
	stmt, _ := p.Sign(StatementHeader{Subject: ComputeSubject(WisdomMediaTypeV1, payload), IssuedAt: 1, ContentType: WisdomMediaTypeV1}, payload)
	wire, _ := stmt.MarshalCBOR()
	out := mustIngest(t, a, wire)
	if out.Disposition != DispositionRejectedNoReceipt {
		t.Fatalf("no-receipt: got %q, want %q", out.Disposition, DispositionRejectedNoReceipt)
	}
	if len(rg.landed) != 0 {
		t.Error("a no-receipt chunk must not land")
	}
}

// The dormant posture: an unconfigured seam refuses rather than half-acting (REQ-2111).
func TestSeamNotConfigured(t *testing.T) {
	a := NewAdapter(nil, nil, nil, nil)
	if _, err := a.Emit(context.Background(), NewChunk(testLayer())); err != ErrSeamNotConfigured {
		t.Errorf("Emit unconfigured: got %v, want ErrSeamNotConfigured", err)
	}
	if _, err := a.Ingest(context.Background(), []byte{0x01}); err != ErrSeamNotConfigured {
		t.Errorf("Ingest unconfigured: got %v, want ErrSeamNotConfigured", err)
	}
}

// REQ-2101 at the Chunk boundary: NewChunk carries only generalizable KINDS + the verified outcome,
// with no free-text diagnosis slot to leak estate data.
func TestNewChunkNoFreeTextDiagnosis(t *testing.T) {
	c := NewChunk(testLayer())
	if c.Wisdom.Diagnosis != "" {
		t.Errorf("NewChunk must not populate a free-text diagnosis: %q", c.Wisdom.Diagnosis)
	}
	if c.Wisdom.Outcome.Verifier != VerifierMechanical {
		t.Errorf("outcome verifier must be mechanical (INV-5): %q", c.Wisdom.Outcome.Verifier)
	}
}

func mustIngest(t *testing.T, a *Adapter, wire []byte) IngestOutcome {
	t.Helper()
	out, err := a.Ingest(context.Background(), wire)
	if err != nil {
		t.Fatalf("Ingest: %v", err)
	}
	return out
}

// signWithReceipt signs a wisdom statement for testPseudonym(seed) and attaches the given receipt
// bytes — used to craft a statement with a chosen (possibly bogus) Receipt.
func signWithReceipt(t *testing.T, receipt []byte) []byte {
	t.Helper()
	p := testPseudonym(t, testSeedHex)
	w := NewChunk(testLayer()).Wisdom
	payload, err := w.Marshal()
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	stmt, err := p.Sign(StatementHeader{
		Subject:     ComputeSubject(WisdomMediaTypeV1, payload),
		IssuedAt:    1,
		ContentType: WisdomMediaTypeV1,
	}, payload)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	stmt.AttachReceipt(receipt)
	wire, err := stmt.MarshalCBOR()
	if err != nil {
		t.Fatalf("marshal statement: %v", err)
	}
	return wire
}

// REQ-2105/2106: a garbage (non-decodable) Receipt is refused — presence is not validity — and never
// reaches re-graduation.
func TestIngestRejectsGarbageReceipt(t *testing.T) {
	a, _, _, rg := testAdapter(t)
	out := mustIngest(t, a, signWithReceipt(t, []byte("not-a-receipt-just-bytes")))
	if out.Disposition != DispositionRejectedBadReceipt {
		t.Fatalf("garbage receipt: got %q, want %q", out.Disposition, DispositionRejectedBadReceipt)
	}
	if len(rg.landed) != 0 {
		t.Error("a bad-receipt chunk must not land")
	}
}

// REQ-2106: a well-formed Receipt LIFTED from another statement (wrong subject binding) is refused —
// a Receipt cannot be replayed onto an unrelated statement.
func TestIngestRejectsCrossStatementReceipt(t *testing.T) {
	a, _, _, rg := testAdapter(t)
	elsewhere, _ := json.Marshal(Receipt{
		Domain: TranslogDomain, Seq: 1, EntryHash: "abc",
		Subject: "sha256:someone-elses-statement", HeadSeq: 1, HeadHash: "abc", Digest: "abc",
	})
	out := mustIngest(t, a, signWithReceipt(t, elsewhere))
	if out.Disposition != DispositionRejectedBadReceipt {
		t.Fatalf("cross-statement receipt: got %q, want %q", out.Disposition, DispositionRejectedBadReceipt)
	}
	if len(rg.landed) != 0 {
		t.Error("a cross-statement-receipt chunk must not land")
	}
}
