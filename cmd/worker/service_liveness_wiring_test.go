package main

import "testing"

// The exit-code contract is the whole honesty of the service-liveness read (TG-464): the probe must never
// mistake a DENIAL for an ANSWER. 0/3/4 are systemctl's own completed-read codes; 42 is the host guard's
// refusal (deploy/actuation-guard/tg-actuator-guard) and 255 the ssh transport's — both "state
// unestablished", which the necessity gate turns into a fail-closed read-error refusal that names the real
// problem, where a false "inactive" would refuse with "nothing to undo" and mask the misconfiguration.
func TestServiceActiveFromExitContract(t *testing.T) {
	cases := []struct {
		code       int
		active, ok bool
		why        string
	}{
		{0, true, true, "is-active exit 0 = the unit is ACTIVE"},
		{3, false, true, "exit 3 = inactive/failed — a completed read answering 'not running'"},
		{4, false, true, "exit 4 = no such unit — completed read, nothing running to undo"},
		{42, false, false, "exit 42 = the host guard DENIED the read — unestablished, never 'inactive'"},
		{255, false, false, "exit 255 = ssh transport failure — unestablished"},
		{1, false, false, "any other exit is an unclassified failure — fail closed"},
	}
	for _, c := range cases {
		if active, ok := serviceActiveFromExit(c.code); active != c.active || ok != c.ok {
			t.Errorf("exit %d: got (active=%v ok=%v), want (active=%v ok=%v) — %s", c.code, active, ok, c.active, c.ok, c.why)
		}
	}
}

// An incomplete actuation identity must yield NO reader (the lane stays unwired and the rollback necessity
// probe falls through to its pre-TG-464 posture) — mirroring buildPerTargetSSHLeaf's own require-all-three
// discipline, and the guestRunningReader nil-store contract. A wired reader with a blank host or unit fails
// closed without dialing anything.
func TestServiceActiveReaderPreconditions(t *testing.T) {
	if r := serviceActiveReader("", "tg-actuator", "env:TG_ACTUATION_SSH_KEY"); r != nil {
		t.Error("no known_hosts ⇒ the reader must be nil (unwired lane), got non-nil")
	}
	if r := serviceActiveReader("/etc/known_hosts", "", "env:TG_ACTUATION_SSH_KEY"); r != nil {
		t.Error("no identity ⇒ the reader must be nil (unwired lane), got non-nil")
	}
	if r := serviceActiveReader("/etc/known_hosts", "tg-actuator", ""); r != nil {
		t.Error("no key ref ⇒ the reader must be nil (unwired lane), got non-nil")
	}
	r := serviceActiveReader("/etc/known_hosts", "tg-actuator", "env:TG_ACTUATION_SSH_KEY")
	if r == nil {
		t.Fatal("a complete identity must build a reader, got nil")
	}
	if active, ok := r(t.Context(), "  ", "nginx"); active || ok {
		t.Errorf("blank host must fail closed without dialing, got (%v,%v)", active, ok)
	}
	if active, ok := r(t.Context(), "app01", " "); active || ok {
		t.Errorf("blank unit must fail closed without dialing, got (%v,%v)", active, ok)
	}
}
