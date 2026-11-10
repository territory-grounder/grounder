package agent

import (
	"encoding/json"
	"strings"
)

// HostReadGate holds a STOP on a service alert until the agent has read the alerting host's own service
// state (TG-638). Measured in the TG-624 G3 soak on 2026-11-06: of 9 Service up/down sessions, every one that
// healed had called check-host-services, and every one that stood down had not. They read only LibreNMS
// ("device UP, no adverse events") and concluded "stale" while a container was stopped or paused on the host.
// A device that is up says nothing about the service on it.
//
// The gate does NOT run the tool itself: it nudges ONCE with the exact call, and the agent makes it through
// the normal tool path (allowlist, read-only check, trajectory veto, recon budget). A second stop is
// accepted. A stop is the fail-safe end state and must never be blocked into grinding on, exactly as the
// REQ-1008 grounded-stop nudge. The gate's tool and host come from the RUNNER (the incident), never from
// model text (INV-08).
type HostReadGate struct {
	Tool string // the host-services read, e.g. check-host-services
	Host string // the alerting host, from the incident envelope
}

// pending reports whether a stop should be held for the host read: the gate is configured, its tool is
// actually in the session's tool set, and no result from that tool has been gathered yet.
func (g *HostReadGate) pending(tools *ToolSet, gathered []ToolResult) bool {
	if g == nil || strings.TrimSpace(g.Tool) == "" || strings.TrimSpace(g.Host) == "" || tools == nil {
		return false
	}
	if _, ok := tools.Get(g.Tool); !ok {
		return false
	}
	for _, r := range gathered {
		if r.Tool == g.Tool {
			return false
		}
	}
	return true
}

// call renders the exact host-services call with json.Marshal, so a host name can never break out of the
// JSON it is embedded in.
func (g *HostReadGate) call() string {
	c, _ := json.Marshal(map[string]any{"action": "tool", "tool": g.Tool, "args": map[string]string{"host": g.Host}})
	return string(c)
}

// handoffNudge is the one-time instruction appended when a service-alert session reaches the hand-off poll
// limit without the host read (TG-638's second shape, seen twice in the G3 soak on 2026-11-06: the agent kept
// calling monitoring tools until the limit and never stopped, so the stop hold never fired). It takes the
// place of the TG-60 decide-now nudge for ONE cycle; that nudge follows on the next cycle as before.
func (g *HostReadGate) handoffNudge() string {
	return "HOST READ REQUIRED before you decide: this is a SERVICE alert and you have not read the host's own " +
		"service state. A device that is UP can still have a stopped, paused or frozen service, and the monitoring " +
		"data you gathered cannot show it. Make exactly this call next: " + g.call() +
		" — after it you will be asked to decide."
}

// nudge is the one-time instruction appended when a stop is held.
func (g *HostReadGate) nudge() string {
	return "STOP NOT YET ACCEPTED: this is a SERVICE alert and you have not read the host's own service state. " +
		"A device that is UP can still have a stopped, paused or frozen service, and the monitoring device status " +
		"cannot show it. Read it first: " + g.call() + " — then decide (propose, or stop citing what you observed)."
}
