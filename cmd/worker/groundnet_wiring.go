package main

import (
	"fmt"
	"log"

	"github.com/territory-grounder/grounder/core/groundnet"
)

// Phase C — the composition-root arm for the DORMANT groundnet federation seam (spec/021, TG-128).
//
// Federation is opt-in, default-off, and FAR-FUTURE: the seam reaches no actuator, and there is no live
// emit/ingest consumer yet (the network itself is a separate far-future thesis, docs/FEDERATION-VISION.md).
// So this does NOT construct a live seam — an emit/ingest path with no network to talk to would be dead
// weight. What it DOES, so the capability is DELIVERED rather than invisible (a control is not delivered until
// its key can arrive):
//   - runs the standing-check self-test at boot, so a regressed groundnet invariant is caught here, loudly;
//   - reads the org-admin membership posture from env (default-off), the arming path an operator uses;
//   - emits a DARK/ARMED boot line that NAMES the exact keys that arm it — never a silent "not configured".
//
// The seam it guards is the delivered, dormant core/groundnet code (8 tasks, e2e-tested); its emit/ingest
// wiring lands with the federation network.

// wireGroundnet runs the boot self-test, reads the posture, and returns the boot-log line describing the
// federation posture. It constructs no live seam and reaches no actuator.
func wireGroundnet() string {
	checked := "standing-check OK"
	if err := groundnet.StandingCheck(); err != nil {
		// DARK-by-default means a groundnet invariant regression cannot affect production (nothing runs), so
		// this is loud but NOT fatal: it refuses to report a healthy seam, and an armed node would refuse to
		// federate on it (below), but a dormant worker still boots.
		checked = "standing-check FAILED: " + err.Error()
		log.Printf("groundnet standing-check FAILED — the federation seam will NOT be offered until fixed: %v", err)
	}
	p := readGroundnetPosture()
	if !p.MayEmit() && !p.MayConsume() && !p.MayUsePublicTier() {
		return "groundnet federation: DARK — opt-in default-off (spec/021, far-future; the seam reaches no " +
			"actuator); " + checked + ". Arm via TG_GROUNDNET_MEMBER=1 + TG_GROUNDNET_EXPORT / _CONSUME / " +
			"_PUBLIC_TIER (org-admin), one per capability."
	}
	return fmt.Sprintf("groundnet federation: ARMED (emit=%v consume=%v public-tier=%v); %s. NOTE: the "+
		"emit/ingest NETWORK integration is far-future — an armed node holds the posture but shares nothing "+
		"until the network lands.", p.MayEmit(), p.MayConsume(), p.MayUsePublicTier(), checked)
}

// readGroundnetPosture derives the node's federation posture from env, starting from the default-off posture
// and enabling each capability an org admin has turned on. getenv routes through the boot-config resolver, so
// an operator-saved override is honored. Enabling any capability is an org-admin decision (REQ-2111); the
// boot-config principal stands in for the operator who set the deployment's .env.
func readGroundnetPosture() groundnet.Posture {
	admin := groundnet.OrgAdmin("boot-config")
	p := groundnet.DefaultPosture()
	// One literal getenv per capability (NOT a key-list loop): the compose-parity guard resolves only a
	// literal key argument, so a literal here is what puts each TG_GROUNDNET_* key UNDER the guard — the
	// guard then fails CI if docker-compose stops forwarding one, which is the whole "a control is not
	// delivered until its key can arrive" contract this seam must honor.
	if getenv("TG_GROUNDNET_MEMBER", "") == "1" {
		p, _, _ = groundnet.SetCapability(p, groundnet.CapMember, true, admin, 0)
	}
	if getenv("TG_GROUNDNET_EXPORT", "") == "1" {
		p, _, _ = groundnet.SetCapability(p, groundnet.CapExport, true, admin, 0)
	}
	if getenv("TG_GROUNDNET_CONSUME", "") == "1" {
		p, _, _ = groundnet.SetCapability(p, groundnet.CapConsume, true, admin, 0)
	}
	if getenv("TG_GROUNDNET_PUBLIC_TIER", "") == "1" {
		p, _, _ = groundnet.SetCapability(p, groundnet.CapPublic, true, admin, 0)
	}
	return p
}
