package eval

// The TG-78 node-plane corpus rows must actually EXERCISE the node routing they were added to grade —
// review 2026-08-25 found two of the three graded a routing that never ran (the fixture loader's
// per-name type flattening destroyed pve_node identity, IsPveNode went false, and the incidents still
// reached their escalate label through unrelated paths: a green oracle that could not fail). This test
// pins reachability itself: for every tg78-* incident the fixture graph must answer IsPveNode=true, the
// composed domain must be proxmox, and — for the rows whose rule text would otherwise classify nothing —
// removing the node signal must change the answer. If the fixture, the loader, or the routing regresses,
// THIS reddens, not just the judged scores.

import (
	"testing"

	"github.com/territory-grounder/grounder/agent/skills"
)

func TestNodePlaneCorpusRowsExerciseTheNodeRouting(t *testing.T) {
	g := loadEstateGraph(t, "estate_fixture.json")
	corpus, err := LoadCorpus("corpus.json")
	if err != nil {
		t.Fatal(err)
	}
	seen := 0
	for _, inc := range corpus {
		switch inc.ExternalRef {
		case "tg78-node-01", "tg78-cluster-01", "tg78-storage-01":
		default:
			continue
		}
		seen++
		if !g.IsPveNode(inc.Host) {
			t.Errorf("%s: fixture graph does not know %s as a PVE node — the incident grades a routing that never runs", inc.ExternalRef, inc.Host)
			continue
		}
		got := skills.DomainOf(inc.AlertRule, inc.Host, skills.HostSignals{Guest: g.IsGuest(inc.Host), PveNode: true})
		if got != skills.DomainProxmox {
			t.Errorf("%s: DomainOf = %q, want proxmox", inc.ExternalRef, got)
		}
		// The node signal must be LOAD-BEARING: with it withheld, the same incident must not reach
		// proxmox through the node branch's work. (tg78-node-01's host is also a netbox-typed guest and
		// its rule is a host-down, so the guest branch legitimately catches that one — the node branch
		// is still what production uses, per DomainOf's stated precedence; the other two rows classify
		// NOTHING without the signal, which is exactly the reachability this test exists to pin.)
		without := skills.DomainOf(inc.AlertRule, inc.Host, skills.HostSignals{Guest: g.IsGuest(inc.Host)})
		if inc.ExternalRef != "tg78-node-01" && without == skills.DomainProxmox {
			t.Errorf("%s: classifies proxmox even WITHOUT the node signal — the row is not measuring the node routing", inc.ExternalRef)
		}
	}
	if seen != 3 {
		t.Fatalf("expected the 3 tg78-* node-plane rows in the corpus, saw %d", seen)
	}
}

// The TG-78 k8s rows route by RULE (the ^(kube|cilium) regex), not by any estate signal — pinned the same
// way as the node-plane rows so the fixture cannot silently stop exercising them. The pod-name host of
// tg78-k8s-01 must classify as NEITHER guest NOR node in the fixture graph: if it ever does, the row
// would be measuring estate routing instead of the rule table.
func TestK8sCorpusRowsRouteByRule(t *testing.T) {
	g := loadEstateGraph(t, "estate_fixture.json")
	corpus, err := LoadCorpus("corpus.json")
	if err != nil {
		t.Fatal(err)
	}
	seen := 0
	for _, inc := range corpus {
		switch inc.ExternalRef {
		case "tg78-k8s-01", "tg78-k8s-02", "tg78-k8s-03":
		default:
			continue
		}
		seen++
		got := skills.DomainOf(inc.AlertRule, inc.Host, skills.HostSignals{Guest: g.IsGuest(inc.Host), PveNode: g.IsPveNode(inc.Host), NetworkDevice: g.IsNetworkDevice(inc.Host), StorageAppliance: g.IsStorageAppliance(inc.Host)})
		if got != skills.DomainKubernetes {
			t.Errorf("%s: DomainOf = %q, want kubernetes (the rule table must classify %q)", inc.ExternalRef, got, inc.AlertRule)
		}
		// KILLING MUTATION: route these rows through an estate signal instead of the rule text → the
		// pod-name assert below reddens (a pod name is no estate entity).
		if inc.ExternalRef == "tg78-k8s-01" && (g.IsGuest(inc.Host) || g.IsPveNode(inc.Host)) {
			t.Errorf("%s: the pod-name host %q must be estate-unknown — rule routing is what this row measures", inc.ExternalRef, inc.Host)
		}
	}
	if seen != 3 {
		t.Fatalf("expected the 3 tg78-k8s rows, saw %d", seen)
	}
}

// The TG-78 PROXMOX-RUNBOOK corpus rows (eval-coverage slice, 2026-08-29) grade the proxmox runbook packs
// (node-storage / cluster-quorum / guest-lifecycle) that shipped 2026-08-26 with no eval pairing. Like the
// node-plane rows, they must actually EXERCISE proxmox routing — pve-node/guest identity in the fixture graph
// is what selects the proxmox competence; if the loader or routing regresses, the judged scores grade a
// routing that never ran. This pins reachability for the four new rows.
func TestTG78ProxmoxRunbookRowsRouteToProxmox(t *testing.T) {
	g := loadEstateGraph(t, "estate_fixture.json")
	corpus, err := LoadCorpus("corpus.json")
	if err != nil {
		t.Fatal(err)
	}
	seen := 0
	for _, inc := range corpus {
		switch inc.ExternalRef {
		case "tg78-pve-storage-01", "tg78-pve-cluster-01":
		default:
			continue
		}
		seen++
		got := skills.DomainOf(inc.AlertRule, inc.Host, skills.HostSignals{Guest: g.IsGuest(inc.Host), PveNode: g.IsPveNode(inc.Host), NetworkDevice: g.IsNetworkDevice(inc.Host), StorageAppliance: g.IsStorageAppliance(inc.Host)})
		if got != skills.DomainProxmox {
			t.Errorf("%s: DomainOf = %q, want proxmox (host %q known-pve=%v guest=%v)", inc.ExternalRef, got, inc.Host, g.IsPveNode(inc.Host), g.IsGuest(inc.Host))
		}
	}
	if seen != 2 {
		t.Fatalf("expected the 2 tg78-pve-* proxmox-runbook rows (guest-02 removed-as-unevaluable, TG-556), saw %d", seen)
	}
}

// The TG-78 linux rows route by RULE + GUEST-signal (the linuxRuleRE allowlist over hostIsGuest) — pinned
// like the node/k8s rows so the fixture cannot silently stop exercising them. Each row's competence claim
// is exactly "an OS-plane fault on a plain guest loads linux-triage": lose the guest signal (fixture drift)
// or narrow the rule allowlist and the classification is LOST, which this test makes a red instead of a
// silently-unmeasured skill (the pve-liveness dead-regex lesson).
func TestLinuxCorpusRowsRouteToLinux(t *testing.T) {
	g := loadEstateGraph(t, "estate_fixture.json")
	corpus, err := LoadCorpus("corpus.json")
	if err != nil {
		t.Fatal(err)
	}
	seen := 0
	for _, inc := range corpus {
		switch inc.ExternalRef {
		case "tg-linux-01", "tg-linux-02", "tg-linux-03":
		default:
			continue
		}
		seen++
		if !g.IsGuest(inc.Host) || g.IsPveNode(inc.Host) {
			t.Errorf("%s: host %q must be a plain GUEST in the fixture (guest=%v pveNode=%v) — the linux lane requires it",
				inc.ExternalRef, inc.Host, g.IsGuest(inc.Host), g.IsPveNode(inc.Host))
		}
		got := skills.DomainOf(inc.AlertRule, inc.Host, skills.HostSignals{Guest: g.IsGuest(inc.Host), PveNode: g.IsPveNode(inc.Host), NetworkDevice: g.IsNetworkDevice(inc.Host), StorageAppliance: g.IsStorageAppliance(inc.Host)})
		if got != skills.DomainLinux {
			t.Errorf("%s: DomainOf = %q, want linux (rule %q on a plain guest)", inc.ExternalRef, got, inc.AlertRule)
		}
		// KILLING MUTATION: withhold the guest signal and the classification must be LOST — proving the
		// routing is reachable through the estate signal, not satisfiable by the rule text alone.
		if got := skills.DomainOf(inc.AlertRule, inc.Host, skills.HostSignals{}); got == skills.DomainLinux {
			t.Errorf("%s: classifies linux even with the guest signal withheld — the lane must require a guest", inc.ExternalRef)
		}
	}
	if seen != 3 {
		t.Fatalf("expected the 3 tg-linux rows, saw %d", seen)
	}
}

// The AP/network rows route by ESTATE IDENTITY (TG-78 network slice): eval-04/06/14 sit on the real
// access points, typed network_device in the fixture exactly as the live LibreNMS topology source now
// stamps them (os=ios) — so the shipped cisco competence composes on them instead of the pre-slice
// DomainUnknown. Pinned with the same withhold-the-signal killing mutation as the linux rows: lose the
// fixture typing (a recapture that flattens types) and this reddens instead of silently un-grading the
// cisco lane on network incidents.
func TestNetworkDeviceCorpusRowsRouteToCisco(t *testing.T) {
	g := loadEstateGraph(t, "estate_fixture.json")
	corpus, err := LoadCorpus("corpus.json")
	if err != nil {
		t.Fatal(err)
	}
	seen := 0
	for _, inc := range corpus {
		switch inc.ExternalRef {
		case "eval-04", "eval-06", "eval-14":
		default:
			continue
		}
		seen++
		if !g.IsNetworkDevice(inc.Host) {
			t.Errorf("%s: host %q must be typed network_device in the fixture (the live topology stamps os=ios)", inc.ExternalRef, inc.Host)
		}
		got := skills.DomainOf(inc.AlertRule, inc.Host, skills.HostSignals{Guest: g.IsGuest(inc.Host), PveNode: g.IsPveNode(inc.Host), NetworkDevice: g.IsNetworkDevice(inc.Host), StorageAppliance: g.IsStorageAppliance(inc.Host)})
		if got != skills.DomainCisco {
			t.Errorf("%s: DomainOf = %q, want cisco (a network device routes to the network competence on every family)", inc.ExternalRef, got)
		}
		if got := skills.DomainOf(inc.AlertRule, inc.Host, skills.HostSignals{}); got != skills.DomainUnknown {
			t.Errorf("%s: classifies %q even with the identity withheld — the routing must come from the estate signal", inc.ExternalRef, got)
		}
	}
	if seen != 3 {
		t.Fatalf("expected the 3 AP rows, saw %d", seen)
	}
}

// The storage rows route by ESTATE IDENTITY (TG-78 storage slice): both Synologies are typed
// storage_appliance in the fixture exactly as the live topology source stamps them (os=dsm), so the
// storage competence composes on the appliance's generic SNMP rules — including the disk-space family
// that must NOT take the linux lane's guest framing, pinned here by the not-a-guest assert. Same
// withhold-the-signal killing mutation as the linux/network rows.
func TestStorageCorpusRowsRouteToStorage(t *testing.T) {
	g := loadEstateGraph(t, "estate_fixture.json")
	corpus, err := LoadCorpus("corpus.json")
	if err != nil {
		t.Fatal(err)
	}
	seen := 0
	for _, inc := range corpus {
		switch inc.ExternalRef {
		case "tg-storage-01":
		default:
			continue
		}
		seen++
		if !g.IsStorageAppliance(inc.Host) || g.IsGuest(inc.Host) || g.IsPveNode(inc.Host) {
			t.Errorf("%s: host %q must be a pure storage_appliance in the fixture (appliance=%v guest=%v node=%v)",
				inc.ExternalRef, inc.Host, g.IsStorageAppliance(inc.Host), g.IsGuest(inc.Host), g.IsPveNode(inc.Host))
		}
		got := skills.DomainOf(inc.AlertRule, inc.Host, skills.HostSignals{Guest: g.IsGuest(inc.Host), PveNode: g.IsPveNode(inc.Host), NetworkDevice: g.IsNetworkDevice(inc.Host), StorageAppliance: g.IsStorageAppliance(inc.Host)})
		if got != skills.DomainStorage {
			t.Errorf("%s: DomainOf = %q, want storage (the appliance identity routes every family)", inc.ExternalRef, got)
		}
		if got := skills.DomainOf(inc.AlertRule, inc.Host, skills.HostSignals{}); got != skills.DomainUnknown {
			t.Errorf("%s: classifies %q with the identity withheld — the routing must come from the estate signal", inc.ExternalRef, got)
		}
	}
	if seen != 1 {
		t.Fatalf("expected the storage row, saw %d", seen)
	}
}
