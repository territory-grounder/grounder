package db

import (
	"context"
	"fmt"
)

// LedgerShapeCount is the governance ledger's answer to the question TG-345 already asks of the evidence
// corpus: does anything stored here LOOK like credential material?
//
// ★ WHY THE LEDGER TOO. TG-57 item 1 is "redaction on model-bound tool outputs AND the ledger". The tool-output
// half is done — agent/loop.go screenToolOutput runs the input screen over every tool RESULT before it
// re-enters the prompt, redacting secrets to [REDACTED:<kind>]. The ledger half is not: core/audit/ledger.go
// writes `reason` with no screen call on the path.
//
// Measured 2026-08-06 on the live ledger: 9,417 rows, 0 PEM blocks, 0 provider keys, 0 assigned-secret
// shapes, 0 redaction markers — and a `reason` field running to 4,886 characters. So it is clean, it is
// unscreened, and the longest field is model-influenced free text.
//
// That is precisely the TG-302 situation one table over, and TG-345's reasoning transfers verbatim: the
// premise is a property of what has happened to be written, not of the design, and nothing was watching
// whether it still held. The ledger is also APPEND-ONLY and hash-chained (prev_hash/hash), so a row that
// does leak cannot be redacted afterwards without breaking the chain — which makes detection the only
// available control for anything already written, and makes noticing early worth more here than elsewhere.
type LedgerShapeCount struct {
	// Rows is the denominator, published even at zero, so an ABSENT series means the watcher is gone
	// rather than the ledger being clean.
	Rows int64
	// RedactionMarker counts rows carrying a redaction marker. On the evidence corpus a marker means the
	// screen fired. On the LEDGER there is no screen on the write path, so a marker here means the text
	// arrived already-redacted from somewhere upstream — still worth knowing, and not the same finding.
	RedactionMarker int64
	// PEMBlock counts rows containing a private-key header.
	PEMBlock int64
	// ProviderKey counts rows containing a recognisable cloud/SaaS key shape.
	ProviderKey int64
	// AssignedValue counts `password=`/`token:`-style assignments with a non-trivial value.
	AssignedValue int64
}

// SecretShaped is the total the alert rule reads — a SUM, for the same reason EvidenceShapeCount sums:
// a row matching two shapes is two things for a human to look at.
func (c LedgerShapeCount) SecretShaped() int64 {
	return c.RedactionMarker + c.PEMBlock + c.ProviderKey + c.AssignedValue
}

// CountLedgerShapes measures the ledger's free-text columns.
//
// It reuses the evidence watcher's four regexes VERBATIM rather than declaring its own. That is deliberate:
// two hygiene watchers answering "is there credential material here" with different definitions produce two
// incomparable numbers, and the next person to widen one silently changes what the other means. If these
// shapes are ever wrong they should be wrong in one place.
//
// Both `reason` and `decision` are scanned. `reason` is the long model-influenced field, but `decision` is
// free text on the same row and excluding it would make the count narrower than the claim.
//
// It reads ONLY counts — never a payload — so a watcher for credential material cannot itself become a way
// to read one.
func (s *LedgerStore) CountLedgerShapes(ctx context.Context) (LedgerShapeCount, error) {
	var c LedgerShapeCount
	const q = `
		SELECT count(*),
		       count(*) FILTER (WHERE reason ~ $1 OR decision ~ $1),
		       count(*) FILTER (WHERE reason ~ $2 OR decision ~ $2),
		       count(*) FILTER (WHERE reason ~ $3 OR decision ~ $3),
		       count(*) FILTER (WHERE reason ~ $4 OR decision ~ $4)
		FROM governance_ledger`
	if err := s.p.QueryRow(ctx, q, evidenceRedactionRe, evidencePEMRe, evidenceProviderRe, evidenceAssignedRe).
		Scan(&c.Rows, &c.RedactionMarker, &c.PEMBlock, &c.ProviderKey, &c.AssignedValue); err != nil {
		return LedgerShapeCount{}, fmt.Errorf("db: count governance_ledger shapes: %w", err)
	}
	return c, nil
}
