package grafana

import (
	"context"
	"sync"
	"testing"
	"time"

	observability "github.com/territory-grounder/grounder/adapters/observability"
)

// This module is a Grafana PROVISIONING surface (REQ-817), not an annotations/sample sink: the control-
// plane dashboards are defined in version control and Grafana is reconciled toward them. There is no
// outbound HTTP, no Doer, no secret and no auth header to assert — the "vendor protocol" that IS reachable
// is the drift contract: the version-controlled hash is the fact, and a live dashboard whose hash diverges
// is a hand-edited panel that must be rejected as drift. These tests drive that real path. (Export exists
// only to satisfy adapters/observability.Exporter and is a deliberate no-op: freshness stamping, INV-15,
// applies to sample-exporting backends, and provisioning carries no samples.)

// TestSourceTypeSlug pins the vendor slug the capability-scoped registry keys on, and re-states the
// package's compile-time proof that the module satisfies the stable observability surface.
func TestSourceTypeSlug(t *testing.T) {
	if got := New().SourceType(); got != "grafana" {
		t.Errorf("SourceType() = %q, want grafana", got)
	}
	if SourceType != "grafana" {
		t.Errorf("SourceType const = %q, want grafana", SourceType)
	}
	// compile-time interface satisfaction is also enforced by the package's var _ observability.Exporter guard.
	var _ observability.Exporter = New()
}

// TestExportIsProvisioningNoop locks the documented contract that Grafana is a provisioning backend, not a
// sample sink: Export must return nil (never error, never "drop") for a nil batch AND for a non-empty batch
// of stamped or unstamped samples. A provisioning backend ships no series, so INV-15 freshness stamping
// does not apply here — but it must still register cleanly under the Exporter surface, which the acceptance
// (grafana_steps_test.go) relies on by calling Export(ctx, nil) and requiring nil.
func TestExportIsProvisioningNoop(t *testing.T) {
	m := New()
	ctx := context.Background()

	if err := m.Export(ctx, nil); err != nil {
		t.Errorf("Export(nil) must be a provisioning no-op returning nil, got %v", err)
	}

	samples := []observability.Sample{
		{Name: "tg_up", Value: 1, Labels: map[string]string{"component": "grounder"}},                  // unstamped
		{Name: "tg_up", Value: 1, Stamped: time.Date(2026, 7, 15, 12, 0, 0, 0, time.UTC), Labels: nil}, // stamped
	}
	if err := m.Export(ctx, samples); err != nil {
		t.Errorf("Export(samples) on a provisioning backend must return nil (no sink to ship to), got %v", err)
	}
}

// TestProvisionAndDetectDrift is the core protocol: after provisioning a dashboard from its version-
// controlled definition, an untouched dashboard (matching hash) is NOT drift, while a hand-edited panel
// (divergent hash for the same UID) IS drift and must be rejected. This mirrors the acceptance scenario
// "hand-edited panels are rejected as drift".
func TestProvisionAndDetectDrift(t *testing.T) {
	m := New()
	m.Provision([]Dashboard{{UID: "cp", Hash: "v1"}})

	if m.DetectDrift(Dashboard{UID: "cp", Hash: "v1"}) {
		t.Error("an untouched, version-controlled dashboard (matching hash) must NOT be flagged as drift")
	}
	if !m.DetectDrift(Dashboard{UID: "cp", Hash: "EDITED"}) {
		t.Error("a hand-edited panel (divergent hash, same UID) must be rejected as drift")
	}
}

// TestDetectDriftUnprovisionedIsNotDrift locks the deliberate branch: a UID that was never provisioned has
// no committed definition to diverge from, so it is NOT drift (returns false) regardless of its live hash.
// Flagging an unknown UID as drift would page on dashboards the control plane never claimed to own.
func TestDetectDriftUnprovisionedIsNotDrift(t *testing.T) {
	m := New()
	// nothing provisioned at all
	if m.DetectDrift(Dashboard{UID: "never-seen", Hash: "whatever"}) {
		t.Error("a UID with no version-controlled definition must NOT be drift (no baseline to diverge from)")
	}
	// a different UID is provisioned, but the queried one still is not
	m.Provision([]Dashboard{{UID: "cp", Hash: "v1"}})
	if m.DetectDrift(Dashboard{UID: "other", Hash: "v1"}) {
		t.Error("an unprovisioned UID must NOT be drift even when a same-hash sibling UID exists")
	}
}

// TestReprovisionOverwritesBaseline locks the documented "re-provisioning a UID overwrites its baseline
// with the newly committed hash": after a new definition is provisioned, the new hash is the fact (not
// drift) and the OLD hash now reads as a hand-edited divergence (drift).
func TestReprovisionOverwritesBaseline(t *testing.T) {
	m := New()
	m.Provision([]Dashboard{{UID: "cp", Hash: "v1"}})
	m.Provision([]Dashboard{{UID: "cp", Hash: "v2"}}) // a new commit lands the v2 definition

	if m.DetectDrift(Dashboard{UID: "cp", Hash: "v2"}) {
		t.Error("the newly committed hash must be the drift baseline (v2 is the fact, not drift)")
	}
	if !m.DetectDrift(Dashboard{UID: "cp", Hash: "v1"}) {
		t.Error("after re-provisioning, the superseded hash must read as drift (divergent from the new baseline)")
	}
}

// TestProvisionMultipleDashboardsIndependent verifies each UID carries its own baseline: drift on one
// dashboard does not leak into another, and a matching hash under a different UID is not confused for the
// queried UID's baseline.
func TestProvisionMultipleDashboardsIndependent(t *testing.T) {
	m := New()
	m.Provision([]Dashboard{
		{UID: "cp", Hash: "cp-v1"},
		{UID: "fleet", Hash: "fleet-v1"},
	})

	if m.DetectDrift(Dashboard{UID: "cp", Hash: "cp-v1"}) {
		t.Error("cp at its committed hash must not be drift")
	}
	if m.DetectDrift(Dashboard{UID: "fleet", Hash: "fleet-v1"}) {
		t.Error("fleet at its committed hash must not be drift")
	}
	// cp edited -> cp drifts; fleet, untouched, must remain clean (no cross-UID contamination).
	if !m.DetectDrift(Dashboard{UID: "cp", Hash: "cp-EDITED"}) {
		t.Error("an edited cp must be drift")
	}
	if m.DetectDrift(Dashboard{UID: "fleet", Hash: "fleet-v1"}) {
		t.Error("editing cp must not affect fleet's drift status")
	}
	// fleet's own committed hash must not be accepted as cp's baseline.
	if !m.DetectDrift(Dashboard{UID: "cp", Hash: "fleet-v1"}) {
		t.Error("a hash committed for a different UID must not satisfy cp's baseline (drift expected)")
	}
}

// TestConcurrentProvisionAndDetectDrift exercises the RWMutex under -race: concurrent Provision writes and
// DetectDrift reads must not race. It asserts no observable corruption of the final baseline rather than any
// particular interleaving.
func TestConcurrentProvisionAndDetectDrift(t *testing.T) {
	m := New()
	m.Provision([]Dashboard{{UID: "cp", Hash: "v0"}})

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			m.Provision([]Dashboard{{UID: "cp", Hash: "v0"}})
		}()
		go func() {
			defer wg.Done()
			_ = m.DetectDrift(Dashboard{UID: "cp", Hash: "v0"})
		}()
	}
	wg.Wait()

	if m.DetectDrift(Dashboard{UID: "cp", Hash: "v0"}) {
		t.Error("the stable baseline (v0) must not be corrupted by concurrent access")
	}
}
