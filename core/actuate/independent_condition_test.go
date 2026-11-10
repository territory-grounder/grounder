package actuate

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/territory-grounder/grounder/core/attribution"
	"github.com/territory-grounder/grounder/core/manifest"
	"github.com/territory-grounder/grounder/core/safety"
)

var condNow = time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC)

func condEvent(kind, actor string, ago time.Duration) attribution.Evidence {
	return attribution.Evidence{Domain: "pve", Actor: actor, ActionKind: kind, ObservedAt: condNow.Add(-ago), Covered: true}
}

func condClassifier(t attribution.Taxonomy) func(string, []attribution.Evidence) attribution.Taxonomy {
	return func(string, []attribution.Evidence) attribution.Taxonomy { return t }
}

func ranRecently(v bool, err error) func(context.Context, string, time.Duration) (bool, error) {
	return func(context.Context, string, time.Duration) (bool, error) { return v, err }
}

func guestCondition(tax attribution.Taxonomy, ev ...attribution.Evidence) *IndependentCondition {
	return &IndependentCondition{
		Now:          func() time.Time { return condNow },
		Classify:     condClassifier(tax),
		RanRecently:  ranRecently(true, nil),
		LastExecuted: neverExecuted,
		GuestEvidence: func(context.Context, string, time.Time, time.Time) ([]attribution.Evidence, error) {
			return ev, nil
		},
	}
}

var startGuestAction = manifest.Action{OpClass: "start-guest", Target: "guest01", Params: map[string]string{"guest": "guest01"}}

func TestGuestConditionRefusesADeliberateStop(t *testing.T) {
	for name, tax := range map[string]attribution.Taxonomy{
		"person":       attribution.AttributedAuthorized,
		"unsanctioned": attribution.AttributedSuspicious,
	} {
		c := guestCondition(tax, condEvent("vzstop", "someone@pam", time.Minute))
		allow, reason, ok := c.Check(context.Background(), startGuestAction)
		if allow || !ok || reason == "" {
			t.Errorf("%s: allow=%v ok=%v reason=%q, want a reasoned refusal", name, allow, ok, reason)
		}
	}
}

func TestGuestConditionAllowsAnUnexplainedOrSanctionedTestStop(t *testing.T) {
	for name, tax := range map[string]attribution.Taxonomy{
		"crash":      attribution.Unattributable,
		"carve-out":  attribution.AuthorizedTest,
		"own action": attribution.AttributedSelf,
	} {
		if allow, _, ok := guestCondition(tax, condEvent("qmstop", "x", time.Minute)).Check(context.Background(), startGuestAction); !allow || !ok {
			t.Errorf("%s: allow=%v ok=%v, want allowed", name, allow, ok)
		}
	}
}

func TestGuestConditionReadsOnlyTheLatestLifecycleEvent(t *testing.T) {
	c := guestCondition(attribution.AttributedAuthorized,
		condEvent("vzstop", "someone@pam", 3*time.Hour),
		condEvent("vzstart", "someone@pam", time.Hour))
	if allow, _, ok := c.Check(context.Background(), startGuestAction); !allow || !ok {
		t.Fatalf("an old stop followed by a start must not refuse a later crash: allow=%v ok=%v", allow, ok)
	}
	c = guestCondition(attribution.AttributedAuthorized,
		condEvent("vzstart", "someone@pam", 3*time.Hour),
		condEvent("vzstop", "someone@pam", time.Hour))
	if allow, _, _ := c.Check(context.Background(), startGuestAction); allow {
		t.Fatal("a stop after a start is the latest event and must refuse")
	}
}

func TestGuestConditionLooksBackBeyondTheTriageWindow(t *testing.T) {
	var since time.Time
	c := &IndependentCondition{
		Now:          func() time.Time { return condNow },
		Classify:     condClassifier(attribution.AttributedAuthorized),
		RanRecently:  ranRecently(true, nil),
		LastExecuted: neverExecuted,
		GuestEvidence: func(_ context.Context, _ string, s, _ time.Time) ([]attribution.Evidence, error) {
			since = s
			return []attribution.Evidence{condEvent("vzstop", "someone@pam", 6*time.Hour)}, nil
		},
	}
	if allow, _, _ := c.Check(context.Background(), startGuestAction); allow {
		t.Fatal("a deliberate stop older than the triage window must still refuse")
	}
	if got := condNow.Sub(since); got != IndependentConditionLookback {
		t.Fatalf("lookback = %s, want %s", got, IndependentConditionLookback)
	}
}

func TestGuestConditionRefusesAStatefulGuestEvenAfterACrash(t *testing.T) {
	for name, a := range map[string]manifest.Action{
		"named in target and guest": {OpClass: "start-guest", Target: "app-db", Params: map[string]string{"guest": "app-db"}},
		"database engine in name":   {OpClass: "start-guest", Target: "site01postgres01", Params: map[string]string{"guest": "site01postgres01"}},
		"named only in the guest":   {OpClass: "start-guest", Target: "guest01", Params: map[string]string{"guest": "app-db"}},
		"named only in the target":  {OpClass: "start-guest", Target: "app-db", Params: map[string]string{"guest": "guest01"}},
		"no guest param":            {OpClass: "start-guest", Target: "app-db"},
	} {
		c := guestCondition(attribution.Unattributable, condEvent("qmstop", "x", time.Minute))
		allow, reason, ok := c.Check(context.Background(), a)
		if allow || !ok || !strings.Contains(reason, "stateful") {
			t.Errorf("%s: allow=%v ok=%v reason=%q, want a stateful refusal", name, allow, ok, reason)
		}
	}
}

func TestGuestConditionKeepsAnOrdinaryGuestAfterACrash(t *testing.T) {
	a := manifest.Action{OpClass: "start-guest", Target: "site01app01", Params: map[string]string{"guest": "site01app01"}}
	c := guestCondition(attribution.Unattributable, condEvent("qmstop", "x", time.Minute))
	if allow, reason, ok := c.Check(context.Background(), a); !allow || !ok {
		t.Errorf("allow=%v ok=%v reason=%q, want allowed", allow, ok, reason)
	}
}

func TestGuestConditionFailsClosedOnAReadError(t *testing.T) {
	c := &IndependentCondition{
		Now:          func() time.Time { return condNow },
		Classify:     condClassifier(attribution.Unattributable),
		RanRecently:  ranRecently(true, nil),
		LastExecuted: neverExecuted,
		GuestEvidence: func(context.Context, string, time.Time, time.Time) ([]attribution.Evidence, error) {
			return nil, errors.New("403")
		},
	}
	if allow, _, ok := c.Check(context.Background(), startGuestAction); allow || ok {
		t.Fatalf("allow=%v ok=%v, want an unreadable result", allow, ok)
	}
	if allow, _, ok := (&IndependentCondition{LastExecuted: neverExecuted}).Check(context.Background(), startGuestAction); allow || ok {
		t.Fatalf("unconfigured source: allow=%v ok=%v, want an unreadable result", allow, ok)
	}
}

func alertCondition(hosts map[string]bool, err error) *IndependentCondition {
	return &IndependentCondition{AlertingHosts: func(context.Context) (map[string]bool, error) { return hosts, err }}
}

var restartServiceAction = manifest.Action{OpClass: "restart-service", Target: "host01.example.net", Params: map[string]string{"unit": "nginx"}}

func neverExecuted(context.Context, string) (ExecutionRecord, bool, error) {
	return ExecutionRecord{}, false, nil
}

func probing(results ...ProbeResult) func(context.Context, string) ProbeResult {
	i := 0
	return func(context.Context, string) ProbeResult {
		r := results[i%len(results)]
		i++
		return r
	}
}

func boundCondition(results ...ProbeResult) *IndependentCondition {
	return &IndependentCondition{
		Bindings:     map[string][]ResourceBinding{"host01": {{Kind: "unit", Name: "nginx", Probe: "tcp://h:1", Ops: []string{"restart-service", "start-service"}}}},
		Probe:        probing(results...),
		ProbeGap:     time.Millisecond,
		LastExecuted: neverExecuted,
	}
}

func TestServiceActionRunsOnlyWhenTheBoundObjectFailsTwice(t *testing.T) {
	if allow, _, ok := boundCondition(ProbeDown).Check(context.Background(), restartServiceAction); !allow || !ok {
		t.Fatalf("a down service: allow=%v ok=%v", allow, ok)
	}
}

func TestAnUnrelatedAlertDoesNotAuthorizeAHealthyService(t *testing.T) {
	c := boundCondition(ProbeUp)
	c.AlertingHosts = func(context.Context) (map[string]bool, error) { return map[string]bool{"host01": true}, nil }
	allow, reason, ok := c.Check(context.Background(), restartServiceAction)
	if allow || !ok || !strings.Contains(reason, "answering") {
		t.Fatalf("allow=%v ok=%v reason=%q", allow, ok, reason)
	}
}

func TestWrongServiceIsRefused(t *testing.T) {
	a := manifest.Action{OpClass: "restart-service", Target: "host01", Params: map[string]string{"unit": "sshd"}}
	if allow, reason, ok := boundCondition(ProbeDown).Check(context.Background(), a); allow || !ok || !strings.Contains(reason, "not configured for autonomous") {
		t.Fatalf("allow=%v ok=%v reason=%q", allow, ok, reason)
	}
}

func TestWrongHostIsRefused(t *testing.T) {
	a := manifest.Action{OpClass: "restart-service", Target: "host02", Params: map[string]string{"unit": "nginx"}}
	if allow, reason, ok := boundCondition(ProbeDown).Check(context.Background(), a); allow || !ok || !strings.Contains(reason, "not configured for autonomous") {
		t.Fatalf("allow=%v ok=%v reason=%q", allow, ok, reason)
	}
}

func TestWrongActionIsRefused(t *testing.T) {
	c := boundCondition(ProbeDown)
	c.Bindings["host01"][0].Ops = []string{"start-service"}
	allow, reason, ok := c.Check(context.Background(), restartServiceAction)
	if allow || !ok || !strings.Contains(reason, "not configured") {
		t.Fatalf("allow=%v ok=%v reason=%q", allow, ok, reason)
	}
}

func withPrior(c *IndependentCondition, rec ExecutionRecord) *IndependentCondition {
	c.Now = func() time.Time { return condNow }
	c.LastExecuted = func(context.Context, string) (ExecutionRecord, bool, error) { return rec, true, nil }
	return c
}

func TestARepeatAfterACleanSuccessIsARecurrenceAndStaysAvailable(t *testing.T) {
	c := withPrior(boundCondition(ProbeDown), ExecutionRecord{At: condNow.Add(-time.Minute), Verdict: "match"})
	allow, reason, ok := c.Check(context.Background(), restartServiceAction)
	if !allow || !ok || !strings.Contains(reason, "recurrence") {
		t.Fatalf("allow=%v ok=%v reason=%q", allow, ok, reason)
	}
}

func TestAGuestRecurrenceStillNeedsItsOwnChecks(t *testing.T) {
	prior := ExecutionRecord{At: condNow.Add(-time.Minute), Verdict: "match"}
	allowed := withPrior(guestCondition(attribution.Unattributable), prior)
	allow, reason, ok := allowed.Check(context.Background(), startGuestAction)
	if !allow || !ok || !strings.Contains(reason, "recurrence") {
		t.Fatalf("a crashed guest after a clean heal: allow=%v ok=%v reason=%q", allow, ok, reason)
	}
	person := withPrior(guestCondition(attribution.AttributedAuthorized, condEvent("vzstop", "someone@pam", time.Second)), prior)
	if allow, _, ok := person.Check(context.Background(), startGuestAction); allow || !ok {
		t.Fatalf("a deliberate stop after a clean heal: allow=%v ok=%v", allow, ok)
	}
}

func TestARecurrenceStillNeedsTheLiveCheck(t *testing.T) {
	c := withPrior(boundCondition(ProbeUp), ExecutionRecord{At: condNow.Add(-time.Minute), Verdict: "match"})
	if allow, reason, ok := c.Check(context.Background(), restartServiceAction); allow || !ok || !strings.Contains(reason, "answering") {
		t.Fatalf("a healthy object must refuse a replay: allow=%v ok=%v reason=%q", allow, ok, reason)
	}
}

func TestARepeatAfterAnUnsettledResultIsVisiblyDeferred(t *testing.T) {
	for _, rec := range []ExecutionRecord{
		{At: condNow.Add(-time.Minute), Verdict: "deviation"},
		{At: condNow.Add(-time.Minute), Verdict: "indeterminate"},
		{At: condNow.Add(-time.Minute), Verdict: "match", Unverifiable: true},
		{At: condNow.Add(-time.Minute)},
	} {
		c := withPrior(boundCondition(ProbeDown), rec)
		allow, reason, ok := c.Check(context.Background(), restartServiceAction)
		if allow || !ok || !strings.Contains(reason, "reconsidered") {
			t.Fatalf("%+v: allow=%v ok=%v reason=%q", rec, allow, ok, reason)
		}
	}
}

func TestARepeatIsJudgedAfreshAfterTheWindow(t *testing.T) {
	c := withPrior(boundCondition(ProbeDown), ExecutionRecord{At: condNow.Add(-DuplicateWindow - time.Minute), Verdict: "deviation"})
	if allow, _, ok := c.Check(context.Background(), restartServiceAction); !allow || !ok {
		t.Fatalf("allow=%v ok=%v, want a fresh judgement after the window", allow, ok)
	}
}

func TestTwoFreshProbesHitTheExactBoundTarget(t *testing.T) {
	var targets []string
	var at []time.Time
	c := boundCondition(ProbeDown)
	c.Bindings["host01"] = append(c.Bindings["host01"], ResourceBinding{Kind: "unit", Name: "other", Probe: "tcp://h:9", Ops: []string{"restart-service"}})
	c.ProbeGap = 25 * time.Millisecond
	c.Probe = func(_ context.Context, target string) ProbeResult {
		targets, at = append(targets, target), append(at, time.Now())
		return ProbeDown
	}
	if allow, _, ok := c.Check(context.Background(), restartServiceAction); !allow || !ok {
		t.Fatalf("allow=%v ok=%v", allow, ok)
	}
	if len(targets) != 2 || targets[0] != "tcp://h:1" || targets[1] != "tcp://h:1" {
		t.Fatalf("probe targets = %v, want the bound target twice and not the other object's", targets)
	}
	if got := at[1].Sub(at[0]); got < c.ProbeGap {
		t.Fatalf("the samples were %s apart, want at least %s", got, c.ProbeGap)
	}
}

func TestDuplicateCheckFailsClosed(t *testing.T) {
	c := boundCondition(ProbeDown)
	c.LastExecuted = nil
	if allow, _, ok := c.Check(context.Background(), restartServiceAction); allow || ok {
		t.Fatalf("no reader: allow=%v ok=%v", allow, ok)
	}
	c.LastExecuted = func(context.Context, string) (ExecutionRecord, bool, error) {
		return ExecutionRecord{}, false, errors.New("db")
	}
	if allow, _, ok := c.Check(context.Background(), restartServiceAction); allow || ok {
		t.Fatalf("read error: allow=%v ok=%v", allow, ok)
	}
}

func unitAction(host, unit string) manifest.Action {
	return manifest.Action{OpClass: "restart-service", Target: host, Params: map[string]string{"unit": unit}}
}

func TestBothSpellingsOfABoundUnitAreAccepted(t *testing.T) {
	for _, unit := range []string{"nginx", "nginx.service"} {
		if allow, _, ok := boundCondition(ProbeDown).Check(context.Background(), unitAction("host01", unit)); !allow || !ok {
			t.Fatalf("%s: allow=%v ok=%v", unit, allow, ok)
		}
	}
}

func TestOnlyTheServiceSuffixIsEquivalent(t *testing.T) {
	for name, unit := range map[string]string{
		"different unit":   "nginx-proxy.service",
		"different prefix": "my-nginx",
		"socket unit":      "nginx.socket",
		"timer unit":       "nginx.timer",
		"path unit":        "nginx.path",
		"different case":   "NGINX.service",
		"suffix twice":     "nginx.service.service",
	} {
		if allow, reason, ok := boundCondition(ProbeDown).Check(context.Background(), unitAction("host01", unit)); allow || !ok || !strings.Contains(reason, "not configured for autonomous") {
			t.Errorf("%s (%s): allow=%v ok=%v reason=%q", name, unit, allow, ok, reason)
		}
	}
}

func TestATemplateInstanceIsItsOwnObject(t *testing.T) {
	c := boundCondition(ProbeDown)
	c.Bindings["host01"] = []ResourceBinding{{Kind: "unit", Name: "getty@tty1", Probe: "tcp://h:1", Ops: []string{"restart-service"}}}
	if allow, _, ok := c.Check(context.Background(), unitAction("host01", "getty@tty1.service")); !allow || !ok {
		t.Fatalf("the bound instance: allow=%v ok=%v", allow, ok)
	}
	for _, unit := range []string{"getty@tty2.service", "getty@.service", "getty"} {
		if allow, _, ok := c.Check(context.Background(), unitAction("host01", unit)); allow || !ok {
			t.Errorf("%s: allow=%v ok=%v", unit, allow, ok)
		}
	}
}

func TestABoundUnitOnTheWrongHostIsRefusedInBothSpellings(t *testing.T) {
	for _, unit := range []string{"nginx", "nginx.service"} {
		if allow, _, ok := boundCondition(ProbeDown).Check(context.Background(), unitAction("host02", unit)); allow || !ok {
			t.Errorf("%s: allow=%v ok=%v", unit, allow, ok)
		}
	}
}

func TestAContainerNameIsMatchedExactly(t *testing.T) {
	c := boundCondition(ProbeDown)
	c.Bindings["host01"] = []ResourceBinding{{Kind: "container", Name: "app.service", Probe: "tcp://h:1", Ops: []string{"restart-container"}}}
	exact := manifest.Action{OpClass: "restart-container", Target: "host01", Params: map[string]string{"container": "app.service"}}
	if allow, _, ok := c.Check(context.Background(), exact); !allow || !ok {
		t.Fatalf("the exact container name must match: allow=%v ok=%v", allow, ok)
	}
	shorter := manifest.Action{OpClass: "restart-container", Target: "host01", Params: map[string]string{"container": "app"}}
	if allow, _, ok := c.Check(context.Background(), shorter); allow || !ok {
		t.Fatalf("a different container name must not match: allow=%v ok=%v", allow, ok)
	}
}

func TestWrongKindIsRefused(t *testing.T) {
	a := manifest.Action{OpClass: "restart-container", Target: "host01", Params: map[string]string{"container": "nginx"}}
	if allow, _, ok := boundCondition(ProbeDown).Check(context.Background(), a); allow || !ok {
		t.Fatalf("allow=%v ok=%v", allow, ok)
	}
}

func TestConflictingSamplesAreRefused(t *testing.T) {
	for _, seq := range [][]ProbeResult{{ProbeDown, ProbeUp}, {ProbeUp, ProbeDown}} {
		allow, reason, ok := boundCondition(seq...).Check(context.Background(), restartServiceAction)
		if allow || !ok || !strings.Contains(reason, "conflicting") {
			t.Fatalf("%v: allow=%v ok=%v reason=%q", seq, allow, ok, reason)
		}
	}
}

func TestUnknownProbeFailsClosed(t *testing.T) {
	for _, seq := range [][]ProbeResult{{ProbeUnknown}, {ProbeDown, ProbeUnknown}, {ProbeUnknown, ProbeDown}} {
		if allow, _, ok := boundCondition(seq...).Check(context.Background(), restartServiceAction); allow || ok {
			t.Fatalf("%v: allow=%v ok=%v, want unreadable", seq, allow, ok)
		}
	}
}

func TestAnAmbiguousBindingIsRefused(t *testing.T) {
	c := boundCondition(ProbeDown)
	c.Bindings["host01"] = append(c.Bindings["host01"], ResourceBinding{Kind: "unit", Name: "nginx", Probe: "tcp://h:2"})
	if allow, _, ok := c.Check(context.Background(), restartServiceAction); allow || !ok {
		t.Fatalf("allow=%v ok=%v", allow, ok)
	}
}

func TestServiceActionFailsClosedWithoutAProbe(t *testing.T) {
	c := boundCondition(ProbeDown)
	c.Probe = nil
	if allow, _, ok := c.Check(context.Background(), restartServiceAction); allow || ok {
		t.Fatalf("allow=%v ok=%v, want unreadable", allow, ok)
	}
}

func TestNoBindingsAuthorizeNothing(t *testing.T) {
	c := &IndependentCondition{Probe: probing(ProbeDown), ProbeGap: time.Millisecond, LastExecuted: neverExecuted}
	if allow, _, ok := c.Check(context.Background(), restartServiceAction); allow || !ok {
		t.Fatalf("allow=%v ok=%v", allow, ok)
	}
}

func TestConditionRefusesAnActionItCannotCheck(t *testing.T) {
	for name, a := range map[string]manifest.Action{
		"no target": {OpClass: "restart-service", Params: map[string]string{"unit": "nginx"}},
		"unknown":   {OpClass: "no-such-class", Target: "host01"},
	} {
		allow, reason, ok := alertCondition(AlertingHostSet([]string{"host01"}), nil).Check(context.Background(), a)
		if allow || !ok || reason == "" {
			t.Errorf("%s: allow=%v ok=%v reason=%q, want a reasoned refusal", name, allow, ok, reason)
		}
	}
}

type stubCondition struct {
	allow, ok   bool
	undoCalled  bool
	checkCalled bool
}

func (s *stubCondition) Check(context.Context, manifest.Action) (bool, string, bool) {
	s.checkCalled = true
	return s.allow, "because", s.ok
}

func (s *stubCondition) CheckUndo(context.Context, manifest.Action, UndoFacts) (bool, string, bool) {
	s.undoCalled = true
	return s.allow, "because", s.ok
}

func TestInterceptorRefusesWhenTheConditionIsRequiredAndUnwired(t *testing.T) {
	act := &fakeActuator{}
	r := goodRequest(t)
	i := wired(safety.NewActuatingChokepoint(), act).WithIndependentCondition(true, nil)
	if out, _ := i.Do(context.Background(), r); out.Executed {
		t.Fatal("an unwired required condition must refuse")
	}
	if act.execs != 0 {
		t.Fatal("the effect leaf ran with the condition unwired")
	}
}

func TestInterceptorHonoursTheConditionAnswer(t *testing.T) {
	cases := map[string]struct {
		allow, ok bool
		execs     int
	}{"allowed": {true, true, 1}, "refused": {false, true, 0}, "unreadable": {false, false, 0}}
	for name, tc := range cases {
		act := &fakeActuator{}
		r := goodRequest(t)
		_, _ = wired(safety.NewActuatingChokepoint(), act).WithIndependentCondition(true, &stubCondition{allow: tc.allow, ok: tc.ok}).Do(context.Background(), r)
		if (act.execs > 0) != (tc.execs > 0) {
			t.Errorf("%s: effect ran=%v, want %v", name, act.execs > 0, tc.execs > 0)
		}
	}
}

func TestInterceptorRoutesAnUndoToItsOwnCheck(t *testing.T) {
	s := &stubCondition{allow: true, ok: true}
	r := goodRequest(t)
	r.InvertsActionID = "forward"
	i := wired(safety.NewActuatingChokepoint(), &fakeActuator{}).WithIndependentCondition(true, s)
	if allow, _, ok := i.evaluateCondition(context.Background(), r); allow || ok {
		t.Fatalf("an undo with no facts: allow=%v ok=%v, want unreadable", allow, ok)
	}
	r.UndoFacts = func(context.Context) (UndoFacts, bool) { return UndoFacts{}, false }
	if allow, _, ok := i.evaluateCondition(context.Background(), r); allow || ok {
		t.Fatalf("unreadable facts: allow=%v ok=%v, want unreadable", allow, ok)
	}
	r.UndoFacts = func(context.Context) (UndoFacts, bool) { return UndoFacts{Found: true}, true }
	if allow, _, ok := i.evaluateCondition(context.Background(), r); !allow || !ok {
		t.Fatalf("readable facts: allow=%v ok=%v", allow, ok)
	}
	if !s.undoCalled || s.checkCalled {
		t.Fatalf("undoCalled=%v checkCalled=%v, want only the undo check", s.undoCalled, s.checkCalled)
	}
}

func TestInterceptorIgnoresTheConditionWhenNotRequired(t *testing.T) {
	act := &fakeActuator{}
	r := goodRequest(t)
	if _, err := wired(safety.NewActuatingChokepoint(), act).Do(context.Background(), r); err != nil {
		t.Fatalf("the default chain changed: %v", err)
	}
	if act.execs == 0 {
		t.Fatal("the default chain no longer reaches the effect")
	}
}

func TestGuestStopClassifierKeepsAStopOlderThanTheTriageWindow(t *testing.T) {
	cfg := attribution.Config{Window: 30 * time.Minute, Sanctioned: map[string][]string{"pve": {"root@pam"}}}
	classify := GuestStopClassifier(cfg)
	old := attribution.Evidence{Domain: "pve", Actor: "root@pam", ActionKind: "vzstop", Target: "guest01", Ref: "UPID:x", ObservedAt: time.Now().Add(-6 * time.Hour), Covered: true}
	if got := classify("guest01", []attribution.Evidence{old}); got != attribution.AttributedAuthorized {
		t.Fatalf("a sanctioned stop six hours old classified as %v, want attributed-authorized", got)
	}
	unknown := old
	unknown.Actor = "stranger@pam"
	if got := classify("guest01", []attribution.Evidence{unknown}); got != attribution.AttributedSuspicious {
		t.Fatalf("an unknown actor classified as %v, want attributed-suspicious", got)
	}
}

func TestGuestStopClassifierDoesNotChangeTheCallersWindow(t *testing.T) {
	cfg := attribution.Config{Window: 30 * time.Minute}
	GuestStopClassifier(cfg)
	if cfg.Window != 30*time.Minute {
		t.Fatalf("the caller's config changed: window=%s", cfg.Window)
	}
}

func TestConditionRefusesAForwardStopOfAGuest(t *testing.T) {
	c := guestCondition(attribution.Unattributable)
	a := manifest.Action{OpClass: "stop-guest", Target: "guest01", Params: map[string]string{"guest": "guest01"}}
	if allow, reason, ok := c.Check(context.Background(), a); allow || !ok || reason == "" {
		t.Fatalf("allow=%v ok=%v reason=%q, want a forward stop refused", allow, ok, reason)
	}
}

func TestGuestConditionRefusesAGuestThatHasNotRunRecently(t *testing.T) {
	c := guestCondition(attribution.Unattributable)
	c.RanRecently = ranRecently(false, nil)
	allow, reason, ok := c.Check(context.Background(), startGuestAction)
	if allow || !ok || !strings.Contains(reason, "not been running") {
		t.Fatalf("allow=%v ok=%v reason=%q", allow, ok, reason)
	}
}

func TestGuestConditionFailsClosedWhenTheUsageHistoryIsUnreadable(t *testing.T) {
	c := guestCondition(attribution.Unattributable)
	c.RanRecently = ranRecently(false, errors.New("403"))
	if allow, _, ok := c.Check(context.Background(), startGuestAction); allow || ok {
		t.Fatalf("allow=%v ok=%v, want unreadable", allow, ok)
	}
	c.RanRecently = nil
	if allow, _, ok := c.Check(context.Background(), startGuestAction); allow || ok {
		t.Fatalf("unconfigured: allow=%v ok=%v, want unreadable", allow, ok)
	}
}

var startServiceOnHost01 = manifest.Action{OpClass: "start-service", Target: "host01", Params: map[string]string{"unit": "nginx"}}

func TestStartServiceFailsClosedWithoutAStateReader(t *testing.T) {
	if allow, _, ok := boundCondition(ProbeDown).Check(context.Background(), startServiceOnHost01); allow || ok {
		t.Fatalf("allow=%v ok=%v, want unreadable", allow, ok)
	}
}

func TestStartServiceRefusesAUnitThatIsAlreadyActive(t *testing.T) {
	c := boundCondition(ProbeDown)
	c.ServiceActive = func(context.Context, string, string) (bool, bool) { return true, true }
	if allow, reason, ok := c.Check(context.Background(), startServiceOnHost01); allow || !ok || !strings.Contains(reason, "already active") {
		t.Fatalf("active: allow=%v ok=%v reason=%q", allow, ok, reason)
	}
	c.ServiceActive = func(context.Context, string, string) (bool, bool) { return false, false }
	if allow, _, ok := c.Check(context.Background(), startServiceOnHost01); allow || ok {
		t.Fatalf("unreadable: allow=%v ok=%v", allow, ok)
	}
	c.ServiceActive = func(context.Context, string, string) (bool, bool) { return false, true }
	if allow, _, ok := c.Check(context.Background(), startServiceOnHost01); !allow || !ok {
		t.Fatalf("inactive: allow=%v ok=%v", allow, ok)
	}
}

var undoStop = manifest.Action{OpClass: "stop-guest", Target: "guest01", Params: map[string]string{"guest": "guest01"}}

var undoForward = manifest.Action{OpClass: "start-guest", Target: "guest01", Params: map[string]string{"guest": "guest01"}}

func undoFactsFor(auto bool, verdict string, age time.Duration) UndoFacts {
	return UndoFacts{Found: true, TargetHost: "guest01", ExecutedAt: condNow.Add(-age), Verdict: verdict, AutoFired: auto, Forward: &undoForward}
}

func signedUndoOf(forward manifest.Action) UndoFacts {
	return UndoFacts{Found: true, TargetHost: forward.Target, ExecutedAt: condNow.Add(-time.Minute), Verdict: "match", Signed: true, Forward: &forward}
}

func TestUndoMustNameTheObjectTheForwardActedOn(t *testing.T) {
	unit := manifest.Action{OpClass: "start-service", Target: "host01", Params: map[string]string{"unit": "nginx"}}
	box := manifest.Action{OpClass: "start-container", Target: "host01", Params: map[string]string{"container": "app"}}
	c := &IndependentCondition{Now: func() time.Time { return condNow }}
	for name, tc := range map[string]struct {
		forward, inverse manifest.Action
		allow            bool
	}{
		"same unit":              {unit, manifest.Action{OpClass: "start-service", Target: "host01", Params: map[string]string{"unit": "nginx"}}, true},
		"other spelling":         {unit, manifest.Action{OpClass: "start-service", Target: "host01", Params: map[string]string{"unit": "nginx.service"}}, true},
		"other unit, same host":  {unit, manifest.Action{OpClass: "start-service", Target: "host01", Params: map[string]string{"unit": "apache2"}}, false},
		"same container":         {box, manifest.Action{OpClass: "start-container", Target: "host01", Params: map[string]string{"container": "app"}}, true},
		"other container":        {box, manifest.Action{OpClass: "start-container", Target: "host01", Params: map[string]string{"container": "db"}}, false},
		"container for a unit":   {unit, manifest.Action{OpClass: "start-container", Target: "host01", Params: map[string]string{"container": "nginx"}}, false},
		"other guest, same host": {undoForward, manifest.Action{OpClass: "stop-guest", Target: "guest01", Params: map[string]string{"guest": "guest02"}}, false},
	} {
		f := signedUndoOf(tc.forward)
		f.TargetHost = ResourceHost(ResourcePath(tc.inverse))
		if name == "other guest, same host" {
			f.TargetHost = "guest02"
		}
		allow, reason, ok := c.CheckUndo(context.Background(), tc.inverse, f)
		if allow != tc.allow || !ok {
			t.Errorf("%s: allow=%v ok=%v reason=%q, want allow=%v", name, allow, ok, reason, tc.allow)
		}
	}
}

func TestUndoMustBeTheDeclaredInverseClass(t *testing.T) {
	c := &IndependentCondition{Now: func() time.Time { return condNow }}
	nginx := map[string]string{"unit": "nginx"}
	for name, tc := range map[string]struct {
		forward, inverse manifest.Action
		allow            bool
	}{
		"declared inverse":              {undoForward, undoStop, true},
		"same class":                    {manifest.Action{OpClass: "restart-service", Target: "host01", Params: nginx}, manifest.Action{OpClass: "restart-service", Target: "host01", Params: nginx}, true},
		"undeclared class":              {manifest.Action{OpClass: "start-service", Target: "host01", Params: nginx}, manifest.Action{OpClass: "restart-service", Target: "host01", Params: nginx}, false},
		"no class":                      {undoForward, manifest.Action{Target: "guest01", Params: map[string]string{"guest": "guest01"}}, false},
		"no class, no declared inverse": {manifest.Action{OpClass: "k8s-set-replicas", Target: "host01"}, manifest.Action{Target: "host01"}, false},
	} {
		allow, reason, ok := c.CheckUndo(context.Background(), tc.inverse, signedUndoOf(tc.forward))
		if allow != tc.allow || !ok {
			t.Errorf("%s: allow=%v ok=%v reason=%q, want allow=%v", name, allow, ok, reason, tc.allow)
		}
	}
}

func TestUndoWithoutASealedForwardIsRefused(t *testing.T) {
	c := &IndependentCondition{Now: func() time.Time { return condNow }}
	f := undoFactsFor(false, "match", time.Minute)
	f.Signed, f.Forward = true, nil
	allow, reason, ok := c.CheckUndo(context.Background(), undoStop, f)
	if allow || !ok || !strings.Contains(reason, "sealed") {
		t.Fatalf("allow=%v ok=%v reason=%q", allow, ok, reason)
	}
}

func TestUndoNeedsARecordedForwardOnTheSameTarget(t *testing.T) {
	c := &IndependentCondition{Now: func() time.Time { return condNow }}
	f := undoFactsFor(false, "match", time.Hour)
	f.Signed = true
	if allow, _, _ := c.CheckUndo(context.Background(), undoStop, f); !allow {
		t.Fatal("a signed undo of a recorded forward must pass")
	}
	missing := f
	missing.Found = false
	if allow, _, _ := c.CheckUndo(context.Background(), undoStop, missing); allow {
		t.Fatal("no record must refuse")
	}
	other := f
	other.TargetHost = "guest02"
	if allow, _, _ := c.CheckUndo(context.Background(), undoStop, other); allow {
		t.Fatal("a different target must refuse")
	}
	old := f
	old.ExecutedAt = condNow.Add(-IndependentConditionLookback - time.Hour)
	if allow, _, _ := c.CheckUndo(context.Background(), undoStop, old); allow {
		t.Fatal("a stale record must refuse")
	}
}

func TestUndoRefusesASecondUndo(t *testing.T) {
	c := &IndependentCondition{Now: func() time.Time { return condNow }}
	f := undoFactsFor(false, "match", time.Hour)
	f.Signed, f.AlreadyUndone = true, true
	if allow, reason, ok := c.CheckUndo(context.Background(), undoStop, f); allow || !ok || !strings.Contains(reason, "already been undone") {
		t.Fatalf("allow=%v ok=%v reason=%q", allow, ok, reason)
	}
}

func TestManualUndoNeedsASignature(t *testing.T) {
	c := &IndependentCondition{Now: func() time.Time { return condNow }}
	allow, reason, ok := c.CheckUndo(context.Background(), undoStop, undoFactsFor(false, "match", time.Hour))
	if allow || !ok || !strings.Contains(reason, "signature") {
		t.Fatalf("allow=%v ok=%v reason=%q", allow, ok, reason)
	}
}

func TestAutomaticUndoMustAnswerANonSuccess(t *testing.T) {
	c := alertCondition(AlertingHostSet(nil), nil)
	c.Now = func() time.Time { return condNow }
	if allow, _, ok := c.CheckUndo(context.Background(), undoStop, undoFactsFor(true, "deviation", time.Minute)); !allow || !ok {
		t.Fatalf("deviation: allow=%v ok=%v", allow, ok)
	}
	if allow, _, ok := c.CheckUndo(context.Background(), undoStop, undoFactsFor(true, "partial", time.Minute)); !allow || !ok {
		t.Fatalf("partial: allow=%v ok=%v", allow, ok)
	}
	allow, reason, ok := c.CheckUndo(context.Background(), undoStop, undoFactsFor(true, "match", time.Minute))
	if allow || !ok || !strings.Contains(reason, "match") {
		t.Fatalf("match: allow=%v ok=%v reason=%q", allow, ok, reason)
	}
	if allow, _, _ := c.CheckUndo(context.Background(), undoStop, undoFactsFor(true, "deviation", 2*time.Hour)); allow {
		t.Fatal("an automatic undo older than the bound must refuse")
	}
}

func TestAutomaticUndoOfAnUnverifiedExecutionNeedsAnAlert(t *testing.T) {
	f := undoFactsFor(true, "", time.Minute)
	f.Unverifiable = true
	quiet := alertCondition(AlertingHostSet(nil), nil)
	quiet.Now = func() time.Time { return condNow }
	if allow, _, ok := quiet.CheckUndo(context.Background(), undoStop, f); allow || !ok {
		t.Fatalf("quiet: allow=%v ok=%v", allow, ok)
	}
	alerting := alertCondition(AlertingHostSet([]string{"guest01"}), nil)
	alerting.Now = func() time.Time { return condNow }
	if allow, _, ok := alerting.CheckUndo(context.Background(), undoStop, f); !allow || !ok {
		t.Fatalf("alerting: allow=%v ok=%v", allow, ok)
	}
	broken := alertCondition(nil, errors.New("403"))
	broken.Now = func() time.Time { return condNow }
	if allow, _, ok := broken.CheckUndo(context.Background(), undoStop, f); allow || ok {
		t.Fatalf("unreadable: allow=%v ok=%v", allow, ok)
	}
}
