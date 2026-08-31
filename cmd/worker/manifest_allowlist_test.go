package main

import (
	"context"
	"errors"
	"testing"

	"github.com/territory-grounder/grounder/core/db"
	"github.com/territory-grounder/grounder/core/estate"
	"github.com/territory-grounder/grounder/core/safety"
	"github.com/territory-grounder/grounder/core/worldmodel"
	sshactuation "github.com/territory-grounder/grounder/modules/actuation/ssh"
)

// TestTheAllowlistSeamIsWiredAtComposition is the ALIVENESS oracle for spec/027 REQ-2704.
//
// Stage 1 shipped a fully-tested surface that was never wired at the composition root (Deps.Proposals nil
// ⇒ a permanently-503 endpoint that looked like intended fail-closed behavior). That class must not recur,
// and for an ALLOWLIST it would be worse than a dead endpoint: adopted entries would silently never
// materialize, and the console would show grants that do nothing.
//
// The aliveness here is COMPILE-TIME plus signature-shaped: nativeSSHLaneFor cannot be called without the
// two providers, so deleting the wiring breaks the build rather than degrading quietly.
func TestTheAllowlistSeamIsWiredAtComposition(t *testing.T) {
	// The pgx store satisfies the chokepoint's persistence contract the providers consume.
	var store worldmodel.Store = db.NewWorldManifestStore(nil)

	unitsProvider := worldmodel.NewAllowlistProvider(store, worldmodel.KindUnit, []string{"nginx.service"})
	containersProvider := worldmodel.NewAllowlistProvider(store, worldmodel.KindContainer, nil)

	// The exact call shape main uses. If the providers are ever dropped from the signature, this fails to
	// compile — the wiring cannot be removed silently.
	lane := nativeSSHLaneFor(safety.NewReadOnlyChokepoint(), nil, unitsProvider, containersProvider)
	if lane == nil {
		t.Fatal("the native-ssh lane must be constructed with the allowlist seams wired")
	}

	// A store whose pool is nil errors on read, so the provider must fall back to the ENV grant — never to
	// an empty list (which would silently revoke every env-typed target) and never to a wider one.
	got := unitsProvider(context.Background())
	if len(got) != 1 || got[0] != "nginx.service" {
		t.Fatalf("an unavailable store must yield exactly the env grant, got %v", got)
	}
}

// TestAdoptedEntriesReachTheProviderThroughTheRealStoreContract pins the mapping the composition root
// depends on: an adopted entry of each kind materializes into ITS OWN leaf allowlist and no other, so
// adopting a container can never hand a name to the unit gate.
func TestAdoptedEntriesReachTheProviderThroughTheRealStoreContract(t *testing.T) {
	adopted := []worldmodel.Entry{
		{EntityType: estate.TypeService, Name: "mealie.service", Status: worldmodel.StatusApproved},
		{EntityType: estate.TypeService, Name: "actualbudget-app", Status: worldmodel.StatusApproved},
		{EntityType: estate.TypeLXC, Name: "dc1mealie01", Status: worldmodel.StatusApproved},
	}
	units := worldmodel.UnionAllowlist(nil, adopted, worldmodel.KindUnit)
	containers := worldmodel.UnionAllowlist(nil, adopted, worldmodel.KindContainer)
	guests := worldmodel.UnionAllowlist(nil, adopted, worldmodel.KindGuest)

	if len(units) != 1 || units[0] != "mealie.service" {
		t.Fatalf("units allowlist must contain only the systemd unit, got %v", units)
	}
	if len(containers) != 1 || containers[0] != "actualbudget-app" {
		t.Fatalf("containers allowlist must contain only the container, got %v", containers)
	}
	if len(guests) != 1 || guests[0] != "dc1mealie01" {
		t.Fatalf("guests allowlist must contain only the guest, got %v", guests)
	}
}

// TestAnAdoptedEntryActuallyReachesTheConstructedLeaf closes the gap a RED control exposed: signature-shaped
// aliveness proves the providers are PASSED, not that they are CONSUMED. Neutering the provider inside the
// lane's build closure broke no test and no build — the same defect class Stage 1 shipped, one level deeper.
//
// This drives the real per-target build and asserts the adopted entry arrives at the leaf, by observing the
// only externally visible consequence: the leaf ACCEPTS the adopted unit and still refuses a neighbour.
func TestAnAdoptedEntryActuallyReachesTheConstructedLeaf(t *testing.T) {
	adopted := []worldmodel.Entry{{
		EntityType: estate.TypeService, Name: "mealie.service", Status: worldmodel.StatusApproved,
	}}
	cfg := perTargetSSHConfig{
		chokepoint: safety.NewActuatingChokepoint(),
		identity:   "tg@estate",
		keyRef:     "bao:secret/data/tg/actuation#key",
		unitsProvider: func(context.Context) []string {
			return worldmodel.UnionAllowlist(nil, adopted, worldmodel.KindUnit)
		},
	}
	leaf, err := buildPerTargetSSHLeaf(context.Background(), cfg, "dc1mealie01")
	if err != nil {
		t.Fatalf("per-target build: %v", err)
	}

	// The adopted unit is accepted (the guard passes; the transport then fails on the fake key ref, which
	// is fine — we are asserting the ALLOWLIST decision, not connectivity).
	_, errAdopted := leaf.Exec(context.Background(), []string{"systemctl", "restart", "mealie.service"}, nil)
	if errors.Is(errAdopted, sshactuation.ErrUnitNotAllowed) {
		t.Fatal("THE SEAM IS DEAD: an ADOPTED unit was refused by the allowlist gate — the provider is not being consumed")
	}

	// A neighbour that was never adopted is still refused BY THE ALLOWLIST — proving the union is the real
	// source of the grant rather than an open door.
	_, errOther := leaf.Exec(context.Background(), []string{"systemctl", "restart", "nginx.service"}, nil)
	if !errors.Is(errOther, sshactuation.ErrUnitNotAllowed) {
		t.Fatalf("a non-adopted unit must still be refused by the allowlist gate, got %v", errOther)
	}
}
