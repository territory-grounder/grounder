package main

import (
	"strings"
	"testing"

	"github.com/territory-grounder/grounder/core/actuate/opschema"
	"github.com/territory-grounder/grounder/modules/actuation/ssh"
)

// EVERY COMMAND THE REGISTRY CAN SEND MUST APPEAR IN THE GUARD'S ALLOWLIST.
//
// The host guard compares SSH_ORIGINAL_COMMAND byte-for-byte with `grep -qxF` and exits 42 otherwise. Its
// allowlist used to be hand-authored in deploy/actuation-guard/install.sh, which emitted three shapes
// (`systemctl restart|reload <unit>`, `docker restart <container>`) while the registry could emit five —
// `systemctl start` and `docker start` were missing, and BOTH are graduated to AUTO. TG could therefore
// choose a start autonomously, clear all six gates, record the approval, and be refused by the host.
//
// THE ENUMERATION COMES FROM THE REGISTRY, NOT FROM A LIST HERE. opschema.Specs() is walked, so an op-class
// added tomorrow is covered by this test the day it is registered — which is the entire point, because a
// hand-kept expectation would be maintained by the same person who forgot the allowlist.
func TestEveryRegistryCommandIsAuthorised(t *testing.T) {
	units := []string{"nginx.service"}
	containers := []string{"actualbudget-actual_server-1"}

	lines, err := Render(units, containers)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	allowed := map[string]bool{}
	for _, l := range lines {
		allowed[l] = true
	}
	if len(allowed) == 0 {
		t.Fatal("the generator produced NOTHING — every assertion below would pass vacuously")
	}

	checkedAny := false
	for _, spec := range opschema.Specs() {
		for _, p := range spec.Params {
			var subjects []string
			switch p.Name {
			case "unit":
				subjects = units
			case "container":
				subjects = containers
			default:
				continue
			}
			for _, s := range subjects {
				params := map[string]string{p.Name: s}

				argv, err := spec.Argv(params)
				if err != nil || len(argv) == 0 {
					continue // this class cannot render for this subject; it sends nothing
				}
				checkedAny = true
				cmd := ssh.RemoteCommand(argv)
				if !allowed[cmd] {
					t.Errorf("op-class %q renders %s and the guard allowlist does NOT contain it — TG can "+
						"choose this action, clear every gate, record the approval, and be refused by the "+
						"host with exit 42", spec.OpClass, cmd)
				}

				// THE ROLLBACK HALF IS THE WORSE HALF. An authorised forward verb with an unauthorised
				// compensating verb means TG can act and then cannot undo, and it discovers that at the
				// moment undoing matters.
				if rb, err := spec.RollbackArgv(params); err == nil && len(rb) > 0 {
					rbCmd := ssh.RemoteCommand(rb)
					if !allowed[rbCmd] {
						t.Errorf("op-class %q rolls back with %s and the guard allowlist does NOT contain it "+
							"— the action can be taken but never undone", spec.OpClass, rbCmd)
					}
				}
			}
		}
	}
	if !checkedAny {
		t.Fatal("no op-class rendered a command for the sample subjects — this oracle asserted nothing")
	}
}

// THE ROLLBACK-NECESSITY READ SHIPS WITH THE VERBS (TG-464). Before firing a stop-* inverse the actuate
// plane confirms the unit is still RUNNING with `systemctl is-active <unit>` over the SAME key this guard
// pins; a guard that authorises the stop but refuses the read leaves every service rollback refusing at
// gate 4i with a fail-closed "unreadable" — the same act-then-cannot-undo asymmetry the generated rollback
// half exists to prevent, one layer up. The line must render through ssh.ServiceActiveProbeArgv (the shape
// the probe itself sends) and must NOT be emitted for a subject the registry refuses (the nasty-unit test
// below already pins that side: a refused unit contributes no line of any kind).
func TestRollbackNecessityReadIsAuthorised(t *testing.T) {
	lines, err := Render([]string{"nginx.service"}, nil)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	want := ssh.RemoteCommand(ssh.ServiceActiveProbeArgv("nginx.service"))
	for _, l := range lines {
		if l == want {
			return
		}
	}
	t.Errorf("the allowlist does not contain the rollback-necessity read %q — the host would deny the "+
		"probe with exit 42, the probe would fail closed, and every service rollback would refuse at the "+
		"necessity gate with the WRONG reason", want)
}

// The generator must be NARROWER than the registry in subjects and EQUAL in verbs: it may only ever authorise
// the operator's own units and containers. A generator that emitted a wildcard, or a subject nobody supplied,
// would hand the host guard's whole purpose away.
func TestNoCommandIsAuthorisedForASubjectTheOperatorDidNotSupply(t *testing.T) {
	lines, err := Render([]string{"nginx.service"}, []string{"web-1"})
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	for _, l := range lines {
		if !strings.Contains(l, "nginx.service") && !strings.Contains(l, "web-1") {
			t.Errorf("allowlist line %q names neither supplied subject — the guard's value is that it is "+
				"NARROWER than the registry, per host", l)
		}
		if strings.Contains(l, "*") || strings.Contains(l, "?") {
			t.Errorf("allowlist line %q contains a glob — the guard matches with grep -qxF, so a wildcard "+
				"here is either dead or a standing grant to anything matching it", l)
		}
	}

	// No subjects means no authorisations. An empty operator list must not silently authorise the verbs
	// with an empty argument, which would be a command the guard accepts and the operator never approved.
	empty, err := Render(nil, nil)
	if err != nil {
		t.Fatalf("render empty: %v", err)
	}
	if len(empty) != 0 {
		t.Errorf("with NO units and NO containers supplied, the generator authorised %d command(s): %v",
			len(empty), empty)
	}
}

// The rendering must go through the SAME function that renders what TG actually sends. If the generator
// reimplemented the quoting, an escaping difference would produce an allowlist that looks right and fails
// byte-for-byte at the guard — the exact failure the generator exists to remove.
func TestQuotingMatchesWhatTGActuallySends(t *testing.T) {
	// A subject with a shell metacharacter and a single quote: the case where two implementations diverge.
	nasty := `weird'unit; rm -rf /.service`
	lines, err := Render([]string{nasty}, nil)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	spec, ok := opschema.Lookup("restart-service")
	if !ok {
		t.Fatal("restart-service is not registered — this oracle cannot check anything")
	}
	argv, err := spec.Argv(map[string]string{"unit": nasty})
	if err != nil {
		// The registry may legitimately refuse this unit name; then there is nothing to authorise and the
		// allowlist must not contain it either.
		for _, l := range lines {
			if strings.Contains(l, "rm -rf") {
				t.Errorf("the registry REFUSED this unit but the allowlist authorised %q", l)
			}
		}
		return
	}
	want := ssh.RemoteCommand(argv)
	found := false
	for _, l := range lines {
		if l == want {
			found = true
		}
	}
	if !found {
		t.Errorf("the generator did not produce %q for a unit the registry accepts — the quoting has "+
			"diverged from what TG sends, so the guard would refuse a legitimate command", want)
	}
}
