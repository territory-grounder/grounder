package groundnet

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
)

// REQ-2105/2106: Register folds a statement into the append-only chain and returns a Receipt that
// verifies against the local log.
func TestTranslogRegisterAndVerify(t *testing.T) {
	tl := NewTranslog()
	stmt := signedWisdom(t, testPseudonym(t, testSeedHex))
	rb, err := tl.Register(context.Background(), stmt)
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	r, err := ParseReceipt(rb)
	if err != nil {
		t.Fatalf("ParseReceipt: %v", err)
	}
	if r.Domain != TranslogDomain || r.Seq != 1 {
		t.Errorf("receipt fields wrong: %+v", r)
	}
	h, _ := stmt.Header()
	if r.Subject != h.Subject {
		t.Errorf("receipt subject %q != statement sub %q", r.Subject, h.Subject)
	}
	if err := tl.VerifyLocal(r); err != nil {
		t.Fatalf("VerifyLocal must accept a genuine receipt: %v", err)
	}
}

// A Receipt with ANY field tampered fails local verification.
func TestReceiptVerifyRejectsTampered(t *testing.T) {
	tl := NewTranslog()
	// register two so the chain is non-trivial and Seq bounds matter
	tl.Register(context.Background(), signedWisdom(t, testPseudonym(t, testSeedHex)))
	rb, _ := tl.Register(context.Background(), signedWisdom(t, testPseudonym(t, testSeed2Hex)))
	good, _ := ParseReceipt(rb)
	if err := tl.VerifyLocal(good); err != nil {
		t.Fatalf("baseline receipt must verify: %v", err)
	}
	for name, mut := range map[string]func(Receipt) Receipt{
		"wrong domain":     func(r Receipt) Receipt { r.Domain = "other"; return r },
		"out-of-range seq": func(r Receipt) Receipt { r.Seq = 99; return r },
		"wrong entry hash": func(r Receipt) Receipt { r.EntryHash = "deadbeef"; return r },
		"forged subject":   func(r Receipt) Receipt { r.Subject = "sha256:forged"; return r },
		"wrong head hash":  func(r Receipt) Receipt { r.HeadHash = "deadbeef"; return r },
		"wrong digest":     func(r Receipt) Receipt { r.Digest = "deadbeef"; return r },
		"empty subject":    func(r Receipt) Receipt { r.Subject = ""; return r },
	} {
		if err := tl.VerifyLocal(mut(good)); err == nil {
			t.Errorf("%s: VerifyLocal accepted a tampered receipt", name)
		}
	}
}

// REQ-2115: RecordIfNew dedups by content-address.
func TestRecordIfNewDedup(t *testing.T) {
	tl := NewTranslog()
	if newly, err := tl.RecordIfNew(context.Background(), "sha256:abc", nil); err != nil || !newly {
		t.Fatalf("first record must be new: newly=%v err=%v", newly, err)
	}
	if newly, err := tl.RecordIfNew(context.Background(), "sha256:abc", nil); err != nil || newly {
		t.Fatalf("replay must not be new: newly=%v err=%v", newly, err)
	}
}

// The atomicity that closes the TOCTOU: under concurrent RecordIfNew for the SAME subject, exactly
// one wins and exactly one entry is recorded.
func TestRecordIfNewConcurrentAtomic(t *testing.T) {
	tl := NewTranslog()
	const n = 64
	var wg sync.WaitGroup
	var mu sync.Mutex
	won := 0
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if ok, _ := tl.RecordIfNew(context.Background(), "sha256:same", nil); ok {
				mu.Lock()
				won++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if won != 1 {
		t.Fatalf("exactly one concurrent RecordIfNew must win, got %d", won)
	}
	if tl.Len() != 1 {
		t.Errorf("exactly one entry should be recorded, got %d", tl.Len())
	}
}

// The honest cross-node integration with the REAL Translog as both TransparencyLog and ReplayGuard:
// producer A emits (a locally-verifiable Receipt); consumer B ingests into its own log and lands a
// candidate; re-ingest on B is a replay. (Same-node re-ingest of a self-emitted statement is also a
// replay, by design — the node already logged it at Emit.)
func TestSeamWithRealTranslog(t *testing.T) {
	ctx := context.Background()

	// Producer A.
	tlA := NewTranslog()
	aEmit := NewAdapter(testPseudonym(t, testSeedHex), tlA, tlA, &fakeRegraduator{})
	stmt, err := aEmit.Emit(ctx, NewChunk(testLayer()))
	if err != nil {
		t.Fatalf("Emit: %v", err)
	}
	rb, ok := stmt.Receipt()
	if !ok {
		t.Fatal("emitted statement carries no Receipt")
	}
	r, _ := ParseReceipt(rb)
	if err := tlA.VerifyLocal(r); err != nil {
		t.Fatalf("the producer's own Receipt must verify locally: %v", err)
	}
	wire, _ := stmt.MarshalCBOR()

	// Consumer B: fresh log + re-graduator.
	tlB := NewTranslog()
	rgB := &fakeRegraduator{}
	bIngest := NewAdapter(testPseudonym(t, testSeed2Hex), tlB, tlB, rgB)

	out, err := bIngest.Ingest(ctx, wire)
	if err != nil {
		t.Fatalf("Ingest: %v", err)
	}
	if !out.Accepted || out.Disposition != DispositionCandidate {
		t.Fatalf("cross-node round-trip should land a candidate: %+v", out)
	}
	out2, _ := bIngest.Ingest(ctx, wire)
	if out2.Disposition != DispositionRejectedReplay {
		t.Fatalf("re-ingest on the same node must be a replay: %+v", out2)
	}
	if len(rgB.landed) != 1 {
		t.Errorf("candidate landed %d times on B, want 1", len(rgB.landed))
	}
}

// ValidateReceiptShape — the consumer-side structural + binding guard (no producer chain needed):
// accepts a well-formed bound Receipt; rejects garbage, wrong domain, a mis-bound subject, and
// incomplete proof fields.
func TestValidateReceiptShape(t *testing.T) {
	const subject = "sha256:aaa"
	good, _ := json.Marshal(Receipt{Domain: TranslogDomain, Seq: 1, EntryHash: "e", Subject: subject, HeadSeq: 1, HeadHash: "h", Digest: "d"})
	if _, err := ValidateReceiptShape(good, subject); err != nil {
		t.Fatalf("well-formed bound receipt must validate: %v", err)
	}
	if _, err := ValidateReceiptShape([]byte("garbage"), subject); err == nil {
		t.Error("garbage receipt must be rejected")
	}
	for name, r := range map[string]Receipt{
		"wrong domain":     {Domain: "other", Seq: 1, EntryHash: "e", Subject: subject, HeadSeq: 1, HeadHash: "h", Digest: "d"},
		"mis-bound":        {Domain: TranslogDomain, Seq: 1, EntryHash: "e", Subject: "sha256:other", HeadSeq: 1, HeadHash: "h", Digest: "d"},
		"zero seq":         {Domain: TranslogDomain, Seq: 0, EntryHash: "e", Subject: subject, HeadSeq: 1, HeadHash: "h", Digest: "d"},
		"empty entry hash": {Domain: TranslogDomain, Seq: 1, EntryHash: "", Subject: subject, HeadSeq: 1, HeadHash: "h", Digest: "d"},
		"empty digest":     {Domain: TranslogDomain, Seq: 1, EntryHash: "e", Subject: subject, HeadSeq: 1, HeadHash: "h", Digest: ""},
	} {
		b, _ := json.Marshal(r)
		if _, err := ValidateReceiptShape(b, subject); err == nil {
			t.Errorf("%s: must be rejected", name)
		}
	}
}
