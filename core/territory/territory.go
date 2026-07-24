// Package territory implements Territory Grounder's namesake control: the TERRITORY GATE. A mutating action
// inside a high-stakes infrastructure territory (k8s, network, edge, pve, native/storage, docker) may proceed
// only once that territory's operating manual has been ACKNOWLEDGED this session — the "grounding" the
// product is named for: an agent must have read the ground rules for the territory it is about to change.
//
// Provenance: [F] the predecessor PreToolUse territory-gate.py (IFRNLLEI01PRD-1408), re-expressed as a pure
// typed gate · [O] a fail-closed write prerequisite that composes over the mechanical safety core.
//
// Fail directions (faithful to the predecessor):
//   - read-only / investigation is NEVER gated (allow);
//   - a mutating action NOT in any high-stakes territory is benign (allow);
//   - a mutating action in a high-stakes territory whose manual is acknowledged (allow);
//   - a mutating action in a high-stakes territory whose manual is NOT acknowledged → BLOCK (load it first);
//   - a CONFIRMED infrastructure write the gate cannot place in a territory → BLOCK (fail closed — never wave
//     through a high-stakes write it cannot verify).
package territory

import (
	"regexp"
	"strings"
)

// Territory is a high-stakes infrastructure domain with its own operating caveats.
type Territory string

const (
	TerritoryK8s     Territory = "k8s"
	TerritoryNetwork Territory = "network"
	TerritoryEdge    Territory = "edge"
	TerritoryPVE     Territory = "pve"
	TerritoryNative  Territory = "native" // storage / NAS — iSCSI PVs, NFS, ZFS
	TerritoryDocker  Territory = "docker"
)

// caveat is the one-line operating rule for each territory, surfaced on a block so the operator/agent knows
// exactly which ground rules to load. Ported from the predecessor's _STAKES map.
var caveat = map[Territory]string{
	TerritoryK8s:     "OpenTofu/Atlantis only — no kubectl apply / helm install on managed resources",
	TerritoryNetwork: "Cisco ASA = Netmiko not NAPALM; hierarchical diffs only; never SSH-deploy; check RUNNING config",
	TerritoryEdge:    "never `netplan apply` once eBGP is up; auth backends must join the BREACH ACL",
	TerritoryPVE:     "every config change stops+restarts the guest (downtime); copy live config first",
	TerritoryNative:  "never restart the storage nodes (iSCSI PVs + NVR NFS); the device is the source of truth",
	TerritoryDocker:  "push-to-main rsync overwrites the host; Galera needs --no-deps; never compose pull",
}

// Caveat returns the operating rule for a territory (empty for an unknown one).
func Caveat(t Territory) string { return caveat[t] }

// classifiers maps each territory to the pattern that identifies an action targeting it, evaluated over the
// action's target + op + op_class. Ordered most-destructive-first so an ambiguous action resolves to the
// territory whose ground rules matter most (storage before compute). These are safety vocabulary, fixed like
// the never-auto floor — an operator extends coverage by declaring extra territories, never by relaxing these.
var classifiers = []struct {
	t  Territory
	re *regexp.Regexp
}{
	{TerritoryNative, regexp.MustCompile(`(?i)\b(?:synology|syno\d*|iscsi\w*|\bnas\b|nfs|nvr|zfs|zpool|truenas|freenas|seaweedfs|weed|synomib|exportfs)\b`)},
	{TerritoryEdge, regexp.MustCompile(`(?i)\b(?:netplan|ebgp|edge[-_]?router|breach[-_]?acl|frr\b|bird\b|birdc|swanctl|haproxy)\b|\bip\s+xfrm\b`)},
	{TerritoryNetwork, regexp.MustCompile(`(?i)\b(?:cisco|asa\b|\bbgp\b|firewall|netmiko|napalm|\bvpn\b|\bacl\b|switch\b|router\b|ios-xe|nxos|vtysh|hier_?config)\b`)},
	// K8s is ordered BEFORE PVE so a stateful k8s guest DOMINATES: a `qm/pct reboot` of a k8s control-plane
	// node (target matches k8s-ctrlr/node/...) requires the k8s grounding, not just PVE. The k8s host pattern
	// carries no leading \b so it matches inside a compound hostname (e.g. dc1k8s-ctrlr01). The command
	// verbs include the IaC/GitOps writers the predecessor maps to k8s (tofu/terraform/argocd/cilium).
	{TerritoryK8s, regexp.MustCompile(`(?i)\b(?:kubectl|helm|kubernetes|k8s|kube[-_]?system|statefulset|daemonset|deployment/|rollout|tofu|terraform|argocd|cilium)\b|k8s[-_](?:ctrlr|node|wrkr|frr|openbao|lb)`)},
	{TerritoryPVE, regexp.MustCompile(`(?i)\b(?:proxmox|\bpve\b|pve\d+|\bpct\b|\bqm\b|qemu|vzdump|pvesh|pvecm|pvesm|ha-manager)\b`)},
	{TerritoryDocker, regexp.MustCompile(`(?i)\b(?:docker|docker-compose|compose\b|galera|swarm)\b`)},
}

// Classify maps an action (its target host, op, and op_class) to its high-stakes territory. ok=false when the
// action is not in any classified territory.
func Classify(parts ...string) (Territory, bool) {
	blob := strings.Join(parts, " ")
	for _, c := range classifiers {
		if c.re.MatchString(blob) {
			return c.t, true
		}
	}
	return "", false
}

// Decision is the gate's ruling.
type Decision int

const (
	Allow Decision = iota // proceed
	Block                 // held — a high-stakes write whose ground rules are not acknowledged, or unverifiable
)

// Result is the gate's ruling with its rationale.
type Result struct {
	Decision  Decision
	Territory Territory // the classified territory (empty when benign / unclassified)
	Reason    string
}

// Gate decides whether a proposed action may touch its territory. Acknowledged is the set of territories
// whose operating manual has been loaded THIS session (the grounding prerequisite).
type Gate struct {
	Acknowledged map[Territory]bool
}

// Permit rules on an action. `mutating` is false for a read-only/investigation action (never gated).
// `confirmedInfra` is a server-side signal that the op is a genuine infrastructure write (e.g.
// safety.IsDestructiveOp) — used to fail CLOSED on a confirmed write the gate cannot place in a territory.
func (g Gate) Permit(mutating, confirmedInfra bool, parts ...string) Result {
	if !mutating {
		return Result{Decision: Allow, Reason: "read-only action — never gated"}
	}
	t, ok := Classify(parts...)
	if !ok {
		if confirmedInfra {
			// a CONFIRMED infrastructure write the gate cannot place — hold it rather than wave it through.
			return Result{Decision: Block, Reason: "confirmed infrastructure write the territory gate cannot place — fail closed"}
		}
		return Result{Decision: Allow, Reason: "not in a high-stakes territory — benign"}
	}
	if g.Acknowledged[t] {
		return Result{Decision: Allow, Territory: t, Reason: "territory " + string(t) + " acknowledged this session"}
	}
	return Result{
		Decision: Block, Territory: t,
		Reason: "territory " + string(t) + " not acknowledged — load its ground rules first: " + caveat[t],
	}
}
