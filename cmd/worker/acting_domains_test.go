package main

import (
	"testing"

	"github.com/territory-grounder/grounder/core/actuate/opschema"
)

// ★ THE ACTING-DOMAIN SET MUST FOLLOW THE ACTUATION SURFACE, NOT A MEMORY OF IT.
//
// `tgActuatesIn` decides where a MISSING self-identity is reported. Get it wrong in the permissive direction
// and operators are sent to wire an identity that cannot matter (the netbox case). Get it wrong the OTHER
// way — drop a domain TG really acts in — and a real gap goes SILENT, which is strictly worse: TG would read
// its own actions there as a stranger's and escalate on itself, with no warning that it might.
//
// That second direction is why this oracle exists. A mutation dropping "awx" from the map left every other
// test GREEN, because a hand-written literal has nothing holding it to reality. Here it is bound to the
// op-class registry's CLOSED effect-kind enumeration — the same enumeration mustBuildRegistry panics on when
// it meets an unknown kind — so adding an actuation channel that reaches a new domain fails this test until
// the domain is declared.
func TestTheActingDomainSetMatchesTheActuationSurface(t *testing.T) {
	// The effect kind an op-class actuates through determines WHERE its evidence lands.
	domainFor := map[string]string{
		opschema.EffectSSHArgv:          "journal",   // sshd records the actuation key's login on the guest
		opschema.EffectAWXLaunch:        "awx",       // the run lands in AWX job history
		opschema.EffectProxmoxLifecycle: "pve",       // vzstart/vzstop land in the Proxmox task log
		opschema.EffectK8sDeclarative:   "gitops-mr", // the merge request the gitops-mr lane opens lands in GitLab
	}

	specs := opschema.Specs()
	if len(specs) == 0 {
		t.Fatal("the op-class registry is empty — this oracle would pass vacuously, which is exactly how the " +
			"hand-written map went unchecked in the first place")
	}

	seen := map[string]string{} // domain -> the op-class that implies it, for the failure message
	for _, s := range specs {
		// Kind() applies the registry's OWN normalisation — an empty effect_kind means the ssh-argv lane.
		// Reading the raw field instead made this oracle reject every SSH op-class, which is a reminder that
		// a checker must consume the same normalisation the dispatcher does, or it grades a different value.
		d, ok := domainFor[s.Kind()]
		if !ok {
			t.Errorf("op-class %q has effect kind %q, which this oracle does not map to an evidence domain. "+
				"A new actuation channel reaches a new place; decide which domain its evidence lands in and "+
				"whether TG needs a self-identity there.", s.OpClass, s.Kind())
			continue
		}
		seen[d] = s.OpClass
		if !tgActuatesIn[d] {
			t.Errorf("op-class %q actuates via %q, so TG's own action lands in the %q domain — but %q is NOT "+
				"in tgActuatesIn. A missing self-identity there would go UNREPORTED, and TG would read its "+
				"own action as a stranger's and escalate on itself.", s.OpClass, s.Kind(), d, d)
		}
	}

	// The converse: every declared member must be implied by something real, or the map accretes domains
	// nobody actuates in and the netbox over-report creeps back.
	for d := range tgActuatesIn {
		if _, ok := seen[d]; !ok {
			t.Errorf("tgActuatesIn declares %q, but NO op-class actuates into it. An unearned member makes "+
				"this diagnostic demand a self-identity that cannot matter.", d)
		}
	}
}

// A READ-ONLY evidence domain must never be declared as one TG acts in. netbox is the live example: its
// reader is ReadOnly() and no code path writes there.
func TestAReadOnlyEvidenceDomainIsNotDeclaredActing(t *testing.T) {
	for _, d := range []string{"netbox"} {
		if tgActuatesIn[d] {
			t.Errorf("%q is declared as a domain TG actuates in, but TG only READS it — there is no TG "+
				"action in its evidence for a self-identity to match", d)
		}
	}
}
