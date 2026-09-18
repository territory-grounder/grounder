package lessons

import (
	"strings"
	"testing"

	"github.com/territory-grounder/grounder/core/knowledge"
	"github.com/territory-grounder/grounder/core/safety"
)

// TestCautionIsTheComplementOfLesson pins TG-52's load-bearing hygiene: Caution and Lesson PARTITION the
// outcomes — a confirmed-clean success is a PRECEDENT and never a caution; a deviation/partial/unverified
// trajectory with an attempted action is a CAUTION and never a precedent; a no-action or identity-less record
// is neither. No session is ever both. Each direction is RED under the obvious mutation (routing a failure
// into the precedent branch, or stamping a caution with precedent provenance).
func TestCautionIsTheComplementOfLesson(t *testing.T) {
	cases := []struct {
		name          string
		ri            ResolvedIncident
		wantPrecedent bool
		wantCaution   bool
	}{
		{"confirmed-clean success → precedent, NOT caution",
			ResolvedIncident{ExternalRef: "i-1", Action: "restart nginx", Verdict: safety.VerdictMatch, ConfirmedClear: true}, true, false},
		{"deviation with action → caution, NOT precedent",
			ResolvedIncident{ExternalRef: "i-2", Action: "restart nginx", Verdict: safety.VerdictDeviation, ConfirmedClear: false}, false, true},
		{"partial with action → caution",
			ResolvedIncident{ExternalRef: "i-3", Action: "grow disk", Verdict: safety.VerdictPartial, ConfirmedClear: false}, false, true},
		{"match but UNCONFIRMED with action → caution (asserted, not verified)",
			ResolvedIncident{ExternalRef: "i-4", Action: "restart svc", Verdict: safety.VerdictMatch, ConfirmedClear: false}, false, true},
		{"no action attempted → neither",
			ResolvedIncident{ExternalRef: "i-5", Action: "", Verdict: safety.VerdictDeviation, ConfirmedClear: false}, false, false},
		{"no external_ref → neither",
			ResolvedIncident{ExternalRef: "", Action: "restart nginx", Verdict: safety.VerdictDeviation}, false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, gotPrecedent := Lesson(tc.ri)
			c, gotCaution := Caution(tc.ri)
			if gotPrecedent != tc.wantPrecedent {
				t.Errorf("Lesson precedent = %v, want %v", gotPrecedent, tc.wantPrecedent)
			}
			if gotCaution != tc.wantCaution {
				t.Errorf("Caution = %v, want %v", gotCaution, tc.wantCaution)
			}
			if gotPrecedent && gotCaution {
				t.Errorf("HYGIENE VIOLATION: this outcome is BOTH a precedent and a caution — they must be mutually exclusive")
			}
			if gotCaution && c.Source != knowledge.ProvenanceCaution {
				t.Errorf("CORPUS POISONING: a caution's Source = %q, want ProvenanceCaution — a failed trajectory must never carry precedent provenance", c.Source)
			}
		})
	}
}

// TestDistillAndDistillCautionsPartition pins that the batch distillers route each incident to at most one
// lane, and that every failed-with-action incident lands in exactly the caution lane.
func TestDistillAndDistillCautionsPartition(t *testing.T) {
	resolved := []ResolvedIncident{
		{ExternalRef: "ok-1", Action: "restart", Verdict: safety.VerdictMatch, ConfirmedClear: true},   // precedent
		{ExternalRef: "dev-1", Action: "restart", Verdict: safety.VerdictDeviation},                    // caution
		{ExternalRef: "unv-1", Action: "restart", Verdict: safety.VerdictMatch, ConfirmedClear: false}, // caution
		{ExternalRef: "noact-1", Action: "", Verdict: safety.VerdictDeviation},                         // neither
	}
	precRefs := cautionRefSet(Distill(resolved))
	cautRefs := cautionRefSet(DistillCautions(resolved))
	if !precRefs["ok-1"] || len(precRefs) != 1 {
		t.Errorf("precedents = %v, want exactly {ok-1}", precRefs)
	}
	if !cautRefs["dev-1"] || !cautRefs["unv-1"] || len(cautRefs) != 2 {
		t.Errorf("cautions = %v, want exactly {dev-1, unv-1}", cautRefs)
	}
	for ref := range precRefs {
		if cautRefs[ref] {
			t.Errorf("ref %q is in BOTH lanes — the distillers must partition", ref)
		}
	}
}

// TestCautionNeverOutranksPrecedentOnMerge is defense in depth: the primary hygiene guard is the separate
// store, but if a caution row ever reached the precedent corpus, a real precedent under the same ref MUST win
// the merge (ProvenanceCaution ranks below every real class). A mutation giving cautions a winning rank
// reddens this.
func TestCautionNeverOutranksPrecedentOnMerge(t *testing.T) {
	precedent := knowledge.Incident{ExternalRef: "x", Resolution: "restart nginx", Source: knowledge.ProvenanceVerifiedResolution}
	caution := knowledge.Incident{ExternalRef: "x", Resolution: "a prior attempt failed", Source: knowledge.ProvenanceCaution}
	merged := knowledge.MergeCorpus([]knowledge.Incident{precedent}, []knowledge.Incident{caution}) // caution is the newer row
	var got knowledge.Incident
	for _, inc := range merged {
		if inc.ExternalRef == "x" {
			got = inc
		}
	}
	if got.Source != knowledge.ProvenanceVerifiedResolution {
		t.Errorf("a caution overwrote a verified precedent on same-ref merge (Source=%q) — corpus poisoning", got.Source)
	}
}

// TestCautionReflectionIsFactualAndNamesTheAttempt pins that the caution CONTENT is a genuine reflection that
// names what was attempted and how it fell short — a fact, so the lane is not empty and gives the reader
// something to weigh.
func TestCautionReflectionIsFactualAndNamesTheAttempt(t *testing.T) {
	c, ok := Caution(ResolvedIncident{ExternalRef: "r", Host: "web01", AlertRule: "ServiceDown",
		Action: "systemctl restart nginx", Verdict: safety.VerdictDeviation})
	if !ok {
		t.Fatal("expected a caution")
	}
	for _, want := range []string{"systemctl restart nginx", "web01", "ServiceDown", "DEVIATED"} {
		if !strings.Contains(c.Resolution, want) {
			t.Errorf("caution reflection %q is missing %q", c.Resolution, want)
		}
	}
}

func cautionRefSet(incs []knowledge.Incident) map[string]bool {
	m := make(map[string]bool, len(incs))
	for _, inc := range incs {
		m[inc.ExternalRef] = true
	}
	return m
}
