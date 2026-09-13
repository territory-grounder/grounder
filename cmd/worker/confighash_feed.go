package main

// The confighash baseline adapter (TG-466 slice 2): binds modules/cmdb/pve/confighash's Collector to the
// core/db-backed guest_config_baseline projection at the composition root. confighash.go's own doc comment
// anticipates exactly this: "The estate-derived projection in core/db (guest_config_baseline, migration
// 0091) mirrors this shape field-for-field — slice 2 binds the two with a composition-root adapter, the
// guest-liveness feed pattern." The two packages deliberately declare INDEPENDENT, field-identical types
// (confighash.Observed/Outcome vs db.GuestConfigObservation/GuestConfigOutcome) rather than one importing
// the other's — core/db must not depend on a leaf cmdb module, and confighash must not depend on core/db —
// so this file is the one place that knows both shapes.

import (
	"context"

	"github.com/territory-grounder/grounder/core/db"
	"github.com/territory-grounder/grounder/modules/cmdb/pve/confighash"
)

// confighashBaselineWriter is the narrow persistence half confighashBaselineAdapter wraps (satisfied by
// *db.GuestConfigBaselineStore). An interface, not the concrete store, so the field translation below is
// unit-testable without a database — the same reason guestLivenessSink (guest_liveness.go) is an interface.
type confighashBaselineWriter interface {
	Record(ctx context.Context, obs db.GuestConfigObservation) (db.GuestConfigOutcome, error)
}

// confighashBaselineAdapter satisfies confighash.Baselines over confighashBaselineWriter. Translation
// only — it mints no decision, applies no fail-safe logic of its own, and holds no state: a Record error
// is returned verbatim (confighash.Diff / Collector.Sweep are what turn an error into "no signal, tally it
// loudly", not this seam).
type confighashBaselineAdapter struct{ store confighashBaselineWriter }

// Record translates confighash's Observed/Outcome to and from the db package's field-identical shape.
func (a confighashBaselineAdapter) Record(ctx context.Context, obs confighash.Observed) (confighash.Outcome, error) {
	out, err := a.store.Record(ctx, db.GuestConfigObservation{
		VMID: obs.VMID, Guest: obs.Guest, Node: obs.Node, Kind: obs.Kind, Hash: obs.Hash,
	})
	if err != nil {
		return confighash.Outcome{}, err
	}
	return confighash.Outcome{
		FirstSighting: out.FirstSighting,
		Changed:       out.Changed,
		PreviousHash:  out.PreviousHash,
	}, nil
}

var _ confighash.Baselines = confighashBaselineAdapter{}

// confighashSweepWarning is the loud, named-misconfiguration line for the HALF-ARMED shape a review caught
// (TG-466 slice 2): TG_PVE_CONFIGHASH_ENABLED set, but the confighash reader never armed (TG_PVE_URL unset,
// or TG_PVE_RO_TOKEN_REF unset/unresolvable). Before this, that state left the READ arm wired anyway (see
// confighashReadArmed) — always answering false against a baseline nothing ever swept, a silent, permanent
// false-negative with no signal telling the operator their flag flip did not do what it looked like it did.
// Fail-safe throughout (no false positive can result), but a flag flip that silently does nothing is a
// misconfiguration worth naming loudly, not the same quiet as the deliberate default. Empty string ⇒ nothing
// to report (either the flag is off, the deliberate dark default, or the reader armed cleanly).
func confighashSweepWarning(flagOn, readerArmed bool) string {
	if flagOn && !readerArmed {
		return "confighash: WARNING — TG_PVE_CONFIGHASH_ENABLED is set but the confighash reader did NOT arm " +
			"(need TG_PVE_URL + a resolvable TG_PVE_RO_TOKEN_REF) — the estate refresh tick will sweep NOTHING " +
			"and the attribution read seam stays UNWIRED (fail-safe: Observation stays the zero value, no " +
			"escalation) — but this is a MISCONFIGURATION: fix the credential/URL or unset the flag."
	}
	return ""
}

// confighashReadArmed decides whether the attribution read seam (Deps.GuestConfigChangedWithin) should wire,
// and the boot line to print, given the sweep arm's actual state. Pure and extracted so the half-armed guard
// is unit-testable without invoking main(): armed requires BOTH a durable pool AND the confighash reader
// itself having armed (readerArmed) — NOT the flag alone. Gating on the flag alone was the review finding:
// flag ON + a credential/URL that never resolved left the read arm wired against a baseline nothing swept.
func confighashReadArmed(dbConnected, readerArmed, flagOn, classifyPlane bool) (armed bool, logLine string) {
	switch {
	case !classifyPlane:
		// The confighash mutation signal is consumed by the triage CLASSIFY path (AttributeActivity, on
		// tg.runner). A pure ACTUATION process runs no classify — so the flag is inert here regardless of its
		// value, and reporting it OFF/UNREACHABLE reads as a gap where there is none (it nearly tripped a
		// drift alarm 2026-08-26: the actuate plane logged OFF while the triage plane was correctly armed).
		// Say plainly that the signal lives on the triage plane.
		return false, "attribution: confighash mutation signal — N/A on the actuation plane (its consumer is the " +
			"triage classify path, armed on the triage plane when TG_PVE_CONFIGHASH_ENABLED is set); this process actuates only"
	case dbConnected && readerArmed:
		return true, "attribution: confighash grounded mutation signal ARMED (TG_PVE_CONFIGHASH_ENABLED) — " +
			"AttributeActivity threads guest_config_baseline.ChangedWithin into Observation.MutationObserved (TG-466 slice 2)"
	case flagOn:
		// The flag signals operator INTENT to arm, but something upstream did not resolve. Distinct from the
		// plain "unset" line below so a misconfiguration can never be mistaken for the deliberate dark default.
		reason := "the confighash reader did not arm (see the confighash WARNING logged during estate-source wiring)"
		if !dbConnected {
			reason = "no durable pool is connected"
		}
		return false, "attribution: WARNING — confighash grounded mutation signal NOT armed despite " +
			"TG_PVE_CONFIGHASH_ENABLED being set — " + reason + ". Observation stays the zero value; the " +
			"covered-but-empty escalation remains UNREACHABLE (fail-safe, but this is a misconfiguration worth fixing)."
	default:
		return false, "attribution: confighash grounded mutation signal OFF (TG_PVE_CONFIGHASH_ENABLED unset) — " +
			"Observation stays the zero value; the covered-but-empty escalation remains UNREACHABLE (TG-466 ship-dark default)"
	}
}
