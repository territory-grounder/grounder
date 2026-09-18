package groundnet

import (
	"encoding/json"
	"errors"
	"fmt"

	"github.com/territory-grounder/grounder/core/audit"
)

// ErrReceiptInvalid is returned when a Receipt does not verify against the local transparency log.
var ErrReceiptInvalid = errors.New("groundnet: transparency-log Receipt does not verify against the local log")

// Receipt is TG's local binding of a SCITT Transparency Service Receipt (REQ-2105): the record that a
// statement was registered in the local transparency log at a position in the append-only hash-chain,
// plus the HEAD anchor witnessing the chain at that point (REQ-2106). It rides a statement's
// unprotected header (label 394) to form a Transparent Statement. For a same-node round-trip the local
// log re-derives and checks it; a foreign consumer's cross-witness verification against a multi-witness
// VDS is far-future (docs/FEDERATION-VISION.md). No field carries estate data — Subject is a
// content-address.
type Receipt struct {
	Domain    string `json:"domain"`     // TranslogDomain
	Seq       int64  `json:"seq"`        // the statement's position in the append-only chain
	EntryHash string `json:"entry_hash"` // the chain fold at this entry — commits to (prev, subject)
	Subject   string `json:"subject"`    // the content-address this receipt is for
	HeadSeq   int64  `json:"head_seq"`   // the chain HEAD position when the receipt was issued
	HeadHash  string `json:"head_hash"`  // the HEAD fold at HeadSeq
	Digest    string `json:"digest"`     // audit.ComputeAnchor digest of the HEAD at HeadSeq
}

// ParseReceipt decodes a marshaled Receipt (as carried in a statement's unprotected header).
func ParseReceipt(b []byte) (Receipt, error) {
	var r Receipt
	if err := json.Unmarshal(b, &r); err != nil {
		return Receipt{}, err
	}
	return r, nil
}

// VerifyLocal returns nil iff the Receipt genuinely proves inclusion in THIS log: the claimed entry
// exists at the claimed sequence and folds the claimed subject to the claimed hash from its
// predecessor, AND the recorded HEAD anchor re-derives against the (append-only, immutable) chain at
// HeadSeq (REQ-2106). It is the producing / same-node check; a foreign consumer's cross-witness
// verification is far-future.
func (t *Translog) VerifyLocal(r Receipt) error {
	if r.Domain != TranslogDomain || r.Subject == "" {
		return ErrReceiptInvalid
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if r.Seq < 1 || int(r.Seq) > len(t.entries) {
		return ErrReceiptInvalid
	}
	e := t.entries[r.Seq-1]
	if e.subject != r.Subject || e.hash != r.EntryHash {
		return ErrReceiptInvalid
	}
	// Re-fold the entry from its predecessor to prove EntryHash commits to (prev, subject).
	prev := translogGenesis
	if r.Seq > 1 {
		prev = t.entries[r.Seq-2].hash
	}
	if translogFold(prev, e.subject) != r.EntryHash {
		return ErrReceiptInvalid
	}
	// The recorded HEAD witness must match the actual chain at HeadSeq and its anchor must re-derive.
	if r.HeadSeq < r.Seq || int(r.HeadSeq) > len(t.entries) {
		return ErrReceiptInvalid
	}
	if t.entries[r.HeadSeq-1].hash != r.HeadHash {
		return ErrReceiptInvalid
	}
	if audit.ComputeAnchor(t.headStateAtLocked(r.HeadSeq)).Digest != r.Digest {
		return ErrReceiptInvalid
	}
	return nil
}

// ValidateReceiptShape structurally validates a marshaled Receipt and BINDS it to the statement it
// accompanies: it decodes the Receipt, checks its domain, requires that the Receipt's content-address
// equal the statement's own subject (so a Receipt cannot be lifted from one statement onto another),
// and requires the proof fields be populated. This is the guard a CONSUMER runs on a FOREIGN chunk at
// ingest, where it does NOT hold the producing node's chain: it refuses a garbage or cross-statement
// Receipt (REQ-2105/2106). It does NOT cryptographically verify the inclusion proof — that needs the
// producing node's witnessed chain (VerifyLocal, same node) or the far-future multi-witness VDS
// (docs/FEDERATION-VISION.md); a foreign consumer's cross-witness verification is out of scope here.
func ValidateReceiptShape(b []byte, subject string) (Receipt, error) {
	r, err := ParseReceipt(b)
	if err != nil {
		return Receipt{}, fmt.Errorf("%w: not a decodable Receipt: %v", ErrReceiptInvalid, err)
	}
	if r.Domain != TranslogDomain {
		return Receipt{}, fmt.Errorf("%w: wrong domain %q", ErrReceiptInvalid, r.Domain)
	}
	if r.Subject != subject {
		return Receipt{}, fmt.Errorf("%w: Receipt subject %q does not bind to the statement %q", ErrReceiptInvalid, r.Subject, subject)
	}
	if r.Seq < 1 || r.EntryHash == "" || r.HeadHash == "" || r.Digest == "" {
		return Receipt{}, fmt.Errorf("%w: incomplete proof fields", ErrReceiptInvalid)
	}
	return r, nil
}
