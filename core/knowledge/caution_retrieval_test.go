package knowledge

import (
	"strings"
	"testing"
	"time"
)

// TestRetrieveCautionsIsTargetedToSignature pins the whole over-caution safety: a caution surfaces ONLY for
// the same (host, rule-family) — never for an unrelated host, an unrelated rule on the same host, or a query
// missing either half of the signature. Each direction is RED under the obvious mutation (dropping the host
// check, dropping the rule check, or removing the empty-signature guard).
func TestRetrieveCautionsIsTargetedToSignature(t *testing.T) {
	corpus := []Incident{
		{ExternalRef: "c-match", Host: "web01", AlertRule: "ServiceDown", Resolution: "prior attempt DEVIATED", Source: ProvenanceCaution},
		{ExternalRef: "c-otherhost", Host: "web02", AlertRule: "ServiceDown", Source: ProvenanceCaution},
		{ExternalRef: "c-otherrule", Host: "web01", AlertRule: "DiskFull", Source: ProvenanceCaution},
	}
	r := NewLexicalRetriever(corpus)
	if got := cautionHitRefs(r.RetrieveCautions(Query{Host: "web01", AlertRule: "ServiceDown"}, 3)); len(got) != 1 || got[0] != "c-match" {
		t.Fatalf("matching (host,rule) → want [c-match], got %v", got)
	}
	if got := r.RetrieveCautions(Query{Host: "web09", AlertRule: "ServiceDown"}, 3); len(got) != 0 {
		t.Errorf("unrelated host must surface NO caution, got %v", cautionHitRefs(got))
	}
	if got := r.RetrieveCautions(Query{Host: "web01", AlertRule: "CpuHigh"}, 3); len(got) != 0 {
		t.Errorf("unrelated rule on same host must surface NO caution (a caution is a signature warning, not a host warning), got %v", cautionHitRefs(got))
	}
	if got := r.RetrieveCautions(Query{AlertRule: "ServiceDown"}, 3); len(got) != 0 {
		t.Errorf("no host → fail closed to silence, got %v", cautionHitRefs(got))
	}
	if got := r.RetrieveCautions(Query{Host: "web01"}, 3); len(got) != 0 {
		t.Errorf("no rule → fail closed to silence, got %v", cautionHitRefs(got))
	}
}

// TestRetrieveCautionsBoundAndRecency pins the top-k bound and most-recent-first ordering: with two matching
// cautions and k=1, the single hit is the FRESHER failure (the more relevant warning).
func TestRetrieveCautionsBoundAndRecency(t *testing.T) {
	corpus := []Incident{
		{ExternalRef: "c-old", Host: "web01", AlertRule: "ServiceDown", ResolvedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), Source: ProvenanceCaution},
		{ExternalRef: "c-recent", Host: "web01", AlertRule: "ServiceDown", ResolvedAt: time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC), Source: ProvenanceCaution},
	}
	r := NewLexicalRetriever(corpus)
	got := cautionHitRefs(r.RetrieveCautions(Query{Host: "web01", AlertRule: "ServiceDown"}, 1))
	if len(got) != 1 || got[0] != "c-recent" {
		t.Fatalf("top-1 must be the most-recent failure, got %v", got)
	}
}

// TestCautionContextIsASeparateAvoidBlock pins that cautions render as their OWN clearly-labeled block, never
// the precedent block, carry the reflection, and render "" when empty (no blanket caveat).
func TestCautionContextIsASeparateAvoidBlock(t *testing.T) {
	hits := []Hit{{Incident: Incident{ExternalRef: "c1", Host: "web01", AlertRule: "ServiceDown",
		Resolution: "a prior attempt on web01/ServiceDown ran \"restart\", and the post-state DEVIATED — it was not a confirmed-clean resolution."}}}
	block := CautionContext(hits)
	if !strings.HasPrefix(block, "CAUTION") {
		t.Errorf("caution block must lead with CAUTION, got %q", block)
	}
	if strings.Contains(block, "PRIOR PRECEDENT") {
		t.Errorf("caution block must NOT be the precedent block, got %q", block)
	}
	if !strings.Contains(block, "c1") || !strings.Contains(block, "DEVIATED") {
		t.Errorf("caution block must name the ref + carry the reflection, got %q", block)
	}
	if CautionContext(hits) == Context(hits) {
		t.Errorf("caution and precedent renderings of the same rows must differ")
	}
	if CautionContext(nil) != "" {
		t.Errorf("empty cautions must render empty (no blanket caveat)")
	}
}

func cautionHitRefs(hits []Hit) []string {
	out := make([]string, 0, len(hits))
	for _, h := range hits {
		out = append(out, h.Incident.ExternalRef)
	}
	return out
}
