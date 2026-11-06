package groundnet

import (
	"context"
	"errors"
	"testing"
)

func validEmit() EmitRecord {
	return EmitRecord{
		Subject:     "sha256:abcd",
		ContentType: WisdomMediaTypeV1,
		Issuer:      "gnpub:node-a",
		KeyID:       "kid-1",
		Receipt:     []byte("receipt-bytes"),
		Retention:   "federation-shared",
	}
}

func TestRecordEmitValidatesAndAppends(t *testing.T) {
	sink := NewMemAuditSink()
	a := NewAudit(sink)
	if err := a.RecordEmit(context.Background(), validEmit()); err != nil {
		t.Fatalf("a valid emit must record: %v", err)
	}
	if len(sink.Emits) != 1 || sink.Emits[0].Subject != "sha256:abcd" {
		t.Fatalf("the emit row must persist: %+v", sink.Emits)
	}
}

func TestRecordEmitRejectsBadShape(t *testing.T) {
	a := NewAudit(NewMemAuditSink())
	for name, mut := range map[string]func(*EmitRecord){
		"empty subject":     func(r *EmitRecord) { r.Subject = "" },
		"empty content":     func(r *EmitRecord) { r.ContentType = "" },
		"non-pseudonym iss": func(r *EmitRecord) { r.Issuer = "acme-corp" },
		"empty receipt":     func(r *EmitRecord) { r.Receipt = nil },
	} {
		r := validEmit()
		mut(&r)
		if err := a.RecordEmit(context.Background(), r); !errors.Is(err, ErrAuditRecordInvalid) {
			t.Errorf("%s must be refused with ErrAuditRecordInvalid, got %v", name, err)
		}
	}
}

func TestRecordIngestIntegrity(t *testing.T) {
	a := NewAudit(NewMemAuditSink())
	// candidate requires verified.
	if err := a.RecordIngest(context.Background(), IngestRecord{Subject: "s", Issuer: "gnpub:n", VerifyResult: VerifyVerified, Disposition: DispositionCandidate}); err != nil {
		t.Errorf("verified + candidate must be valid: %v", err)
	}
	if err := a.RecordIngest(context.Background(), IngestRecord{Subject: "s", Issuer: "gnpub:n", VerifyResult: VerifyRejected, Disposition: DispositionCandidate}); !errors.Is(err, ErrAuditRecordInvalid) {
		t.Error("rejected + candidate must be refused — a candidate landing requires a verified statement")
	}
	// verified but rejected DOWNSTREAM (bad receipt / replay) is legitimate — verify does not imply candidate.
	if err := a.RecordIngest(context.Background(), IngestRecord{Subject: "s", Issuer: "gnpub:n", VerifyResult: VerifyVerified, Disposition: DispositionRejectedReplay}); err != nil {
		t.Errorf("verified + rejected-downstream must be valid: %v", err)
	}
	if err := a.RecordIngest(context.Background(), IngestRecord{Subject: "s", Issuer: "gnpub:n", VerifyResult: VerifyRejected, Disposition: DispositionRejectedUnverified}); err != nil {
		t.Errorf("rejected + rejected-unverified must be valid: %v", err)
	}
	// bad shape.
	if err := a.RecordIngest(context.Background(), IngestRecord{Subject: "s", Issuer: "not-a-pseudonym", VerifyResult: VerifyVerified, Disposition: DispositionCandidate}); !errors.Is(err, ErrAuditRecordInvalid) {
		t.Error("a non-pseudonym issuer must be refused")
	}
}

func TestNilSinkFailsClosed(t *testing.T) {
	a := NewAudit(nil)
	if err := a.RecordEmit(context.Background(), validEmit()); !errors.Is(err, ErrAuditNotConfigured) {
		t.Errorf("a nil sink must fail closed with ErrAuditNotConfigured, got %v", err)
	}
	var nilA *Audit
	if err := nilA.RecordIngest(context.Background(), IngestRecord{}); !errors.Is(err, ErrAuditNotConfigured) {
		t.Errorf("a nil recorder must fail closed, got %v", err)
	}
}
