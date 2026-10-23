package trace

import (
	"context"
	"errors"
	"strings"
	"unicode/utf8"
)

// SessionDiagnosis is the READ-SIDE form of the typed CLAIM a proposal rests on (TG-201) — what the operator
// console reads back on the #reasoning surface.
//
// THERE IS EXACTLY ONE STORE FOR THIS FACT, AND IT IS NOT HERE. The claim is persisted by the terminal triage
// write as `session_triage.diagnosis` (migration 0056, core/proposal.Diagnosis marshalled whole), because the
// ASYNCHRONOUS judge scores it off that row hours later. This type is a PROJECTION of that column, produced by
// core/db.TriageStore.Diagnosis on the way to the HTTP surface — never a second copy of the claim. An earlier
// cut of this feature landed a parallel `session_diagnosis` TABLE with its own writer, which is precisely the
// two-stores-for-one-fact drift this codebase keeps paying for: the judge would have graded one store while
// the operator read the other, and nothing would have said which was stale.
//
// WHY A SECOND SHAPE AND NOT core/proposal.Diagnosis DIRECTLY. core/trace deliberately imports NOTHING from
// this repo (check: `go list -deps ./core/trace`). It is the seam every read surface shares, and pulling
// core/proposal in would drag core/manifest, core/safety and core/breaker behind it into the HTTP read path.
// The domain type stays where the agent loop uses it; this is its projection, and the two are held to
// agreement by a parity oracle (core/db/diagnosis_read_test.go) so they cannot drift.
//
// WHY THE SURFACE EXISTS AT ALL. Before it, core/proposal.Diagnosis was bound to the gathered evidence in
// agent/loop.go, logged as a screen note when it contradicted itself, persisted for a judge that runs hours
// later — and shown to nobody. The whole point of a typed claim is that a HUMAN can check the agent's
// reasoning against its evidence, and an operator could not see it. A claim only a cron can read is prose
// with extra steps.
//
// DATA ONLY, NEVER AUTHORITY (INV-08). Nothing here decides anything. It is read over an authenticated GET
// and consumed by a renderer.
type SessionDiagnosis struct {
	ExternalRef string
	// RootCause and Mechanism are the two halves of the claim: WHAT is broken and HOW that produces the
	// symptom. Separate because "disk full" and "journald grew unbounded because vacuuming is disabled" are
	// different claims with different fixes.
	RootCause string
	Mechanism string
	// Supporting and Contradicting are the evidence the model said bears FOR and AGAINST its own root cause.
	// Contradicting is the reason this record exists: the recorded A2 failure is TG holding a disconfirming
	// observation and proposing anyway, with no field to put it in and no surface to show it on.
	Supporting    []DiagnosisRef
	Contradicting []DiagnosisRef
	// RuledOut is the alternatives the model considered and discarded, each with its reason.
	RuledOut []DiagnosisAlternative
	// Clipped records that at least one text field was longer than MaxDiagnosisField and was cut. A clipped
	// body that does not SAY it is clipped is a lie told by the one surface an operator opens to check
	// whether the agent's claim matches the evidence — the same honesty contract agent_step_evidence holds.
	Clipped bool
}

// DiagnosisRef binds one assertion to one orchestrator-captured observation.
//
// Cited is NOT "ID is non-empty". A model naming an id the orchestrator never captured is exactly the failure
// this guards — a plausible, well-formed, fabricated citation. It is decided in agent/loop.go against the
// ToolResults the orchestrator actually gathered, and it travels here already decided: nothing downstream may
// re-derive it, because everything downstream only has the model's word.
type DiagnosisRef struct {
	ID    string
	Claim string
	Cited bool
}

// DiagnosisAlternative is one discarded cause and why it was discarded.
type DiagnosisAlternative struct {
	Cause  string
	Reason string
	ID     string
	Cited  bool
}

// ErrDiagnosisNotFound signals that no typed claim was recorded for a session.
//
// It is an ORDINARY answer, not a fault: a stand-down records no proposal at all, a model may return a
// proposal with no diagnosis (the field is optional by construction so nothing regressed on the day it
// shipped), and every session recorded before migration 0056 has none. The console renders "no typed claim
// was recorded" for those — which is true — rather than an error banner or, worse, an empty claim that reads
// as "the agent asserted nothing".
//
// It lives HERE, beside the reader interface, for the same reason ErrEvidenceNotFound does: core/db imports
// core/httpapi, so a handler that needed to name a db-side sentinel to map it to 404 would close an import
// cycle, and an unnameable sentinel becomes a 503 — "the store is broken" reported for the completely
// ordinary case of a session that simply has no claim.
var ErrDiagnosisNotFound = errors.New("trace: no typed diagnosis recorded for that session")

// MaxDiagnosisField bounds one SERVED text field of a diagnosis.
//
// The type's own contract is "one claim, not a paragraph", so 4 KiB is far above any honest value and exists
// only to stop a model that looped a token into the jsonb column from being handed whole to a console that
// renders it. It bounds the READ because the column is written whole for the judge — the grader must see what
// the agent actually said, while the operator's page must not be a denial-of-service surface.
const MaxDiagnosisField = 4 * 1024

// MaxDiagnosisRefs bounds how many refs one list may carry. A diagnosis citing more than 64 observations per
// side is not a diagnosis; it is a dump, and rendering it would bury the one contradiction that matters.
const MaxDiagnosisRefs = 64

// Bound returns the record clipped to the serving bounds, RECORDING ON THE RECORD ITSELF that it clipped
// anything — the console renders that flag, because a body silently cut is a lie told by the one surface an
// operator opens to check whether the agent's claim matches its evidence. Rune-safe: slicing mid-sequence
// leaves a trailing partial rune that renders as U+FFFD, which reads as CORRUPTED rather than merely cut —
// the worse of the two lies on an evidence surface.
func (d SessionDiagnosis) Bound() SessionDiagnosis {
	clipped := false
	clip := func(s string) string {
		out, cut := clipRunes(s, MaxDiagnosisField)
		clipped = clipped || cut
		return out
	}
	d.RootCause, d.Mechanism = clip(d.RootCause), clip(d.Mechanism)
	boundRefs := func(in []DiagnosisRef) []DiagnosisRef {
		if len(in) > MaxDiagnosisRefs {
			in, clipped = in[:MaxDiagnosisRefs], true
		}
		out := make([]DiagnosisRef, 0, len(in))
		for _, r := range in {
			r.ID, r.Claim = clip(r.ID), clip(r.Claim)
			out = append(out, r)
		}
		return out
	}
	d.Supporting, d.Contradicting = boundRefs(d.Supporting), boundRefs(d.Contradicting)
	if len(d.RuledOut) > MaxDiagnosisRefs {
		d.RuledOut, clipped = d.RuledOut[:MaxDiagnosisRefs], true
	}
	alts := make([]DiagnosisAlternative, 0, len(d.RuledOut))
	for _, a := range d.RuledOut {
		a.Cause, a.Reason, a.ID = clip(a.Cause), clip(a.Reason), clip(a.ID)
		alts = append(alts, a)
	}
	d.RuledOut = alts
	d.Clipped = d.Clipped || clipped
	return d
}

// clipRunes bounds s to n bytes on a RUNE boundary, reporting whether it cut anything. Same backoff as
// Truncate (the evidence path) and for the same reason: DecodeLastRuneInString returns (RuneError, 1) for a
// sequence this slice cut in half and (RuneError, 3) for a legitimately-encoded U+FFFD the model itself
// emitted, so size distinguishes our damage from its content.
func clipRunes(s string, n int) (string, bool) {
	if len(s) <= n {
		return s, false
	}
	b := s[:n]
	for len(b) > 0 {
		if r, size := utf8.DecodeLastRuneInString(b); r != utf8.RuneError || size > 1 {
			break
		}
		b = b[:len(b)-1]
	}
	return b, true
}

// Present reports whether a diagnosis was recorded at all. It MIRRORS core/proposal.Diagnosis.Present, and the
// mirror is load-bearing: the read decides 404-vs-200 with this predicate, so a divergence means the console
// says "no typed claim was recorded" about a claim the judge is busy scoring. Held to that by a parity oracle
// (core/db/diagnosis_read_test.go).
//
// ★ RuledOut COUNTS. It did not here, and the domain type had already been corrected to count it (TG-201
// part 1) — so the honest-uncertainty shape, "I ruled out X and Y against captured observations and I do not
// know the cause", read as PRESENT to the agent and the judge and as ABSENT to this surface. The one operator
// who most needs to see the working would have been told nothing was recorded.
func (d SessionDiagnosis) Present() bool {
	return strings.TrimSpace(d.RootCause) != "" || len(d.Supporting) > 0 || len(d.Contradicting) > 0 ||
		len(d.RuledOut) > 0
}

// HasGroundedContradiction reports whether the model cited GROUNDED evidence against its own root cause.
//
// Grounded is the operative word, exactly as in the domain type: only refs that matched a real gathered
// observation count, because a signal that can be conjured from an uncaptured id is a signal that will
// eventually be conjured from nothing.
func (d SessionDiagnosis) HasGroundedContradiction() bool {
	for _, e := range d.Contradicting {
		if e.Cited {
			return true
		}
	}
	return false
}

// UncitedAssertions counts assertions carrying no grounded observation — the "assertion 2 of 4 is uncited"
// a flat []string could never express, and the number the console marks in the render.
func (d SessionDiagnosis) UncitedAssertions() int {
	n := 0
	for _, e := range append(append([]DiagnosisRef(nil), d.Supporting...), d.Contradicting...) {
		if !e.Cited {
			n++
		}
	}
	for _, a := range d.RuledOut {
		if !a.Cited {
			n++
		}
	}
	return n
}

// THERE IS NO DiagnosisSink, ON PURPOSE. This seam is READ-ONLY because the claim has exactly one writer —
// the terminal triage record (temporal/runner, core/db.TriageStore.RecordTriage, session_triage.diagnosis).
// A second sink here is what produced the duplicate `session_diagnosis` table this feature briefly shipped:
// two writers, two stores, one fact, and a judge grading whichever one it happened to be pointed at.

// DiagnosisReader serves one session's recorded claim to the console. ErrDiagnosisNotFound for a session that
// recorded none — never a zero-value record, which the console cannot distinguish from an empty claim.
type DiagnosisReader interface {
	Diagnosis(ctx context.Context, externalRef string) (SessionDiagnosis, error)
}
