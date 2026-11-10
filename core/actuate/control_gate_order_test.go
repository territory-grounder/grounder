package actuate

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/territory-grounder/grounder/core/attribution"
	"github.com/territory-grounder/grounder/core/manifest"
	"github.com/territory-grounder/grounder/core/safety"
	"github.com/territory-grounder/grounder/core/trace"
)

type gateCapture struct{ rows []trace.GateVerdict }

func (g *gateCapture) Emit(_ context.Context, v trace.GateVerdict) error {
	g.rows = append(g.rows, v)
	return nil
}

func (g *gateCapture) refusedBy() string {
	for _, v := range g.rows {
		if v.Verdict == "refuse" {
			return v.Gate
		}
	}
	return ""
}

const controlScope = "guest01 ctl01 app-db host01"

func controlRequest(t *testing.T, a manifest.Action, band safety.Band) Request {
	t.Helper()
	m, err := manifest.New(a, band, "plan", "prediction")
	if err != nil {
		t.Fatal(err)
	}
	return Request{Manifest: m, Gated: true, Argv: []string{"start", a.Target}, Evidence: boundEvidence(), Observe: noObserved, StillFaulted: stillFaulted, Band: band, Approved: band == safety.BandPollPause}
}

func startOf(guest string) manifest.Action {
	return manifest.Action{OpClass: "start-guest", Op: "start", Target: guest, Params: map[string]string{"guest": guest}, Reversible: true}
}

func TestAControlInsideTheScopeIsStoppedByItsOwnGate(t *testing.T) {
	docker := manifest.Action{OpClass: "restart-service", Op: "restart", Target: "host01", Params: map[string]string{"unit": "docker"}, Reversible: true}
	for name, tc := range map[string]struct {
		action manifest.Action
		band   safety.Band
		tax    attribution.Taxonomy
		gate   string
		want   string
	}{
		"person stopped it":      {startOf("ctl01"), safety.BandPollPause, attribution.AttributedAuthorized, "independent-condition", "made deliberately by"},
		"unsanctioned actor":     {startOf("ctl01"), safety.BandPollPause, attribution.AttributedSuspicious, "independent-condition", "unsanctioned actor"},
		"stateful, approved":     {startOf("app-db"), safety.BandPollPause, attribution.Unattributable, "independent-condition", "names a stateful workload"},
		"stateful, not approved": {startOf("app-db"), safety.BandAuto, attribution.Unattributable, "stateful-floor", "stateful floor (adapter)"},
		"unbound unit":           {docker, safety.BandPollPause, attribution.Unattributable, "independent-condition", "is not configured for autonomous action"},
		"outside the scope":      {startOf("other01"), safety.BandPollPause, attribution.Unattributable, "actuation-scope", "outside the configured actuation scope"},
	} {
		act := &fakeActuator{}
		c := guestCondition(tc.tax, condEvent("vzstop", "someone@pam", time.Minute))
		i := wired(safety.NewActuatingChokepoint(), act).WithActuationScope(ParseActuationScope(controlScope)).WithIndependentCondition(true, c)
		gates := &gateCapture{}
		i = i.WithGateVerdictSink(gates)
		out, err := i.Do(context.Background(), controlRequest(t, tc.action, tc.band))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if !out.Refused || act.execs != 0 || !strings.Contains(out.Reason, tc.want) || gates.refusedBy() != tc.gate {
			t.Errorf("%s: refused=%v execs=%d gate=%q reason=%q, want gate %q naming %q", name, out.Refused, act.execs, gates.refusedBy(), out.Reason, tc.gate, tc.want)
		}
	}
}

func TestAnOrdinaryCrashInsideTheScopeStillHeals(t *testing.T) {
	act := &fakeActuator{}
	c := guestCondition(attribution.Unattributable, condEvent("vzstop", "x", time.Minute))
	i := wired(safety.NewActuatingChokepoint(), act).WithActuationScope(ParseActuationScope(controlScope)).WithIndependentCondition(true, c)
	out, err := i.Do(context.Background(), controlRequest(t, startOf("guest01"), safety.BandPollPause))
	if err != nil {
		t.Fatal(err)
	}
	if out.Refused || act.execs != 1 {
		t.Fatalf("refused=%v execs=%d reason=%q", out.Refused, act.execs, out.Reason)
	}
}

func TestRefuseAllScopeRefusesEveryEffect(t *testing.T) {
	act := &fakeActuator{}
	c := guestCondition(attribution.Unattributable, condEvent("vzstop", "x", time.Minute))
	i := wired(safety.NewActuatingChokepoint(), act).WithActuationScope(RefuseAllScope()).WithIndependentCondition(true, c)
	out, err := i.Do(context.Background(), controlRequest(t, startOf("guest01"), safety.BandPollPause))
	if err != nil {
		t.Fatal(err)
	}
	if !out.Refused || act.execs != 0 || !strings.Contains(out.Reason, "outside the configured actuation scope") {
		t.Fatalf("refused=%v execs=%d reason=%q", out.Refused, act.execs, out.Reason)
	}
}
