package ssh

import (
	"errors"
	"strings"

	"github.com/territory-grounder/grounder/core/safety"
)

// FOLLOW-ESTATE (TG-626). The owner ruled on 2026-09-13 that TG heals ALL guests, and on 2026-11-05 that the
// service and container heal scope follows the estate with stateful workloads excluded. Until then the leaf's
// allowed-units/containers lists named the July test pool only (nginx/apache2; mealie/ghostfolio/actualbudget),
// so a heal on any other host — gpu01's uvicorn, refused on 10-19 and 11-02 — had no execution path.
//
// WithFollowEstate adds ONE admission path to resolveOp: a syntactically valid unit or container that is NOT
// on the operator's list is admitted when it is neither on the NEVER-TOUCH list below nor on a stateful HOST.
// Everything else is unchanged and still runs first: the never-auto floor, the stateful-workload clamp on
// the subject, the reversible-op registry, the strict token shape. It widens WHICH subjects, never WHAT verb.
//
// The never-touch list is the set of units whose restart can sever TG from the host, the host from the
// estate, or a hypervisor from its cluster — an autonomous restart of any of them is an outage generator, not
// a heal. Listed mode (no WithFollowEstate) never consults it: an operator who explicitly lists one keeps the
// grant they authored.

// ErrNeverTouchUnit refuses a follow-estate admission of a unit on the never-touch list.
var ErrNeverTouchUnit = errors.New("ssh: unit is on the never-touch list (access, networking, container runtime, hypervisor, cluster, routing or VPN plane) — follow-estate never admits it")

// ErrStatefulHost refuses a follow-estate admission on a host whose NAME marks it a stateful workload.
var ErrStatefulHost = errors.New("ssh: target host is a stateful workload host — follow-estate never admits a subject on it")

// neverTouchExact are unit names (case-insensitive, with or without .service) follow-estate never admits.
var neverTouchExact = []string{
	// access + session plane
	"ssh", "sshd", "dbus", "messagebus", "sssd", "slapd", "qemu-guest-agent",
	// network + firewall plane (an nftables restart flushes rules and kills Docker)
	"networking", "network", "NetworkManager", "networkd-dispatcher", "connman", "wpa_supplicant",
	"nftables", "iptables", "ip6tables", "ufw", "firewalld", "netfilter-persistent",
	// container runtimes
	"docker", "containerd", "podman", "cri-o", "crio",
	// hypervisor + cluster + routing + VPN
	"corosync", "pacemaker", "qemu-server", "kubelet", "keepalived", "frr", "zebra", "bgpd", "ipsec",
	"tailscaled", "tailscale", "portainer", "watchtower", "wg-easy",
	// storage plane
	"glusterd", "iscsid", "multipathd", "autofs",
}

// neverTouchPrefix are unit-name prefixes (case-insensitive) follow-estate never admits.
var neverTouchPrefix = []string{
	"systemd-", "pve", "lxc", "k3s", "rke2", "wg-quick@", "bird", "strongswan", "openvpn", "getty@", "user@",
	"ssh@", "sshd@", "ifup@", "openvswitch", "ovs-", "ceph", "nfs-", "zfs-", "krb5", "cloud-", "awx", "tg-",
}

// neverTouchSuffix are systemd unit TYPES follow-estate never admits: only services (bare names or .service)
// are heal subjects. `systemctl restart multi-user.target` or a .mount/.slice/.socket restart is a host-wide
// event, not a service heal.
var neverTouchSuffix = []string{
	".target", ".mount", ".automount", ".slice", ".socket", ".device", ".swap", ".path", ".timer", ".scope",
}

// NeverTouchUnit reports whether follow-estate must refuse this unit.
func NeverTouchUnit(unit string) bool {
	raw := strings.ToLower(strings.TrimSpace(unit))
	for _, s := range neverTouchSuffix {
		if strings.HasSuffix(raw, s) {
			return true
		}
	}
	u := strings.TrimSuffix(raw, ".service")
	for _, n := range neverTouchExact {
		if u == strings.ToLower(n) {
			return true
		}
	}
	for _, p := range neverTouchPrefix {
		if strings.HasPrefix(u, strings.ToLower(p)) {
			return true
		}
	}
	return false
}

// WithFollowEstate turns on the follow-estate admission path (see the file comment). It requires
// WithMutation: a module without the mutating configuration has no execution path for it to widen.
func WithFollowEstate() Option { return func(m *Module) { m.followEstate = true } }

// FollowsEstate reports whether this leaf admits unlisted subjects under the follow-estate rules.
func (m *Module) FollowsEstate() bool { return m.followEstate && m.gate != nil }

// admitUnlisted decides a follow-estate admission for a subject that is NOT on the operator's list. The subject
// already passed the never-auto floor, the stateful clamp on its own name, and the strict token shape.
func (m *Module) admitUnlisted(subject string, isUnit bool, notAllowed error) error {
	if !m.FollowsEstate() {
		return notAllowed
	}
	if safety.IsStatefulWorkload(m.host) {
		return ErrStatefulHost
	}
	// The never-touch set applies to containers too (a `tailscale` or `portainer` container is the same plane as
	// its unit), which also keeps the leaf and the host guard's DENY — subject-blind on kind — in exact parity.
	if NeverTouchUnit(subject) {
		return ErrNeverTouchUnit
	}
	return nil
}

// GuardDenyERE renders the never-touch and stateful lists as ONE POSIX extended regular expression, to be
// matched case-insensitively (grep -Eiq) against the bare SUBJECT token by the host guard's pattern mode
// (deploy/actuation-guard). It is the host-side twin of admitUnlisted + safety.IsStatefulWorkload, so a leaked
// actuator key cannot restart on a host what this leaf would refuse. guardDenyParityCases pins the parity.
func GuardDenyERE() string {
	var exact []string
	for _, n := range neverTouchExact {
		exact = append(exact, n)
	}
	alts := []string{"^(" + strings.Join(exact, "|") + `)(\.service)?$`}
	for _, p := range neverTouchPrefix {
		alts = append(alts, "^"+p)
	}
	var sfx []string
	for _, s := range neverTouchSuffix {
		sfx = append(sfx, strings.TrimPrefix(s, "."))
	}
	alts = append(alts, `\.(`+strings.Join(sfx, "|")+`)$`)
	// The stateful substrings (safety.statefulDenyRE minus its \w* tails, which a substring match subsumes).
	alts = append(alts, strings.Join(statefulGuardTokens, "|"))
	return strings.Join(alts, "|")
}

// statefulGuardTokens is the POSIX rendering of core/safety's stateful deny list, as SUBSTRINGS. Parity with
// safety.IsStatefulWorkload is asserted by TestGuardDenyEREMatchesTheLeaf.
var statefulGuardTokens = []string{
	"etcd", "postgres", "mysql", "mariadb", "seaweedfs", "thanos", "redis", "prometheus", "mongo", "cassandra",
	"elasticsearch", "opensearch", "vault", "openbao", "consul", "clickhouse", "kafka", "zookeeper", "rabbitmq",
	"nats", "minio", "influxdb", "victoria", "loki", "cockroach", "mssql", "sqlserver", "oracle", "couch",
	"neo4j", "qdrant", "weaviate", "valkey", "percona", "proxysql", "graylog", "statefulset", "-db", "-database",
}
