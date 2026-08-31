package pack

// The proxmox + kubernetes packs are THIN BY RULING, and this test makes the thinness a pinned decision
// rather than an omission (TG-78, 2026-08-25). The trap it guards: copying cisco's POLL_PAUSE floor to
// proxmox would compose stricter-wins into every proxmox session's band and CLAMP the guest lane below
// AUTO forever — silently defeating the owner-ruled ladder re-promotion of start-guest. A future posture
// for either pack is a conscious rewrite of this file in its own reviewed MR.

import "testing"

func TestThinPacksCarryIdentityOnly(t *testing.T) {
	for _, tc := range []struct{ domain, id string }{
		{"proxmox", "proxmox"},
		{"kubernetes", "kubernetes"},
		{"linux", "linux"},
		{"storage", "storage"},
	} {
		p, ok := For(tc.domain)
		if !ok || p.ID != tc.id {
			t.Fatalf("For(%s) must select the %s pack (ok=%v id=%q)", tc.domain, tc.id, ok, p.ID)
		}
		if p.TierHint != "" {
			t.Errorf("%s: TierHint must stay EMPTY — no evidence a tier buys anything (k8s has no read tool; proxmox's common plane is guest triage): %q", tc.id, p.TierHint)
		}
		if p.Band.Applies {
			t.Errorf("%s: Band overlay must NOT apply — a pack floor here clamps the graduation ladder (the start-guest AUTO re-promotion, owner-ruled 2026-08-25)", tc.id)
		}
		if len(p.Tools) != 0 || len(p.Skills) != 0 {
			t.Errorf("%s: Tools/Skills scoping must stay empty (the cisco rationale: subsetting blinds, allowlists drop universals): %v %v", tc.id, p.Tools, p.Skills)
		}
		if got := p.LedgerToken(); got != "pack:"+tc.id+"@1.0.0" {
			t.Errorf("%s: ledger token = %q", tc.id, got)
		}
	}
	// The catalog stays sorted by ID and cisco keeps its posture (its own test pins the details).
	all, err := All()
	if err != nil || len(all) != 5 || all[0].ID != "cisco" || all[1].ID != "kubernetes" || all[2].ID != "linux" || all[3].ID != "proxmox" || all[4].ID != "storage" {
		t.Fatalf("catalog order/size drifted: %v (%v)", all, err)
	}
}
