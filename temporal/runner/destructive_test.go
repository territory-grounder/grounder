package runner

import (
	"testing"

	"github.com/territory-grounder/grounder/core/manifest"
	"github.com/territory-grounder/grounder/core/risk"
	"github.com/territory-grounder/grounder/core/safety"
)

// TG-641, the G3 soak case: a start-guest whose rationale mentions a shutdown that did NOT happen.
func TestDestructiveOf_ProseAboutAShutdownDoesNotHoldAStartGuest(t *testing.T) {
	a := manifest.Action{OpClass: "start-guest", Op: "start", Target: "site01app01", Reversible: true}
	if destructiveOf(a, "the eventlog contains no evidence of a planned shutdown or disabling") {
		t.Fatal("a start-guest must not read destructive because its rationale mentions a shutdown")
	}
	if !destructiveOf(a, "then run shutdown -h now on the node") {
		t.Fatal("a rationale that spells out a shutdown command must still read destructive")
	}
	if !destructiveOf(manifest.Action{OpClass: "restart-service", Op: "dropdb prod", Target: "db01"}, "routine") {
		t.Fatal("a destructive ACTION must read destructive whatever the rationale says")
	}
}

// TG-653: the 2026-11-07 proposal, restart-service pve-cluster.service on a hypervisor, passed classification
// and the fallback approver and was refused only at the leaf. A never-touch unit or container now reads
// destructive at classify time, so the classifier polls and the fallback approver holds.
func TestDestructiveOf_NeverTouchSubjectIsDestructive(t *testing.T) {
	for _, a := range []manifest.Action{
		{OpClass: "restart-service", Op: "restart", Target: "hv01", Params: map[string]string{"unit": "pve-cluster.service"}},
		{OpClass: "restart-service", Op: "restart", Target: "app01", Params: map[string]string{"unit": "sshd"}},
		{OpClass: "restart-service", Op: "restart", Target: "app01", Params: map[string]string{"unit": "docker.service"}},
		{OpClass: "restart-container", Op: "restart", Target: "app01", Params: map[string]string{"container": "tailscale"}},
		{OpClass: "start-service", Op: "start", Target: "app01", Params: map[string]string{"unit": "multi-user.target"}},
	} {
		if !destructiveOf(a, "routine") {
			t.Errorf("%s %v must read destructive (never-touch subject)", a.OpClass, a.Params)
		}
	}
}

// And the heal lanes TG actually runs stay clean: the live allowlisted units and the soak pool's subjects.
func TestDestructiveOf_OrdinaryServiceAndContainerHealsStayClean(t *testing.T) {
	for _, a := range []manifest.Action{
		{OpClass: "restart-service", Op: "restart", Target: "app01", Params: map[string]string{"unit": "nginx"}},
		{OpClass: "start-service", Op: "start", Target: "app01", Params: map[string]string{"unit": "apache2.service"}},
		{OpClass: "restart-service", Op: "restart", Target: "app01", Params: map[string]string{"unit": "habitica"}},
		{OpClass: "start-container", Op: "start", Target: "app01", Params: map[string]string{"container": "linkwarden"}},
		{OpClass: "restart-container", Op: "restart", Target: "app01", Params: map[string]string{"container": "leantime_leantime_1"}},
		{OpClass: "start-guest", Op: "start", Target: "app01", Params: map[string]string{"guest": "app01"}},
	} {
		if destructiveOf(a, "routine") {
			t.Errorf("%s %v must NOT read destructive", a.OpClass, a.Params)
		}
	}
}

// TG-653 end to end through the approver: the exact 2026-11-07 shape (a NOVEL incident, the only first-look
// reason the approver may clear) plus the never-touch subject must come out HELD, never ACTED.
func TestNeverTouchRestartIsHeldByTheFallbackApprover(t *testing.T) {
	a := manifest.Action{OpClass: "restart-service", Op: "restart", Target: "hv01", Params: map[string]string{"unit": "pve-cluster.service"}}
	gi := fbaAutoInput()
	gi.NovelIncident = true
	gi.ServerDestructive = destructiveOf(a, "TargetDown for an NFS stale file handle")
	d := risk.Classify(gi)
	if d.Band != safety.BandPollPause {
		t.Fatalf("a never-touch restart must poll, got %s (%v)", d.Band, d.Signals)
	}
	if r := resolvePollAutonomously(gi, d); !r.Resolved || r.Approve {
		t.Fatalf("the fallback approver must HOLD a never-touch restart, got %+v", r)
	}
	// Vacuity control: the same novel incident on an ordinary unit is ACTED (only a first look was missing).
	gi.ServerDestructive = destructiveOf(manifest.Action{OpClass: "restart-service", Op: "restart", Target: "app01", Params: map[string]string{"unit": "nginx"}}, "routine")
	if r := resolvePollAutonomously(gi, risk.Classify(gi)); !r.Resolved || !r.Approve {
		t.Fatalf("an ordinary novel restart must still be acted on, got %+v", r)
	}
}
