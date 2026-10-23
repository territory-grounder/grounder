package main

import (
	"context"
	"time"

	"github.com/territory-grounder/grounder/core/db"
	"github.com/territory-grounder/grounder/tools/faultinjector"
	"github.com/territory-grounder/grounder/tools/observeprobe"
)

// probeStoreAdapter adapts core/db's ObservationProbeStore (db-native strings + a PendingProbe struct) to BOTH
// the observeprobe.ProbeStore and observeprobe.AlertReader interfaces the orchestrator reasons from. The split
// exists so core/db keeps its no-tools-import direction (TG-180): the durable schema knows nothing of the
// tools/observeprobe domain types, and this worker-side adapter is the one place they meet.
type probeStoreAdapter struct{ s *db.ObservationProbeStore }

// RecordProbe appends a probe run. The note is fixed here rather than threaded through the domain type — the
// ledger's provenance is the same for every row this loop writes.
func (a probeStoreAdapter) RecordProbe(ctx context.Context, run observeprobe.ProbeRun) (int64, error) {
	return a.s.RecordProbe(ctx, run.Host, string(run.Class), run.InjectedAt, run.WindowEnd, run.Ran, "TG-180 observe-probe")
}

func (a probeStoreAdapter) SetVerdict(ctx context.Context, id int64, v observeprobe.Verdict) error {
	return a.s.SetProbeVerdict(ctx, id, string(v))
}

// PendingProbes maps the db-native rows to the orchestrator's domain type. FaultClass is a plain string in the
// durable row and becomes a faultinjector.Class here — the same widening the injector's Class carries, so a
// verdict reconciled a cycle later reads the class the probe was injected with.
func (a probeStoreAdapter) PendingProbes(ctx context.Context) ([]observeprobe.PendingProbe, error) {
	rows, err := a.s.PendingProbes(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]observeprobe.PendingProbe, 0, len(rows))
	for _, r := range rows {
		out = append(out, observeprobe.PendingProbe{
			ID: r.ID,
			Run: observeprobe.ProbeRun{
				Host:       r.Host,
				Class:      faultinjector.Class(r.FaultClass),
				InjectedAt: r.InjectedAt,
				WindowEnd:  r.WindowEnd,
				Ran:        r.Ran,
			},
		})
	}
	return out, nil
}

func (a probeStoreAdapter) ConfirmedHosts(ctx context.Context) (map[string]bool, error) {
	return a.s.ProbeConfirmedHosts(ctx)
}

func (a probeStoreAdapter) AlertTimes(ctx context.Context, host string, since, until time.Time) ([]time.Time, error) {
	return a.s.ProbeAlertTimes(ctx, host, since, until)
}

// One type satisfies both interfaces — the orchestrator holds it as ProbeStore in one field and AlertReader in
// another, and these asserts fail the build if either method set drifts from what the orchestrator requires.
var (
	_ observeprobe.ProbeStore  = probeStoreAdapter{}
	_ observeprobe.AlertReader = probeStoreAdapter{}
)
