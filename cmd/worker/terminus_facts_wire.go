package main

// TG-662 wiring: the terminus FACTS store (migration 0122) and the OBJECT-STATE reader behind the clear-confirm
// loop's positive object read. Lives outside main() (the TG-501 ratchet) and reads only seams main() already
// holds — the guest-liveness reader, the hostdiag access rules + credential resolver, the durable pool.

import (
	"context"
	"log"
	"strings"

	"github.com/territory-grounder/grounder/core/actuate/opschema"
	"github.com/territory-grounder/grounder/core/db"
	"github.com/territory-grounder/grounder/core/predict"
	"github.com/territory-grounder/grounder/modules/observability/hostdiag"
	"github.com/territory-grounder/grounder/modules/observability/syslogng"
	"github.com/territory-grounder/grounder/temporal/runner"
)

// wireTerminusFacts installs runner.Deps.ObjectState, TriageMarkFacts and TriageFacts. The object reader is
// built from the three per-family sources this plane already has: the guest-liveness projection (guests, any
// plane with a database), and the read-only diagnostic lane for units and containers (the triage plane, which
// holds the hostdiag key — the actuation plane withholds it, so there the unit/container read is honestly
// unobserved and the clear loop, which runs on the triage queue, never asks it). A nil pool leaves the facts
// seams nil: MarkTerminusFactsActivity falls back to the legacy mark and the consult reads PENDING.
func wireTerminusFacts(deps *runner.Deps, pool *db.Pool, gate *predict.PredictionGate,
	hdAccess []hostdiag.Access, hdKnownHosts string, resolver hostdiag.IdentityResolver) {
	// A nil gate (no prediction gate wired on this plane) leaves the guest read nil: guests then read UNOBSERVED,
	// never healthy — the same fail-closed reading an unwired family gets.
	var guestRunning func(context.Context, string) (bool, string, bool)
	if gate != nil {
		guestRunning = gate.GuestRunning
	}
	var hostRead func(ctx context.Context, host, kind, name string) hostdiag.ObjectReading
	if len(hdAccess) > 0 && resolver != nil {
		hostRead = hostdiag.NewObjectStateReader(syslogng.NewNativeRunner(hdKnownHosts), resolver)
	}
	deps.ObjectState = objectStateReader(guestRunning, hostRead)
	wireCausality(deps, pool)
	if pool != nil {
		ts := db.NewTriageStore(pool)
		deps.TriageMarkFacts = func(ctx context.Context, ref string, f runner.TerminusFacts) error {
			return ts.MarkTerminusFacts(ctx, ref, toDBTerminusFacts(f))
		}
		deps.TriageFacts = func(ctx context.Context, ref string) (runner.TerminusFacts, bool, error) {
			f, found, err := ts.TerminusFactsFor(ctx, ref)
			return fromDBTerminusFacts(f), found, err
		}
	}
	log.Printf("terminus facts (TG-662): object-state reader guests=%v units/containers=%v; facts store=%v — ConfirmedClear needs host-quiet AND a positive object read; the commit-confirm window confirms only on the persisted facts",
		guestRunning != nil, hostRead != nil, pool != nil)
}

// toDBTerminusFacts / fromDBTerminusFacts carry the terminus facts across the runner ↔ db seam WHOLE (TG-695).
// Until 2026-11-08 both directions were field-by-field literals that left out Causality and CausalityEvidence:
// the terminus computed the TG-662 causality, the triage row stored NULL, and the commit-confirm window — which
// decides every commit-confirmed class's credit from these facts — read "" and withheld EVERY confirmed heal as
// causality-indeterminate. The ladder was never fed. The two structs are field-for-field identical, so a Go struct
// CONVERSION carries every field and stops compiling the day either struct gains one the other lacks: a dropped
// field becomes a build error, not a silent NULL.
func toDBTerminusFacts(f runner.TerminusFacts) db.TerminusFacts { return db.TerminusFacts(f) }

func fromDBTerminusFacts(f db.TerminusFacts) runner.TerminusFacts { return runner.TerminusFacts(f) }

// objectStateReader maps an op-class onto its object read by FAMILY (registry data, never the model's word):
// guest-lifecycle ⇒ the guest-liveness projection (goal state from the class's declared precondition —
// start-guest requires not-running so its goal is running; stop-guest the reverse); service-lifecycle ⇒ the
// unit named by params["unit"]; container-lifecycle ⇒ params["container"]. Every other family, an unregistered
// class, a missing param or a nil source reads UNOBSERVED (indeterminate) — never healthy.
func objectStateReader(guestRunning func(context.Context, string) (bool, string, bool),
	hostRead func(ctx context.Context, host, kind, name string) hostdiag.ObjectReading) func(ctx context.Context, host, opClass string, params map[string]string) runner.ObjectStateReading {
	unobserved := func(detail string) runner.ObjectStateReading { return runner.ObjectStateReading{Detail: detail} }
	fromHost := func(r hostdiag.ObjectReading) runner.ObjectStateReading {
		return runner.ObjectStateReading{Healthy: r.Healthy && r.Observed, Observed: r.Observed, Detail: r.Detail}
	}
	return func(ctx context.Context, host, opClass string, params map[string]string) runner.ObjectStateReading {
		spec, ok := opschema.Lookup(opClass)
		if !ok {
			return unobserved("op-class " + opClass + " is not registered — no object to read")
		}
		switch spec.Family {
		case opschema.FamilyGuestLifecycle:
			if guestRunning == nil {
				return unobserved("no guest-liveness reader wired")
			}
			guest := strings.TrimSpace(params[opschema.ParamGuest])
			if guest == "" {
				guest = strings.TrimSpace(host)
			}
			running, prov, observed := guestRunning(ctx, guest)
			if !observed {
				return unobserved("guest " + guest + ": " + prov)
			}
			goalRunning := spec.RequiresTargetState != opschema.RequiresRunning
			return runner.ObjectStateReading{Healthy: running == goalRunning, Observed: true,
				Detail: "guest " + guest + " running=" + boolWord(running) + " (" + prov + ")"}
		case opschema.FamilyServiceLifecycle:
			if hostRead == nil {
				return unobserved("no unit reader wired on this plane")
			}
			return fromHost(hostRead(ctx, host, hostdiag.ObjectUnit, params[opschema.ParamUnit]))
		case opschema.FamilyContainerLifecycle:
			if hostRead == nil {
				return unobserved("no container reader wired on this plane")
			}
			return fromHost(hostRead(ctx, host, hostdiag.ObjectContainer, params[opschema.ParamContainer]))
		default:
			return unobserved("no object read for family " + spec.Family)
		}
	}
}

func boolWord(b bool) string {
	if b {
		return "true"
	}
	return "false"
}
