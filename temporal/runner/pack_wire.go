package runner

// The platform-pack seams (TG-80 P2-5 / TG-81 borrow 5). Each helper here is the testable core of one
// wiring site in activities.go; every one of them is a NO-OP when no pack selects (and the compiled
// catalog ships empty), so the pre-pack seed, tool set, tier and floor are byte-identical — the seed
// goldens pin that.

import (
	"github.com/territory-grounder/grounder/agent/skills"
	"github.com/territory-grounder/grounder/core/pack"
	"github.com/territory-grounder/grounder/core/proposal"
	"github.com/territory-grounder/grounder/core/risk"
)

// packSkillAllow returns the pack's skill allowlist, or nil when no pack governs (nil = no filtering).
func packSkillAllow(p pack.Pack, ok bool) []string {
	if !ok {
		return nil
	}
	return p.Skills
}

// filterSkillList keeps only the named skills, preserving registry order. The allowlist is a FILTER over
// what AppliesWhen already composed — a name that never composed is not resurrected here, and an empty
// allow list means no scoping (everything passes).
func filterSkillList(all []skills.Skill, allow []string) []skills.Skill {
	if len(allow) == 0 {
		return all
	}
	keep := make(map[string]bool, len(allow))
	for _, n := range allow {
		keep[n] = true
	}
	out := make([]skills.Skill, 0, len(all))
	for _, s := range all {
		if keep[s.Name] {
			out = append(out, s)
		}
	}
	return out
}

// applyPackPosture composes a pack's declared band floor into the classifier input through the ONE
// composition seam (risk.GatedInput.BandFloor), stricter-wins via proposal.ComposeFloor — two floor
// producers must COMPOSE, never overwrite (the trap core/risk/input.go documents: a second assignment
// site is a second place a band can be adjusted downward). The pack id always lands on the signals map
// so the audit row names WHICH pack governed, and the reason names the stricter contributor (both, on a
// tie). A pack with no applied overlay leaves the input untouched.
func applyPackPosture(gi *risk.GatedInput, p pack.Pack) {
	if gi.Signals == nil {
		gi.Signals = map[string]string{}
	}
	gi.Signals["policy_pack"] = p.ID
	if !p.Band.Applies {
		return
	}
	if !gi.BandFloorApplies {
		gi.BandFloor, gi.BandFloorApplies, gi.BandFloorReason = p.Band.Floor, true, "pack-"+p.ID
		return
	}
	composed := proposal.ComposeFloor(gi.BandFloor, p.Band.Floor)
	switch {
	case composed == gi.BandFloor && composed == p.Band.Floor:
		gi.BandFloorReason += "+pack-" + p.ID
	case composed == p.Band.Floor:
		gi.BandFloorReason = "pack-" + p.ID
	}
	gi.BandFloor = composed
}
