package main

import (
	"testing"
	"time"

	"github.com/territory-grounder/grounder/core/attribution"
	"github.com/territory-grounder/grounder/core/metrics"
)

// THE LAPSE OF A CARVE-OUT MUST BE ALERTABLE, NOT JUST LOGGED ONCE AT BOOT.
//
// A carve-out is a bounded suspension of the security path: inside it, the harness's sanctioned faults
// resolve to authorized-test and TG heals them. Making the bound mandatory (REQ-2309) closed a real hole —
// both live carve-outs had NO bound and were therefore permanent — but it also put a date on the learning
// regime. Past that date the same faults revert toward stand-down, which withholds actuation. The estate
// keeps running, TG stops healing, and the config file still lists every host.
//
// A boot log alone cannot carry that: it is printed once, days or weeks before the lapse, into a stream
// nobody re-reads. So the remaining life has to be on /metrics, where a threshold alert can fire. This test
// drives the REAL samples() assembly — not CarveOutExpiries directly — because the defect class it guards
// against is a correct derivation with no reachable call site.
func TestCarveOutExpiryReachesMetrics(t *testing.T) {
	now := time.Now().UTC()
	adm, _, _ := newTestAdmin(t, "", false)
	adm = adm.withCarveOuts([]attribution.CarveOut{
		{ID: "healthy-pool", Domain: "pve", ValidFrom: now.Add(-24 * time.Hour), ValidUntil: now.Add(60 * 24 * time.Hour)},
		{ID: "lapsing-soon", Domain: "journal", ValidFrom: now.Add(-24 * time.Hour), ValidUntil: now.Add(3 * 24 * time.Hour)},
		{ID: "already-lapsed", Domain: "netbox", ValidFrom: now.Add(-48 * time.Hour), ValidUntil: now.Add(-1 * time.Hour)},
		{ID: "no-bound-at-all", Domain: "awx"}, // the live defect's shape
	})

	remaining := map[string]float64{}
	expired := map[string]float64{}
	for _, s := range adm.samples() {
		switch s.Name {
		case "tg_attribution_carveout_remaining_seconds":
			if s.Kind != metrics.Gauge {
				t.Errorf("remaining_seconds must be a gauge, got %v", s.Kind)
			}
			remaining[s.Labels["carve_out"]] = s.Value
		case "tg_attribution_carveout_expired":
			expired[s.Labels["carve_out"]] = s.Value
		}
	}

	// EVERY carve-out emits BOTH series. The defect this guards is inferring "expired" from a missing series:
	// an absent series and a zero series look the same on a graph and behave differently in an alert.
	for _, id := range []string{"healthy-pool", "lapsing-soon", "already-lapsed", "no-bound-at-all"} {
		if _, ok := remaining[id]; !ok {
			t.Errorf("carve-out %q emitted no remaining_seconds series", id)
		}
		if _, ok := expired[id]; !ok {
			t.Errorf("carve-out %q emitted no expired series", id)
		}
	}

	// The SIGN is the whole point: an alert reads "remaining < 604800", so a lapsed carve-out must go
	// negative rather than clamp at zero.
	if remaining["already-lapsed"] >= 0 {
		t.Errorf("a lapsed carve-out must report NEGATIVE remaining seconds, got %.0f — clamping at zero makes "+
			"'expired' indistinguishable from 'expires this instant' in a threshold alert", remaining["already-lapsed"])
	}
	if remaining["healthy-pool"] <= 0 {
		t.Errorf("an in-force carve-out must report positive remaining, got %.0f", remaining["healthy-pool"])
	}
	if expired["already-lapsed"] != 1 || expired["healthy-pool"] != 0 {
		t.Errorf("expired flags wrong: lapsed=%v healthy=%v", expired["already-lapsed"], expired["healthy-pool"])
	}

	// ★ AN UNBOUNDED CARVE-OUT REPORTS EXPIRED, NOT INFINITE. This is the reading that matters for the live
	// defect: a carve-out with no bound now matches nothing, so the honest signal is "not in force". Reporting
	// it as healthy — or omitting it — would hide exactly the configuration that caused the problem.
	if expired["no-bound-at-all"] != 1 {
		t.Errorf("a carve-out with NO bound must report expired=1 (it matches nothing), got %v", expired["no-bound-at-all"])
	}

	// And with no carve-outs declared there is no suspension to report the end of, so no series at all — an
	// invented zero would claim a security exception exists where none does.
	bare, _, _ := newTestAdmin(t, "", false)
	for _, s := range bare.samples() {
		if s.Name == "tg_attribution_carveout_remaining_seconds" || s.Name == "tg_attribution_carveout_expired" {
			t.Errorf("an estate with no carve-outs must emit no carve-out series, got %s%v", s.Name, s.Labels)
		}
	}
}

// The renewal-warning classification, over the boundary rather than one comfortable sample.
func TestCarveOutRenewalWarningBoundary(t *testing.T) {
	now := time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC)
	mk := func(d time.Duration) attribution.CarveOut {
		return attribution.CarveOut{ID: "x", Domain: "pve", ValidFrom: now.Add(-time.Hour), ValidUntil: now.Add(d)}
	}
	for _, c := range []struct {
		name       string
		in         time.Duration
		wantRenew  bool
		wantExpire bool
	}{
		{"comfortably in force", 30 * 24 * time.Hour, false, false},
		{"one second outside the warning", attribution.CarveOutRenewalWarning + time.Second, false, false},
		{"exactly at the warning boundary", attribution.CarveOutRenewalWarning, true, false},
		{"inside the warning", 24 * time.Hour, true, false},
		{"lapsed one second ago", -time.Second, true, true},
	} {
		got := attribution.CarveOutExpiries(attribution.Config{CarveOuts: []attribution.CarveOut{mk(c.in)}}, now)
		if len(got) != 1 {
			t.Fatalf("%s: want 1 expiry, got %d", c.name, len(got))
		}
		if got[0].Renew != c.wantRenew || got[0].Expired != c.wantExpire {
			t.Errorf("%s: renew=%v expired=%v, want renew=%v expired=%v",
				c.name, got[0].Renew, got[0].Expired, c.wantRenew, c.wantExpire)
		}
	}
}
