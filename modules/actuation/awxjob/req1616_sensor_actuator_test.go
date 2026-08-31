package awxjob

// spec/016 REQ-1616 (T-016-11) — the SENSOR/ACTUATOR SPLIT over the Ansible platform, pinned as one
// oracle because the requirement is about the BOUNDARY between two capabilities that share a vendor:
//
//   "read-only fact-gathering is a Phase-1-safe SENSOR, and governed playbook / job-template actuation
//    routes through the mode chokepoint and the never-auto floor as a mutating channel."
//
// Both halves already exist in this tree — the actuator is this package (the awx-job effect lane) and
// the read-only capability is the knowledge/awxplaybooks client plus hostdiag's fixed-argv SSH reads.
// What had no oracle was the SPLIT ITSELF: that the two are distinct channels, that the mutating one
// cannot act at Shadow, and that the reading one carries no effect path at all. Those are the claims a
// future change could silently break — by giving the sensor an Exec, or by letting the actuator read a
// mode from somewhere other than the chokepoint.
//
// WHY THERE IS NO SEPARATE "ansible" ACTUATOR MODULE, stated here rather than left as an absence:
//   - An UN-CONTROLLED Ansible actuator (running ansible-playbook directly) is unbuildable under this
//     architecture, not merely unbuilt: the worker image is distroless, INV-02 forbids a shell, and
//     there is no ansible binary to exec. The credential connector reads an un-controlled inventory as
//     DATA (modules/credsource/ansible) precisely because that is the only safe thing to do with it.
//   - Governed playbook actuation therefore goes through the platform's API — which is this lane. AWX
//     IS Ansible with an API, and a job template is a strictly narrower channel than raw SSH: a fixed
//     template id plus typed extra_vars, with no command string anywhere in the launch body.
//   - An AWX ad-hoc "gather facts" sensor was considered and REFUSED: an ad-hoc command RUNS a module
//     on the target, so it is a remote execution channel however read-only the module is — and it would
//     buy facts that hostdiag already gathers over a fixed-argv, host-key-verified read-only SSH read.
//     Adding an execution path to obtain data we already obtain without one is a net loss.

import (
	"context"
	"testing"

	actuation "github.com/territory-grounder/grounder/adapters/actuation"
	"github.com/territory-grounder/grounder/core/safety"
	"github.com/territory-grounder/grounder/modules/knowledge/awxplaybooks"
)

// The MUTATING half: at Shadow the actuator is INERT twice over — it reports itself read-only (it holds
// no reachable launch path in this posture) and its Exec refuses at its own leaf before any network
// call, which is the defense-in-depth check that holds even if something ever called it outside the
// interceptor.
func TestREQ1616_ActuationHalfIsInertAtShadow(t *testing.T) {
	act, err := New(Config{
		Client:    &Client{},
		Allowlist: TemplateAllowlist{7: {OpClass: "restart-service"}},
		ModeGate:  safety.NewReadOnlyChokepoint(),
	})
	if err != nil {
		t.Fatalf("actuator: %v", err)
	}
	if !act.ReadOnly() {
		t.Fatal("at Shadow the mutating lane must report read-only — it has no reachable launch path, and " +
			"claiming otherwise would advertise an effect capability the mode forbids")
	}
	if _, err := act.Exec(context.Background(), []string{LaunchVerb}, nil); err == nil {
		t.Fatal("at Shadow the actuator must refuse at its own leaf, before any network launch")
	}
	// VACUITY FLOOR: the refusal above must come from the MODE, not from a missing allowlist or a
	// malformed argv — otherwise this test would pass against an actuator with no mode gate at all.
	armed, err := New(Config{
		Client:    &Client{},
		Allowlist: TemplateAllowlist{7: {OpClass: "restart-service"}},
		ModeGate:  safety.NewActuatingChokepoint(),
	})
	if err != nil {
		t.Fatalf("actuator: %v", err)
	}
	if armed.ReadOnly() {
		t.Fatal("with the mode permitting actuation AND an allowlist declared, the lane is a genuine effect " +
			"channel — if it still reports read-only, the Shadow assertion above proves nothing")
	}
}

// The SENSOR half: the read-only AWX capability is a DIFFERENT type with no effect path. This asserts
// the split structurally — the sensor does not satisfy the Actuator interface, so it cannot be handed to
// an interceptor, registered as a lane, or reached by anything that dispatches an effect.
func TestREQ1616_SensorHalfCarriesNoEffectPath(t *testing.T) {
	sensor, err := awxplaybooks.NewClient(awxplaybooks.ClientConfig{
		BaseURL:  "https://awx.example.net",
		TokenRef: "env:AWX_RO_TOKEN", // the READ-ONLY sensor token, distinct from the launch token
	})
	if err != nil {
		t.Fatalf("the read-only sensor must construct from a token reference: %v", err)
	}
	if _, isActuator := any(sensor).(actuation.Actuator); isActuator {
		t.Fatal("the fact-gathering sensor must NOT satisfy the Actuator interface — a Phase-1-safe reader " +
			"that can be dispatched as an effect leaf is not a reader")
	}
	// And the actuator IS one: the two halves are distinguishable by type, which is what makes the split
	// structural rather than a naming convention.
	act, err := New(Config{Client: &Client{}})
	if err != nil {
		t.Fatalf("actuator: %v", err)
	}
	if _, isActuator := any(act).(actuation.Actuator); !isActuator {
		t.Fatal("the awx-job lane must satisfy Actuator — it is the governed mutating channel")
	}
}
