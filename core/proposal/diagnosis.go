package proposal

import "strings"

// Diagnosis is the typed, source-bound CLAIM a proposal rests on (TG-201).
//
// ★ WHY A TYPE AND NOT A LONGER RATIONALE. Until this existed, a proposal carried `Rationale string` plus a
// FLAT `EvidenceIDs []string`, and the citation gate (agent/loop.go, REQ-1007/INV-11) was all-or-nothing: it
// could assert "at least one cited id was actually gathered" and nothing more. It could not express
// "assertion 2 of 4 is uncited", and — the expensive gap — it had NO WAY AT ALL to represent
// "observation lnms-x CONTRADICTS the stated cause".
//
// That is the recorded A2 root cause, not a hypothetical. On the same incident the predecessor checks PVE
// task history, sees the guest was stopped DELIBERATELY, and stands down. TG holds the very same
// observation and proposes a restart anyway — because nothing binds a piece of evidence to the assertion it
// bears on. It is not fixable by prompt engineering: there was no field to put the contradiction in. This
// is that field.
//
// DATA ONLY, NEVER AUTHORITY (INV-08). Nothing here decides anything by itself. A populated Contradicting
// list does not veto a proposal — a model can be wrong about what contradicts what. It makes the
// contradiction VISIBLE: to the judge as a scored dimension, to the operator on the console, and to the
// gate as a signal it can act on deterministically. The whole value is that the claim becomes checkable.
//
// The json tags are the DURABLE shape (session_triage.diagnosis, migration 0056) — the claim has to survive
// the trip to the asynchronous judge, which runs hours after the session on a record, not on the live
// session. They deliberately mirror the model-facing grammar's key names (parse.go's diagnosisJSON), so the
// wire form, the stored form and the judged form read identically in a console, a psql session and a
// transcript. No behaviour depends on them; they exist so a persisted claim is legible.
type Diagnosis struct {
	// RootCause is the single stated cause. One claim, not a paragraph — a diagnosis that needs prose to
	// state is one nothing can be bound to. EMPTY IS A LEGITIMATE ANSWER: "I ruled these out and I do not
	// know" is honest triage, and the judge dimension scores it as such (core/judge.ScoreDiagnosis).
	RootCause string `json:"root_cause"`
	// Mechanism is HOW the cause produces the symptom. Separate from RootCause because "disk full" and
	// "journald grew unbounded because vacuuming is disabled" are different claims with different fixes.
	Mechanism string `json:"mechanism"`
	// Supporting and Contradicting are evidence the model says bears FOR and AGAINST its own root cause.
	// Contradicting is the point of this type: a model that has seen disconfirming evidence and proposes
	// anyway is now saying so out loud, in a field, rather than leaving it in an unread transcript.
	Supporting    []EvidenceRef `json:"supporting,omitempty"`
	Contradicting []EvidenceRef `json:"contradicting,omitempty"`
	// RuledOut is the alternative causes the model considered and discarded, each with its reason. An empty
	// list on a confident proposal is itself informative.
	RuledOut []RuledOut `json:"ruled_out,omitempty"`
}

// EvidenceRef binds one assertion to one orchestrator-captured observation.
//
// Cited is NOT derived from ID being non-empty: a model naming an id the orchestrator never captured is
// exactly the failure this guards — a plausible, well-formed, fabricated citation. Cited means "this id was
// matched against the ToolResults the orchestrator actually gathered, and it was there".
type EvidenceRef struct {
	// ID is the orchestrator-captured ToolResult id the model cited, or "" when it asserted without one.
	ID string `json:"id"`
	// Claim is what this evidence is being offered as proof OF. Without it a citation is a bare pointer and
	// nobody can tell whether the evidence supports the assertion or merely coexists with it.
	Claim string `json:"claim"`
	// Cited is true ONLY when ID matched a gathered ToolResult. An uncited assertion is kept, never
	// dropped — dropping it would hide that the model asserted something it could not ground.
	//
	// PERSISTED (not recomputed downstream) because the ToolResults that decided it are not kept forever:
	// the judge runs on the durable record, and re-deriving `cited` there would either need the whole
	// transcript or would trust the model's own id — which is the failure this flag exists to prevent.
	Cited bool `json:"cited"`
}

// RuledOut is one alternative cause and why it was discarded.
type RuledOut struct {
	Cause  string `json:"cause"`
	Reason string `json:"reason"`
	// ID is the observation that ruled it out, when the model named one.
	ID    string `json:"id"`
	Cited bool   `json:"cited"`
}

// Present reports whether the model returned a diagnosis at all. Absent is NOT a defect: this field is
// additive and a proposal without one behaves exactly as before, so nothing regresses on the day it ships.
//
// ★ RuledOut COUNTS (TG-201 part 1). It did not, and that omission punished the one behaviour this whole
// type is meant to reward. The honest-uncertainty shape — "I ruled out X and Y, each against a captured
// observation; the root cause is still unknown" — sets ONLY RuledOut. Under the old predicate it read as
// ABSENT, so agent/loop.go (which binds only when Present) never called BindEvidence on it, every ruled-out
// alternative kept Cited=false, and the judge dimension would have scored the most honest diagnosis a model
// can return as "nothing cited". A rubric that punishes admitting uncertainty trains the agent to fabricate
// confidence — so the predicate has to see the shape before the rubric can score it.
func (d Diagnosis) Present() bool {
	return strings.TrimSpace(d.RootCause) != "" || len(d.Supporting) > 0 || len(d.Contradicting) > 0 ||
		len(d.RuledOut) > 0
}

// HasContradiction reports whether the model cited GROUNDED evidence against its own root cause.
//
// Grounded is the operative word: only refs that matched a real gathered observation count. A model can
// otherwise manufacture a contradiction out of an id nobody captured, and a signal that can be conjured
// from nothing is a signal that will eventually be conjured from nothing.
func (d Diagnosis) HasContradiction() bool {
	for _, e := range d.Contradicting {
		if e.Cited {
			return true
		}
	}
	return false
}

// UncitedAssertions counts assertions carrying no grounded observation — the "assertion 2 of 4 is uncited"
// the flat []string could never express.
func (d Diagnosis) UncitedAssertions() int {
	n := 0
	for _, e := range append(append([]EvidenceRef(nil), d.Supporting...), d.Contradicting...) {
		if !e.Cited {
			n++
		}
	}
	for _, r := range d.RuledOut {
		if !r.Cited {
			n++
		}
	}
	return n
}

// CitedAssertions counts assertions that DID land on a gathered observation — the positive half of
// UncitedAssertions, needed because "no uncited assertions" and "grounded in something" are different
// facts: a diagnosis that names a root cause and binds NOTHING at all has zero uncited assertions and zero
// grounding, and a rubric that only counted the uncited ones would score it perfect (core/judge).
func (d Diagnosis) CitedAssertions() int {
	n := 0
	for _, e := range append(append([]EvidenceRef(nil), d.Supporting...), d.Contradicting...) {
		if e.Cited {
			n++
		}
	}
	for _, r := range d.RuledOut {
		if r.Cited {
			n++
		}
	}
	return n
}

// AssertsRootCause reports whether the model committed to a named cause. Deliberately distinct from
// Present: a diagnosis that ruled alternatives out and named no cause is PRESENT and asserts nothing, which
// is the honest-uncertainty shape the judge must not punish.
func (d Diagnosis) AssertsRootCause() bool { return strings.TrimSpace(d.RootCause) != "" }

// BindEvidence marks each ref Cited iff its id is among the observations the ORCHESTRATOR captured.
//
// The gathered set is the authority, never the model's own claim about it. This is the same property
// INV-11 rests on: the orchestrator captured these ToolResult ids, so a citation is checkable against
// something the model could not author.
func (d Diagnosis) BindEvidence(gathered map[string]struct{}) Diagnosis {
	bind := func(refs []EvidenceRef) []EvidenceRef {
		out := make([]EvidenceRef, 0, len(refs))
		for _, e := range refs {
			e.ID = strings.TrimSpace(e.ID)
			_, ok := gathered[e.ID]
			e.Cited = e.ID != "" && ok
			out = append(out, e)
		}
		return out
	}
	d.Supporting = bind(d.Supporting)
	d.Contradicting = bind(d.Contradicting)
	for i := range d.RuledOut {
		d.RuledOut[i].ID = strings.TrimSpace(d.RuledOut[i].ID)
		_, ok := gathered[d.RuledOut[i].ID]
		d.RuledOut[i].Cited = d.RuledOut[i].ID != "" && ok
	}
	return d
}
