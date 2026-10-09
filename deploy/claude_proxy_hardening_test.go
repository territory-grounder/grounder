package deploy

// THE SIDECAR HOST IS PART OF TG, AND HAD NONE OF ITS HARDENING (TG-288).
//
// deploy/claude-proxy/compose.yml is a SECOND compose file on a SECOND host, and it escaped every control
// the tg01 stack acquired. Measured 2026-08-04: `network_mode: host`, no cap_drop, no security_opt, on the
// box that holds an OpenBao ROOT token and runs a CLI executing model-directed work.
//
// `network_mode: host` was never needed — the service binds ONE port. It put the container on the same
// loopback as kubectl port-forwards (19090/19091), an ssh-forwarded postgres (15432), and :4000/:8099/
// :4010/:8201-8204/:8384, all of which assume loopback is private. It also made `LISTEN_ADDR 0.0.0.0:8094`
// bind every interface while main.rs still claimed "the listener is host-local" — which is why
// /probe-auth and /metrics were left unauthenticated (TG-279).
//
// TG-289 put a parity test on the tg01 compose for exactly this reason: hardening that nothing asserts
// regresses silently, and did. This is the same guard for the file TG-289 could not see.

import (
	"fmt"
	"os"
	"testing"

	"gopkg.in/yaml.v3"
)

func sidecarService(t *testing.T) map[string]any {
	t.Helper()
	raw, err := os.ReadFile("claude-proxy/compose.yml")
	if err != nil {
		t.Fatalf("read sidecar compose: %v", err)
	}
	var doc map[string]any
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("parse sidecar compose: %v", err)
	}
	svcs, _ := doc["services"].(map[string]any)
	s, ok := svcs["sidecar"].(map[string]any)
	if !ok {
		t.Fatalf("the sidecar service is absent — this guard is scanning a file that no longer describes " +
			"the deployment, and would pass by checking nothing")
	}
	return s
}

// KILLING MUTATION: restore `network_mode: host` (the shipped state). RED.
func TestTheSidecarDoesNotShareTheHostNetworkNamespace(t *testing.T) {
	s := sidecarService(t)
	if nm := fmt.Sprint(s["network_mode"]); nm == "host" {
		t.Fatal("sidecar is back on network_mode: host. It binds ONE port and does not need the host " +
			"netns — sharing it puts a container that runs model-directed work onto the same loopback as " +
			"kubectl port-forwards, an ssh-forwarded postgres and eight other services that assume " +
			"loopback is private, on the box holding an OpenBao root token.")
	}
	ports, ok := s["ports"].([]any)
	if !ok || len(ports) == 0 {
		t.Fatal("no published port — dropping host networking without publishing 8094 makes the sidecar " +
			"unreachable and takes the entire model gateway down")
	}
}

// KILLING MUTATION: delete cap_drop or security_opt. RED.
func TestTheSidecarDropsCapabilitiesAndPrivilegeEscalation(t *testing.T) {
	s := sidecarService(t)
	caps, _ := s["cap_drop"].([]any)
	if len(caps) == 0 || fmt.Sprint(caps[0]) != "ALL" {
		t.Errorf("cap_drop is %v, want [ALL] — this container spawns a CLI that executes model-directed "+
			"work on the host holding an OpenBao root token", caps)
	}
	opt, _ := s["security_opt"].([]any)
	found := false
	for _, o := range opt {
		if fmt.Sprint(o) == "no-new-privileges:true" {
			found = true
		}
	}
	if !found {
		t.Errorf("security_opt is %v, want no-new-privileges:true", opt)
	}
}

// The service must keep running as a non-root uid; dropping that would undo the rest.
func TestTheSidecarStillRunsAsNonRoot(t *testing.T) {
	s := sidecarService(t)
	u := fmt.Sprint(s["user"])
	if u == "" || u == "<nil>" || u == "0:0" || u == "root" {
		t.Fatalf("sidecar user is %q — it must not run as root", u)
	}
}

// VACUITY FLOOR: if the file is reshaped and the checks above start reading an empty map, they pass by
// asserting nothing. Pin the fields they depend on being present at all.
func TestTheSidecarScanIsReadingARealService(t *testing.T) {
	s := sidecarService(t)
	for _, k := range []string{"image", "container_name", "environment"} {
		if _, ok := s[k]; !ok {
			t.Fatalf("sidecar service has no %q key — the hardening assertions above are examining a "+
				"structure that is not the service, and would pass on anything", k)
		}
	}
}
