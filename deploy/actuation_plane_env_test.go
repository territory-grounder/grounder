package deploy

// THE ACTUATION PLANE NEEDS ITS OWN VARIABLE NAMESPACE (TG-153).
//
// THE DEFECT, found by deploying the split on a real single host rather than by reading the diff. Both
// worker services interpolate from the SAME .env. Splitting requires removing the actuation refs from the
// triage worker's environment — the boot check refuses to start otherwise, correctly — but the actuation
// service interpolated the SAME variable names, so that removal emptied its configuration too. The
// actuation worker came up holding nothing.
//
// The split was therefore undeployable on one host, which is the shape most installations have. It would
// have looked fine in review: every file was correct in isolation.
//
// The fix is the convention that block already used for TG_ACTUATE_OPENBAO_*: two planes are two
// identities and two configurations that happen to share a file, so they need distinct names. The triage
// worker then cannot see a value that does not exist — a stronger guarantee than asking it not to look.

import (
	"fmt"
	"os"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func serviceEnv(t *testing.T, svc string) map[string]string {
	t.Helper()
	raw, err := os.ReadFile("docker-compose.yml")
	if err != nil {
		t.Fatalf("read compose: %v", err)
	}
	var doc map[string]any
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("parse compose: %v", err)
	}
	services, _ := doc["services"].(map[string]any)
	s, ok := services[svc].(map[string]any)
	if !ok {
		t.Fatalf("service %q absent — this guard is scanning a file that no longer describes the deployment", svc)
	}
	env, ok := s["environment"].(map[string]any)
	if !ok {
		t.Fatalf("service %q has no map-shaped environment block (%T)", svc, s["environment"])
	}
	out := map[string]string{}
	for k, v := range env {
		out[k] = fmt.Sprint(v)
	}
	return out
}

// The credentials that MUTATE the estate. If the actuation service interpolates any of these from the
// unprefixed name, it collides with the triage worker's environment and the split cannot be deployed.
var actuationPlaneVars = []string{
	"TG_ACTUATION_SSH_KEY", "TG_PROXMOX_TOKEN_REF", "TG_AWXJOB_LAUNCH_TOKEN_REF",
	"TG_ACTUATION_ALLOWED_UNITS", "TG_ACTUATION_ALLOWED_CONTAINERS", "TG_PROXMOX_ALLOWED_GUESTS",
}

// KILLING MUTATION: point any of these back at the unprefixed variable (the state the split shipped in).
// RED — that is the configuration where satisfying the triage worker's boot check silently strips the
// actuation worker's credentials.
func TestTheActuationPlaneReadsItsOwnVariables(t *testing.T) {
	env := serviceEnv(t, "worker-actuate")
	checked := 0
	for _, v := range actuationPlaneVars {
		val, ok := env[v]
		if !ok {
			continue // not forwarded to this service at all — nothing to collide
		}
		checked++
		if !strings.Contains(val, "${TG_ACTUATE_") {
			t.Errorf("worker-actuate takes %s from %s, the SAME variable the triage worker reads.\n"+
				"Splitting the planes requires deleting that variable from .env (the triage worker refuses "+
				"to boot while an actuation ref is in its environment), which would empty this too — the "+
				"actuation worker would come up holding no credentials and the split would be undeployable "+
				"on a single host.", v, val)
		}
	}
	// VACUITY FLOOR. If the service is renamed or its env block reshaped, the loop above checks nothing
	// and passes — which is exactly how this class of defect survives review.
	if checked == 0 {
		t.Fatalf("none of the %d actuation-plane variables was found on worker-actuate — this test is "+
			"examining nothing and would pass on any configuration", len(actuationPlaneVars))
	}
}

// The mirror: the TRIAGE worker must keep reading the unprefixed names, or every existing single-worker
// deployment loses its configuration on upgrade.
func TestTheTriageWorkerStillReadsTheUnprefixedNames(t *testing.T) {
	env := serviceEnv(t, "worker")
	v, ok := env["TG_ACTUATION_SSH_KEY"]
	if !ok {
		t.Skip("the worker service no longer forwards TG_ACTUATION_SSH_KEY at all")
	}
	if strings.Contains(v, "TG_ACTUATE_") {
		t.Fatalf("the default worker now reads %s — an existing `both`-plane deployment would silently "+
			"lose its actuation configuration on upgrade", v)
	}
}
