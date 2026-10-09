package runner

import (
	"strings"
	"testing"

	"github.com/territory-grounder/grounder/core/ingest"
	"github.com/territory-grounder/grounder/core/knowledge"
)

// TestCautionSeedBlockIsSeparateAndTargeted pins the TG-52 retrieval boundary end to end through the runner:
// a caution surfaces ONLY for the matching (host, rule-family), renders as its OWN block (never the precedent
// block), records its attribution, and rides a distinct <caution> envelope in the composed seed — its content
// never leaks into <precedent>. Uses a REAL caution Holder so the strict targeting is exercised, not faked.
func TestCautionSeedBlockIsSeparateAndTargeted(t *testing.T) {
	cautionCorpus := []knowledge.Incident{{
		ExternalRef: "c-web01-down", Host: "web01", AlertRule: "NginxDown",
		Resolution: `a prior attempt on web01/NginxDown ran "restart nginx", and the post-state DEVIATED — it was not a confirmed-clean resolution.`,
		Source:     knowledge.ProvenanceCaution,
	}}
	a := &Activities{D: Deps{Cautions: knowledge.NewHolder(knowledge.NewLexicalRetriever(cautionCorpus))}}

	// Unrelated incident → NO caution block (the whole over-caution safety).
	if blk, _ := a.caution(ingest.IncidentEnvelope{ExternalRef: "i-x", Host: "db09", AlertRule: "DiskFull"}); blk != "" {
		t.Errorf("caution must NOT surface for an unrelated (host,rule), got %q", blk)
	}

	// Matching incident → caution block, distinct from precedent, carrying the reflection + its attribution.
	blk, notes := a.caution(ingest.IncidentEnvelope{ExternalRef: "i-1", Host: "web01", AlertRule: "NginxDown"})
	if !strings.HasPrefix(blk, "CAUTION") || !strings.Contains(blk, "DEVIATED") {
		t.Fatalf("caution block must surface for the matching signature, got %q", blk)
	}
	if strings.Contains(blk, "PRIOR PRECEDENT") {
		t.Errorf("caution must never render as the precedent block")
	}
	if !cautionNotesHas(notes, "caution:c-web01-down") {
		t.Errorf("caution attribution note missing, got %v", notes)
	}

	// Composed seed: the caution rides its OWN <caution> envelope, and its content is never inside <precedent>.
	env := ingest.IncidentEnvelope{ExternalRef: "i-1", Host: "web01", AlertRule: "NginxDown"}
	seed, _ := composeSeed(env, "", "", "", "", "", "PRECEDENT-DATA", blk, "", "guide")
	if !strings.Contains(seed, "<caution>") || !strings.Contains(seed, "</caution>") {
		t.Fatalf("composed seed must carry the <caution> envelope:\n%s", seed)
	}
	precStart, precEnd, cautIdx := strings.Index(seed, "<precedent>"), strings.Index(seed, "</precedent>"), strings.Index(seed, "CAUTION")
	if precStart >= 0 && cautIdx > precStart && cautIdx < precEnd {
		t.Errorf("caution content leaked INTO the precedent block:\n%s", seed)
	}
}

func cautionNotesHas(ss []string, want string) bool {
	for _, s := range ss {
		if s == want {
			return true
		}
	}
	return false
}
