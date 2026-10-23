package skillstore

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/territory-grounder/grounder/core/audit"
)

// composeGen answers the composer from a script and keeps every prompt it was shown.
type composeGen struct {
	replies []string
	prompts []string
	err     error
}

func (g *composeGen) Complete(_ context.Context, _, _ string, prompt string) (string, error) {
	g.prompts = append(g.prompts, prompt)
	if g.err != nil {
		return "", g.err
	}
	if len(g.replies) == 0 {
		return "", errors.New("composeGen: out of replies")
	}
	r := g.replies[0]
	g.replies = g.replies[1:]
	return r, nil
}

type fakeProber struct{ reports map[string]string }

func (f fakeProber) ProbeDraft(_ context.Context, _ string, body string, _ map[string]bool) (string, error) {
	for marker, rep := range f.reports {
		if strings.Contains(body, marker) {
			return rep, nil
		}
	}
	return "", nil
}

func composeTrigger() GenTrigger {
	return GenTrigger{SkillName: "triage-protocol", Dimension: "falsifiable_prediction", Mean: 1.5, Threshold: 3.5, Window: 30, Source: "flywheel:eval-failure:run-9"}
}

// proposerDraft lands a draft shaped exactly like GenerateCandidatesFromEvidence lands it (name + cited rows).
func proposerDraft(t *testing.T, m *MemStore, cur Version, name, suffix, body, rows string) Version {
	t.Helper()
	v, err := m.CreateVersion(context.Background(), Version{
		SkillName: cur.SkillName, Version: cur.Version + "-" + suffix, Body: body, AppliesWhen: cur.AppliesWhen,
		ContentHash: ContentHash(body, cur.AppliesWhen), Author: "flywheel", Source: "flywheel:eval-failure:run-9",
		Rationale: "[draft] " + triggerRationale(composeTrigger()) + " — proposer " + name + "; edits cite the record:\n" + rows, ParentVersionID: cur.ID,
	})
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func composeReply(body string) string {
	return "<notes>\nkept -> stop after the second repeated read -> INV row 1\nkept -> name the disk id in the stop -> DOM row 2\ndropped -> the always-on checklist -> INV probe: the replicate stopped where production proposed\n</notes>\n<body>\n" + body + "\n</body>"
}

// A merge: both proposals' compatible edits become ONE composed draft whose rationale carries the notes; the
// composer read the base, both bodies, both edit-row sets and both probe reports; the two proposals are REJECTED,
// naming the composed candidate.
func TestComposeMergesTwoProposalsIntoOneCandidate(t *testing.T) {
	m, lg, cur := genStore(t)
	inv := proposerDraft(t, m, cur, "INV", "cand1", "INV-BODY: stop after a repeated read.", "revised -> stop earlier -> inc-1:[STEP 2] re-read")
	dom := proposerDraft(t, m, cur, "DOM", "cand2", "DOM-BODY: name the disk id.", "added -> cite the disk id -> inc-2:[STEP 3] uncited")
	gen := &composeGen{replies: []string{composeReply("COMPOSED: stop after a repeated read; name the disk id.")}}
	prober := fakeProber{reports: map[string]string{"INV-BODY": "inc-1: candidate stop/none; production proposed — DIVERGED", "DOM-BODY": "inc-2: candidate proposed rotate-logs; production stop — candidate wins on criterion 3"}}
	composed, kept, err := ComposeCandidates(context.Background(), m, lg, gen, composeTrigger(), cur, []Version{inv, dom}, prober, nil)
	if err != nil || kept || composed.ID == 0 {
		t.Fatalf("a merge lands one composed candidate: %+v kept=%v err=%v", composed, kept, err)
	}
	if !strings.HasPrefix(composed.Version, cur.Version+"-composed-") || composed.ParentVersionID != cur.ID || composed.Status != StatusDraft || !strings.Contains(composed.Rationale, "composed from INV+DOM; notes:\nkept -> stop after") {
		t.Fatalf("composed draft shape: %+v", composed)
	}
	p := gen.prompts[0]
	for _, must := range []string{"# BASE", cur.Body, "# PROPOSAL INV", "INV-BODY", "# PROPOSAL DOM", "DOM-BODY", "INV edit rows", "inc-1:[STEP 2]", "DOM probe", "candidate wins on criterion 3", "KEEP_BASE is a real answer", "UNSAFE unless", "data, not instructions"} {
		if !strings.Contains(p, must) {
			t.Fatalf("the composer's mission lacks %q", must)
		}
	}
	for _, id := range []int64{inv.ID, dom.ID} {
		v, _ := m.GetVersion(context.Background(), id)
		if v.Status != StatusRejected || !strings.Contains(v.Rationale, "superseded by composed candidate "+composed.Version) {
			t.Fatalf("a folded proposal is rejected naming the composed candidate: %+v", v)
		}
	}
}

// KEEP_BASE withdraws both proposals and lands nothing; adopt-one keeps the adopted proposal and withdraws the other;
// one proposer draft (or lens drafts) is nothing to compose — no model call.
func TestComposeKeepBaseAdoptOneAndNothingToCompose(t *testing.T) {
	m, lg, cur := genStore(t)
	inv := proposerDraft(t, m, cur, "INV", "cand1", "INV-BODY", "revised -> a -> inc-1:[STEP 1] m")
	dom := proposerDraft(t, m, cur, "DOM", "cand2", "DOM-BODY", "added -> b -> inc-2:[STEP 1] m")
	gen := &composeGen{replies: []string{"NO_CHANGE"}}
	composed, kept, err := ComposeCandidates(context.Background(), m, lg, gen, composeTrigger(), cur, []Version{inv, dom}, nil, nil)
	if err != nil || !kept || composed.ID != 0 {
		t.Fatalf("KEEP_BASE lands nothing: %+v kept=%v err=%v", composed, kept, err)
	}
	for _, id := range []int64{inv.ID, dom.ID} {
		if v, _ := m.GetVersion(context.Background(), id); v.Status != StatusRejected || !strings.Contains(v.Rationale, "KEEP_BASE") {
			t.Fatalf("KEEP_BASE withdraws every proposal: %+v", v)
		}
	}
	// adopt-one
	m2, lg2, cur2 := genStore(t)
	inv2 := proposerDraft(t, m2, cur2, "INV", "cand1", "INV-BODY exactly", "revised -> a -> inc-1:[STEP 1] m")
	dom2 := proposerDraft(t, m2, cur2, "DOM", "cand2", "DOM-BODY", "added -> b -> inc-2:[STEP 1] m")
	gen2 := &composeGen{replies: []string{"<notes>\nkept -> everything INV proposed -> INV row 1\ndropped -> the DOM rule -> DOM probe: nothing delivered\n</notes>\n<body>\nINV-BODY exactly\n</body>"}}
	adopted, kept, err := ComposeCandidates(context.Background(), m2, lg2, gen2, composeTrigger(), cur2, []Version{inv2, dom2}, nil, nil)
	if err != nil || kept || adopted.ID != inv2.ID {
		t.Fatalf("a composition equal to one proposal adopts it: %+v kept=%v err=%v", adopted, kept, err)
	}
	if v, _ := m2.GetVersion(context.Background(), inv2.ID); v.Status != StatusDraft {
		t.Fatalf("the adopted proposal stays a draft for admission: %+v", v)
	}
	if v, _ := m2.GetVersion(context.Background(), dom2.ID); v.Status != StatusRejected || !strings.Contains(v.Rationale, "adopted INV") {
		t.Fatalf("the other proposal is withdrawn naming the adoption: %+v", v)
	}
	// nothing to compose
	m3, lg3, cur3 := genStore(t)
	only := proposerDraft(t, m3, cur3, "INV", "cand1", "INV-BODY", "revised -> a -> inc-1:[STEP 1] m")
	lens, _ := m3.CreateVersion(context.Background(), Version{SkillName: cur3.SkillName, Version: cur3.Version + "-cand9", Body: "lens body", AppliesWhen: cur3.AppliesWhen, ContentHash: ContentHash("lens body", cur3.AppliesWhen), Author: "flywheel", Source: "s", Rationale: "[draft] x — lens: concise", ParentVersionID: cur3.ID})
	gen3 := &composeGen{}
	if c, k, err := ComposeCandidates(context.Background(), m3, lg3, gen3, composeTrigger(), cur3, []Version{only, lens}, nil, nil); err != nil || k || c.ID != 0 || len(gen3.prompts) != 0 {
		t.Fatalf("one proposer draft is nothing to compose and costs no call: %+v %v %v %d", c, k, err, len(gen3.prompts))
	}
}

// The validator refuses a reply without note rows, a row without evidence, prose outside the blocks and an over-cap
// body — each refusal is retried ONCE with the validator's words; two refusals leave the proposals untouched.
func TestComposeValidatorRetriesOnceThenLeavesTheProposals(t *testing.T) {
	m, lg, cur := genStore(t)
	inv := proposerDraft(t, m, cur, "INV", "cand1", "INV-BODY", "revised -> a -> inc-1:[STEP 1] m")
	dom := proposerDraft(t, m, cur, "DOM", "cand2", "DOM-BODY", "added -> b -> inc-2:[STEP 1] m")
	gen := &composeGen{replies: []string{"<notes>\nkept -> x -> because I said so\n</notes>\n<body>\nX\n</body>", composeReply("COMPOSED after correction")}}
	composed, _, err := ComposeCandidates(context.Background(), m, lg, gen, composeTrigger(), cur, []Version{inv, dom}, nil, nil)
	if err != nil || composed.ID == 0 || len(gen.prompts) != 2 || !strings.Contains(gen.prompts[1], "YOUR PREVIOUS ATTEMPT WAS REJECTED") || !strings.Contains(gen.prompts[1], "cites no evidence") {
		t.Fatalf("an uncited row is retried once with the validator's words: %+v err=%v prompts=%d", composed, err, len(gen.prompts))
	}
	m2, lg2, cur2 := genStore(t)
	inv2 := proposerDraft(t, m2, cur2, "INV", "cand1", "INV-BODY", "revised -> a -> inc-1:[STEP 1] m")
	dom2 := proposerDraft(t, m2, cur2, "DOM", "cand2", "DOM-BODY", "added -> b -> inc-2:[STEP 1] m")
	gen2 := &composeGen{replies: []string{"Sure! <notes>\nkept -> x -> INV row 1\n</notes>\n<body>\nX\n</body>", "<notes>\n</notes>\n<body>\nX\n</body>"}}
	composed, kept, err := ComposeCandidates(context.Background(), m2, lg2, gen2, composeTrigger(), cur2, []Version{inv2, dom2}, nil, nil)
	if err != nil || kept || composed.ID != 0 {
		t.Fatalf("two refusals land nothing: %+v kept=%v err=%v", composed, kept, err)
	}
	for _, id := range []int64{inv2.ID, dom2.ID} {
		if v, _ := m2.GetVersion(context.Background(), id); v.Status != StatusDraft {
			t.Fatalf("a composer that cannot answer leaves the proposals as drafts: %+v", v)
		}
	}
	for name, bad := range map[string]string{
		"over cap":  "<notes>\nkept -> x -> INV row 1\n</notes>\n<body>\n" + strings.Repeat("y", 70000) + "\n</body>",
		"bad row":   "<notes>\nmaybe -> x -> INV row 1\n</notes>\n<body>\nX\n</body>",
		"no body":   "<notes>\nkept -> x -> INV row 1\n</notes>",
		"body only": "<body>\nX\n</body>",
	} {
		if _, err := parseComposition(bad, []string{"INV", "DOM"}, 8192); err == nil {
			t.Fatalf("%s must be refused", name)
		}
	}
}

// failingLedger fails its Nth append — a transient ledger fault in the middle of the proposals' withdrawal.
type failingLedger struct {
	inner Ledger
	n     int
	calls int
}

func (f *failingLedger) Append(d audit.GovDecision) (audit.LedgerEntry, error) {
	f.calls++
	if f.calls == f.n {
		return audit.LedgerEntry{}, errors.New("ledger: transient write failure")
	}
	return f.inner.Append(d)
}

// A withdrawal that fails midway never leaves three admissible rows: the composed draft is withdrawn too, the
// surviving proposal stays a draft, and StillDrafts hands the caller exactly that one.
func TestComposeCompensatesAFailedWithdrawal(t *testing.T) {
	m, lg, cur := genStore(t)
	inv := proposerDraft(t, m, cur, "INV", "cand1", "INV-BODY", "revised -> a -> inc-1:[STEP 1] m")
	dom := proposerDraft(t, m, cur, "DOM", "cand2", "DOM-BODY", "added -> b -> inc-2:[STEP 1] m")
	gen := &composeGen{replies: []string{composeReply("COMPOSED body")}}
	// the composed CreateVersion appends nothing; withdrawal 1 (INV) succeeds, withdrawal 2 (DOM) fails
	fl := &failingLedger{inner: lg, n: 2}
	composed, kept, err := ComposeCandidates(context.Background(), m, fl, gen, composeTrigger(), cur, []Version{inv, dom}, nil, nil)
	if err == nil || kept || composed.ID != 0 {
		t.Fatalf("a failed withdrawal must surface and return no candidate: %+v kept=%v err=%v", composed, kept, err)
	}
	drafts := 0
	for _, v := range []Version{} {
		_ = v
	}
	all, _ := m.FlywheelDrafts(context.Background())
	for _, v := range all {
		if v.SkillName == "triage-protocol" && v.Status == StatusDraft {
			drafts++
		}
	}
	if drafts != 1 {
		t.Fatalf("exactly ONE admissible row must remain (the un-withdrawn proposal), got %d", drafts)
	}
	survivors := StillDrafts(context.Background(), m, []Version{inv, dom})
	if len(survivors) != 1 || survivors[0].ID != dom.ID {
		t.Fatalf("StillDrafts must return the surviving proposal only: %+v", survivors)
	}
}

// The composed body obeys the workspace-facts rule: a session ref the proposals cite (or their probe names) may not
// be INTRODUCED into the body; the refusal is retried once with the rule named. A body equal to the base is KEEP_BASE.
func TestComposeRefusesIncidentFactsAndTreatsBaseAsKeep(t *testing.T) {
	m, lg, cur := genStore(t)
	inv := proposerDraft(t, m, cur, "INV", "cand1", "INV-BODY", "revised -> a -> inc-77:[STEP 1] m")
	dom := proposerDraft(t, m, cur, "DOM", "cand2", "DOM-BODY", "added -> b -> inc-78:[STEP 1] m")
	gen := &composeGen{replies: []string{composeReply("when inc-77 recurs, rotate the logs"), composeReply("COMPOSED without the fact")}}
	prober := fakeProber{reports: map[string]string{"INV-BODY": "inc-99: candidate stop; production proposed"}}
	composed, _, err := ComposeCandidates(context.Background(), m, lg, gen, composeTrigger(), cur, []Version{inv, dom}, prober, nil)
	if err != nil || composed.ID == 0 || len(gen.prompts) != 2 || !strings.Contains(gen.prompts[1], "fact about ONE incident") || !strings.Contains(gen.prompts[1], `"inc-77"`) {
		t.Fatalf("a body carrying a cited ref is refused once with the rule named, then the correction lands: %+v err=%v prompts=%d", composed, err, len(gen.prompts))
	}
	if !strings.Contains(composed.Version, "-composed-") || len(composed.Version) < len(cur.Version)+len("-composed-")+8 {
		t.Fatalf("the composed version is unique per body (content-hash suffix): %s", composed.Version)
	}
	m2, lg2, cur2 := genStore(t)
	inv2 := proposerDraft(t, m2, cur2, "INV", "cand1", "INV-BODY", "revised -> a -> inc-1:[STEP 1] m")
	dom2 := proposerDraft(t, m2, cur2, "DOM", "cand2", "DOM-BODY", "added -> b -> inc-2:[STEP 1] m")
	gen2 := &composeGen{replies: []string{composeReply(cur2.Body)}}
	c, kept, err := ComposeCandidates(context.Background(), m2, lg2, gen2, composeTrigger(), cur2, []Version{inv2, dom2}, nil, nil)
	if err != nil || !kept || c.ID != 0 {
		t.Fatalf("a composition equal to the base is KEEP_BASE: %+v kept=%v err=%v", c, kept, err)
	}
	// a probe ref introduced into the body is refused too
	if err := factsLeak(composition{body: "see inc-99 for the pattern"}, composeFacts([]proposalView{{v: inv, probe: "inc-99: candidate stop"}}, cur.Body)); err == nil {
		t.Fatal("a probe-named ref must be refused")
	}
	// notes are bounded
	long := strings.Repeat("kept -> x -> INV row 1\n", 30)
	if _, err := parseComposition("<notes>\n"+long+"</notes>\n<body>\nX\n</body>", []string{"INV"}, 8192); err == nil {
		t.Fatal("more than maxNoteRows rows must be refused")
	}
}

// runnerProber is a stub OfflineRunner that also probes — the production shape skillgen's runner has.
type runnerProber struct{ report string }

func (runnerProber) RunOffline(context.Context, Version, string) (OfflineResult, error) {
	return OfflineResult{RegressionPass: true, DiscoveryDelta: 1}, nil
}
func (r runnerProber) ProbeDraft(context.Context, string, string, map[string]bool) (string, error) {
	return r.report, nil
}

// Through the generator: two proposer drafts are composed into one candidate, the composer's mission carried the
// Runner-derived probe report, and the trigger yields exactly one draft.
func TestGenerateForVersionComposesProposalsWithTheRunnersProbe(t *testing.T) {
	m, lg, prod := genStore(t)
	gen := &capturingGen{replies: []string{validReply("INV body"), validReply("DOM body"), composeReply("COMPOSED from both")}}
	d := CreationDeps{Store: m, Ledger: lg, Model: gen, Evidence: fakeEvidence{packs: samplePacks()}, Runner: runnerProber{report: "inc-9: candidate proposed; production stop — PROBE-REPORT-MARKER"}}
	trig := GenTrigger{SkillName: "triage-protocol", Dimension: "falsifiable_prediction", Mean: 1.5, Threshold: 3.5, Window: 30, Source: "flywheel:eval-failure:run-1"}
	out, err := d.generateForVersion(context.Background(), trig, prod.ID)
	if err != nil || len(out) != 1 || !strings.Contains(out[0].Version, "-composed-") {
		t.Fatalf("two proposals compose into ONE candidate: %d err=%v %+v", len(out), err, out)
	}
	if len(gen.prompts) != 3 || !strings.Contains(gen.prompts[2], "PROBE-REPORT-MARKER") {
		t.Fatalf("the composer must read the Runner-derived probe report: %d prompts", len(gen.prompts))
	}
	all, _ := m.FlywheelDrafts(context.Background())
	drafts := 0
	for _, v := range all {
		if v.Status == StatusDraft && v.SkillName == "triage-protocol" {
			drafts++
		}
	}
	if drafts != 1 {
		t.Fatalf("exactly one admissible draft after composition, got %d", drafts)
	}
}

// Through the generator, a composer fault midway (one proposal withdrawn, the next withdrawal failing) yields ONLY
// the proposals still standing — never the withdrawn one, never the (compensated) composed draft.
func TestGenerateForVersionReturnsOnlySurvivorsAfterAComposerFault(t *testing.T) {
	m, lg, prod := genStore(t)
	gen := &capturingGen{replies: []string{validReply("INV body"), validReply("DOM body"), composeReply("COMPOSED from both")}}
	fl := &failingLedger{inner: lg, n: 2} // withdrawal 1 (INV) succeeds, withdrawal 2 (DOM) fails
	d := CreationDeps{Store: m, Ledger: fl, Model: gen, Evidence: fakeEvidence{packs: samplePacks()}, Runner: runnerProber{report: "r"}}
	trig := GenTrigger{SkillName: "triage-protocol", Dimension: "falsifiable_prediction", Mean: 1.5, Threshold: 3.5, Window: 30, Source: "flywheel:eval-failure:run-1"}
	out, err := d.generateForVersion(context.Background(), trig, prod.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 1 || out[0].Status != StatusDraft || !strings.Contains(out[0].Rationale, "proposer DOM") {
		t.Fatalf("only the surviving proposal (DOM) may be offered to admission, got %d: %+v", len(out), out)
	}
	all, _ := m.FlywheelDrafts(context.Background())
	drafts := 0
	for _, v := range all {
		if v.Status == StatusDraft && v.SkillName == "triage-protocol" {
			drafts++
		}
	}
	if drafts != 1 {
		t.Fatalf("exactly one admissible row after the fault, got %d", drafts)
	}
}
