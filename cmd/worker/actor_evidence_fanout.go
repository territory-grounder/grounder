package main

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/territory-grounder/grounder/adapters/actorevidence"
	"github.com/territory-grounder/grounder/core/attribution"
	"github.com/territory-grounder/grounder/core/attribution/readertally"
)

// makeActorEvidenceReader builds the read-across-every-domain function the actor-evidence agent tool uses.
//
// ★ IT WAS AN ANONYMOUS CLOSURE INSIDE main(), AND THAT IS WHY ITS ACCOUNTING COULD GO UNTESTED. The tally
// package below it has its own tests and its own RED mutation controls — and every one of them would still
// pass if this fan-out never called the tally at all. That is the failure mode the standing rule names:
// controls that prove a component correct in isolation while the system does not use it. A closure only
// reachable by booting the worker cannot be driven by a test, so it was lifted out and given a name.
//
// Behaviour is unchanged from the closure: one read per reader, per-reader failures advisory (REQ-2307) so
// one unreachable domain cannot hide the others, and a TOTAL failure surfaced as an error so the tool
// reports UNKNOWN rather than the empty-result message — a reader outage rendering as "no actor evidence"
// is how a reader failure becomes a confident false causal claim.
func makeActorEvidenceReader(readers []actorevidence.Reader, tally *readertally.Tally) func(context.Context, string, time.Time, time.Time) ([]attribution.Evidence, error) {
	return func(ctx context.Context, host string, since, until time.Time) ([]attribution.Evidence, error) {
		var all []attribution.Evidence
		var failed int
		for _, r := range readers {
			if r == nil {
				continue
			}
			ev, rerr := r.Read(ctx, host, since, until)
			if rerr != nil {
				// Advisory per reader (REQ-2307) — one domain being unreachable must not hide the others.
				// ★ A FAILURE IS NOT A ZERO-ROW READ. Counting it as one would understate every domain that
				// is merely unreachable and make an outage read as an empty scope — opposite remedies.
				failed++
				tally.Failed(r.Domain())
				log.Printf("actor-evidence tool: reader %s failed for %s (advisory): %v", r.Domain(), host, rerr)
				continue
			}
			// ★ WHICH READER IS ACTUALLY CONTRIBUTING WAS NOT A MEASURABLE FACT. This loop merges every
			// domain's rows into one slice, and the only per-reader signal was the failure line above — so a
			// reader that answered promptly with NOTHING was indistinguishable from one carrying the whole
			// result set. "5 of 6 evidence readers have returned zero rows all-time" rode in status reviews
			// for weeks as an assertion the system could not produce. Each Evidence has always carried its
			// Domain; nothing ever counted them.
			tally.Read(r.Domain(), len(ev))
			all = append(all, ev...)
		}
		// ...but if EVERY reader failed we surface an error, so the tool reports UNKNOWN rather than the
		// empty-result message. A total outage rendering as "no actor evidence" is how a reader failure
		// becomes a confident false causal claim — the precise error this phase exists to remove.
		if failed > 0 && len(all) == 0 {
			return nil, fmt.Errorf("all %d actor-evidence reader(s) failed", failed)
		}
		return all, nil
	}
}
