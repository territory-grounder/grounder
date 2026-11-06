package pack

import (
	"fmt"
	"slices"
	"sort"

	"github.com/territory-grounder/grounder/core/safety"
)

// packs is the ONE place a platform pack is declared — a closed compiled set, the modules/catalog
// discipline. EMPTY TODAY, DELIBERATELY: this package ships as the substrate (TG-80 P2-5 / TG-81 b5)
// and the first content pack (cisco, TG-78/85) lands as one Pack literal here, in its own reviewed MR.
// With no packs declared, every seam this package feeds is byte-identical to pre-feature behavior —
// For never selects, no tool subset is taken, no tier escalates, no floor composes, no ledger token is
// appended; the seed goldens pin that.
func packs() []Pack {
	return []Pack{
		{
			// THE FIRST CONTENT PACK (TG-78/85): Cisco ASA/IOS. Selection is strict on the cisco domain
			// (skills.DomainOf's ^asa rule); everything here NAMES artifacts owned elsewhere.
			ID:      "cisco",
			Title:   "Cisco ASA/IOS platform pack",
			Summary: "Firewall/router triage competence: the cisco-triage skill, the cisco-show closed read catalog, primary-tier reasoning, and a human-poll band floor on every proposal.",
			Version: "1.0.0",
			Domains: []string{"cisco"},
			// Tools/Skills stay UNSCOPED deliberately: a cisco incident still needs the general
			// observability set (LibreNMS reads, estate context) BESIDE cisco-show, and cisco-triage
			// already selects itself via ciscoDeep — a filter here would only be a second authority to
			// drift from the first. Scoping can tighten later with evidence; it cannot start tight and
			// blind the agent.
			// TierHint: firewall triage is never fast-lane work — a FAST-classed cisco incident
			// escalates to the primary investigate tier (escalate-only; a primary/deep class is
			// untouched).
			TierHint: "primary",
			// Band: the skill's never-auto doctrine as a MACHINE floor, not prose — every cisco
			// proposal polls a human. Raise-only by construction (BandPollPause is the strictest band);
			// the Reason lands on the audit row beside the composed floor.
			Band: BandOverlay{
				Floor:   safety.BandPollPause,
				Applies: true,
				Reason:  "cisco pack: firewall/security surface — every Cisco change polls a human (the pack's never-auto doctrine, machine-enforced)",
			},
		},
		{
			// THE PROXMOX PACK (TG-78): deliberately THIN — identity + provenance only. No TierHint: the
			// guest plane (the most common proxmox incident) is ordinary fast-lane triage, and a pack-wide
			// escalation would buy primary reasoning for every guest-down. No Band overlay: proxmox is the
			// ONE domain whose guest lane is DESIGNED to graduate to AUTO (start-guest re-promotes via the
			// earned-op-class ladder, owner-ruled 2026-08-25) — a pack floor of POLL_PAUSE here would clamp
			// every proxmox proposal below AUTO forever and silently defeat that ladder. The node/storage/
			// cluster planes' never-auto doctrine is enforced where it belongs: core/safety's floor holds
			// the host-op classes regardless, and the skill names the planes. Tools/Skills unscoped for the
			// same reasons as cisco.
			ID:      "proxmox",
			Title:   "Proxmox VE platform pack",
			Summary: "Hypervisor-estate identity: the proxmox-triage skill's four-plane doctrine (guest/node/storage/cluster), guest-lifecycle competence, and pack provenance on every governed session. Deliberately carries no tier or band posture — the graduation ladder and the never-auto floor govern.",
			Version: "1.0.0",
			Domains: []string{"proxmox"},
		},
		{
			// THE KUBERNETES PACK (TG-78): equally THIN. No TierHint — TG has no k8s read tool yet, so
			// extra reasoning cannot buy extra evidence; the honest posture on k8s incidents today is
			// diagnose-what-is-reachable-and-escalate (the corpus trio pins it), which needs no primary
			// tier. No Band overlay: nothing k8s-mutating is armed anywhere (the k8s actuation module's
			// delete/drain are floor-clamped already). The pack exists for identity + provenance and as
			// the landing site for the read connector's tool scoping when it arrives.
			ID:      "kubernetes",
			Title:   "Kubernetes platform pack",
			Summary: "K8s-estate identity: the k8s-triage skill's runtime-vs-declarative doctrine under strict GitOps, and pack provenance on every governed session. Deliberately carries no tier or band posture until the k8s read connector lands.",
			Version: "1.0.0",
			Domains: []string{"kubernetes"},
		},
		{
			// THE LINUX PACK (TG-78 linux slice, owner-commissioned 2026-08-30): THIN like proxmox/k8s —
			// identity + provenance only. No TierHint: OS-plane triage (systemd/journald/disk/mem) is the
			// estate's bread-and-butter lane and the always-on protocols already pace it; a primary tier
			// would spend depth where the evidence is one hostdiag read away. No Band overlay: the
			// service-lifecycle verbs (start/restart/reload-service) are DESIGNED to graduate via the
			// earned-op-class ladder — a pack floor here would clamp them below AUTO forever, the exact
			// trap catalog_thin_test.go pins for proxmox's start-guest.
			ID:      "linux",
			Title:   "Linux guest platform pack",
			Summary: "Debian/Ubuntu guest identity: the linux-triage skill's unit/pressure/host-plane doctrine on plain guests, and pack provenance on every governed session. Deliberately carries no tier or band posture — the graduation ladder and the never-auto floor govern.",
			Version: "1.0.0",
			Domains: []string{"linux"},
		},
		{
			// THE STORAGE PACK (TG-78 storage slice): THIN like proxmox/k8s/linux — identity + provenance.
			// No TierHint: the appliance lane's evidence is one storage-health read away and the honest
			// outcomes are stand-down/escalate (no storage op-class exists) — depth buys nothing. No Band
			// overlay for the same reason: there is nothing to actuate, and a floor would only clamp
			// whatever future storage verb ever graduates.
			ID:      "storage",
			Title:   "Storage appliance platform pack",
			Summary: "Synology-DSM appliance identity: the storage-triage skill's capacity/array/appliance doctrine plus the get-device-storage-health read, and pack provenance on every governed session. Deliberately carries no tier or band posture — no storage op-class exists to govern.",
			Version: "1.0.0",
			Domains: []string{"storage"},
		},
	}
}

// All returns every declared pack, validated and deterministically ordered by ID. A malformed or
// duplicate declaration is an error from HERE — caught by the catalog test once, at authoring time,
// rather than composing wrongly per session.
func All() ([]Pack, error) {
	out := packs()
	seen := map[string]bool{}
	for _, p := range out {
		if err := p.Validate(); err != nil {
			return nil, err
		}
		if seen[p.ID] {
			return nil, fmt.Errorf("pack: duplicate ID %q", p.ID)
		}
		seen[p.ID] = true
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

// Lookup returns one pack by ID.
func Lookup(id string) (Pack, bool) {
	all, err := All()
	if err != nil {
		return Pack{}, false
	}
	for _, p := range all {
		if p.ID == id {
			return p, true
		}
	}
	return Pack{}, false
}

// For selects the pack for a domain — a PURE function of the typed domain fact (skills.DomainOf's
// output), the INV-08 selector shape: no model token participates, and the unknown domain ("") selects
// nothing. First match in ID order wins; overlapping domain claims are an authoring smell the catalog
// test surfaces, not a runtime negotiation. A catalog that fails validation selects nothing — the same
// fail-toward-baseline direction every skill-compose failure takes.
func For(domain string) (Pack, bool) {
	all, err := All()
	if err != nil {
		return Pack{}, false
	}
	return selectIn(all, domain)
}

// selectIn is For's testable core over an explicit pack list.
func selectIn(all []Pack, domain string) (Pack, bool) {
	if domain == "" {
		return Pack{}, false
	}
	for _, p := range all {
		if slices.Contains(p.Domains, domain) {
			return p, true
		}
	}
	return Pack{}, false
}
