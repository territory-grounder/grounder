package runner

import (
	"testing"

	"github.com/territory-grounder/grounder/agent/skills"
	"github.com/territory-grounder/grounder/core/pack"
	"github.com/territory-grounder/grounder/core/risk"
	"github.com/territory-grounder/grounder/core/safety"
)

func ciscoFixturePack() pack.Pack {
	return pack.Pack{
		ID: "cisco", Title: "t", Summary: "s", Version: "1.0.0", Domains: []string{"cisco"},
		Band: pack.BandOverlay{Floor: safety.BandPollPause, Applies: true, Reason: "cisco-never-auto"},
	}
}

// TG-80 P2-5: two band-floor producers must COMPOSE stricter-wins through the one seam — never
// overwrite. KILLING MUTATION: replace ComposeFloor with plain assignment in applyPackPosture and the
// "ladder stricter" case fails (the pack's looser floor would erase POLL_PAUSE).
func TestApplyPackPostureComposesStricterWins(t *testing.T) {
	p := ciscoFixturePack()

	// No prior floor: the pack's floor lands whole.
	gi := risk.GatedInput{}
	applyPackPosture(&gi, p)
	if !gi.BandFloorApplies || gi.BandFloor != safety.BandPollPause || gi.BandFloorReason != "pack-cisco" {
		t.Fatalf("pack floor not applied: %+v", gi)
	}
	if gi.Signals["policy_pack"] != "cisco" {
		t.Fatalf("audit signal missing: %+v", gi.Signals)
	}

	// Ladder AUTO_NOTICE + pack POLL_PAUSE: the stricter pack floor wins and names itself.
	gi = risk.GatedInput{BandFloor: safety.BandAutoNotice, BandFloorApplies: true, BandFloorReason: "ladder-auto-notice"}
	applyPackPosture(&gi, p)
	if gi.BandFloor != safety.BandPollPause || gi.BandFloorReason != "pack-cisco" {
		t.Fatalf("stricter pack floor lost: %+v", gi)
	}

	// Ladder POLL_PAUSE + pack AUTO_NOTICE: the ladder's stricter floor SURVIVES — the overwrite bug
	// this helper exists to prevent.
	p.Band.Floor = safety.BandAutoNotice
	gi = risk.GatedInput{BandFloor: safety.BandPollPause, BandFloorApplies: true, BandFloorReason: "prior-strict"}
	applyPackPosture(&gi, p)
	if gi.BandFloor != safety.BandPollPause || gi.BandFloorReason != "prior-strict" {
		t.Fatalf("a looser pack floor overwrote a stricter prior: %+v", gi)
	}

	// Equal floors: both contributors are named on the audit reason.
	gi = risk.GatedInput{BandFloor: safety.BandAutoNotice, BandFloorApplies: true, BandFloorReason: "ladder-auto-notice"}
	applyPackPosture(&gi, p)
	if gi.BandFloor != safety.BandAutoNotice || gi.BandFloorReason != "ladder-auto-notice+pack-cisco" {
		t.Fatalf("tie must name both: %+v", gi)
	}

	// No applied overlay: the floor is untouched; the governing pack is still named on the audit row.
	p.Band = pack.BandOverlay{}
	gi = risk.GatedInput{BandFloor: safety.BandAutoNotice, BandFloorApplies: true, BandFloorReason: "ladder-auto-notice"}
	applyPackPosture(&gi, p)
	if gi.BandFloor != safety.BandAutoNotice || gi.BandFloorReason != "ladder-auto-notice" {
		t.Fatalf("an overlay-less pack changed the floor: %+v", gi)
	}
	if gi.Signals["policy_pack"] != "cisco" {
		t.Fatalf("governing pack unnamed: %+v", gi.Signals)
	}
}

// The skill allowlist is a FILTER: order preserved, nothing resurrected, nil/empty = no scoping.
func TestFilterSkillListIsAFilterNotASelector(t *testing.T) {
	all := []skills.Skill{{Name: "a"}, {Name: "b"}, {Name: "c"}}

	if got := filterSkillList(all, nil); len(got) != 3 {
		t.Fatalf("nil allow must pass everything, got %d", len(got))
	}
	got := filterSkillList(all, []string{"c", "a", "never-composed"})
	if len(got) != 2 || got[0].Name != "a" || got[1].Name != "c" {
		t.Fatalf("filter broke order or resurrected a name: %+v", got)
	}
}

func TestPackSkillAllowIsNilWithoutAPack(t *testing.T) {
	if packSkillAllow(pack.Pack{Skills: []string{"x"}}, false) != nil {
		t.Fatal("no governing pack must mean no filtering")
	}
	if got := packSkillAllow(ciscoFixturePack(), true); got != nil {
		t.Fatalf("the fixture declares no Skills scoping, want nil, got %v", got)
	}
}
