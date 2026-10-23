package deploy

// THE REBOOT-SURVIVAL GUARD (TG-565).
//
// 2026-09-16 the host rebooted and TG stayed DOWN for 57h with nothing noticing: no long-running compose
// service carried a restart policy, and nothing ran `docker compose up` at boot, so the one-shot inits that
// write the /dev/shm secret drops never re-ran either (the sidecar — the one service that did restart —
// crash-looped on its missing env file for two days). Both halves of the cure are pinned here:
//
//   - every LONG-RUNNING service is `restart: unless-stopped`, every ONE-SHOT init is `restart: "no"` — and
//     which services are inits is DERIVED from their dependents' `service_completed_successfully`
//     conditions, not listed, so a service added later is classified without anyone remembering this file;
//   - deploy/host/tg-stack.service runs `docker compose up -d` in the deploy dir at boot, retrying.
//
// WHAT IT CANNOT DO: prove the unit is INSTALLED and enabled on the box (the deploy playbook does that, and
// the TG-565 reboot drill proved it live) — this is the source-of-truth half.

import (
	"os"
	"sort"
	"strings"
	"testing"
)

const (
	bootUnitPath = "host/tg-stack.service"
	// restartPolicyServiceFloor is a vacuity floor: the stack has 18 services; a parse that sees far fewer is
	// reading the wrong document, and "every service is fine" over three services proves nothing.
	restartPolicyServiceFloor = 15
)

// composeOneShotInits returns the services some other service gates on `service_completed_successfully` —
// the one-shot inits. Short-syntax depends_on (a plain list) carries no condition and marks nothing.
func composeOneShotInits(services map[string]map[string]any) map[string]bool {
	inits := map[string]bool{}
	for _, svc := range services {
		deps, ok := svc["depends_on"].(map[string]any)
		if !ok {
			continue
		}
		for dep, spec := range deps {
			if m, ok := spec.(map[string]any); ok && m["condition"] == "service_completed_successfully" {
				inits[dep] = true
			}
		}
	}
	return inits
}

func TestComposeRestartPolicyLetsTheStackSurviveAReboot(t *testing.T) {
	services := composeServices(t)
	if len(services) < restartPolicyServiceFloor {
		t.Fatalf("docker-compose.yml parsed to %d services, below the floor of %d — the guard is not reading "+
			"the deployment and must not report a pass", len(services), restartPolicyServiceFloor)
	}
	inits := composeOneShotInits(services)
	if len(inits) == 0 {
		t.Fatal("no service is gated by service_completed_successfully, so no one-shot init was derived — the " +
			"secret-drop inits exist, so the derivation is blind (or the gating was removed, and litellm/the " +
			"sidecar would start without their secrets)")
	}
	names := make([]string, 0, len(services))
	for name := range services {
		names = append(names, name)
	}
	sort.Strings(names)
	long, oneShot := 0, 0
	for _, name := range names {
		got, _ := services[name]["restart"].(string)
		if inits[name] {
			oneShot++
			if got != "no" {
				t.Errorf("one-shot init %q has restart %q, want \"no\" — a restarting init rewrites its secrets in "+
					"a loop; compose re-runs it on every `up` (tg-stack.service does that at boot)", name, got)
			}
			continue
		}
		long++
		if got != "unless-stopped" {
			t.Errorf("long-running service %q has restart %q, want \"unless-stopped\" — without it dockerd leaves it "+
				"DOWN after a crash or a host reboot (the 2026-09-16 57h outage, TG-565)", name, got)
		}
	}
	t.Logf("restart policy: %d long-running `unless-stopped` + %d one-shot inits `\"no\"` of %d services",
		long, oneShot, len(services))
}

// bootUnitDirectives parses the unit into section → key → values, ignoring comment and blank lines, so a
// directive mentioned only in prose cannot satisfy the guard.
func bootUnitDirectives(t *testing.T) map[string]map[string][]string {
	t.Helper()
	raw, err := os.ReadFile(bootUnitPath)
	if err != nil {
		t.Fatalf("read %s: %v — the boot unit is GONE, and with it the only thing that runs `docker compose up` "+
			"after a reboot (the secret-drop inits never re-run, litellm and the sidecar crash-loop)", bootUnitPath, err)
	}
	out := map[string]map[string][]string{}
	section := ""
	for line := range strings.SplitSeq(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, ";") {
			continue
		}
		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
			section = strings.Trim(line, "[]")
			if out[section] == nil {
				out[section] = map[string][]string{}
			}
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok || section == "" {
			t.Fatalf("%s: unparseable line %q", bootUnitPath, line)
		}
		out[section][strings.TrimSpace(k)] = append(out[section][strings.TrimSpace(k)], strings.TrimSpace(v))
	}
	return out
}

func TestBootUnitBringsTheStackUpAtBoot(t *testing.T) {
	d := bootUnitDirectives(t)
	want := func(section, key, value string) {
		t.Helper()
		for _, v := range d[section][key] {
			for f := range strings.FieldsSeq(v) {
				if f == value {
					return
				}
			}
		}
		t.Errorf("%s: [%s] %s must include %q (have %q)", bootUnitPath, section, key, value, d[section][key])
	}
	want("Unit", "Requires", "docker.service")
	want("Unit", "After", "docker.service")
	want("Unit", "StartLimitIntervalSec", "0") // OpenBao may boot slower: retry until the stack converges
	want("Service", "Type", "oneshot")
	want("Service", "RemainAfterExit", "yes")
	want("Service", "Restart", "on-failure")
	want("Install", "WantedBy", "multi-user.target")

	wd := d["Service"]["WorkingDirectory"]
	if len(wd) != 1 || !strings.HasPrefix(wd[0], "/") {
		t.Errorf("%s: [Service] WorkingDirectory must be the absolute deploy dir, have %q", bootUnitPath, wd)
	}
	exec := d["Service"]["ExecStart"]
	if len(exec) != 1 {
		t.Fatalf("%s: want exactly one ExecStart, have %d (%q)", bootUnitPath, len(exec), exec)
	}
	f := strings.Fields(exec[0])
	if len(f) < 2 || !strings.HasSuffix(f[0], "/docker") || f[1] != "compose" {
		t.Fatalf("%s: ExecStart must run `docker compose`, have %q", bootUnitPath, exec[0])
	}
	has := map[string]bool{}
	composeFile := ""
	for i, a := range f {
		has[a] = true
		if a == "-f" && i+1 < len(f) {
			composeFile = f[i+1]
		}
	}
	if !has["up"] || !has["-d"] {
		t.Errorf("%s: ExecStart must be `docker compose ... up -d`, have %q", bootUnitPath, exec[0])
	}
	for _, bad := range []string{"down", "stop", "rm", "--remove-orphans", "--force-recreate", "pull"} {
		if has[bad] {
			t.Errorf("%s: ExecStart carries %q — a boot must only bring the existing stack up, have %q", bootUnitPath, bad, exec[0])
		}
	}
	// The compose file the unit names must be THIS deploy's compose file (the unit runs in the deploy dir the
	// playbook copies deploy/ into, so the relative name must exist here).
	if composeFile == "" {
		t.Errorf("%s: ExecStart names no compose file (-f)", bootUnitPath)
	} else if _, err := os.Stat(composeFile); err != nil {
		t.Errorf("%s: ExecStart's compose file %q does not exist in deploy/: %v", bootUnitPath, composeFile, err)
	}
}
