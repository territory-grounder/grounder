package runner

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/territory-grounder/grounder/core/verify"
)

// TG-58: the coarse pre-state producer serializes what the execute site already knows, and answers
// ok=false — never a clean-looking empty snapshot — when NOTHING could be established. KILLING
// MUTATION: return a marshaled empty snap when both arms fail — the "nothing established" case fails.
func TestPreStateProducerSnapshotsAndFailsHonestly(t *testing.T) {
	obs := func(context.Context) ([]verify.ObservedAlert, bool) {
		return []verify.ObservedAlert{{Host: "web01", Rule: "NginxDown"}}, true
	}
	inc := func(context.Context, time.Time) (map[string]bool, bool) {
		return map[string]bool{"web01": true}, true
	}

	ps, ok := preStateProducer("web01", "restart-service", obs, inc)(context.Background())
	if !ok || ps.Kind != "observed-state/v1" {
		t.Fatalf("both arms up must capture: ok=%v kind=%q", ok, ps.Kind)
	}
	var snap map[string]any
	if err := json.Unmarshal(ps.Data, &snap); err != nil {
		t.Fatalf("snapshot must round-trip: %v", err)
	}
	if snap["target"] != "web01" || snap["op_class"] != "restart-service" {
		t.Fatalf("snapshot lost its identity: %v", snap)
	}
	if _, has := snap["observed_alerts"]; !has {
		t.Fatal("the pair arm's alerts must ride the snapshot")
	}
	if !strings.Contains(string(ps.Data), "open_incident_hosts") {
		t.Fatal("the host arm must ride the snapshot")
	}

	// One arm down: still established, the dead arm simply absent.
	deadObs := func(context.Context) ([]verify.ObservedAlert, bool) { return nil, false }
	ps, ok = preStateProducer("web01", "restart-service", deadObs, inc)(context.Background())
	if !ok || strings.Contains(string(ps.Data), "observed_alerts") {
		t.Fatalf("a dead arm must be absent, not fabricated: ok=%v data=%s", ok, ps.Data)
	}

	// BOTH arms down: no snapshot — the interceptor records the capture gap instead.
	deadInc := func(context.Context, time.Time) (map[string]bool, bool) { return nil, false }
	if _, ok := preStateProducer("web01", "restart-service", deadObs, deadInc)(context.Background()); ok {
		t.Fatal("nothing established must be ok=false, never an empty snapshot that reads as clean")
	}
	if _, ok := preStateProducer("web01", "restart-service", nil, nil)(context.Background()); ok {
		t.Fatal("nil arms must be ok=false")
	}
}
