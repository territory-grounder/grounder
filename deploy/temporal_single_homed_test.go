package deploy

// TEMPORAL MUST BE ON EXACTLY ONE NETWORK (TG-327), AND THIS COST AN OUTAGE TO LEARN.
//
// On 2026-08-05 the TG-160 segmentation deploy put temporal on tg-backplane AND tg-frontdoor — frontdoor
// only so its published :7233 could serve, since an internal-only container cannot publish. Both temporal
// and worker went Exited(1) and took the control plane with them.
//
// The mechanism: temporalio/auto-setup's entrypoint defaults BIND_ON_IP to `hostname -i`. In a
// multi-homed container that is ambiguous — it resolved to 192.168.181.31, the HOST's LAN address, which
// the container does not own. temporal therefore bound nothing usable:
//
//	temporal: failed to start ringpop: join duration of 51.3s exceeded max 30s
//	worker:   dial tcp 172.24.0.3:7233: connect: connection refused
//
// Worth stating plainly, because the intuitive diagnosis is wrong: the backplane is 172.24/16, so DNS
// handed the worker the RIGHT address on the RIGHT shared network. Nothing was misresolved and the
// segmentation was not at fault. temporal was simply not listening. Every other service binds 0.0.0.0
// and stayed healthy throughout — grounder, worker and console up, prometheus still scraping.
//
// So the invariant is narrow and specific: THIS image, because of ITS bind default, gets one network.

import (
	"os"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestTemporalIsSingleHomed(t *testing.T) {
	raw, err := os.ReadFile("docker-compose.yml")
	if err != nil {
		t.Fatalf("read docker-compose.yml: %v", err)
	}
	var doc struct {
		Services map[string]struct {
			Image    string   `yaml:"image"`
			Networks networkNames `yaml:"networks"`
			Ports    []string `yaml:"ports"`
		} `yaml:"services"`
	}
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("parse docker-compose.yml: %v", err)
	}

	svc, ok := doc.Services["temporal"]
	if !ok {
		t.Fatal("the temporal service is absent — this guard is scanning a file that no longer describes " +
			"the deployment, and would pass by checking nothing")
	}

	// Vacuity floor: if the compose file stopped declaring per-service networks entirely, "len == 1" would
	// be false for the wrong reason and this test would start failing confusingly — or, if inverted, pass
	// while asserting nothing. Say which world we are in.
	if len(svc.Networks) == 0 {
		t.Fatal("temporal declares NO networks. If segmentation was removed deliberately, delete this " +
			"guard deliberately too; do not leave it asserting a property the file no longer expresses.")
	}

	if len(svc.Networks) != 1 {
		t.Errorf("temporal is on %d networks (%s), but temporalio/auto-setup defaults BIND_ON_IP to "+
			"`hostname -i`, which is AMBIGUOUS when multi-homed. Measured 2026-08-05: it resolved to the "+
			"host's own LAN address, temporal bound nothing usable, ringpop failed to join, the worker got "+
			"`connection refused` on the correct backplane address, and both containers Exited(1) — the "+
			"control plane was down. If temporal must serve a published port, set BIND_ON_IP explicitly "+
			"to a fixed address; do not simply add a second network.",
			len(svc.Networks), strings.Join(svc.Networks, ", "))
	}

	// The published port is the reason the second network was added in the first place, so it is part of
	// the same invariant: re-adding it invites re-adding the network.
	for _, p := range svc.Ports {
		if strings.Contains(p, "7233") {
			t.Errorf("temporal publishes %q again. Publishing is what motivated the second network that "+
				"caused the outage, because an internal-only container cannot serve a published port. "+
				"Nothing on the host had ever connected to it (measured with `ss -tnp`), and the operator "+
				"recipe uses `docker exec … --address <container-ip>:7233`. If it is genuinely needed, "+
				"pin BIND_ON_IP first and record why here.", p)
		}
	}
}
