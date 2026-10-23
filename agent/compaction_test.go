package agent

import (
	"strings"
	"testing"

	"github.com/territory-grounder/grounder/adapters/model"
)

// obsMsg builds a tool-observation user message in the observationEnvelope SUCCESS shape (loop.go).
func obsMsg(id, tool, payload string) model.Message {
	return model.Message{Role: "user", Content: "TOOL_OUTCOME[" + id + "]: SUCCEEDED — the " + tool +
		" call returned; the OBSERVATION below is a real reading you may cite.\nOBSERVATION[" + id + "]: " + payload}
}

// obsMsgFailed builds a tool-observation user message in the observationEnvelope FAILED shape (loop.go:1017 —
// a longer TOOL_OUTCOME sentence, the same OBSERVATION[<id>] suffix).
func obsMsgFailed(id, tool, payload string) model.Message {
	return model.Message{Role: "user", Content: "TOOL_OUTCOME[" + id + "]: FAILED — the " + tool +
		" call did NOT succeed. What follows is a failure message, not a reading of the estate: it may read like a real answer, but it proves nothing, so do NOT cite " + id + " as evidence in a proposal or a stop. Fix the call or try a DIFFERENT tool; if it keeps failing, say so and stop — this attempt already spent one of your limited cycles.\nOBSERVATION[" + id + "]: " + payload}
}

func msgsIdentical(a, b []model.Message) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].Role != b[i].Role || a[i].Content != b[i].Content {
			return false
		}
	}
	return true
}

// TestCompactionElidesOldPreservesIDsAndRecent is the HEADLINE oracle (TG-47), red before compaction.go existed:
// a transcript over budget has its OLDEST observation payloads elided, but EVERY observation id stays
// present+citable, the most recent compactKeepRecent observations and the seed stay byte-identical, and the
// result is brought under budget.
func TestCompactionElidesOldPreservesIDsAndRecent(t *testing.T) {
	big := strings.Repeat("x", 5000)
	seed := []model.Message{{Role: "user", Content: "PREAMBLE + SEED (incident context — never compacted)"}}
	obs := []model.Message{
		obsMsg("trk-1", "get-logs", "OLD-1 "+big),
		obsMsg("trk-2", "get-logs", "OLD-2 "+big),
		obsMsg("trk-3", "get-logs", "OLD-3 "+big),
		obsMsg("trk-4", "get-logs", "RECENT-4 "+big),
		obsMsg("trk-5", "get-logs", "RECENT-5 "+big),
		obsMsg("trk-6", "get-logs", "RECENT-6 "+big),
	}
	msgs := append(append([]model.Message{}, seed...), obs...) // ~30 KB of observations
	out := compactObservationBudget(msgs, len(seed), 20000)

	total := 0
	for _, m := range out {
		total += len(m.Content)
	}
	if total > 20000 {
		t.Errorf("compaction left %d bytes, over the 20000 budget", total)
	}
	// THE SAFETY INVARIANT: every observation id is still present, so every reading stays citable.
	var joined strings.Builder
	for _, m := range out {
		joined.WriteString(m.Content)
		joined.WriteByte('\n')
	}
	all := joined.String()
	for _, id := range []string{"trk-1", "trk-2", "trk-3", "trk-4", "trk-5", "trk-6"} {
		if !strings.Contains(all, "OBSERVATION["+id+"]:") {
			t.Errorf("observation id %s was DROPPED — no longer citable (compaction must preserve every id)", id)
		}
	}
	if out[0].Content != seed[0].Content {
		t.Error("the seed/preamble prefix was modified — it must stay verbatim")
	}
	// the most recent compactKeepRecent (trk-4/5/6) stay VERBATIM (still carry the full 5 KB payload).
	for _, id := range []string{"trk-4", "trk-5", "trk-6"} {
		verbatim := false
		for _, m := range out {
			if strings.Contains(m.Content, "OBSERVATION["+id+"]:") && strings.Contains(m.Content, big) {
				verbatim = true
			}
		}
		if !verbatim {
			t.Errorf("recent observation %s was elided — the most recent %d must stay verbatim", id, compactKeepRecent)
		}
	}
	// the oldest (trk-1) WAS elided: its id is kept but the big payload is gone, replaced by the marker.
	elided := false
	for _, m := range out {
		if strings.Contains(m.Content, "OBSERVATION[trk-1]:") {
			elided = strings.Contains(m.Content, compactElisionMarker) && !strings.Contains(m.Content, big)
		}
	}
	if !elided {
		t.Error("the oldest observation trk-1 was NOT elided despite the transcript being over budget")
	}
}

// TestCompactionNoOpCases: a 0 budget disables compaction, and an under-budget transcript is byte-identical.
func TestCompactionNoOpCases(t *testing.T) {
	big := strings.Repeat("y", 3000)
	base := []model.Message{{Role: "user", Content: "SEED"}, obsMsg("trk-1", "t", "A "+big), obsMsg("trk-2", "t", "B "+big)}
	orig := append([]model.Message{}, base...)
	if out := compactObservationBudget(append([]model.Message{}, base...), 1, 0); !msgsIdentical(out, orig) {
		t.Error("budget 0 must disable compaction (byte-identical)")
	}
	if out := compactObservationBudget(append([]model.Message{}, base...), 1, 1_000_000); !msgsIdentical(out, orig) {
		t.Error("an under-budget transcript must be byte-identical")
	}
}

// TestCompactionIdempotent: compacting twice equals compacting once (the elision marker guards re-elision, so a
// second pass over an already-elided payload does nothing).
func TestCompactionIdempotent(t *testing.T) {
	big := strings.Repeat("z", 5000)
	mk := func() []model.Message {
		return []model.Message{
			{Role: "user", Content: "SEED"},
			obsMsg("trk-1", "t", "1 "+big), obsMsg("trk-2", "t", "2 "+big), obsMsg("trk-3", "t", "3 "+big),
			obsMsg("trk-4", "t", "4 "+big), obsMsg("trk-5", "t", "5 "+big),
		}
	}
	once := compactObservationBudget(mk(), 1, 12000)
	twice := compactObservationBudget(compactObservationBudget(mk(), 1, 12000), 1, 12000)
	if !msgsIdentical(once, twice) {
		t.Error("compaction is not idempotent — a second pass changed the result")
	}
}

// TestCompactionSkipsNonObservations: nudge/rejection user messages (not a TOOL_OUTCOME[ envelope) are never
// touched, even when large — only tool observations carry a discardable payload.
func TestCompactionSkipsNonObservations(t *testing.T) {
	big := strings.Repeat("q", 8000)
	nudge := model.Message{Role: "user", Content: "REJECTED — ungrounded proposal. " + big}
	msgs := []model.Message{
		{Role: "user", Content: "SEED"},
		obsMsg("trk-1", "t", "1 "+big), obsMsg("trk-2", "t", "2 "+big),
		obsMsg("trk-3", "t", "3 "+big), obsMsg("trk-4", "t", "4 "+big),
		nudge,
	}
	out := compactObservationBudget(msgs, 1, 12000)
	found := false
	for _, m := range out {
		if m.Content == nudge.Content {
			found = true
		}
	}
	if !found {
		t.Error("a non-observation message (a REJECTED nudge) was compacted — only TOOL_OUTCOME[ observations may be")
	}
}

// TestCompactionPreservesFailedEnvelopeID exercises the FAILED envelope shape (a longer TOOL_OUTCOME sentence,
// same OBSERVATION[<id>] suffix) on the safety-critical path: compaction must elide its payload but keep the id
// AND the FAILED header verbatim, exactly like the SUCCESS shape — the shape most likely to break.
func TestCompactionPreservesFailedEnvelopeID(t *testing.T) {
	big := strings.Repeat("f", 6000)
	msgs := []model.Message{
		{Role: "user", Content: "SEED"},
		obsMsgFailed("trk-fail", "get-logs", "FAILURE-DETAIL "+big), // oldest → elidable
		obsMsg("trk-2", "t", "2 "+big), obsMsg("trk-3", "t", "3 "+big),
		obsMsg("trk-4", "t", "4 "+big), obsMsg("trk-5", "t", "5 "+big),
	}
	out := compactObservationBudget(msgs, 1, 20000)
	var found, elided bool
	for _, m := range out {
		if strings.Contains(m.Content, "OBSERVATION[trk-fail]:") {
			found = true
			elided = strings.Contains(m.Content, compactElisionMarker) && !strings.Contains(m.Content, big)
			if !strings.Contains(m.Content, "TOOL_OUTCOME[trk-fail]: FAILED") {
				t.Error("the FAILED envelope's TOOL_OUTCOME[...] header was corrupted by compaction")
			}
		}
	}
	if !found {
		t.Error("FAILED observation id trk-fail was DROPPED — no longer citable")
	}
	if !elided {
		t.Error("the oldest FAILED observation payload was not elided despite being over budget")
	}
}
