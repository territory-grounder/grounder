package hostdiag

import (
	"strings"
	"testing"
)

// TG-624 G3: a SIGSTOPped unit is active to systemd and in no failed/inactive list. The agent read it as up
// and proposed start-service, a no-op. The process state is the only read that shows it.
func TestFrozenUnitsSummary_NamesAStoppedUnit(t *testing.T) {
	out := frozenUnitsSummary(map[string]string{svcProcStateLabel: strings.Join([]string{
		"      1 Ss   init.scope                      systemd",
		" 201059 Ts   nginx.service                   nginx",
		" 201060 T    nginx.service                   nginx",
		"    300 Ssl  docker.service                  dockerd",
		"    400 T    -                               bash",
		"    500 T    tg-restore-servicehang-9-1.service systemctl",
	}, "\n")})
	if !strings.Contains(out, "FROZEN") || !strings.Contains(out, "nginx.service: nginx (pid 201059, Ts), nginx (pid 201060, T)") {
		t.Fatalf("a unit whose processes are in state T must be named with its pids, got:\n%s", out)
	}
	if strings.Contains(out, "docker.service") || strings.Contains(out, "bash") || strings.Contains(out, "tg-restore") {
		t.Fatalf("running units, unit-less processes and harness units must not be named, got:\n%s", out)
	}
}

func TestFrozenUnitsSummary_SilentWhenNothingIsFrozenOrUnread(t *testing.T) {
	if s := frozenUnitsSummary(map[string]string{svcProcStateLabel: "  1 Ss init.scope systemd\n 99 Sl nginx.service nginx"}); s != "" {
		t.Fatalf("nothing frozen must say nothing, got %q", s)
	}
	if s := frozenUnitsSummary(map[string]string{}); s != "" {
		t.Fatalf("an unread step must say nothing (not an all-clear), got %q", s)
	}
}

// The process table is a QUIET step: the derived summary uses it, the agent does not get the raw listing, and
// the down-services summary composes the frozen line alongside its other families.
func TestDownServicesSummary_IncludesFrozenUnits(t *testing.T) {
	out := downServicesSummary(map[string]string{
		svcEnabledLabel:   "nginx.service enabled enabled",
		svcProcStateLabel: " 201059 Ts nginx.service nginx",
	})
	if !strings.Contains(out, "FROZEN") || !strings.Contains(out, "nginx.service") {
		t.Fatalf("the service summary must carry the frozen unit, got:\n%s", out)
	}
	if !quietSteps[svcProcStateLabel] {
		t.Fatal("the process table must be a quiet step: its raw listing would bury the summary")
	}
}

// The new read is a pinned wire command, so the read-only guard allowlist rendered from the catalogue
// carries it (and the rollout must ship the regenerated list).
func TestProcessStateReadIsAWireCommand(t *testing.T) {
	found := false
	for _, c := range ReadOnlyWireCommands() {
		if strings.Contains(c, "pid,stat,unit,comm") {
			found = true
		}
	}
	if !found {
		t.Fatal("the process-state read must be in the read-only wire command set (the guard allowlist)")
	}
}

// temporal/runner's TG-638 host-read gate names this check; a rename would silently disarm the gate.
func TestCheckHostServicesNameIsStable(t *testing.T) {
	for _, c := range checks {
		if c.name == "check-host-services" {
			return
		}
	}
	t.Fatal(`the host-services check must keep the name "check-host-services" (temporal/runner hostServicesTool)`)
}
