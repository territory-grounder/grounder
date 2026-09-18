package runner

// The TG-58 first-slice pre-state PRODUCER. The capture seam (actuate.Request.CaptureState) and the
// durable sink (action_prestate, migration 0102) have both been merged for weeks and NOTHING produced
// into them — the classic present-not-reaching gap. This closes it with the COARSE producer buildable
// from what the execute activity already holds: the pair-arm monitoring snapshot and the open-incident
// map, serialized as an "observed-state/v1" snapshot. It answers "what did the world look like the
// instant before this mutation" — the adjudication context an applied-undo needs first. Op-class-
// SPECIFIC probes (a unit's active/enabled flags, a guest's power state) are the declared follow-on and
// will ride the same seam with their own Kind.

import (
	"context"
	"encoding/json"
	"time"

	"github.com/territory-grounder/grounder/core/actuate"
	"github.com/territory-grounder/grounder/core/verify"
)

// preStateProducer builds the CaptureState hook for one governed execution. Fail-soft by contract with
// the interceptor's own retry: ok=false when NOTHING could be established (both arms nil/failed) —
// never a fabricated empty snapshot that reads as "the world was clean".
func preStateProducer(target, opClass string,
	observe func(context.Context) ([]verify.ObservedAlert, bool),
	openIncidents func(context.Context, time.Time) (map[string]bool, bool),
) func(context.Context) (actuate.PreState, bool) {
	return func(ctx context.Context) (actuate.PreState, bool) {
		snap := map[string]any{"target": target, "op_class": opClass}
		established := false
		if observe != nil {
			if alerts, ok := observe(ctx); ok {
				snap["observed_alerts"] = alerts
				established = true
			}
		}
		if openIncidents != nil {
			if hosts, ok := openIncidents(ctx, time.Now().UTC()); ok {
				snap["open_incident_hosts"] = hosts
				established = true
			}
		}
		if !established {
			return actuate.PreState{}, false
		}
		b, err := json.Marshal(snap)
		if err != nil {
			return actuate.PreState{}, false
		}
		return actuate.PreState{Kind: "observed-state/v1", Data: b}, true
	}
}
