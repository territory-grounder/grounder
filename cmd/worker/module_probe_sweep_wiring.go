package main

import (
	"context"
	"log"
	"strings"
	"time"

	"github.com/territory-grounder/grounder/adapters/notifier"
	"github.com/territory-grounder/grounder/modules/catalog"
	"github.com/territory-grounder/grounder/temporal/moduletest"
)

// wireModuleProbeSweep arms the scheduled module-probe sweep, carved out of main()'s composition root
// (TG-501 LOC-debt paydown): see cmd/worker/probe_sweep.go for why the console's TEST button alone is not
// enough — every fault the first live run found had been true for a long time and produced no symptom
// until somebody pressed a button by hand. Config-not-code and OFF by default (TG_MODULE_PROBE_INTERVAL
// unset). Behaviour is unchanged by the move.
func wireModuleProbeSweep(moduleProbers map[string]moduletest.Prober, notifierSinks []notifier.Notifier) {
	// ARM THE SCHEDULED SWEEP. Config-not-code and OFF by default: an interval must be declared, because a
	// monitor that starts itself is one nobody chose to run. See cmd/worker/probe_sweep.go for why the
	// TEST button alone is not enough — every fault the first live run found had been true for a long
	// time and produced no symptom until somebody pressed a button by hand.
	if iv := getenv("TG_MODULE_PROBE_INTERVAL", ""); strings.TrimSpace(iv) != "" {
		if d, derr := time.ParseDuration(iv); derr == nil && d > 0 {
			// The EMITTING set comes from the DESCRIPTORS, so the sweep's exclusions are exactly the ones
			// the operator was shown in the dialog rather than a second list that can drift from it.
			emitting := map[string]bool{}
			if described, cerr := catalog.All(); cerr == nil {
				for _, dd := range described {
					if dd.Test.Emits {
						emitting[dd.Surface+"/"+dd.SourceType] = true
					}
				}
			}
			// Its OWN fanout, deliberately not deps.Notify. The governed seam's yield register counts
			// offered-vs-produced GOVERNANCE notices; pushing config alerts through it would inflate that
			// measurement with traffic that is not a governance decision — corrupting the very instrument
			// this codebase uses to detect lanes that produce nothing.
			var sweepNotify func(context.Context, string)
			if len(notifierSinks) > 0 {
				sweepSink := notifier.NewFanout(notifierSinks...)
				sweepNotify = func(ctx context.Context, body string) {
					_, _ = sweepSink.NotifyReport(ctx, notifier.Notice{DecisionID: "tg-probe-sweep", Body: body})
				}
			}
			sweep := newProbeSweep(moduleProbers, emitting, sweepNotify, log.Printf, 30*time.Second)
			go func() {
				// One immediate run, so a broken connector is reported at boot rather than one interval
				// later — the window in which a fresh deploy looks healthy and is not.
				sweep.run(context.Background())
				t := time.NewTicker(d)
				defer t.Stop()
				for range t.C {
					sweep.run(context.Background())
				}
			}()
			log.Printf("module probe sweep: armed every %s over %d prober(s); %d emitting probe(s) excluded "+
				"(they post where people can see — press TEST for those); notify=%v",
				d, len(moduleProbers), len(emitting), sweepNotify != nil)
		} else {
			log.Printf("module probe sweep: invalid TG_MODULE_PROBE_INTERVAL %q — sweep DISABLED", iv)
		}
	} else {
		log.Printf("module probe sweep: disabled (TG_MODULE_PROBE_INTERVAL unset) — a module fault surfaces " +
			"only when an operator presses TEST")
	}
}
