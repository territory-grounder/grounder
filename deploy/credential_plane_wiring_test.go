package deploy

// THE CREDENTIAL-PLANE COMPOSE WIRING GUARD (TG-153).
//
// WHY THIS EXISTS — a defect found in review of TG-153's own first cut, not a hypothetical.
//
// TG-153 gives the worker a plane selector (TG_CREDENTIAL_PLANE = triage | actuation | both) and ships a
// runbook whose step 1 is "in .env set TG_CREDENTIAL_PLANE=triage". That step DID NOTHING. A compose
// `.env` file is interpolation-only: it substitutes ${VARS} while compose parses the YAML, and it is NOT
// injected into containers. A variable reaches a container only if the service names it under
// `environment:` (or pulls it via `env_file:`, which the worker service does not use).
//
// So an operator who followed the runbook exactly would get:
//   - the triage worker booting as `both`, because its plane declaration never arrived;
//   - every actuation variable still forwarded to it, because the worker's environment block hard-passes
//     TG_ACTUATION_SSH_KEY and friends;
//   - and BOTH workers polling tg.actuate, racing for the same estate-mutating tasks — the credential-less
//     one winning a share of them and failing closed.
//
// The boot log would have said `plane=both ... This is not a split`, which is honest, but the operator had
// already been told the split was applied. A split that is BELIEVED and not IN EFFECT is the worst of the
// three outcomes, because it is the one nobody goes back to check. That is the same failure shape as the
// pre-TG-153 worker printing "plane split OK" while co-holding both planes.
//
// WHAT THIS ASSERTS: the plumbing, not the value. The worker must be ABLE to receive a plane, and its
// default must be `both` so an un-opted-in stack is unchanged; the actuation worker must be pinned to
// `actuation` and must not be startable by a default `docker compose up`.

import (
	"strings"
	"testing"
)

// composeServices (hardening_parity_test.go) is reused deliberately: it already fails loudly on an
// unreadable or serviceless compose file, and yaml.v3's duplicate-key rejection is exactly the protection
// this guard wants — a pasted-over `worker` block must not silently discard the plane wiring below.

// envOf returns a service's environment block as a map. Compose accepts both the mapping form and the
// `- KEY=VALUE` list form; the worker uses the mapping form, and a silent switch to the list form is
// exactly the kind of drift that would make a naive lookup report "not set".
func envOf(t *testing.T, svc map[string]any, name string) map[string]string {
	t.Helper()
	out := map[string]string{}
	switch e := svc["environment"].(type) {
	case map[string]any:
		for k, v := range e {
			out[k], _ = v.(string)
		}
	case []any:
		for _, item := range e {
			s, _ := item.(string)
			if k, v, ok := strings.Cut(s, "="); ok {
				out[k] = v
			}
		}
	default:
		t.Fatalf("service %q has no readable environment block (%T) — the plane wiring cannot be checked", name, svc["environment"])
	}
	if len(out) == 0 {
		t.Fatalf("service %q has an EMPTY environment block — this guard would then pass while the plane variable reached nothing", name)
	}
	return out
}

// TestWorkerServiceCanReceiveTheCredentialPlane is the guard for the defect described at the top of this
// file: the triage side of the split is only real if the plane declaration can actually reach the worker.
//
// KILLING MUTATION (executed 2026-08-04): delete the `TG_CREDENTIAL_PLANE: ${TG_CREDENTIAL_PLANE:-both}`
// line from the `worker` service in docker-compose.yml — i.e. restore the state this ticket's first cut
// shipped in. This test then FAILS with
//
//	"the worker service does not name TG_CREDENTIAL_PLANE ... an operator who sets it in .env would have
//	 it SILENTLY DISCARDED, the triage worker would boot as `both` holding every actuation credential,
//	 and it would race worker-actuate for tg.actuate tasks"
//
// restored, green.
func TestWorkerServiceCanReceiveTheCredentialPlane(t *testing.T) {
	services := composeServices(t)
	worker, ok := services["worker"]
	if !ok {
		t.Fatal("docker-compose.yml has no `worker` service — the plane wiring guard is looking at the wrong file")
	}
	env := envOf(t, worker, "worker")

	spec, present := env["TG_CREDENTIAL_PLANE"]
	if !present {
		t.Fatal("the worker service does not name TG_CREDENTIAL_PLANE in its environment block. A compose .env " +
			"file is INTERPOLATION-ONLY and is not injected into containers, so an operator who sets it in .env " +
			"would have it SILENTLY DISCARDED: the triage worker would boot as `both` holding every actuation " +
			"credential this block already forwards, and it would race worker-actuate for tg.actuate tasks. " +
			"Add: TG_CREDENTIAL_PLANE: ${TG_CREDENTIAL_PLANE:-both}")
	}
	// The DEFAULT is the no-regression half. `both` is the pre-TG-153 posture; a stack that never sets the
	// variable must be unchanged, and a default of anything else would split every existing deployment on
	// upgrade without asking — the one thing this security fix is not allowed to do.
	if !strings.Contains(spec, ":-both}") {
		t.Fatalf("the worker's TG_CREDENTIAL_PLANE is %q but must default to `both` (i.e. ${TG_CREDENTIAL_PLANE:-both}). "+
			"Any other default silently changes the posture of every existing single-worker deployment on upgrade.", spec)
	}
	// It must be OPERATOR-SETTABLE, not hardcoded: a literal `both` here would make the documented split
	// impossible to perform and the runbook a lie for the second time.
	if !strings.Contains(spec, "${TG_CREDENTIAL_PLANE") {
		t.Fatalf("the worker's TG_CREDENTIAL_PLANE is hardcoded to %q — the operator could not switch this worker "+
			"to the triage plane at all, so the documented split cannot be performed", spec)
	}
}

// TestActuationWorkerIsPinnedAndOptIn asserts the other half of the deployment contract: the actuation
// worker declares its plane (it is never ambiguous about which credentials it may hold), and it is behind
// a compose profile so the DEFAULT stack is untouched by this ticket.
//
// KILLING MUTATION (executed 2026-08-04): remove `profiles: ["split-planes"]` from worker-actuate ⇒ RED
// with the message below, which is the real consequence: a plain `docker compose up -d` on an existing
// deployment would start a second worker that immediately competes for tg.actuate tasks.
func TestActuationWorkerIsPinnedAndOptIn(t *testing.T) {
	services := composeServices(t)
	wa, ok := services["worker-actuate"]
	if !ok {
		t.Fatal("docker-compose.yml has no `worker-actuate` service — TG-153 ships the actuation plane as a " +
			"second deployment, and without it the split has only one process and is not a split")
	}
	env := envOf(t, wa, "worker-actuate")
	if got := env["TG_CREDENTIAL_PLANE"]; got != "actuation" {
		t.Fatalf("worker-actuate must pin TG_CREDENTIAL_PLANE=actuation (got %q) — an actuation worker that "+
			"inherited `both` would register the agent toolset and the ingest pollers, so the process holding "+
			"the estate-mutating key would read untrusted alert/syslog/host content. That is TG-153 inverted.", got)
	}
	profiles, _ := wa["profiles"].([]any)
	if len(profiles) == 0 {
		t.Fatal("worker-actuate declares no compose profile — a plain `docker compose up -d` would start it on " +
			"every existing deployment, giving a second worker that competes for tg.actuate tasks with an " +
			"AppRole the operator has not configured yet. It must stay opt-in.")
	}
	found := false
	for _, p := range profiles {
		if s, _ := p.(string); s == "split-planes" {
			found = true
		}
	}
	if !found {
		t.Fatalf("worker-actuate must sit behind the `split-planes` profile (got %v) — that is the name the "+
			"runbook and docs/THREAT-MODEL.md tell the operator to bring up", profiles)
	}
	// The actuation worker must NOT be handed the untrusted-content readers. This is the compose-level
	// mirror of TestActuationPlaneCannotReachAnyUntrustedContentSource: the binary would withhold them
	// anyway, but forwarding them here would mean the container image and the process disagree about what
	// this plane is for, and the next person to relax the binary would find the door already open.
	banned := []string{"TG_SYSLOGNG_DEPLOYMENTS", "TG_HOSTDIAG_DEPLOYMENTS", "TG_JOURNAL_DEPLOYMENTS",
		"TG_LIBRENMS_ALERT_POLL_INTERVAL", "TG_PVE_LIVENESS_POLL_INTERVAL",
		"TG_DISCOVERY_SYSTEMD_HOSTS", "TG_DISCOVERY_DOCKER_HOSTS"}
	if len(banned) == 0 {
		t.Fatal("no banned keys listed — this scan would pass vacuously")
	}
	checked := 0
	for _, k := range banned {
		checked++
		if _, present := env[k]; present {
			t.Fatalf("worker-actuate forwards %s — that is an UNTRUSTED-CONTENT reader (alert bodies, device "+
				"syslog, or the stdout of commands run on estate hosts) being handed to the process that holds "+
				"the credential which mutates the estate. Remove it from the service's environment block.", k)
		}
	}
	if checked != len(banned) {
		t.Fatal("the banned-key scan did not run over its own list — vacuous")
	}
}
