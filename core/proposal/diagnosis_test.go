package proposal

import "testing"

func gathered(ids ...string) map[string]struct{} {
	m := map[string]struct{}{}
	for _, i := range ids {
		m[i] = struct{}{}
	}
	return m
}

// THE CASE THIS TYPE EXISTS FOR (TG-201, axis A2). On the recorded incident the predecessor checks PVE task
// history, sees the guest was stopped DELIBERATELY, and stands down. TG held the SAME observation and
// proposed a restart, because a flat []string could say "this observation is relevant" and could not say
// "this observation argues against my own conclusion".
//
// KILLING MUTATION: drop Contradicting and keep one flat evidence list. RED — the contradiction becomes
// unrepresentable and this assertion cannot even be written.
func TestEvidenceAgainstTheRootCauseIsRepresentable(t *testing.T) {
	d := Diagnosis{
		RootCause: "the guest crashed and needs restarting",
		Supporting: []EvidenceRef{
			{ID: "lnms-dev-dc1pve01", Claim: "the guest is not running"},
		},
		Contradicting: []EvidenceRef{
			{ID: "pve-tasks-101", Claim: "the stop was a DELIBERATE operator task, not a crash"},
		},
	}.BindEvidence(gathered("lnms-dev-dc1pve01", "pve-tasks-101"))

	if !d.HasContradiction() {
		t.Fatal("grounded evidence against the root cause did not register as a contradiction — this is " +
			"the A2 failure: TG proposes a restart while holding the observation that refutes it")
	}
}

// KILLING MUTATION: set Cited from `ID != ""` instead of matching the gathered set. RED — a model naming an
// id the orchestrator never captured would manufacture a contradiction (or a citation) out of nothing, and
// a signal that can be conjured from nothing will eventually be conjured from nothing.
func TestAFabricatedIdIsNotAGroundedCitation(t *testing.T) {
	d := Diagnosis{
		RootCause:     "disk full",
		Contradicting: []EvidenceRef{{ID: "observation-that-was-never-captured", Claim: "df says 12% free"}},
	}.BindEvidence(gathered("lnms-dev-x"))

	if d.Contradicting[0].Cited {
		t.Fatal("an id the orchestrator never captured was marked cited — the model authored its own proof")
	}
	if d.HasContradiction() {
		t.Fatal("a fabricated id raised the contradiction signal; only GROUNDED evidence may raise it")
	}
}

// KILLING MUTATION: drop uncited refs during binding. RED — dropping them hides that the model asserted
// something it could not ground, which is precisely what the all-or-nothing gate already failed to show.
func TestAnUncitedAssertionIsKeptAndCounted(t *testing.T) {
	d := Diagnosis{
		RootCause: "journald grew unbounded",
		Supporting: []EvidenceRef{
			{ID: "check-host-disk-x", Claim: "/ is at 98%"},
			{ID: "", Claim: "vacuuming is disabled"}, // asserted with no observation at all
		},
	}.BindEvidence(gathered("check-host-disk-x"))

	if len(d.Supporting) != 2 {
		t.Fatalf("binding dropped an assertion (%d of 2 kept) — an ungrounded claim must stay VISIBLE", len(d.Supporting))
	}
	if n := d.UncitedAssertions(); n != 1 {
		t.Fatalf("uncited=%d, want 1 — 'assertion 2 of 2 is uncited' is the thing the flat []string could "+
			"never express, and it is why this type exists", n)
	}
}

// Additive by construction: a proposal with no diagnosis must behave exactly as before.
func TestAnAbsentDiagnosisIsNotAFailure(t *testing.T) {
	var d Diagnosis
	if d.Present() {
		t.Fatal("an empty diagnosis reported present")
	}
	if d.HasContradiction() || d.UncitedAssertions() != 0 {
		t.Fatal("an absent diagnosis produced a signal — this field is additive and must not change behaviour")
	}
}

// A ruled-out alternative is bound the same way; an unsourced dismissal is exactly as interesting as an
// unsourced claim.
func TestARuledOutAlternativeIsBoundToo(t *testing.T) {
	d := Diagnosis{
		RootCause: "service crashed",
		RuledOut:  []RuledOut{{Cause: "disk full", Reason: "/ is at 12%", ID: "check-host-disk-x"}, {Cause: "OOM", Reason: "no evidence sought"}},
	}.BindEvidence(gathered("check-host-disk-x"))

	if !d.RuledOut[0].Cited {
		t.Fatal("a grounded ruled-out alternative was not bound")
	}
	if d.RuledOut[1].Cited {
		t.Fatal("an alternative dismissed with no observation was marked cited")
	}
	if n := d.UncitedAssertions(); n != 1 {
		t.Fatalf("uncited=%d, want 1 — an unsourced dismissal must count", n)
	}
}

// THE HONEST-UNCERTAINTY SHAPE MUST BE VISIBLE BEFORE IT CAN BE SCORED (TG-201 part 1).
//
// "I ruled out X and Y, each against a captured observation; the root cause is still unknown" sets ONLY
// RuledOut. Present() used to ignore RuledOut, so this read as ABSENT — and agent/loop.go binds evidence
// only when Present, so every ruled-out alternative kept Cited=false and the judge dimension would have
// scored the most honest answer a model can give as "asserted, cited nothing". A rubric that punishes
// admitting uncertainty trains the agent to fabricate confidence.
//
// KILLING MUTATION: drop `|| len(d.RuledOut) > 0` from Present(). RED — the honest diagnosis reports absent
// and its citations never bind, so honest uncertainty is graded as an ungrounded claim.
func TestRuledOutOnlyDiagnosisIsPresentAndBinds(t *testing.T) {
	d := Diagnosis{
		RuledOut: []RuledOut{
			{Cause: "disk full", Reason: "/ is at 12%", ID: "check-host-disk-x"},
			{Cause: "OOM", Reason: "no oom-kill in the journal", ID: "check-host-journal-x"},
		},
	}
	if !d.Present() {
		t.Fatal("a diagnosis that ruled alternatives out and named no cause reported ABSENT — the honest " +
			"'I do not know yet' shape is invisible, so nothing binds it and nothing can score it")
	}
	d = d.BindEvidence(gathered("check-host-disk-x", "check-host-journal-x"))
	if n := d.CitedAssertions(); n != 2 {
		t.Fatalf("cited=%d, want 2 — the ruled-out alternatives never bound to the observations that ruled them out", n)
	}
	if n := d.UncitedAssertions(); n != 0 {
		t.Fatalf("uncited=%d, want 0 — an honest, fully-sourced 'root cause unknown' must carry no ungrounded assertion", n)
	}
	if d.AssertsRootCause() {
		t.Fatal("an empty root cause reported as an assertion — admitting uncertainty is not a claim")
	}
}

// A named cause with NOTHING bound is the case UncitedAssertions alone cannot see: zero refs means zero
// uncited, which would read as flawless. CitedAssertions is the other half of that measurement.
//
// KILLING MUTATION: make CitedAssertions return len(Supporting)+len(Contradicting)+len(RuledOut). RED —
// an unbound assertion counts as grounded and an evidence-free claim scores as a sourced one.
func TestAClaimWithNothingBoundHasNoGrounding(t *testing.T) {
	d := Diagnosis{RootCause: "the service crashed"}.BindEvidence(gathered("lnms-x"))
	if !d.AssertsRootCause() {
		t.Fatal("a named root cause did not report as an assertion")
	}
	if n := d.CitedAssertions(); n != 0 {
		t.Fatalf("cited=%d, want 0 — a bare claim with no evidence refs is grounded in nothing", n)
	}
	if n := d.UncitedAssertions(); n != 0 {
		t.Fatalf("uncited=%d, want 0 — there are no assertions to be uncited; the defect is the ABSENCE of any", n)
	}
}
