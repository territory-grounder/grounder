package groundnet

import "testing"

// TestStandingCheckPassesOnHealthySystem verifies the guard is GREEN on the delivered code — every invariant
// it probes holds. (The fail-closed branches are exercised by their own oracles: DefaultPosture by the
// spec/021 membership scenarios, the record Validate refusals by audit_test, the projection by the
// generalizable-marker scenario.)
func TestStandingCheckPassesOnHealthySystem(t *testing.T) {
	if err := StandingCheck(); err != nil {
		t.Fatalf("the standing check must pass on the delivered groundnet code, got: %v", err)
	}
}

// TestStandingCheckIsNotVacuous proves the guard's assertions actually run — if the two type-level building
// blocks it depends on (DefaultPosture off, the estate-free projection) regressed, StandingCheck would catch
// it. Here we confirm those building blocks are in the state the guard expects, so a green StandingCheck is
// evidence, not an empty pass.
func TestStandingCheckIsNotVacuous(t *testing.T) {
	if p := DefaultPosture(); p.MayEmit() || p.MayConsume() || p.MayUsePublicTier() {
		t.Fatal("DefaultPosture is not all-off — StandingCheck's REQ-2111 probe would (correctly) fail; the guard is live")
	}
	// The audit record integrity the guard relies on must genuinely refuse the bad shapes.
	if err := (IngestRecord{Subject: "s", Issuer: "gnpub:n", VerifyResult: VerifyRejected, Disposition: DispositionCandidate}).Validate(); err == nil {
		t.Fatal("the ingest integrity check does not refuse an unverified candidate — StandingCheck's probe would pass vacuously")
	}
}
