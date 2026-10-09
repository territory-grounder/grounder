package opclasscat

// MECHANICAL FAMILY/TIER ASSIGNMENT (TG-227 blocker 1, REQ-2811).
//
// REQ-2811 requires family and tier "mechanically assigned from the closed sets" before a candidate may
// reach ratify_ready — and no assigner existed anywhere in the tree. This one is a deterministic keyword
// table over the candidate's slug and observed op, mapping into opschema's closed family set.
//
// AMBIGUITY FAILS CLOSED. Zero matching families, or two DIFFERENT matching families, returns ok=false and
// the candidate simply stays a candidate — a mislabeled family would put a class on a graduation ladder no
// operator reviewed for it, so a guess is worse than a wait. The cost of a miss is slowness, never a wrong
// grant; extending the table is an ordinary reviewed edit.

import "strings"

// familyKeywords maps a token observed in the slug/op to its family. One token, one family — the table
// itself cannot express an ambiguity; ambiguity arises only when DIFFERENT tokens vote differently, and
// that is exactly the case AssignFamily refuses.
var familyKeywords = map[string]string{
	// service-lifecycle: unit-verb tokens. "systemd" itself deliberately does NOT vote — it names the
	// SUBSTRATE, not the action, and it appears in slugs of other families ("vacuum-systemd-journal" is
	// disk-reclaim). The verb tokens are the discriminators; a substrate token voting would turn every
	// systemd-adjacent action into a family conflict.
	"service": "service-lifecycle", "unit": "service-lifecycle",
	"systemctl": "service-lifecycle", "daemon": "service-lifecycle",
	// container-lifecycle
	"container": "container-lifecycle", "docker": "container-lifecycle", "compose": "container-lifecycle",
	"pod": "k8s-workload", "rollout": "k8s-workload", "cordon": "k8s-workload", "drain": "k8s-workload",
	"deployment": "k8s-workload", "replica": "k8s-workload",
	// guest-lifecycle: hypervisor guests
	"guest": "guest-lifecycle", "vm": "guest-lifecycle", "lxc": "guest-lifecycle", "qm": "guest-lifecycle",
	// disk-reclaim: the destructive space-freeing family
	"prune": "disk-reclaim", "vacuum": "disk-reclaim", "rotate": "disk-reclaim", "trim": "disk-reclaim",
	"journal": "disk-reclaim", "logrotate": "disk-reclaim", "clean": "disk-reclaim",
	// resource-resize
	"resize": "resource-resize", "grow": "resource-resize", "memory": "resource-resize", "cores": "resource-resize",
	// network-device: vendor gear that can partition the estate
	"cisco": "network-device", "pfsense": "network-device", "switch": "network-device",
	"router": "network-device", "firewall": "network-device", "apc": "network-device",
	// storage
	"zfs": "storage", "lvm": "storage", "ceph": "storage", "synology": "storage",
	"volume": "storage", "dataset": "storage", "iscsi": "storage",
	// process
	"kill": "process", "renice": "process", "process": "process",
	// package
	"package": "package", "apt": "package", "yum": "package", "dnf": "package", "pip": "package",
}

// AssignFamily mechanically derives the closed-set family for a candidate from its slug and observed op.
// ok=false means "no unambiguous mechanical answer" — the caller must treat the dossier as incomplete.
func AssignFamily(opClass, op string) (string, bool) {
	votes := map[string]bool{}
	for _, tok := range tokensOf(opClass + " " + op) {
		if fam, hit := familyKeywords[tok]; hit {
			votes[fam] = true
		}
	}
	if len(votes) != 1 {
		return "", false // zero hits or a conflict — fail closed, stay a candidate
	}
	for fam := range votes {
		return fam, true
	}
	return "", false // unreachable
}

// AssignTier mechanically derives the safety tier from the auto-barred verdict and the family.
//
// A machine never assigns an auto-eligible tier: TierLowReversible is a claim that a clean inverse exists
// and is idempotent, which only an operator can attest. So the mechanical floor is: barred or destructive
// ⇒ irreversible ("may never auto" is the tier's own definition); vendor network gear ⇒ vendor-critical
// (a wrong call can partition the estate); everything else ⇒ medium. The operator may TIGHTEN this at
// ratification; the ratify writer refuses the loosening direction for barred classes.
func AssignTier(family string, autoBarred bool) string {
	if family == "network-device" {
		return "vendor-critical"
	}
	if autoBarred {
		return "irreversible"
	}
	return "medium"
}

// tokensOf splits a slug/op string on the separators slugs actually use.
func tokensOf(s string) []string {
	return strings.FieldsFunc(strings.ToLower(s), func(r rune) bool {
		return r == '-' || r == '_' || r == ' ' || r == '.' || r == '/' || r == ':'
	})
}
