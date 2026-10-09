package groundnet

import "testing"

// REQ-2111: a fresh node federates NOTHING — every capability is off in the born state.
func TestDefaultPostureFederatesNothing(t *testing.T) {
	p := DefaultPosture()
	if p.MayEmit() || p.MayConsume() || p.MayUsePublicTier() {
		t.Fatalf("a fresh node must federate nothing: emit=%v consume=%v public=%v", p.MayEmit(), p.MayConsume(), p.MayUsePublicTier())
	}
	if (p != Posture{}) {
		t.Errorf("DefaultPosture must be the zero value, got %+v", p)
	}
}

// REQ-2111: a posture change requires org-admin authority; without it the posture is unchanged.
func TestPostureChangeRequiresOrgAdmin(t *testing.T) {
	p := DefaultPosture()
	if _, _, err := SetCapability(p, CapMember, true, Authority{}, 100); err != ErrNotOrgAdmin {
		t.Fatalf("no authority: got %v, want ErrNotOrgAdmin", err)
	}
	np, rec, err := SetCapability(p, CapMember, true, OrgAdmin("admin@nl"), 100)
	if err != nil {
		t.Fatalf("org-admin enable: %v", err)
	}
	if !np.Member {
		t.Error("member capability should be enabled")
	}
	if rec.Capability != CapMember || !rec.Enabled || rec.Principal != "admin@nl" || rec.At != 100 {
		t.Errorf("audit record wrong: %+v", rec)
	}
	if p.Member {
		t.Error("the input posture must not be mutated (value semantics)")
	}
}

// REQ-2112: consumption is never gated behind contribution or export — a member that shares nothing
// still consumes, and consume can be enabled without export.
func TestConsumptionIndependentOfContribution(t *testing.T) {
	shareNothing := Posture{Member: true, Consume: true, Export: false}
	if !shareNothing.MayConsume() {
		t.Error("a member that shares nothing must still consume (REQ-2112)")
	}
	if shareNothing.MayEmit() {
		t.Error("export off must forbid emit")
	}
	np, _, err := SetCapability(Posture{Member: true}, CapConsume, true, OrgAdmin("a"), 1)
	if err != nil || !np.MayConsume() || np.MayEmit() {
		t.Errorf("consume must be enableable without export: %+v err=%v", np, err)
	}
}

// REQ-2114: an emitted chunk is stamped as an unrecallable governed record, and the opt-in notice
// states there is no recall after export.
func TestUnrecallableGovernedRecord(t *testing.T) {
	r := NewGovernedRecord("sha256:abc", "compliance-7y", "sha256:abc")
	if !r.Unrecallable {
		t.Error("a governed record must declare unrecallable")
	}
	if r.Subject != "sha256:abc" || r.Retention != "compliance-7y" || r.ReceiptRef != "sha256:abc" {
		t.Errorf("governed record fields: %+v", r)
	}
	if UnrecallableNotice == "" {
		t.Error("the opt-in notice must state there is no recall")
	}
}

// An unknown capability is refused even with authority.
func TestSetCapabilityUnknown(t *testing.T) {
	if _, _, err := SetCapability(DefaultPosture(), Capability("bogus"), true, OrgAdmin("a"), 1); err == nil {
		t.Error("an unknown capability must be refused")
	}
}
