package actuate

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/territory-grounder/grounder/core/audit"
	"github.com/territory-grounder/grounder/core/safety"
)

type fakeDispatch struct {
	seen map[string]bool
	err  error
}

func (f *fakeDispatch) ClaimDispatch(_ context.Context, id, _, _ string) (bool, error) {
	if f.err != nil {
		return false, f.err
	}
	if f.seen == nil {
		f.seen = map[string]bool{}
	}
	if f.seen[id] {
		return false, nil
	}
	f.seen[id] = true
	return true, nil
}

func dispatchInterceptor(act *fakeActuator, c DispatchClaim, identity func(context.Context) (string, bool)) *Interceptor {
	return actuatingInterceptor(safety.NewActuatingChokepoint(), act, audit.NewLedger()).WithDispatchClaim(c, identity)
}

func fixedDispatch(id string) func(context.Context) (string, bool) {
	return func(context.Context) (string, bool) { return id, true }
}

func TestAFirstDispatchReachesTheEffect(t *testing.T) {
	act := &fakeActuator{}
	out, err := dispatchInterceptor(act, &fakeDispatch{}, fixedDispatch("wf/run/5")).Do(context.Background(), goodRequest(t))
	if err != nil || !out.Executed || act.execs != 1 {
		t.Fatalf("a first dispatch must execute once: %+v err=%v calls=%d", out, err, act.execs)
	}
}

func TestASecondArrivalOfOneDispatchNeverFiresTheEffect(t *testing.T) {
	act := &fakeActuator{}
	claim := &fakeDispatch{seen: map[string]bool{"wf/run/5": true}}
	out, err := dispatchInterceptor(act, claim, fixedDispatch("wf/run/5")).Do(context.Background(), goodRequest(t))
	if err != nil {
		t.Fatal(err)
	}
	if act.execs != 0 {
		t.Fatalf("a duplicate dispatch must not reach the effect, got %d", act.execs)
	}
	if !out.Indeterminate || out.Executed || out.Refused || !strings.Contains(out.Reason, RefusalDuplicateDispatch) {
		t.Fatalf("a prior attempt reached the effect, so the state is unknown: %+v", out)
	}
}

func TestANewDispatchOfTheSameShapeIsAdmitted(t *testing.T) {
	act := &fakeActuator{}
	claim := &fakeDispatch{}
	if _, err := dispatchInterceptor(act, claim, fixedDispatch("wf/run-1/5")).Do(context.Background(), goodRequest(t)); err != nil {
		t.Fatal(err)
	}
	out, err := dispatchInterceptor(act, claim, fixedDispatch("wf/run-2/5")).Do(context.Background(), goodRequest(t))
	if err != nil || !out.Executed || act.execs != 2 {
		t.Fatalf("a legitimate repeat heal must run: %+v err=%v calls=%d", out, err, act.execs)
	}
}

func TestAnUnrecordableClaimRefusesBeforeTheEffect(t *testing.T) {
	act := &fakeActuator{}
	out, err := dispatchInterceptor(act, &fakeDispatch{err: errors.New("db down")}, fixedDispatch("wf/run/5")).Do(context.Background(), goodRequest(t))
	if err != nil {
		t.Fatal(err)
	}
	if !out.Refused || act.execs != 0 {
		t.Fatalf("an unreadable store must refuse, got %+v calls=%d", out, act.execs)
	}
}

func TestACallWithNoDispatchIdentityRefuses(t *testing.T) {
	act := &fakeActuator{}
	out, err := dispatchInterceptor(act, &fakeDispatch{}, func(context.Context) (string, bool) { return "", false }).Do(context.Background(), goodRequest(t))
	if err != nil {
		t.Fatal(err)
	}
	if !out.Refused || act.execs != 0 {
		t.Fatalf("no identity means no claim and no effect, got %+v calls=%d", out, act.execs)
	}
}

func TestWithoutTheSeamTheChainIsUnchanged(t *testing.T) {
	act := &fakeActuator{}
	out, err := actuatingInterceptor(safety.NewActuatingChokepoint(), act, audit.NewLedger()).WithDispatchClaim(nil, nil).Do(context.Background(), goodRequest(t))
	if err != nil || !out.Executed {
		t.Fatalf("an unwired seam must change nothing: %+v %v", out, err)
	}
}
