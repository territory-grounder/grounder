package knowledge

import (
	"context"
	"testing"
)

// qrCapturingRetriever records the Query its Retrieve was called with — the seam under test.
type qrCapturingRetriever struct {
	got  Query
	hits []Hit
}

func (r *qrCapturingRetriever) Retrieve(q Query, _ int) []Hit { r.got = q; return r.hits }

// TG-50 query-rewrite: OFF (nil) the base sees the raw query; armed, the base retrieves on the REWRITTEN
// query; an empty or unchanged rewrite falls back to the original (a rewrite must never fail a retrieval).
func TestQueryRewriteRetriever(t *testing.T) {
	q := Query{Host: "db1", AlertRule: "DiskFull", Summary: "out of space"}

	// OFF (nil Rewrite): passthrough — the base sees the raw query, byte-identical.
	off := &qrCapturingRetriever{}
	(&QueryRewriteRetriever{Base: off}).Retrieve(q, 3)
	if off.got.Summary != "out of space" {
		t.Errorf("nil Rewrite must pass the raw query; got summary %q", off.got.Summary)
	}

	// Armed: the base retrieves on the rewritten query, typed fields preserved. KILLING MUTATION: return q
	// unchanged from Retrieve (drop the rewrite branch) ⇒ this reddens.
	on := &qrCapturingRetriever{}
	(&QueryRewriteRetriever{Base: on, Rewrite: func(_ context.Context, in Query) Query {
		nq := in
		nq.Summary = "postgres data volume at 100 percent, service down"
		return nq
	}}).Retrieve(q, 3)
	if on.got.Summary != "postgres data volume at 100 percent, service down" {
		t.Errorf("armed must retrieve on the rewrite; got %q", on.got.Summary)
	}
	if on.got.Host != "db1" || on.got.AlertRule != "DiskFull" {
		t.Errorf("typed fields must be preserved through the rewrite; got host=%q rule=%q", on.got.Host, on.got.AlertRule)
	}

	// An UNCHANGED rewrite (the rewriter's skip signal) ⇒ fall back to the original.
	same := &qrCapturingRetriever{}
	(&QueryRewriteRetriever{Base: same, Rewrite: func(_ context.Context, in Query) Query { return in }}).Retrieve(q, 3)
	if same.got.Summary != "out of space" {
		t.Errorf("an unchanged rewrite must retrieve on the original; got %q", same.got.Summary)
	}

	// An EMPTY rewrite (all fields blank) ⇒ fall back to the original, never an empty query.
	empty := &qrCapturingRetriever{}
	(&QueryRewriteRetriever{Base: empty, Rewrite: func(_ context.Context, _ Query) Query { return Query{} }}).Retrieve(q, 3)
	if empty.got.Summary != "out of space" {
		t.Errorf("an empty rewrite must retrieve on the original; got %q", empty.got.Summary)
	}
}
