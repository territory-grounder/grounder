package actuate

import "context"

// TargetAdmission is the DURABLE, cross-process per-target admission seam (TG-81 borrow 2; clean-room
// inversion of h-ssh's fail-open active-set, attribution: SOURCE-BENCHMARK-CATALOG). The in-process
// frequency governor (gate 4h, TG-166a) already leases session/target concurrency inside ONE worker;
// this seam is the half a per-process lease cannot deliver: with two actuation-capable processes (the
// direct chain and the regime lanes today, siblings later), only a shared durable claim can guarantee
// "one actuation in flight per target, estate-wide" — the same reasoning that made the mutation breaker
// durable (migration 0021).
//
// Two tiers, both FAIL-CLOSED once armed:
//   - ACTIVE-SET admission: Admit atomically claims the target before the pre-effect sequence begins
//     (baselines, necessity probe, Exec). A target already claimed by ANY process refuses. A stale claim
//     (a crashed worker's leftover) is taken over only after the claim TTL.
//   - COOLDOWN-ON-ERROR: Release with disturbed=true parks the target for the cooldown window — a
//     failed or killed effect leaves the target in an unknown state, and the next actuation against it
//     must wait out the dust rather than pile on. Refusals BEFORE the effect release undisturbed.
//
// An Admit error is a REFUSAL, whatever its cause: a held claim, an active cooldown, and an unreachable
// store all refuse (the h-ssh posture inverted — their active-set admitted on store failure). A nil
// TargetAdmission on the interceptor is the documented unarmed pass-through, exactly like the mutation
// breaker: absence is a deployment shape, failure never is.
type TargetAdmission interface {
	// Admit claims target for ref (the session's external ref — recorded so a refusal can say WHO holds
	// the claim). A nil return means the claim is held and MUST be released; any error means refused.
	Admit(ctx context.Context, target, ref string) error
	// Release drops ref's claim on target. disturbed=true records that the effect fired and failed (or
	// was killed mid-flight), starting the cooldown window. Best-effort: a failed release leaves a stale
	// claim that ages out via the claim TTL — fail-closed, never fail-open.
	Release(ctx context.Context, target, ref string, disturbed bool)
}
