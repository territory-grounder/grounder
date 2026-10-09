package deploy

import (
	"os/exec"
	"strings"
	"testing"
)

// TestConsoleAuthGateRuntime exercises the REAL nginx auth_request / error_page / header-inheritance
// semantics against a stub grounder (console/itest/authgate.sh), so the behavioural contract the static
// config oracles in console_api_proxy_test.go cannot check — an unauthenticated request leaks NOTHING
// (login page only, never the bundle/fixtures/endpoint names), an authenticated request gets the app, a
// grounder blip fails to the login page (not a bare 500), and hardened headers ship on every response — is
// a falsifiable artifact IN THE REPO rather than an uncommitted citation.
//
// It SKIPS (never fails) where docker is unavailable or under -short: the CI go-test image has no
// docker-in-docker, so this runs locally and on any docker-capable runner. A logged SKIP is visible; the
// failure mode this test exists to prevent is a behavioural claim that no committed artifact backs, so it
// must never pass vacuously — hence the explicit "0 failed" assertion on the harness output, not just its
// exit code.
func TestConsoleAuthGateRuntime(t *testing.T) {
	if testing.Short() {
		t.Skip("-short: skipping the docker-backed console auth-gate runtime test")
	}
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker not on PATH — the console auth-gate runtime contract is unverified in this environment")
	}
	if err := exec.Command("docker", "info").Run(); err != nil {
		t.Skip("docker present but not usable — the console auth-gate runtime contract is unverified here")
	}
	out, err := exec.Command("bash", "console/itest/authgate.sh").CombinedOutput()
	t.Logf("console/itest/authgate.sh:\n%s", out)
	if err != nil {
		t.Fatalf("the console auth-gate does not behave as the config claims — harness failed: %v (see FAIL lines above)", err)
	}
	if !strings.Contains(string(out), "0 failed") {
		t.Fatalf("the auth-gate harness did not report all-pass:\n%s", out)
	}
}
