package main

import (
	"context"
	"strings"
	"testing"

	"github.com/territory-grounder/grounder/core/credential"
	"github.com/territory-grounder/grounder/core/verify"
	"github.com/territory-grounder/grounder/temporal/runner"
)

// TG-625: the actuation-only plane reads post-state from the ledger (its LibreNMS /alerts read is 403 by
// design, TG-337); every other plane keeps the independent monitor unless the operator says otherwise.
func TestPostStateSourceByPlane(t *testing.T) {
	env := func(v string) func(k, def string) string {
		return func(k, def string) string {
			if k == "TG_POSTSTATE_SOURCE" && v != "" {
				return v
			}
			return def
		}
	}
	cases := []struct {
		val   string
		plane credential.ProcessPlane
		want  string
	}{
		{"", credential.ProcessPlaneActuation, "ledger"},
		{"", credential.ProcessPlaneTriage, "librenms"},
		{"", credential.ProcessPlaneBoth, "librenms"}, // the default deployment stays byte-identical
		{"auto", credential.ProcessPlaneActuation, "ledger"},
		{"librenms", credential.ProcessPlaneActuation, "librenms"}, // the rollback knob
		{"LEDGER", credential.ProcessPlaneTriage, "ledger"},
		{"bogus", credential.ProcessPlaneTriage, "librenms"},
	}
	for _, c := range cases {
		if got := postStateSource(env(c.val), c.plane); got != c.want {
			t.Errorf("TG_POSTSTATE_SOURCE=%q plane=%s: got %q, want %q", c.val, c.plane, got, c.want)
		}
	}
}

// The assignment itself: the PAIR reader must back the verdict and the HOST reader the clear/necessity check.
// Swapping them, or dropping either, is a silent regression this pins.
func TestApplyLedgerReadersAssignsBothReaders(t *testing.T) {
	var deps runner.Deps
	pairs := func(context.Context, string, string) ([]verify.ObservedAlert, bool) {
		return []verify.ObservedAlert{{Host: "h", Rule: "pair"}}, true
	}
	hosts := func(context.Context, string, string) ([]verify.ObservedAlert, bool) {
		return []verify.ObservedAlert{{Host: "h"}}, true
	}
	applyLedgerReaders(&deps, pairs, hosts)
	if deps.PostStateObserve == nil || deps.ClearObserve == nil {
		t.Fatal("both readers must be installed")
	}
	if got, _ := deps.PostStateObserve(context.Background(), "", ""); len(got) != 1 || got[0].Rule != "pair" {
		t.Fatalf("PostStateObserve must be the PAIR reader (the verdict needs rules), got %+v", got)
	}
	if got, _ := deps.ClearObserve(context.Background(), "", ""); len(got) != 1 || got[0].Rule != "" {
		t.Fatalf("ClearObserve must be the HOST reader (host-quiet, the TG-454 fallback's granularity), got %+v", got)
	}
}

// ORDER IN main(): the falsify scorer captures deps.PostStateObserve BY VALUE and must keep the LibreNMS
// reader; runner.NewActivities copies deps and must see the ledger readers. Pin both orderings.
func TestWirePostStateSourceSitsBetweenFalsifyAndNewActivities(t *testing.T) {
	src := stripGoComments(readWorkerMain(t))
	f := strings.Index(src, "falsify.Observer(deps.PostStateObserve)")
	w := strings.Index(src, "wirePostStateSource(&deps, dbPool, credentialPlane)")
	n := strings.Index(src, "runner.NewActivities(deps)")
	if f < 0 || w < 0 || n < 0 {
		t.Fatalf("pin lost its subjects (falsify=%d wire=%d newActivities=%d)", f, w, n)
	}
	if !(f < w && w < n) {
		t.Fatalf("wirePostStateSource must run AFTER the falsify capture and BEFORE NewActivities (falsify=%d wire=%d newActivities=%d)", f, w, n)
	}
}
