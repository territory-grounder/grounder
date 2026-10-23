package proxmox

// THE ADOPTED GUEST MUST BE ACTUATABLE WITHOUT A RESTART (TG-232).
//
// The operator surprise this closes: someone adopts a guest in the console, the manifest says "approved",
// and TG declines to act on it — with no signal explaining why. The ssh lane already resolved its
// per-target grant live; this actuator was constructed once at worker boot from a frozen slice, so the
// grant did not reach it until the next start. An operator who cannot tell a default-deny refusal from a
// not-yet-reloaded one learns to distrust the refusal, which is worse than the delay.

import (
	"context"
	"testing"

	"github.com/territory-grounder/grounder/core/config"
	"github.com/territory-grounder/grounder/core/safety"
)

// A guest adopted AFTER construction is allowed on the very next attempt.
func TestAGuestAdoptedAfterBootIsAllowedWithoutARestart(t *testing.T) {
	adopted := []string{}
	m := New("https://pve.invalid:8006", config.SecretRef("env:NOPE"),
		WithMutationProvider(safety.NewActuatingChokepoint(), func(context.Context) []string { return adopted }))

	if m.guestAllowed(context.Background(), "ct-web01") {
		t.Fatal("an un-adopted guest was allowed — the default-deny floor is gone and every assertion " +
			"below is meaningless")
	}
	adopted = append(adopted, "ct-web01") // the operator adopts it in the console; no restart
	if !m.guestAllowed(context.Background(), "ct-web01") {
		t.Error("a guest adopted after construction is STILL refused. The manifest says approved, TG " +
			"declines, and nothing tells the operator the grant has not reached this actuator yet.")
	}
}

// A guest REVOKED after construction must stop being allowed immediately. Live resolution has to work in
// the safe direction too, or it is a one-way widening.
func TestARevokedGuestStopsBeingAllowedImmediately(t *testing.T) {
	adopted := []string{"ct-web01"}
	m := New("https://pve.invalid:8006", config.SecretRef("env:NOPE"),
		WithMutationProvider(safety.NewActuatingChokepoint(), func(context.Context) []string { return adopted }))
	if !m.guestAllowed(context.Background(), "ct-web01") {
		t.Fatal("the adopted guest was not allowed to begin with")
	}
	adopted = nil
	if m.guestAllowed(context.Background(), "ct-web01") {
		t.Error("a revoked guest is still allowed. Live resolution that only ever WIDENS is worse than a " +
			"frozen list: the operator's revocation appears to take effect and does not.")
	}
}

// THE DEFAULT-DENY FLOOR IS BYTE-UNCHANGED. This is the property the whole per-guest gate exists for, and
// the one a "make it live" change is most likely to erode.
func TestDefaultDenyFloorSurvivesLiveResolution(t *testing.T) {
	gate := safety.NewActuatingChokepoint()
	cases := map[string]func(context.Context) []string{
		"nil provider":       nil,
		"empty result":       func(context.Context) []string { return nil },
		"blank entries only": func(context.Context) []string { return []string{"", "   "} },
	}
	for name, provider := range cases {
		m := New("https://pve.invalid:8006", config.SecretRef("env:NOPE"), WithMutationProvider(gate, provider))
		if m.guestAllowed(context.Background(), "ct-web01") {
			t.Errorf("%s: a guest was ALLOWED. An actuator with no usable allowlist must refuse "+
				"everything — that is the floor, not a degraded mode.", name)
		}
		// And the empty name must never match a blank entry.
		if m.guestAllowed(context.Background(), "") || m.guestAllowed(context.Background(), "   ") {
			t.Errorf("%s: an empty guest name was allowed", name)
		}
	}
}

// The frozen-slice form still works and still means what it said — every existing caller passes a slice.
func TestTheSliceFormStillFreezesWhatItWasGiven(t *testing.T) {
	src := []string{"ct-web01"}
	m := New("https://pve.invalid:8006", config.SecretRef("env:NOPE"),
		WithMutation(safety.NewActuatingChokepoint(), src))
	if !m.guestAllowed(context.Background(), "ct-web01") {
		t.Fatal("the slice form does not allow what it was given")
	}
	// Mutating the caller's backing array must not change the actuator's answer: a caller reusing its
	// slice would otherwise silently re-scope a live actuation gate.
	src[0] = "ct-database01"
	if m.guestAllowed(context.Background(), "ct-database01") {
		t.Error("mutating the caller's slice changed the allowlist — WithMutation must copy, or a shared " +
			"backing array becomes an unaudited path to widening the gate")
	}
	if !m.guestAllowed(context.Background(), "ct-web01") {
		t.Error("the originally-granted guest was lost when the caller's slice changed")
	}
}
