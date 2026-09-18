package main

import (
	"os"
	"strings"
	"testing"
)

// TG-315 — THE COMPOSITION-ROOT GUARD, and it is not optional here.
//
// This whole ticket exists because a capability was built and never reached: the authlog parser merged
// in !1022, its ingest route, its `sources` row, its OpenBao bearer token and its never-delivered gauge
// were all live, and NOTHING READ THE LOGS. `ingest_alert` carried 3,167 rows across three availability
// sources and zero rows with category=security-incident.
//
// Shipping a collector that main.go never calls would reproduce that exact defect one layer up — present
// in the tree, absent from the process — which is why this reads main.go rather than trusting that a
// function's existence means it runs.

func authlogMainSource(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatalf("read main.go: %v", err)
	}
	return string(b)
}

func TestTheAuthlogCollectorIsStartedAtTheCompositionRoot(t *testing.T) {
	src := authlogMainSource(t)
	if !strings.Contains(src, "startAuthlogCollector(") {
		t.Fatal("cmd/worker/main.go never calls startAuthlogCollector — the collector would be defined and " +
			"unwired, which is the SAME defect this ticket was filed on (a parser, a route, a token and a " +
			"gauge, all live, with nothing reading the logs)")
	}
}

// The register must be chained too. A collector that runs and publishes nothing is only half-wired, and
// the half that is missing is the half that would tell anyone.
func TestTheAuthlogYieldRegisterIsChainedToTheMetricsSurface(t *testing.T) {
	src := authlogMainSource(t)
	if !strings.Contains(src, "withAuthlogYield(") {
		t.Fatal("main.go never chains withAuthlogYield — the collector's offered-vs-produced register " +
			"would exist and publish nothing, so 'not configured', 'configured but dead' and 'quiet " +
			"estate' would remain one observation")
	}
}

// It must be given the REAL transport, not a fresh one. A second runner would carry its own host-key
// posture, and the syslog-ng package's verification is mandatory and fails closed for a reason.
func TestTheCollectorReusesTheSyslogRunnerRatherThanBuildingASecond(t *testing.T) {
	src := authlogMainSource(t)
	if !strings.Contains(src, "authlogServers, authlogRunner = sgServers, sgRunner") {
		t.Error("the collector does not reuse the syslog-ng servers/runner the investigation tools use — " +
			"a second transport would carry its own host-key posture, and this package's verification is " +
			"mandatory and fail-closed by design")
	}
	if strings.Contains(src, "startAuthlogCollector(planeEnv, nil,") {
		t.Error("the collector is started with no servers — it would refuse on every boot")
	}
}

// PLANE-SCOPED: it must read through planeEnv, never getenv. getenv would let the ACTUATION worker arm a
// lane that mints triage sessions from untrusted device-log content.
func TestTheCollectorIsStartedWithThePlaneScopedEnvReader(t *testing.T) {
	src := authlogMainSource(t)
	i := strings.Index(src, "startAuthlogCollector(")
	if i < 0 {
		t.Skip("covered by TestTheAuthlogCollectorIsStartedAtTheCompositionRoot")
	}
	call := src[i : i+len("startAuthlogCollector(planeEnv,")]
	if !strings.Contains(call, "planeEnv") {
		t.Errorf("startAuthlogCollector is not given planeEnv (call begins %q) — reading through getenv "+
			"would let the actuation plane arm a triage-minting lane (TG-153)", call)
	}
}
