package groundnet

import (
	"bytes"
	"fmt"

	"github.com/territory-grounder/grounder/core/trace"
)

// standingcheck.go — the groundnet standing-check guard (spec/021 T-021-8, § "Standing-check invariant").
//
// StandingCheck is a boot/self-test that FAILS CLOSED if a groundnet safety invariant does not hold. It is
// the runtime witness for the invariants the seam ships dormant behind; an armed node runs it before it may
// federate.
//
// HONESTY ABOUT WHAT A RUNTIME PROBE CAN CHECK. Several standing invariants are STRUCTURAL — enforced by the
// type system or the Go compiler, not falsifiable by a runtime probe — so this guard NAMES them rather than
// pretending to re-check them (a guard that claims coverage it does not have is worse than none):
//
//   - REQ-2101 (Emit never reads the estate-specific layer): the Chunk / trace.GeneralizableLayer TYPE has no
//     estate slot, so an exporter CANNOT read estate data. Type-enforced; below we additionally probe the
//     projection end-to-end as defense-in-depth.
//   - REQ-2108/2113 (the local tracer does not depend on the contract): the Go compiler forbids core/trace
//     importing core/groundnet (the cycle would not build); spec/020 acceptance asserts it via go list -deps.
//   - REQ-2109/2110 (subordinate-not-authority): the seam imports no actuator and returns no authority — an
//     ingested chunk is a hint that re-graduates locally; enforced by the package's import set + the ReGrad
//     design (it holds the graduation STORE for reading, never the ladder for writing).
//   - REQ-2107 (reputation is quality-weighted, never volume/token): the Reputation type keys by
//     (confirmer, subject) and weights by verdict quality; unit-tested, structurally incapable of counting
//     volume.
//
// WHAT IT PROBES AT RUNTIME (each a fail-closed assertion):
//   - REQ-2111: a fresh node federates NOTHING (DefaultPosture all-off).
//   - REQ-2101: a projection of an estate-populated session yields an estate-free chunk (defense-in-depth).
//   - REQ-2104/2109: an unverified statement cannot land as a re-graduation candidate.
//   - REQ-2103/INV-13: an emitted row's issuer must be a gnpub pseudonym, never a real-world/estate identity.
func StandingCheck() error {
	// REQ-2111 — default-off membership: a fresh node emits, consumes, and public-tiers nothing.
	if p := DefaultPosture(); p.MayEmit() || p.MayConsume() || p.MayUsePublicTier() {
		return fmt.Errorf("groundnet standing-check FAILED (REQ-2111): a fresh node must federate nothing, got %+v", p)
	}

	// REQ-2101 — de-identification by construction: project a session carrying estate data in every estate
	// slot and confirm the shareable chunk carries none of it.
	est := trace.SessionTrace{
		Host: "standingcheck-estate-host", ExternalRef: "STANDINGCHECK-TICKET", ActionID: "sc-action",
		PlanHash: "sc-plan", Band: "AUTO", Verdict: VerdictClean,
	}
	proj := trace.ProjectGeneralizable(est, trace.GeneralizableClasses{
		OpClass: "restart-service", AlertClass: "service-down/http",
		KnownOpClasses: []string{"restart-service"}, KnownAlertClasses: []string{"service-down/http"},
	})
	chunk := NewChunk(proj)
	payload, err := chunk.Wisdom.Marshal()
	if err != nil {
		return fmt.Errorf("groundnet standing-check: marshalling the generalizable chunk: %w", err)
	}
	for _, estate := range []string{"standingcheck-estate-host", "STANDINGCHECK-TICKET", "sc-action", "sc-plan"} {
		if bytes.Contains(payload, []byte(estate)) {
			return fmt.Errorf("groundnet standing-check FAILED (REQ-2101): the generalizable chunk leaked estate data %q: %s", estate, payload)
		}
	}

	// REQ-2104/2109 — subordinate hint, verified-only candidacy: an unverified statement must NOT land as a
	// candidate (the audit record integrity mirrors the 0113 CHECK).
	if err := (IngestRecord{Subject: "sc", Issuer: "gnpub:sc", VerifyResult: VerifyRejected, Disposition: DispositionCandidate}).Validate(); err == nil {
		return fmt.Errorf("groundnet standing-check FAILED (REQ-2104/2109): an unverified statement must not land as a re-graduation candidate")
	}

	// REQ-2103/INV-13 — pseudonymous producer only: an emit row whose issuer is not a gnpub pseudonym (a
	// real-world or estate identity) must be refused before it reaches the durable trail.
	if err := (EmitRecord{Subject: "sc", ContentType: WisdomMediaTypeV1, Issuer: "acme-corp", Receipt: []byte("r")}).Validate(); err == nil {
		return fmt.Errorf("groundnet standing-check FAILED (REQ-2103): a non-pseudonym issuer must be refused")
	}

	return nil
}
