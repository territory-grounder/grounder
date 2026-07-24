package policy

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/territory-grounder/grounder/core/safety"
)

// fixedClock is a deterministic clock for the audit-timestamp field.
func fixedClock() func() time.Time {
	t := time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC)
	return func() time.Time { return t }
}

// redAck is a valid distinct red double-confirmation.
func redAck(text string) Warning { return Warning{Text: text, Acknowledged: true, DoubleConfirm: true} }

func mustFloor(t *testing.T, entries ...FloorEntry) *Floor {
	t.Helper()
	f, err := NewFloor(entries...)
	if err != nil {
		t.Fatalf("NewFloor: %v", err)
	}
	return f.WithClock(fixedClock())
}

// TestApplyFloor_FlooredOpClassDeniesExecutionButNotProposal is the load-bearing REQ-1511 oracle: a matched
// floor entry floors the EXECUTION verdict to deny even when the proposal was `auto`/`approve`, while the
// PROPOSAL is preserved unchanged in the record (the vote path still sees it).
func TestApplyFloor_FlooredOpClassDeniesExecutionButNotProposal(t *testing.T) {
	f := mustFloor(t, FloorEntry{ID: "floor-reboot", Match: Match{OpClass: "reboot"}})
	in := EvalInput{OpClass: "reboot", Reversible: false}

	for _, proposal := range []Verdict{VerdictAuto, VerdictApprove} {
		exec, rec := f.ApplyFloor(proposal, in)
		if exec != VerdictDeny {
			t.Fatalf("proposal %q: execution verdict = %q, want deny (floored)", proposal, exec)
		}
		if !rec.Floored || rec.MatchedEntryID != "floor-reboot" {
			t.Fatalf("proposal %q: record did not capture the floor: %+v", proposal, rec)
		}
		// The proposal is UNCHANGED — the floor is an execution floor, not a proposal floor.
		if rec.ProposalVerdict != proposal {
			t.Fatalf("floor mutated the proposal: record proposal %q, want %q", rec.ProposalVerdict, proposal)
		}
		if rec.ExecutionVerdict != VerdictDeny {
			t.Fatalf("record execution verdict = %q, want deny", rec.ExecutionVerdict)
		}
	}
}

// TestApplyFloor_ProposalStillProducedWhenFloored proves the pipeline still PROPOSES a floor-class action (the
// engine's Decide is not altered by the floor) — the deny is applied only as a separate execution step.
func TestApplyFloor_ProposalStillProducedWhenFloored(t *testing.T) {
	// The engine proposes `auto` for the action (a permissive operator rule).
	r, err := NewRule(Rule{ID: "allow-reboot", Match: Match{OpClass: "reboot"}, Verdict: VerdictAuto})
	if err != nil {
		t.Fatal(err)
	}
	e, err := NewEngine(t.Context(), RuleSet{Rules: []Rule{r}})
	if err != nil {
		t.Fatal(err)
	}
	in := EvalInput{OpClass: "reboot"}
	dec, err := e.Decide(t.Context(), in)
	if err != nil {
		t.Fatal(err)
	}
	// The PROPOSAL is the pre-composition Rego base verdict `auto` — the pipeline suggested the action with
	// rationale. (Decide's own COMPOSED verdict is additionally clamped to approve by the constitutional
	// never-auto floor, since reboot is a floor-class op; the execution floor is the distinct layer under test.)
	if bv := dec.Audit().Base.Verdict; bv != VerdictAuto {
		t.Fatalf("proposal (base) verdict = %q, want auto (the pipeline still proposes the floor-class action)", bv)
	}
	if dec.Reason() == "" {
		t.Fatal("proposal has no rationale")
	}
	// Now the execution floor bites — only at execution.
	f := mustFloor(t, FloorEntry{ID: "floor-reboot", Match: Match{OpClass: "reboot"}})
	exec, rec := f.ApplyFloor(dec.Audit().Base.Verdict, in)
	if exec != VerdictDeny {
		t.Fatalf("execution verdict = %q, want deny", exec)
	}
	if rec.ProposalVerdict != VerdictAuto {
		t.Fatalf("proposal not preserved through the floor: %q", rec.ProposalVerdict)
	}
}

// TestApplyFloor_NoMatchLeavesProposalUnchanged: an action no active entry matches actuates on its proposal.
func TestApplyFloor_NoMatchLeavesProposalUnchanged(t *testing.T) {
	f := mustFloor(t, FloorEntry{ID: "floor-reboot", Match: Match{OpClass: "reboot"}})
	exec, rec := f.ApplyFloor(VerdictAuto, EvalInput{OpClass: "service.restart"})
	if exec != VerdictAuto || rec.Floored {
		t.Fatalf("unrelated action was floored: exec=%q rec=%+v", exec, rec)
	}
}

// TestRemoveFloorEntry_RequiresAck is the REQ-1513 warn-don't-block oracle: without the acknowledged
// double-confirmation the removal is REFUSED (fail closed, floor stays); with it the removal succeeds, the
// entry stops flooring, and a FloorChangeRecord is emitted.
func TestRemoveFloorEntry_RequiresAck(t *testing.T) {
	in := EvalInput{OpClass: "reboot"}

	// Refused without an ack.
	for _, bad := range []Warning{
		{},                                                  // empty
		{Text: "ok", Acknowledged: true},                    // acknowledged but NOT double-confirmed
		{Text: "", Acknowledged: true, DoubleConfirm: true}, // double-confirmed but empty text
		{Text: "ok", DoubleConfirm: true},                   // text + double but NOT acknowledged
	} {
		f := mustFloor(t, FloorEntry{ID: "floor-reboot", Match: Match{OpClass: "reboot"}})
		if _, err := f.RemoveFloorEntry("floor-reboot", "op:alice", bad); !errors.Is(err, ErrFloorRemovalNotConfirmed) {
			t.Fatalf("ack %+v: err = %v, want ErrFloorRemovalNotConfirmed", bad, err)
		}
		// Floor STAYS in force after a refused removal.
		if exec, _ := f.ApplyFloor(VerdictAuto, in); exec != VerdictDeny {
			t.Fatalf("floor did not stay in force after a refused removal: exec=%q", exec)
		}
	}

	// Succeeds with the red double-confirmation.
	f := mustFloor(t, FloorEntry{ID: "floor-reboot", Match: Match{OpClass: "reboot"}})
	rec, err := f.RemoveFloorEntry("floor-reboot", "op:alice", redAck("I accept lowering the reboot floor"))
	if err != nil {
		t.Fatalf("RemoveFloorEntry with ack: %v", err)
	}
	if rec.Change != "remove-floor-entry" || rec.EntryID != "floor-reboot" || rec.Actor != "op:alice" ||
		!rec.RedConfirm || !rec.Lowering || rec.WarningText == "" || rec.Timestamp.IsZero() {
		t.Fatalf("FloorChangeRecord not fully populated: %+v", rec)
	}
	// After removal the entry no longer floors — the execution verdict is the proposal unchanged.
	if exec, r := f.ApplyFloor(VerdictAuto, in); exec != VerdictAuto || r.Floored {
		t.Fatalf("removed entry still floors: exec=%q rec=%+v", exec, r)
	}
}

// TestRemoveFloorEntry_ConstitutionalFloorStillAppliesBeneath asserts the DISTINCTION + NON-BYPASS: removing a
// POLICY floor entry does NOT lift the constitutional mechanical never-auto floor (core/safety, INV-09), which
// still clamps floor-class ops beneath the removed policy floor. The policy floor exposes no bypass of it.
func TestRemoveFloorEntry_ConstitutionalFloorStillAppliesBeneath(t *testing.T) {
	f := mustFloor(t, FloorEntry{ID: "floor-reboot", Match: Match{OpClass: "reboot"}})
	if _, err := f.RemoveFloorEntry("floor-reboot", "op:alice", redAck("lower it")); err != nil {
		t.Fatal(err)
	}
	// The constitutional never-auto floor is a DIFFERENT, mechanical, non-removable layer beneath the engine.
	reboot := EvalInput{OpClass: "reboot", Reversible: true}
	if !safety.IsNeverAuto("reboot") {
		t.Fatal("precondition: reboot must be on the constitutional never-auto floor")
	}
	if !NeverAutoApplies(reboot) {
		t.Fatal("policy floor removal must NOT lift the constitutional never-auto floor beneath the engine")
	}
	// Defence-in-depth: even a force→auto band composition still clamps to approve for a never-auto op.
	if v, r := ComposeBand(VerdictAuto, safety.BandAuto, BandForce, NeverAutoApplies(reboot)); v == VerdictAuto || !r.FloorClamped {
		t.Fatalf("constitutional floor did not clamp beneath the removed policy floor: v=%q rec=%+v", v, r)
	}
}

// TestNoSilentFloorRemoval is the NEGATIVE CONTROL: there is no exported path that removes/deactivates a floor
// entry without an acknowledged confirmation. Both mutation entry points (RemoveFloorEntry and SelectTemplate
// bare) refuse without the red double-confirmation and change nothing.
func TestNoSilentFloorRemoval(t *testing.T) {
	in := EvalInput{OpClass: "reboot"}

	f := mustFloor(t, FloorEntry{ID: "floor-reboot", Match: Match{OpClass: "reboot"}})
	if _, err := f.RemoveFloorEntry("floor-reboot", "op:mallory", Warning{}); err == nil {
		t.Fatal("RemoveFloorEntry removed a floor entry silently (no ack)")
	}
	if _, _, err := f.SelectTemplate(TemplateBare, "op:mallory", Warning{}); err == nil {
		t.Fatal("SelectTemplate(bare) removed the floor silently (no red-confirm)")
	}
	// The floor is still fully in force — nothing was removed.
	if exec, _ := f.ApplyFloor(VerdictAuto, in); exec != VerdictDeny {
		t.Fatalf("floor was silently weakened: exec=%q", exec)
	}
	for _, s := range f.Entries() {
		if s.Removed {
			t.Fatalf("entry %q was removed with no audited confirmation", s.ID)
		}
	}
}

// TestConservativeTemplate_CarriesPredecessorDenyPatternsAndGovernor is the REQ-1512 oracle: the conservative
// template loads the predecessor argv deny-patterns as deny rules and a 30-per-minute governor.
func TestConservativeTemplate_CarriesPredecessorDenyPatternsAndGovernor(t *testing.T) {
	rs, err := ConservativeTemplate()
	if err != nil {
		t.Fatalf("ConservativeTemplate: %v", err)
	}
	// The 30-per-minute governor lives on the global default and resolves through EffectiveParams.
	if rs.Default.RateLimit == nil || *rs.Default.RateLimit != 30 {
		t.Fatalf("conservative governor = %v, want a 30-per-minute rate limit", rs.Default.RateLimit)
	}
	// The predecessor argv deny-patterns are present as deny rules.
	want := []string{"rm -rf /", "mkfs", "reboot", "shutdown", "kubectl delete namespace", "dd if=/dev/zero"}
	have := map[string]bool{}
	denies := 0
	for _, r := range rs.Rules {
		if r.Verdict != VerdictDeny {
			t.Fatalf("conservative template rule %q is %q, want every rule to be a deny (safe-default)", r.ID, r.Verdict)
		}
		denies++
		have[strings.ToLower(strings.TrimSpace(r.Match.ArgvPattern))] = true
	}
	if denies < 20 {
		t.Fatalf("conservative template has only %d deny rules, want the full predecessor deny-list", denies)
	}
	for _, w := range want {
		if !have[w] {
			t.Fatalf("conservative template is missing the predecessor deny-pattern %q", w)
		}
	}
}

// TestConservativeTemplate_DeniesFloorClassAtExecution proves the conservative template actually DENIES a
// floor-class command through the engine's deny-overrides evaluation (approve-gated / denied by default posture).
func TestConservativeTemplate_DeniesFloorClassAtExecution(t *testing.T) {
	rs, err := ConservativeTemplate()
	if err != nil {
		t.Fatal(err)
	}
	e, err := NewEngine(t.Context(), rs)
	if err != nil {
		t.Fatal(err)
	}
	// A destructive command matches a predecessor deny-pattern → deny (deny-overrides).
	dec, err := e.Decide(t.Context(), EvalInput{Argv: "sudo rm -rf /var/lib/postgres"})
	if err != nil {
		t.Fatal(err)
	}
	if dec.Verdict() != VerdictDeny {
		t.Fatalf("conservative template did not deny a destructive command: %q", dec.Verdict())
	}
	// An unmatched action falls through to the engine's fail-closed default (approve — route to a human).
	dec2, err := e.Decide(t.Context(), EvalInput{Argv: "systemctl restart librespeed"})
	if err != nil {
		t.Fatal(err)
	}
	if dec2.Verdict() != VerdictApprove {
		t.Fatalf("unmatched action = %q, want the fail-closed approve default (approve-gated posture)", dec2.Verdict())
	}
}

// TestBareTemplate_RequiresRedConfirmAndWarns is the REQ-1512/1513 oracle: `bare` is allow-all with a single
// access-list, permitted ONLY behind the red double-confirmation, and it emits a lowering warning record.
func TestBareTemplate_RequiresRedConfirmAndWarns(t *testing.T) {
	// The bare RuleSet is allow-all with exactly one access-list rule and no operator denies.
	rs, err := BareTemplate()
	if err != nil {
		t.Fatalf("BareTemplate: %v", err)
	}
	if len(rs.Rules) != 1 || rs.Rules[0].Verdict == VerdictDeny {
		t.Fatalf("bare template is not a single allow-all access-list: %+v", rs.Rules)
	}

	// Refused without the red double-confirmation (fail closed) — nothing removed.
	f := mustFloor(t, FloorEntry{ID: "floor-reboot", Match: Match{OpClass: "reboot"}})
	if _, _, err := f.SelectTemplate(TemplateBare, "op:alice", Warning{Text: "single ack", Acknowledged: true}); !errors.Is(err, ErrFloorRemovalNotConfirmed) {
		t.Fatalf("bare without red-confirm err = %v, want ErrFloorRemovalNotConfirmed", err)
	}
	if exec, _ := f.ApplyFloor(VerdictAuto, EvalInput{OpClass: "reboot"}); exec != VerdictDeny {
		t.Fatal("bare was applied without the red-confirm — floor was weakened")
	}

	// Permitted behind the red double-confirmation; emits a lowering warning record; removes the floor protections.
	got, rec, err := f.SelectTemplate(TemplateBare, "op:alice", redAck("I accept allow-all"))
	if err != nil {
		t.Fatalf("SelectTemplate(bare) with red-confirm: %v", err)
	}
	if len(got.Rules) != 1 {
		t.Fatalf("bare RuleSet not returned: %+v", got)
	}
	if rec.Change != "select-template:bare" || !rec.RedConfirm || !rec.Lowering || rec.WarningText == "" {
		t.Fatalf("bare warning record not a lowering red-confirm record: %+v", rec)
	}
	if exec, r := f.ApplyFloor(VerdictAuto, EvalInput{OpClass: "reboot"}); exec != VerdictAuto || r.Floored {
		t.Fatalf("bare did not remove the floor protections: exec=%q rec=%+v", exec, r)
	}
	// But the constitutional floor still applies beneath bare.
	if !NeverAutoApplies(EvalInput{OpClass: "reboot", Reversible: true}) {
		t.Fatal("bare must NOT lift the constitutional never-auto floor")
	}
}

// TestSelectTemplate_UnknownIsErrorNotAllowAll is the fail-closed oracle: an unknown template is an error, never
// a silent allow-all, and it changes nothing.
func TestSelectTemplate_UnknownIsErrorNotAllowAll(t *testing.T) {
	f := mustFloor(t, FloorEntry{ID: "floor-reboot", Match: Match{OpClass: "reboot"}})
	rs, rec, err := f.SelectTemplate("permissive-oops", "op:alice", redAck("go"))
	if !errors.Is(err, ErrUnknownTemplate) {
		t.Fatalf("unknown template err = %v, want ErrUnknownTemplate", err)
	}
	if len(rs.Rules) != 0 || rec.Change != "" {
		t.Fatalf("unknown template returned data/record: rs=%+v rec=%+v", rs, rec)
	}
	// The floor is untouched — an unknown template never falls open.
	if exec, _ := f.ApplyFloor(VerdictAuto, EvalInput{OpClass: "reboot"}); exec != VerdictDeny {
		t.Fatal("unknown template weakened the floor (fell open)")
	}
}

// TestSelectTemplate_ConservativeReseedsFloorNoConfirm: selecting conservative is a tightening — it needs no
// confirmation and re-seeds the floor with the predecessor deny-patterns.
func TestSelectTemplate_ConservativeReseedsFloorNoConfirm(t *testing.T) {
	f := mustFloor(t) // empty floor
	rs, rec, err := f.SelectTemplate(TemplateConservative, "op:alice", Warning{})
	if err != nil {
		t.Fatalf("SelectTemplate(conservative): %v", err)
	}
	if len(rs.Rules) < 20 || rec.Lowering {
		t.Fatalf("conservative select not a tightening re-seed: rules=%d rec=%+v", len(rs.Rules), rec)
	}
	// The re-seeded floor now denies a destructive command at execution.
	if exec, r := f.ApplyFloor(VerdictAuto, EvalInput{Argv: "mkfs.ext4 /dev/sdb"}); exec != VerdictDeny || !r.Floored {
		t.Fatalf("conservative re-seed did not floor a destructive command: exec=%q rec=%+v", exec, r)
	}
}

// TestConservativeFloor_SeededFromSameSource: the ConservativeFloor is seeded from the SAME template deny-rules.
func TestConservativeFloor_SeededFromSameSource(t *testing.T) {
	f, err := ConservativeFloor()
	if err != nil {
		t.Fatalf("ConservativeFloor: %v", err)
	}
	if !f.AppliesTo(EvalInput{Argv: "sudo reboot now"}) {
		t.Fatal("conservative floor does not floor a reboot command")
	}
	rs, _ := ConservativeTemplate()
	denies := 0
	for _, r := range rs.Rules {
		if r.Verdict == VerdictDeny {
			denies++
		}
	}
	if len(f.Entries()) != denies {
		t.Fatalf("floor has %d entries, template has %d deny rules — not the same single source", len(f.Entries()), denies)
	}
}

// TestNewFloor_MalformedEntryDeniesEverything is the fail-closed oracle: a floor entry that constrains no
// dimension DENIES everything rather than matching nothing.
func TestNewFloor_MalformedEntryDeniesEverything(t *testing.T) {
	f := mustFloor(t, FloorEntry{ID: "malformed"}) // no match dimension
	snap := f.Entries()
	if len(snap) != 1 || !snap[0].Malformed {
		t.Fatalf("empty-match entry not flagged malformed: %+v", snap)
	}
	// It denies an arbitrary action (fail closed, deny-all) rather than falling open.
	if exec, r := f.ApplyFloor(VerdictAuto, EvalInput{OpClass: "anything", Host: "h1"}); exec != VerdictDeny || !r.Floored {
		t.Fatalf("malformed floor entry fell open: exec=%q rec=%+v", exec, r)
	}
}

// TestNewFloor_RejectsBadEntries: empty id and duplicate id are rejected at construction (fail closed).
func TestNewFloor_RejectsBadEntries(t *testing.T) {
	if _, err := NewFloor(FloorEntry{ID: "", Match: Match{OpClass: "x"}}); err == nil {
		t.Fatal("empty floor entry id accepted")
	}
	if _, err := NewFloor(
		FloorEntry{ID: "dup", Match: Match{OpClass: "x"}},
		FloorEntry{ID: "dup", Match: Match{OpClass: "y"}},
	); err == nil {
		t.Fatal("duplicate floor entry id accepted")
	}
}
