package pack

// The cisco pack literal's posture, pinned (TG-85 item 6). Every assertion here is a DECISION, not a
// description: the band floor is the skill's never-auto doctrine machine-enforced, the tier hint buys
// reasoning for a security surface, and the EMPTINESS of Tools/Skills is deliberate (scoping either
// would blind the agent to the general observability set / filter out the universal skills) — so a
// future tightening must consciously rewrite this test in its own reviewed MR.

import (
	"testing"

	"github.com/territory-grounder/grounder/core/safety"
)

func TestCiscoPackPosture(t *testing.T) {
	all, err := All()
	if err != nil {
		t.Fatalf("catalog must validate: %v", err)
	}
	if len(all) != 5 {
		// 4→5 rewritten DELIBERATELY (TG-78 storage slice): the storage identity pack joined. The next
		// pack rewrites this count in its own MR.
		t.Fatalf("the catalog holds exactly cisco+kubernetes+linux+proxmox+storage today (got %d)", len(all))
	}
	p := all[0] // sorted by ID: cisco first
	if p.ID != "cisco" || p.Version != "1.0.0" {
		t.Fatalf("identity drifted: %s@%s", p.ID, p.Version)
	}
	if got := p.LedgerToken(); got != "pack:cisco@1.0.0" {
		t.Fatalf("ledger token = %q", got)
	}
	if len(p.Domains) != 1 || p.Domains[0] != "cisco" {
		// The string is skills.DomainCisco's value; pack selection keys on it via DomainOf at the
		// runner's wiring site. A rename there must show up here.
		t.Fatalf("the pack governs exactly the cisco domain, got %v", p.Domains)
	}
	if p.TierHint != "primary" {
		t.Fatalf("firewall triage is never fast-lane work — tier hint must escalate to primary, got %q", p.TierHint)
	}
	if !p.Band.Applies || p.Band.Floor != safety.BandPollPause || p.Band.Reason == "" {
		t.Fatalf("the never-auto doctrine must be a MACHINE floor: every cisco proposal polls a human (got applies=%v floor=%v reason=%q)",
			p.Band.Applies, p.Band.Floor, p.Band.Reason)
	}
	if len(p.Tools) != 0 || len(p.Skills) != 0 {
		t.Fatalf("Tools/Skills scoping is DELIBERATELY empty (subsetting blinds the agent; a skill allowlist filters out the universal skills) — tightening is a conscious act: %v %v", p.Tools, p.Skills)
	}
	if !p.VendorHint.zero() {
		t.Fatalf("no vendor hint is declared yet (nothing consumes it operationally); declaring one owes the transport resolver wiring at the boot attestation: %+v", p.VendorHint)
	}
}

func TestCiscoPackSelectsOnCiscoDomainOnly(t *testing.T) {
	if p, ok := For("cisco"); !ok || p.ID != "cisco" {
		t.Fatalf("For(cisco) must select the pack, got ok=%v id=%q", ok, p.ID)
	}
	for _, other := range []string{"host", "k8s", "vmware", ""} { // "storage" left this list when its pack landed (TG-78)
		if _, ok := For(other); ok {
			t.Fatalf("selection is STRICT: no pack may govern domain %q", other)
		}
	}
	// Resolve on the shipped literal: nothing declared, nothing missing, transport vacuously OK — the
	// boot attestation prints the healthy line for exactly this shape.
	p, _ := For("cisco")
	av := Resolve(p, func(string) bool { return false }, nil)
	if len(av.ToolsMissing) != 0 || !av.TransportOK {
		t.Fatalf("an unscoped pack must resolve clean even against an EMPTY registry: %+v", av)
	}
}
