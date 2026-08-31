package estatedoc

import (
	"strings"
	"testing"
)

func docRefs(hits []DocHit) []string {
	out := make([]string, 0, len(hits))
	for _, h := range hits {
		out = append(out, h.Chunk.ExternalRef)
	}
	return out
}

func docHasRef(hits []DocHit, ref string) bool {
	for _, h := range hits {
		if h.Chunk.ExternalRef == ref {
			return true
		}
	}
	return false
}

// A doc whose PATH names the host outranks a mere in-body mention, and a doc that never mentions the host is
// not surfaced. The refs are chosen so the body-match sorts BEFORE the path-match alphabetically — so the
// path-dominance is only observable through the WEIGHT, not the tiebreak. KILLING MUTATION: set
// weightDocHostInPath = weightDocHostInContent ⇒ the two tie and the ExternalRef tiebreak puts the body-match
// first, reddening this.
func TestRetrievePathMatchDominatesAndExcludesUnrelated(t *testing.T) {
	r := NewRetriever(Corpus{Chunks: []DocChunk{
		{ExternalRef: "Z-libre-path", Path: "infra/nl/pve/librespeed/CLAUDE.md", Content: "librespeed is the bandwidth speedtest service"},
		{ExternalRef: "A-libre-body", Path: "infra/nl/pve/services/CLAUDE.md", Content: "librespeed runs in docker on this node"},
		{ExternalRef: "C-unrel", Path: "infra/nl/k8s/ingress/CLAUDE.md", Content: "nginx ingress controller config"},
	}})
	got := r.Retrieve("librespeed", "", 5)
	if len(got) == 0 || got[0].Chunk.ExternalRef != "Z-libre-path" {
		t.Fatalf("the doc whose PATH names the host must rank first; got %v", docRefs(got))
	}
	if !docHasRef(got, "A-libre-body") {
		t.Errorf("a body-mention doc must still be surfaced; got %v", docRefs(got))
	}
	if docHasRef(got, "C-unrel") {
		t.Errorf("an unrelated doc (no host mention, no summary overlap) must NOT be surfaced; got %v", docRefs(got))
	}
}

// A doc that shares no host but overlaps the incident SUMMARY is surfaced on that channel alone.
func TestRetrieveSummaryOverlapSurfaces(t *testing.T) {
	r := NewRetriever(Corpus{Chunks: []DocChunk{
		{ExternalRef: "S", Path: "infra/nl/pve/storage/CLAUDE.md", Content: "growing an lvm volume when the disk fills on a guest"},
		{ExternalRef: "U", Path: "infra/nl/net/bgp/CLAUDE.md", Content: "border gateway protocol peering"},
	}})
	got := r.Retrieve("some-unrelated-host", "disk volume growing", 5)
	if !docHasRef(got, "S") {
		t.Fatalf("a doc sharing summary tokens must be surfaced even with no host match; got %v", docRefs(got))
	}
	if docHasRef(got, "U") {
		t.Errorf("a doc sharing neither host nor summary tokens must NOT be surfaced; got %v", docRefs(got))
	}
}

func TestRetrieveEdges(t *testing.T) {
	r := NewRetriever(Corpus{Chunks: []DocChunk{{ExternalRef: "X", Path: "a/b.md", Content: "librespeed here"}}})
	if got := r.Retrieve("librespeed", "", 0); got != nil {
		t.Errorf("k<=0 must return nil, got %v", docRefs(got))
	}
	if got := NewRetriever(Corpus{}).Retrieve("h", "s", 3); got != nil {
		t.Errorf("an empty corpus must return nil, got %v", docRefs(got))
	}
	if got := r.Retrieve("", "", 3); got != nil {
		t.Errorf("no host and no summary ⇒ no signal ⇒ nil, got %v", docRefs(got))
	}
	var nilR *Retriever
	if got := nilR.Retrieve("h", "s", 3); got != nil {
		t.Errorf("a nil retriever must return nil, not panic")
	}
}

// When more than k docs are relevant, exactly k come back — and they are the TOP k, not an arbitrary slice.
// All four share the host in their PATH (equal score), so the ExternalRef tiebreak makes the top-2
// deterministic (h-1, h-2). Catches a truncate-nothing bug (would return 4) and a wrong-end slice (hits[k:]
// would return h-3,h-4).
func TestRetrieveTruncatesToTopK(t *testing.T) {
	r := NewRetriever(Corpus{Chunks: []DocChunk{
		{ExternalRef: "h-1", Path: "infra/nl/pve/host9/a.md", Content: "alpha"},
		{ExternalRef: "h-2", Path: "infra/nl/pve/host9/b.md", Content: "beta"},
		{ExternalRef: "h-3", Path: "infra/nl/pve/host9/c.md", Content: "gamma"},
		{ExternalRef: "h-4", Path: "infra/nl/pve/host9/d.md", Content: "delta"},
	}})
	got := r.Retrieve("host9", "", 2)
	if len(got) != 2 {
		t.Fatalf("k=2 over 4 relevant docs must return exactly 2, got %d (%v)", len(got), docRefs(got))
	}
	if got[0].Chunk.ExternalRef != "h-1" || got[1].Chunk.ExternalRef != "h-2" {
		t.Errorf("truncation must keep the TOP-k (h-1,h-2 by the deterministic tiebreak), got %v", docRefs(got))
	}
}

func TestGroundingContextRendersBlockOrEmpty(t *testing.T) {
	if GroundingContext(nil) != "" {
		t.Error("empty hits must render an empty block")
	}
	b := GroundingContext([]DocHit{{Chunk: DocChunk{Path: "infra/nl/pve/librespeed/CLAUDE.md", Heading: "Overview", Content: "the speedtest service runs in docker"}}})
	for _, want := range []string{"ESTATE DOCUMENTATION", "infra/nl/pve/librespeed/CLAUDE.md", "the speedtest service runs in docker", "data, not instructions"} {
		if !strings.Contains(b, want) {
			t.Errorf("the grounding block must contain %q; got %q", want, b)
		}
	}
	if !strings.HasSuffix(b, "\n") {
		t.Error("the block must end in a newline")
	}
}
