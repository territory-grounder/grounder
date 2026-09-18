package actuate

// spec/016 T-016-5 (REQ-1604) — the authn-compose gate's composition oracles. The layer-order claims:
//
//   1. AFTER authorization: a policy DENY short-circuits the chain BEFORE the composer — identity
//      resolution never runs for an action authorization already refused.
//   2. BEFORE execute: a wired composer that cannot resolve the target refuses with the effect leaf
//      never reached (the drill-matrix "authn-compose" case owns that half); a composer that resolves
//      passes exactly once and the effect runs.
//   3. HONEST WHEN DARK: with no composer wired (today's deployment), the chain is byte-identical and
//      the gate row SAYS the control is unarmed — never a row implying an identity check that did not
//      happen (the silently-armed/silently-dark defect class this repo keeps re-finding).

import (
	"context"
	"strings"
	"testing"

	"github.com/territory-grounder/grounder/core/audit"
	"github.com/territory-grounder/grounder/core/policy"
	"github.com/territory-grounder/grounder/core/safety"
	"github.com/territory-grounder/grounder/core/trace"
)

// tpT0165GateSink records the observe-only gate trail rows for the dark-state oracle.
type tpT0165GateSink struct{ rows []trace.GateVerdict }

func (s *tpT0165GateSink) Emit(_ context.Context, v trace.GateVerdict) error {
	s.rows = append(s.rows, v)
	return nil
}

func TestComposeRunsAfterAuthorizationNeverForADeniedAction(t *testing.T) {
	act := &fakeActuator{}
	composed := 0
	i := NewInterceptor(safety.NewActuatingChokepoint(), act, audit.NewLedger()).
		WithPolicyDecider(&fakeDecider{verdict: policy.VerdictDeny}, func() policy.Mode { return policy.ModeFullAuto }).
		WithComposer(func(context.Context, string) (string, error) { composed++; return "rule-1", nil })
	r := goodRequest(t)
	r.Approved = true
	out, err := i.Do(context.Background(), r)
	if err != nil || !out.Refused {
		t.Fatalf("a policy deny must refuse: out=%+v err=%v", out, err)
	}
	if composed != 0 {
		t.Fatalf("REQ-1604 order: identity must resolve AFTER a non-deny verdict — the composer ran %d time(s) for a DENIED action", composed)
	}
	if act.execs != 0 {
		t.Fatalf("nothing may execute on a deny, execs=%d", act.execs)
	}
}

func TestComposeResolutionPassesOnceAndTheEffectRuns(t *testing.T) {
	act := &fakeActuator{}
	composed := 0
	sink := &tpT0165GateSink{}
	i := wired(safety.NewActuatingChokepoint(), act).
		WithComposer(func(_ context.Context, host string) (string, error) {
			composed++
			if host == "" {
				t.Error("the composer must receive the manifest's target host")
			}
			return "rule-web01", nil
		}).WithGateVerdictSink(sink)
	out, err := i.Do(context.Background(), goodRequest(t))
	if err != nil || out.Refused {
		t.Fatalf("a resolved identity must not refuse: out=%+v err=%v", out, err)
	}
	if composed != 1 || act.execs != 1 {
		t.Fatalf("want exactly one compose then one execute, got compose=%d execs=%d", composed, act.execs)
	}
	for _, row := range sink.rows {
		if row.Gate == "authn-compose" {
			if row.Verdict != "pass" || !strings.Contains(row.Reason, "rule-web01") {
				t.Fatalf("the pass row must carry the winning rule id (provenance), got %+v", row)
			}
			return
		}
	}
	t.Fatal("no authn-compose row on the trail — the gate did not record itself")
}

func TestNilComposerIsByteIdenticalAndTheRowSaysSo(t *testing.T) {
	act := &fakeActuator{}
	sink := &tpT0165GateSink{}
	i := wired(safety.NewActuatingChokepoint(), act).WithGateVerdictSink(sink)
	out, err := i.Do(context.Background(), goodRequest(t))
	if err != nil || out.Refused || act.execs != 1 {
		t.Fatalf("an unwired composer must change NOTHING: out=%+v err=%v execs=%d", out, err, act.execs)
	}
	for _, row := range sink.rows {
		if row.Gate == "authn-compose" {
			if row.Verdict != "pass" || !strings.Contains(row.Reason, "no composer wired") {
				t.Fatalf("the dark-state row must SAY the control is unarmed, got %+v", row)
			}
			return
		}
	}
	t.Fatal("no authn-compose row — even the unarmed state must be visible on the trail")
}
