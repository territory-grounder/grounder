package main

// The estate-derived per-host signals the runner consumes (TG-78), in their own wire-file per the TG-501
// main() LOC ratchet. Each closure reads the LIVE holder at call time — never a graph captured at boot —
// so a reseeded estate changes the answers without a restart. All three are nil-safe fail-closed on the
// runner side: an unseeded estate routes rule-only (domain-unknown), never a wrong competence.

import (
	"github.com/territory-grounder/grounder/core/estate"
	"github.com/territory-grounder/grounder/temporal/runner"
)

func wireEstateSignals(deps *runner.Deps, estateHolder *estate.Holder) {
	deps.HostSite = func(host string) (string, bool) { return estateHolder.Graph().SiteOf(host) }
	// TG-78: a guest-DOWN routes to Proxmox guest-lifecycle competence; a PVE NODE routes there on EVERY
	// alert family (the never-touch-host floor dominates whatever the symptom). See skills.DomainOf.
	deps.HostIsGuest = func(host string) bool { return estateHolder.Graph().IsGuest(host) }
	deps.HostIsPveNode = func(host string) bool { return estateHolder.Graph().IsPveNode(host) }
	// TG-78 network+storage slices: the device-identity signals — a NETWORK DEVICE routes to the cisco
	// pack's competence and a STORAGE APPLIANCE to storage competence on EVERY alert family (the estate
	// identity dominates the symptom; neither class has an honest rule prefix).
	deps.HostIsNetworkDevice = func(host string) bool { return estateHolder.Graph().IsNetworkDevice(host) }
	deps.HostIsStorageAppliance = func(host string) bool { return estateHolder.Graph().IsStorageAppliance(host) }
}
