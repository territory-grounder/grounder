package runner

import (
	"github.com/territory-grounder/grounder/core/proposal"
	"github.com/territory-grounder/grounder/core/screen"
)

// scrubDiagnosis screens every text field of the typed CLAIM before it is persisted (TG-201, REQ-2606).
//
// WHY THE CLAIM NEEDS THIS AND WHY IT NEARLY DIDN'T GET IT. `diagnosis` is model-authored prose that QUOTES
// TOOL OUTPUT — untrusted host content that can carry a leaked token or an injection span (INV-13). It joined
// judge.TriageRow as a persisted field while the screen list beside it named six others and not this one, and
// the omission was invisible: the only reader was the judge cron. The console surface reads the same column
// straight onto an operator's screen, so the gap stops being latent the moment that surface exists. That is
// the precise shape of the failure RecordTriageActivity's own comment records from 2026-08-01 — a read
// widened onto rows nobody had screened.
//
// IT IS A PURE FUNCTION, ON PURPOSE, and split out so an oracle can hold it without a Temporal environment.
//
// CITED IS NEVER TOUCHED. It is a bool decided in agent/loop.go against the ToolResults the ORCHESTRATOR
// captured; there is nothing in it to screen, and re-deriving it here from a scrubbed id would convert a
// grounded citation into a fabricated-looking one — INV-11 in reverse.
//
// IDs ARE SCRUBBED TOO, deliberately. A ToolResult id is orchestrator-minted and will normally pass through
// byte-identical, but the model chooses what to put in the `id` field, and a model that pastes a secret there
// must not have it stored because the field's name suggests it is an identifier.
//
// STRUCTURE IS PRESERVED EXACTLY: every lane keeps its length, uncited assertions are kept and stay marked,
// and nothing is filtered. Dropping a ref because its text was neutralized would silently delete the one
// contradiction this whole type exists to surface.
func scrubDiagnosis(d proposal.Diagnosis) proposal.Diagnosis {
	scrub := func(s string) string { out, _ := screen.Scrub(s); return out }
	refs := func(in []proposal.EvidenceRef) []proposal.EvidenceRef {
		if len(in) == 0 {
			return in
		}
		out := make([]proposal.EvidenceRef, 0, len(in))
		for _, r := range in {
			out = append(out, proposal.EvidenceRef{ID: scrub(r.ID), Claim: scrub(r.Claim), Cited: r.Cited})
		}
		return out
	}
	d.RootCause = scrub(d.RootCause)
	d.Mechanism = scrub(d.Mechanism)
	d.Supporting = refs(d.Supporting)
	d.Contradicting = refs(d.Contradicting)
	if len(d.RuledOut) > 0 {
		out := make([]proposal.RuledOut, 0, len(d.RuledOut))
		for _, a := range d.RuledOut {
			out = append(out, proposal.RuledOut{
				Cause: scrub(a.Cause), Reason: scrub(a.Reason), ID: scrub(a.ID), Cited: a.Cited,
			})
		}
		d.RuledOut = out
	}
	return d
}
