package db

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/territory-grounder/grounder/core/trace"
)

// KILLING MUTATION: return a zero-value AgentStepEvidence instead of ErrEvidenceNotFound. RED — the console
// cannot then tell "nobody kept what the tool returned" from "the tool returned nothing", and it renders those
// two facts differently on purpose. Only one of them is about the estate.
func TestAMissingObservationIsNotAnEmptyObservation(t *testing.T) {
	m := NewMemAgentStepEvidenceStore()
	_, err := m.Evidence(context.Background(), "sess-1", "never-recorded")
	if !errors.Is(err, trace.ErrEvidenceNotFound) {
		t.Fatalf("got err=%v, want ErrEvidenceNotFound — a walk with no stored evidence must be distinguishable "+
			"from a tool that produced nothing", err)
	}
}

// KILLING MUTATION: let a later EmitEvidence overwrite an earlier one. RED — the table is append-only
// (0053 REVOKEs UPDATE/DELETE) precisely so recorded evidence cannot be revised after an operator reads it,
// and Temporal RETRIES the investigate activity, so a second emit of the same cycle is routine.
func TestARetriedActivityCannotRewriteRecordedEvidence(t *testing.T) {
	m := NewMemAgentStepEvidenceStore()
	ctx := context.Background()
	first := trace.AgentStepEvidence{ExternalRef: "sess-1", Cycle: 4, EvidenceID: "ev-1", Tool: "check-host-services", Payload: "4 failed units"}
	if err := m.EmitEvidence(ctx, first); err != nil {
		t.Fatal(err)
	}
	revised := first
	revised.Payload = "0 failed units"
	if err := m.EmitEvidence(ctx, revised); err != nil {
		t.Fatalf("a duplicate emit must be a silent no-op, not an error: %v", err)
	}
	got, err := m.Evidence(ctx, "sess-1", "ev-1")
	if err != nil {
		t.Fatal(err)
	}
	if got.Payload != "4 failed units" {
		t.Fatalf("stored payload is %q — a re-emit REVISED evidence an operator may already have audited", got.Payload)
	}
}

// KILLING MUTATION: drop the Truncated/FullBytes bookkeeping and just clip the string. RED — a clipped body
// that does not SAY it is clipped is a lie, and this is the one surface where that costs the most: the
// operator is reading it precisely to check whether the agent's claim matches what the host said.
func TestATruncatedPayloadSaysSoAndReportsTheRealSize(t *testing.T) {
	big := strings.Repeat("x", trace.MaxEvidenceBytes+5000)
	m := NewMemAgentStepEvidenceStore()
	if err := m.EmitEvidence(context.Background(), trace.AgentStepEvidence{
		ExternalRef: "sess-1", Cycle: 1, EvidenceID: "ev-big", Payload: big,
	}); err != nil {
		t.Fatal(err)
	}
	got, err := m.Evidence(context.Background(), "sess-1", "ev-big")
	if err != nil {
		t.Fatal(err)
	}
	if !got.Truncated {
		t.Fatal("a payload over the cap was stored without recording that it was truncated")
	}
	if got.FullBytes != len(big) {
		t.Fatalf("full_bytes=%d, want %d — the console cannot say \"showing X of Y\" without the real size", got.FullBytes, len(big))
	}
	if len(got.Payload) > trace.MaxEvidenceBytes {
		t.Fatalf("stored %d bytes, cap is %d", len(got.Payload), trace.MaxEvidenceBytes)
	}
}

// KILLING MUTATION: slice at the byte cap without backing off to a rune boundary. RED — a payload cut
// mid-sequence ends in a partial rune that renders as U+FFFD, so evidence looks CORRUPTED rather than merely
// cut. Estate output is full of multi-byte characters: systemctl's own status bullet is "●" (3 bytes).
func TestTruncationLandsOnARuneBoundary(t *testing.T) {
	// Build a payload whose multi-byte rune straddles the cap exactly.
	pad := strings.Repeat("a", trace.MaxEvidenceBytes-1)
	out, truncated, _ := trace.Truncate(pad + "●" + "tail")
	if !truncated {
		t.Fatal("payload over the cap was not truncated")
	}
	if strings.ContainsRune(out, '�') {
		t.Fatal("truncation left a replacement character — the slice cut a rune in half")
	}
	if !strings.HasSuffix(out, "a") {
		t.Fatalf("expected the trailing partial rune to be dropped, got tail %q", out[len(out)-4:])
	}
}

// KILLING MUTATION: accept an empty evidence id. RED — the id is the console's only handle on the row, so an
// empty one writes evidence nothing can ever fetch: stored, invisible, and indistinguishable from absent.
func TestEvidenceWithNoIdIsRefused(t *testing.T) {
	m := NewMemAgentStepEvidenceStore()
	if err := m.EmitEvidence(context.Background(), trace.AgentStepEvidence{
		ExternalRef: "sess-1", Cycle: 1, EvidenceID: "", Payload: "something",
	}); err == nil {
		t.Fatal("evidence with an empty id was accepted — nothing could ever retrieve it")
	}
}
