package deploy

// COMPOSE AND HELM MUST AGREE ON WHAT A SERVICE MAY CONSUME (TG-170).
//
// The helm chart has carried per-service resource limits and probes since spec/009. Compose had NEITHER,
// so on the DEPLOYED stack — which is compose, not helm — an OOM or a runaway was unbounded and a wedged
// process sat "Up" forever.
//
// The limits are mirrored from helm rather than invented, so the two deployment paths cannot drift into
// disagreeing about the same service. This guard is what makes "mirrored" true tomorrow as well as today.
//
// ONE NUMBER IS LOAD-BEARING AND IS NOT UNIFORM: litellm gets 1G where the Go services get 512M. Measured
// 2026-08-05, litellm holds 608 MiB resident — 512M would OOM-kill it, and TG runs single-brain through
// that process (TG-293), so an OOM there fails triage entirely rather than degrading it. A future
// "tidy-up" that harmonises all four services onto one number is the specific regression this catches.

import (
	"os"
	"regexp"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

type limitParitySvc struct {
	Deploy struct {
		Resources struct {
			Limits map[string]string `yaml:"limits"`
		} `yaml:"resources"`
	} `yaml:"deploy"`
	Healthcheck struct {
		Test []string `yaml:"test"`
	} `yaml:"healthcheck"`
}

func limitParityServices(t *testing.T) map[string]limitParitySvc {
	t.Helper()
	raw, err := os.ReadFile("docker-compose.yml")
	if err != nil {
		t.Fatalf("read docker-compose.yml: %v", err)
	}
	var doc struct {
		Services map[string]limitParitySvc `yaml:"services"`
	}
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("parse docker-compose.yml: %v", err)
	}
	if len(doc.Services) == 0 {
		t.Fatal("vacuity floor: docker-compose.yml declares no services, so every check below would pass " +
			"by inspecting nothing")
	}
	return doc.Services
}

// Every long-running application service carries a memory limit AND a healthcheck. Both halves matter and
// they fail differently: without the limit a runaway is unbounded, without the probe a wedged process is
// undetected. The pre-TG-170 state had neither.
func TestApplicationServicesAreBoundedAndProbed(t *testing.T) {
	svcs := limitParityServices(t)
	for _, name := range []string{"grounder", "worker", "worker-actuate", "litellm"} {
		sv, ok := svcs[name]
		if !ok {
			t.Errorf("service %q is gone from compose — if it was renamed, re-derive this guard rather "+
				"than letting it silently stop checking a service that still runs", name)
			continue
		}
		if sv.Deploy.Resources.Limits["memory"] == "" {
			t.Errorf("service %q has no memory limit. On the deployed stack an OOM or a runaway is then "+
				"unbounded — and worker-actuate in particular is the plane that mutates the estate.", name)
		}
		if len(sv.Healthcheck.Test) == 0 {
			t.Errorf("service %q has no healthcheck, so a wedged process reports Up forever and nothing "+
				"restarts it", name)
		}
	}
}

// litellm's limit must stay ABOVE the Go services'. This is the measurement, pinned.
func TestLitellmKeepsItsLargerLimit(t *testing.T) {
	svcs := limitParityServices(t)
	lite := svcs["litellm"].Deploy.Resources.Limits["memory"]
	worker := svcs["worker"].Deploy.Resources.Limits["memory"]
	if lite == "" || worker == "" {
		t.Fatal("a limit is missing; the comparison below would be vacuous")
	}
	if mib(t, lite) <= mib(t, worker) {
		t.Errorf("litellm memory limit (%s) is not above the worker's (%s).\n"+
			"litellm held 608 MiB resident when measured, so the Go services' limit would OOM-kill it — "+
			"and TG runs single-brain through that process, so the failure is total rather than partial. "+
			"Harmonising these onto one number is the regression this test exists to catch.", lite, worker)
	}
	// 2G floor, and the number is a scar. The first version of this shipped a 1G limit sized from a single
	// 608 MiB measurement taken while the estate was IDLE; it OOM-killed litellm on the first deploy
	// (exit 137) and left TG with no model gateway. Uncapped it settles at ~1022 MiB at rest — above the
	// limit chosen from the idle sample. An idle measurement is not a peak, and this floor stops the next
	// person sizing it from one.
	if mib(t, lite) < 2048 {
		t.Errorf("litellm memory limit is %s — under 2G.\n"+
			"A 1G limit sized from an idle 608 MiB sample OOM-killed this container on its first deploy; "+
			"uncapped it rests at ~1022 MiB. TG runs single-brain through it (TG-293, no fallbacks), so an "+
			"OOM here stops triage rather than degrading it.", lite)
	}
}

// The distroless services must probe via the BINARY, not a shell. `docker exec ... /bin/sh` fails on these
// images, so a CMD-SHELL healthcheck would mark them unhealthy forever — which is worse than no probe,
// because it makes every container look broken and trains an operator to ignore the column.
func TestDistrolessServicesProbeWithoutAShell(t *testing.T) {
	svcs := limitParityServices(t)
	for _, name := range []string{"grounder", "worker", "worker-actuate"} {
		test := svcs[name].Healthcheck.Test
		if len(test) == 0 {
			continue // reported by the test above
		}
		if test[0] == "CMD-SHELL" {
			t.Errorf("service %q uses a CMD-SHELL healthcheck, but its image is distroless and has no "+
				"shell. The probe cannot run, so the container reports unhealthy forever.", name)
		}
		joined := strings.Join(test, " ")
		for _, forbidden := range []string{"curl", "wget", "/bin/sh", "/bin/bash"} {
			if strings.Contains(joined, forbidden) {
				t.Errorf("service %q healthcheck invokes %q, which is not present in a distroless image",
					name, forbidden)
			}
		}
	}
}

func mib(t *testing.T, v string) int {
	t.Helper()
	m := regexp.MustCompile(`^(\d+)([MG])$`).FindStringSubmatch(strings.TrimSpace(v))
	if m == nil {
		t.Fatalf("unparseable memory value %q", v)
	}
	n := 0
	for _, c := range m[1] {
		n = n*10 + int(c-'0')
	}
	if m[2] == "G" {
		n *= 1024
	}
	return n
}
