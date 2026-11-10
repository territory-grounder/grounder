package main

import (
	"os"
	"strings"
	"testing"
)

// Composition-root guard (TG-501): pin that wireLibrenmsAlertPoll still wires the opt-in LibreNMS
// active-alert pull — the plane-scoped interval gate, the deployment resolver, the MIN_AGE safety-net
// parse, the alert source construction, the upstream-probe hand-off, and the dedup-minting ticker loop —
// so the god-file carve that extracted it from main() cannot silently drop a piece. It returns nothing
// observable from outside the package besides the probe-source hand-off (a fire-and-forget background
// loop gated on an interval), so — the same reasoning worker_wiring_inventory_test.go and
// worker_model_budget_test.go rely on — the guard reads the source as text and asserts the wiring, rather
// than exercising a live LibreNMS poll.
func TestWireLibrenmsAlertPollWiresTheActiveAlertPull(t *testing.T) {
	src, err := os.ReadFile("librenms_alert_poll_wiring.go")
	if err != nil {
		t.Fatalf("read librenms_alert_poll_wiring.go: %v", err)
	}
	s := string(src)
	for _, want := range []string{
		`planeEnv("TG_LIBRENMS_ALERT_POLL_INTERVAL", "")`,
		`alertDeps := librenmsDeployments(planeEnv("TG_LIBRENMS_DEPLOYMENTS_AGENT_TOOLS", ""))`,
		`alertSrc := librenms.NewAlertSource(alertDeps, librenms.WithAlertHTTPClient(estateHTTPClient(truthyEnv("TG_LIBRENMS_INSECURE"))), librenms.WithAlertMinAge(minAge))`,
		`upstreamProbeSource = alertSrc`,
		`c.ExecuteWorkflow(ctx, client.StartWorkflowOptions{`,
		`WorkflowIDReusePolicy: enumspb.WORKFLOW_ID_REUSE_POLICY_REJECT_DUPLICATE,`,
	} {
		if !strings.Contains(s, want) {
			t.Errorf("wireLibrenmsAlertPoll no longer wires %q — a LibreNMS alert-poll piece was dropped in the carve", want)
		}
	}
}

// main() must actually CALL the extracted constructor — a carve that leaves it uncalled is a dark seam
// (present in the tree, absent from the process; the same class TG-315's authlog collector shipped as).
func TestMainCallsWireLibrenmsAlertPoll(t *testing.T) {
	mainSrc, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatalf("read main.go: %v", err)
	}
	if !strings.Contains(string(mainSrc), "upstreamProbeSource = wireLibrenmsAlertPoll(c)") {
		t.Error("main.go no longer calls upstreamProbeSource = wireLibrenmsAlertPoll(c) — the extracted LibreNMS alert-poll wiring is unreferenced")
	}
}
