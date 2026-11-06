package ssh

import (
	"errors"
	"regexp"
	"testing"

	"github.com/territory-grounder/grounder/core/safety"
)

// TG-626: follow-estate admits unlisted subjects on non-stateful hosts, except the never-touch set; listed mode
// is byte-identical.

func followLeaf(host string, units, containers []string, follow bool) *Module {
	opts := []Option{WithMutation(safety.NewActuatingChokepoint(), units, containers)}
	if follow {
		opts = append(opts, WithFollowEstate())
	}
	return New(host, "root", &fakeRunner{}, opts...)
}

func TestFollowEstateAdmitsAnUnlistedUnitOnANonStatefulHost(t *testing.T) {
	m := followLeaf("dc1gpu01", []string{"nginx"}, nil, true)
	cmd, rb, err := m.resolveOp("restart-service", "uvicorn")
	if err != nil {
		t.Fatalf("TG-626: gpu01's uvicorn (refused 10-19 and 11-02) must resolve under follow-estate: %v", err)
	}
	if len(cmd) == 0 || len(rb) == 0 {
		t.Fatalf("the forward and rollback argv must both render, got %v / %v", cmd, rb)
	}
	if _, _, err := m.resolveOp("restart-container", "openwebui"); err != nil {
		t.Fatalf("an unlisted container must resolve under follow-estate: %v", err)
	}
}

func TestFollowEstateRefusals(t *testing.T) {
	cases := []struct {
		host, op, subject string
		want              error
	}{
		{"dc1gpu01", "restart-service", "sshd", ErrNeverTouchUnit},
		{"dc1gpu01", "restart-service", "ssh.service", ErrNeverTouchUnit},
		{"dc1gpu01", "restart-service", "systemd-journald", ErrNeverTouchUnit},
		{"dc1gpu01", "restart-service", "docker", ErrNeverTouchUnit},
		{"dc1gpu01", "restart-service", "NetworkManager.service", ErrNeverTouchUnit},
		{"dc1pve03", "restart-service", "pveproxy", ErrNeverTouchUnit},
		{"dc1wg01", "restart-service", "wg-quick@wg0", ErrNeverTouchUnit},
		{"dc1cl01mariadb01", "restart-service", "nginx-exporter", ErrStatefulHost},
		{"dc1redis02", "restart-container", "sidecar", ErrStatefulHost},
		{"dc1gpu01", "restart-service", "postgresql", ErrStatefulWorkload}, // the subject clamp still runs first
		{"dc1gpu01", "restart-service", "a b", ErrInvalidUnit},
		{"dc1gpu01", "restart-container", "-rm", ErrInvalidContainer},
		// security-review hardening (TG-626)
		{"dc1gpu01", "restart-service", "--all", ErrInvalidUnit},
		{"dc1gpu01", "restart-service", "-.mount", ErrInvalidUnit},
		{"dc1gpu01", "restart-service", "nftables", ErrNeverTouchUnit},
		{"dc1gpu01", "restart-service", "messagebus", ErrNeverTouchUnit},
		{"dc1gpu01", "restart-service", "ssh.socket", ErrNeverTouchUnit},
		{"dc1gpu01", "restart-service", "sshd@0-192.0.2.1:22", ErrNeverTouchUnit},
		{"dc1gpu01", "restart-service", "multi-user.target", ErrNeverTouchUnit},
		{"dc1gpu01", "restart-service", "ceph-osd@1", ErrNeverTouchUnit},
		{"dc1gpu01", "restart-container", "portainer", ErrNeverTouchUnit},
	}
	for _, c := range cases {
		m := followLeaf(c.host, nil, nil, true)
		if _, _, err := m.resolveOp(c.op, c.subject); !errors.Is(err, c.want) {
			t.Errorf("%s %s on %s: got %v, want %v", c.op, c.subject, c.host, err, c.want)
		}
	}
}

func TestListedModeIsByteIdentical(t *testing.T) {
	off := followLeaf("dc1gpu01", []string{"nginx", "docker"}, []string{"mealie"}, false)
	if _, _, err := off.resolveOp("restart-service", "uvicorn"); !errors.Is(err, ErrUnitNotAllowed) {
		t.Fatalf("without follow-estate an unlisted unit is refused exactly as before, got %v", err)
	}
	if _, _, err := off.resolveOp("restart-container", "openwebui"); !errors.Is(err, ErrContainerNotAllowed) {
		t.Fatalf("without follow-estate an unlisted container is refused exactly as before, got %v", err)
	}
	// An operator who explicitly LISTS a never-touch unit keeps the grant they authored (the list wins).
	if _, _, err := off.resolveOp("restart-service", "docker"); err != nil {
		t.Fatalf("an explicitly listed unit must resolve in listed mode, got %v", err)
	}
	on := followLeaf("dc1gpu01", []string{"docker"}, nil, true)
	if _, _, err := on.resolveOp("restart-service", "docker"); err != nil {
		t.Fatalf("follow-estate never narrows the operator's explicit list, got %v", err)
	}
}

func TestFollowEstateNeedsTheMutatingConfiguration(t *testing.T) {
	m := New("dc1gpu01", "root", &fakeRunner{}, WithFollowEstate())
	if m.FollowsEstate() {
		t.Fatal("follow-estate on a read-only module must not report active — it has no execution path to widen")
	}
}

// The host guard's pattern mode refuses, from a leaked key, exactly what this leaf refuses: never-touch units and
// stateful subjects. Compiled with Go's RE2 (the rendered ERE uses only constructs RE2 and POSIX ERE share).
func TestGuardDenyEREMatchesTheLeaf(t *testing.T) {
	re := regexp.MustCompile("(?i)" + GuardDenyERE())
	samples := []string{
		"sshd", "ssh.service", "dbus", "systemd-resolved", "docker", "docker.service", "containerd", "pveproxy",
		"pve-cluster", "lxcfs", "kubelet", "k3s-agent", "wg-quick@wg0", "bird2", "strongswan-starter", "frr",
		"postgresql", "mariadb", "redis-server", "prometheus-node-exporter", "openbao", "app-db", "influxdb2",
		"nginx", "apache2", "uvicorn", "ollama", "mealie", "rsyslog", "cron", "php8.2-fpm", "gitlab-runner",
		"Docker", "SSHD", "nftables", "messagebus", "ssh.socket", "sshd@0-1.2.3.4:22", "multi-user.target",
		"home.mount", "ceph-osd@1", "portainer", "tg-worker", "awx-task", "nfs-server", "traefik", "unbound",
	}
	for _, s := range samples {
		leaf := NeverTouchUnit(s) || safety.IsStatefulWorkload(s)
		if got := re.MatchString(s); got != leaf {
			t.Errorf("parity broken for %q: guard deny=%v, leaf refuse=%v", s, got, leaf)
		}
	}
}
