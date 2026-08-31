package model

import (
	"context"
	"errors"
	"net/http"
	"testing"
)

// deadTransport fails any HTTP round trip — the oracle that a refused call never left the process.
type deadTransport struct{ used bool }

func (d *deadTransport) RoundTrip(*http.Request) (*http.Response, error) {
	d.used = true
	return nil, errors.New("deadTransport: the gateway must not dial on an empty model name")
}

// TG-530: an empty model name is refused at the gateway chokepoint — typed, observed, and WITHOUT an HTTP
// round trip. Before this guard the completions path happily POSTed model:"" (~170-620 doomed LiteLLM 400s
// per day from a present-but-empty tier env), each burning a breaker sample and naming no caller.
// KILLING MUTATION: delete the guard at the top of CompleteWithUsage — the fake transport is dialed and
// this fails on all three asserts.
func TestCompleteRefusesEmptyModelNameWithoutDialing(t *testing.T) {
	dt := &deadTransport{}
	obs := &capObs{}
	t.Setenv("TG530_TEST_KEY", "k")
	g := &Gateway{BaseURL: "http://litellm.invalid:4000", APIKeyRef: "env:TG530_TEST_KEY",
		HTTP: &http.Client{Transport: dt}, Obs: obs}

	for _, m := range []string{"", "   "} {
		out, usage, err := g.CompleteWithUsage(context.Background(), "hyde", m, []Message{{Role: "user", Content: "x"}})
		if err == nil || out != "" || usage.Measured {
			t.Fatalf("model=%q: want a typed refusal with no output, got out=%q usage=%+v err=%v", m, out, usage, err)
		}
		var me *ModelError
		if !errors.As(err, &me) || me.Class != "bad_request" {
			t.Fatalf("model=%q: want *ModelError class bad_request, got %v", m, err)
		}
	}
	if dt.used {
		t.Fatal("the gateway dialed the provider on an empty model name — the whole point is refusing the doomed round trip")
	}
	if obs.n != 2 || obs.outcome != "bad_request" || obs.caller != "hyde" {
		t.Fatalf("the refusal must be observed with the caller named: n=%d outcome=%q caller=%q", obs.n, obs.outcome, obs.caller)
	}
}
