package runner

import (
	"strings"

	"github.com/territory-grounder/grounder/agent"
)

// hostServicesTool is the host's own service-state read (modules/observability/hostdiag check
// "check-host-services"; its name is pinned by TestCheckHostServicesNameIsStable there).
const hostServicesTool = "check-host-services"

// hostReadGateFor arms the TG-638 stop gate for a SERVICE alert: the agent may not stand down on monitoring
// data alone before it has read the alerting host's service state. The rule family and the host come from the
// incident envelope, never from model text. Any other alert, an empty host, or a session whose tool set lacks
// the read returns nil (the loop is then unchanged; the gate's own pending check repeats the tool-set test).
func hostReadGateFor(alertRule, host string, tools *agent.ToolSet) *agent.HostReadGate {
	if !isServiceAlertRule(alertRule) || strings.TrimSpace(host) == "" || tools == nil {
		return nil
	}
	if _, ok := tools.Get(hostServicesTool); !ok {
		return nil
	}
	return &agent.HostReadGate{Tool: hostServicesTool, Host: host}
}

// isServiceAlertRule recognises the service up/down family by name (LibreNMS "Service up/down", normalised to
// "Service-up/down" at ingest). Deliberately narrow: device, disk and memory rules keep the loop unchanged.
func isServiceAlertRule(rule string) bool {
	return strings.Contains(strings.ToLower(rule), "service")
}

// unitTargetGateFor arms the TG-647 proposal gate on EVERY alert, not only service ones: the live case was a
// memory-pressure alert. A session whose tool set lacks the host read returns nil (the loop is then unchanged).
func unitTargetGateFor(tools *agent.ToolSet) *agent.UnitTargetGate {
	if tools == nil {
		return nil
	}
	if _, ok := tools.Get(hostServicesTool); !ok {
		return nil
	}
	return &agent.UnitTargetGate{Tool: hostServicesTool}
}
