package deploy

// EVERY WORKER ADMIN SURFACE IS SCRAPED, OR THE SAFETY SERIES DESCRIBE PART OF THE FLEET.
//
// The worker binary publishes its safety and egress series on an admin port set by TG_WORKER_ADMIN_ADDR.
// The stack runs that binary twice: `worker` on :8444 (triage plane) and `worker-actuate` on :8445
// (actuation plane). Only :8444 was in the Prometheus scrape config.
//
// So every question asked of these metrics was silently scoped to the triage plane. TG-324 gates flipping
// the egress meter to enforce on "tg_egress_offallowlist_requests_total flat at 0 for a full production
// cycle" — and measured 2026-08-05 that series read 0 with tg_egress_allowlist_rules at 33, which looks
// like a satisfied precondition. It was not: the actuation worker was not scraped, was not even running an
// image that contained the meter, and is the plane that actually mutates the estate. A zero measured over
// half the fleet is not a zero, and the half it skipped is the dangerous half.
//
// This guard ties the two files together: a service that exposes an admin port must have a scrape target
// pointing at that exact service and port. Adding a third worker without a target fails here rather than
// eight weeks later when someone reads a clean graph and believes it.

import (
	"fmt"
	"os"
	"regexp"
	"sort"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// adminSurfaces returns service name -> admin port, for every compose service that sets
// TG_WORKER_ADMIN_ADDR. The value is of the form ":8444" or "0.0.0.0:8444".
func adminSurfaces(t *testing.T, path string) map[string]string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var doc struct {
		Services map[string]struct {
			Environment map[string]string `yaml:"environment"`
		} `yaml:"services"`
	}
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	out := map[string]string{}
	for name, svc := range doc.Services {
		addr, ok := svc.Environment["TG_WORKER_ADMIN_ADDR"]
		if !ok || strings.TrimSpace(addr) == "" {
			continue
		}
		// Strip any ${VAR:-default} wrapper down to the port that is actually bound.
		if m := regexp.MustCompile(`:(\d+)`).FindStringSubmatch(addr); m != nil {
			out[name] = m[1]
		}
	}
	return out
}

func scrapeTargets(t *testing.T, path string) map[string]bool {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var doc struct {
		ScrapeConfigs []struct {
			JobName       string `yaml:"job_name"`
			StaticConfigs []struct {
				Targets []string `yaml:"targets"`
			} `yaml:"static_configs"`
		} `yaml:"scrape_configs"`
	}
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	out := map[string]bool{}
	for _, sc := range doc.ScrapeConfigs {
		for _, st := range sc.StaticConfigs {
			for _, tgt := range st.Targets {
				out[strings.TrimSpace(tgt)] = true
			}
		}
	}
	return out
}

func TestEveryWorkerAdminSurfaceHasAScrapeTarget(t *testing.T) {
	surfaces := adminSurfaces(t, "docker-compose.yml")
	if len(surfaces) == 0 {
		t.Fatal("vacuity floor: no compose service sets TG_WORKER_ADMIN_ADDR, so this guard compared " +
			"nothing. If the admin surface was renamed, re-derive this test rather than letting it pass quietly.")
	}
	targets := scrapeTargets(t, "monitoring/prometheus.yml")
	if len(targets) == 0 {
		t.Fatal("vacuity floor: the Prometheus config declares no static targets at all")
	}

	var missing []string
	for svc, port := range surfaces {
		if !targets[fmt.Sprintf("%s:%s", svc, port)] {
			missing = append(missing, fmt.Sprintf("%s:%s", svc, port))
		}
	}
	sort.Strings(missing)
	for _, m := range missing {
		t.Errorf("compose service exposes an admin surface at %q but Prometheus scrapes no such target.\n"+
			"Its safety and egress series will be absent, and every query over them will quietly describe "+
			"only the workers that ARE scraped — which is how the actuation plane went unobserved while "+
			"tg_egress_offallowlist_requests_total read a reassuring 0.", m)
	}

	// The reverse direction matters too: a target for a service that no longer exposes an admin port is a
	// permanently-down scrape, and MetricsTargetDown fires on `up == 0`. A gate nobody can keep green is a
	// gate that gets muted, taking the real signal with it.
	svcPorts := map[string]bool{}
	for svc, port := range surfaces {
		svcPorts[fmt.Sprintf("%s:%s", svc, port)] = true
	}
	for tgt := range targets {
		host := strings.SplitN(tgt, ":", 2)[0]
		if host == "localhost" || host == "grounder" {
			continue // the grounder publishes on its public port, not a worker admin surface
		}
		if strings.HasPrefix(host, "worker") && !svcPorts[tgt] {
			t.Errorf("Prometheus scrapes %q but no compose service exposes that admin surface — this "+
				"target can never come up, and MetricsTargetDown will fire on it forever", tgt)
		}
	}
}
