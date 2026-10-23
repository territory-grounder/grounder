package db

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/territory-grounder/grounder/core/judge"
	"github.com/territory-grounder/grounder/core/proposal"
	"github.com/territory-grounder/grounder/core/trace"
)

// theA2Claim is the recorded failure in its own shape: the agent proposes a restart while HOLDING a grounded
// observation that an operator stopped the guest deliberately, plus one supporting assertion whose id the
// orchestrator never captured (a fabricated citation, which must stay visible as uncited).
func theA2Claim() proposal.Diagnosis {
	return proposal.Diagnosis{
		RootCause: "guest 101 is down because its unit failed to start",
		Mechanism: "systemd gave up after 3 restart attempts inside 60s",
		Supporting: []proposal.EvidenceRef{
			{ID: "incident-history-101", Claim: "two prior unclean shutdowns", Cited: true},
			{ID: "unit-config-101", Claim: "the unit restarts on boot", Cited: false},
		},
		Contradicting: []proposal.EvidenceRef{
			{ID: "pve-task-history-101", Claim: "root@pam ran vzstop on 101 four minutes before the alert", Cited: true},
		},
		RuledOut: []proposal.RuledOut{
			{Cause: "host out of memory", Reason: "the node reports 41% in use", ID: "host-metrics", Cited: true},
		},
	}
}

// TG-201 — THE PROJECTION KEEPS THE EVIDENCE AGAINST THE CLAIM.
//
// diagnosisRead is where the load-bearing property of the whole feature can be lost in silence: drop the
// Contradicting lane and the row still reads, the route still serves, the console still renders a claim — one
// that looks unopposed while the agent held a grounded observation against it. That is the A2 failure with a
// typed field on top of it.
//
// KILLING MUTATION (executed): drop `Contradicting: refs(d.Contradicting)` from diagnosisRead. RED — "the
// projection dropped the CONTRADICTING evidence".
func TestTheReadProjectionKeepsTheEvidenceAgainstTheClaim(t *testing.T) {
	rec := diagnosisRead("ext-1", theA2Claim())

	if len(rec.Contradicting) != 1 || !strings.Contains(rec.Contradicting[0].Claim, "vzstop") {
		t.Fatalf("the projection dropped the CONTRADICTING evidence (%+v) — the served claim then reads as "+
			"unopposed, and an operator auditing it can never learn the agent had disconfirming evidence in hand",
			rec.Contradicting)
	}
	if !rec.HasGroundedContradiction() {
		t.Fatal("the served record does not report a grounded contradiction — the console's marker reads that " +
			"answer from here")
	}
	if rec.ExternalRef != "ext-1" {
		t.Fatalf("external_ref = %q, want ext-1 — the console keys the claim to the walk by this field", rec.ExternalRef)
	}
}

// CITED IS COPIED, NEVER RE-DERIVED. Only agent/loop.go held the gathered ToolResult set; anything this
// projection "derived" would be the model's own word about its own citation.
//
// KILLING MUTATION (executed): in diagnosisRead, set Cited from `r.ID != ""` instead of copying r.Cited.
// RED — "an assertion whose id the orchestrator never captured was served as CITED".
func TestReadCitedIsCopiedFromTheOrchestratorsDecision(t *testing.T) {
	rec := diagnosisRead("ext-1", theA2Claim())
	if len(rec.Supporting) != 2 {
		t.Fatalf("supporting refs = %d, want 2 — an ungrounded assertion was dropped rather than kept and marked",
			len(rec.Supporting))
	}
	if rec.Supporting[1].Cited {
		t.Fatal("an assertion whose id the orchestrator never captured was served as CITED — a fabricated " +
			"citation promoted to evidence, which is exactly what INV-11 exists to prevent")
	}
	if rec.Supporting[1].ID == "" {
		t.Fatal("the ungrounded citation id was blanked — \"asserted with no citation\" and \"cited an id nobody " +
			"captured\" are different failures and the console tells them apart by this field")
	}
}

// PARITY WITH THE DOMAIN TYPE. core/trace cannot import core/proposal (it is the dependency-free seam every
// read surface shares), so the two carry their own copies of Present / HasContradiction / UncitedAssertions.
// This is the only place both are visible — if they disagree, two surfaces are saying different things about
// the SAME claim, and Present in particular decides 404-vs-200 on the console.
//
// ★ THIS CAUGHT A REAL DRIFT. trace.Present did not count RuledOut while proposal.Present had already been
// corrected to count it (TG-201 part 1). The honest-uncertainty shape — "I ruled these out against captured
// observations and I still do not know the cause" — was therefore PRESENT to the agent and the judge and
// ABSENT to this surface, so the console would have told an operator that no claim was recorded for exactly
// the sessions whose working is most worth reading.
//
// KILLING MUTATION (executed): remove `len(d.RuledOut) > 0` from trace.SessionDiagnosis.Present. RED —
// "record.Present=false, domain Present=true for the ruled-out-only claim".
func TestTheReadSeamAndTheDomainTypeAgreeAboutTheSameClaim(t *testing.T) {
	// The ruled-out-only shape is listed FIRST because it is the one that drifted; a table that only carried
	// the fully-populated claim agrees by luck.
	cases := map[string]proposal.Diagnosis{
		"ruled out only, no cause named": {
			RuledOut: []proposal.RuledOut{
				{Cause: "host out of memory", Reason: "the node reports 41% in use", ID: "host-metrics", Cited: true},
			},
		},
		"the A2 claim":         theA2Claim(),
		"root cause only":      {RootCause: "the disk filled"},
		"supporting only":      {Supporting: []proposal.EvidenceRef{{ID: "e1", Claim: "c", Cited: true}}},
		"contradicting only":   {Contradicting: []proposal.EvidenceRef{{ID: "e2", Claim: "c", Cited: false}}},
		"nothing bound at all": {},
	}
	for name, dom := range cases {
		t.Run(name, func(t *testing.T) {
			rec := diagnosisRead("ext-1", dom)
			if rec.Present() != dom.Present() {
				t.Errorf("record.Present=%v, domain Present=%v for the same claim — the read decides 404-vs-200 "+
					"with one and the agent/judge decide existence with the other, so the console would report "+
					"\"no typed claim was recorded\" about a claim the judge is scoring",
					rec.Present(), dom.Present())
			}
			if rec.HasGroundedContradiction() != dom.HasContradiction() {
				t.Errorf("record.HasGroundedContradiction=%v but proposal.HasContradiction=%v for the SAME claim — "+
					"the served answer and the agent-side answer have drifted",
					rec.HasGroundedContradiction(), dom.HasContradiction())
			}
			if rec.UncitedAssertions() != dom.UncitedAssertions() {
				t.Errorf("record counts %d uncited assertions, the domain type counts %d — the console's "+
					"\"N uncited\" chip would then disagree with the dimension scored off the same row",
					rec.UncitedAssertions(), dom.UncitedAssertions())
			}
		})
	}
}

// An over-long field is CLIPPED AND SAYS SO. The column is written whole (the judge must grade what the agent
// actually said), so the bound belongs on the read — and a body silently cut is a lie told by the one surface
// an operator opens to check the claim against its evidence.
//
// KILLING MUTATION (executed): return `rec` instead of `rec.Bound()` from diagnosisRead. RED — "an over-long
// field was served whole and the clipped flag stayed false".
func TestAnOverlongClaimIsClippedAndSaysSo(t *testing.T) {
	rec := diagnosisRead("ext-1", proposal.Diagnosis{
		RootCause: strings.Repeat("x", trace.MaxDiagnosisField+512),
	})
	if len(rec.RootCause) > trace.MaxDiagnosisField {
		t.Errorf("root cause served at %d bytes, bound is %d", len(rec.RootCause), trace.MaxDiagnosisField)
	}
	if !rec.Clipped {
		t.Error("an over-long field was clipped without setting Clipped — the console renders that flag, and a " +
			"body cut in silence is the surface lying about the claim it was opened to check")
	}
	// VACUITY FLOOR: a claim inside the bound must NOT be flagged, or the assertion above passes for a
	// projection that simply marks everything clipped.
	if diagnosisRead("ext-1", theA2Claim()).Clipped {
		t.Error("a claim well inside the bound was flagged clipped — the flag then carries no information")
	}
}

// TG-201 — THE OPERATOR READS THE EXACT BYTES THE JUDGE SCORED, THROUGH REAL POSTGRES.
//
// This is the whole point of the rebase. The claim has ONE store: session_triage.diagnosis. RecordTriage
// writes it for the asynchronous judge; Diagnosis serves it to the console. Exercised through the store's own
// pair — never hand-written SQL — so it fails if either half drifts and cannot pass by agreeing with a query
// written in this file. Against a REAL Postgres: a fake round-trips a column missing from the INSERT
// perfectly, which is exactly the defect this file exists to catch.
//
// KILLING MUTATION (executed): change the SELECT in TriageStore.Diagnosis to read `conclusion` instead of
// `diagnosis`. RED against real Postgres — the decode fails and the claim never reaches the console.
func TestTheConsoleReadsTheSameStoredClaimTheJudgeScores(t *testing.T) {
	dsn := skipWithoutDB(t)
	ctx := context.Background()
	p, err := Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer p.Close()

	st := &TriageStore{p: p}
	const ref = "gold-diagnosis-read-1"
	_, _ = p.Exec(ctx, `DELETE FROM session_triage WHERE external_ref = $1`, ref)
	defer func() { _, _ = p.Exec(ctx, `DELETE FROM session_triage WHERE external_ref = $1`, ref) }()

	if err := st.RecordTriage(ctx, judge.TriageRow{
		ExternalRef: ref, Host: "dc1pve01", AlertRule: "HostDown", Band: "POLL_PAUSE",
		Outcome: "proposed", Proposed: true, Op: "start-guest", CreatedAt: time.Now().UTC(),
		DiagnosisRecorded: true, Diagnosis: theA2Claim(),
	}); err != nil {
		t.Fatalf("record: %v", err)
	}

	got, err := st.Diagnosis(ctx, ref)
	if err != nil {
		t.Fatalf("read back the claim the judge will score: %v", err)
	}
	if len(got.Contradicting) != 1 || !strings.Contains(got.Contradicting[0].Claim, "vzstop") {
		t.Fatalf("the CONTRADICTING lane did not survive the round trip (%+v) — the operator's surface and the "+
			"judge's dimension read the same column, so this is the console showing an unopposed claim",
			got.Contradicting)
	}
	if !got.Contradicting[0].Cited {
		t.Fatal("the orchestrator's `cited` flag was lost in the round trip — a grounded contradiction came back " +
			"as an ungrounded assertion, which is the one distinction the whole mechanism rests on")
	}
	if len(got.Supporting) != 2 || got.Supporting[1].Cited {
		t.Fatalf("supporting refs came back %+v — the uncited assertion must survive AND stay marked", got.Supporting)
	}
	if !got.HasGroundedContradiction() || got.UncitedAssertions() != 1 {
		t.Fatalf("served contradiction=%v uncited=%d, want true/1", got.HasGroundedContradiction(), got.UncitedAssertions())
	}

	// A session that recorded no claim is ErrDiagnosisNotFound, NEVER a zero-value record: the console renders
	// "no typed claim was recorded" for the first and "the agent asserted nothing" for the second, and they are
	// different facts about the estate.
	const bare = "gold-diagnosis-read-2"
	_, _ = p.Exec(ctx, `DELETE FROM session_triage WHERE external_ref = $1`, bare)
	defer func() { _, _ = p.Exec(ctx, `DELETE FROM session_triage WHERE external_ref = $1`, bare) }()
	if err := st.RecordTriage(ctx, judge.TriageRow{
		ExternalRef: bare, Host: "dc1pve01", AlertRule: "HostDown", Band: "AUTO",
		Outcome: "grounded_stop", CreatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("record bare: %v", err)
	}
	if _, err := st.Diagnosis(ctx, bare); !errors.Is(err, trace.ErrDiagnosisNotFound) {
		t.Fatalf("a session that bound no claim answered %v, want ErrDiagnosisNotFound — anything else puts an "+
			"empty claim on the console, which reads as \"the agent asserted nothing\"", err)
	}
	if _, err := st.Diagnosis(ctx, "gold-diagnosis-no-such-session"); !errors.Is(err, trace.ErrDiagnosisNotFound) {
		t.Fatalf("an unknown session answered %v, want ErrDiagnosisNotFound", err)
	}
}
