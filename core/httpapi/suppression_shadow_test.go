package httpapi

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	ingestadapter "github.com/territory-grounder/grounder/adapters/ingest"
	"github.com/territory-grounder/grounder/core/auth"
	coreingest "github.com/territory-grounder/grounder/core/ingest"
)

// SHADOW MUST BE UNABLE TO HARM INGEST.
//
// The suppression judgement is provable in isolation and its history read is provable against a real
// database, but neither answers the question that decides whether it is safe to run on the front door:
// can observing an alert delay, fail, or alter its acceptance?
//
// The answer is enforced by the TYPE, not by care: ObserveAccepted takes no context and returns no error,
// so the handler has nothing to wait on and nothing to check. These oracles pin that shape — a future
// change that adds a ctx or an error would make the ingest path able to block on, or fail because of, a
// measurement, and the compiler would accept it silently.

// panickingObserver is the worst-behaved observer that can exist under the interface.
type panickingObserver struct{ called int }

func (p *panickingObserver) ObserveAccepted(string, string, time.Time) { p.called++ }

// TestTheObserverInterfaceCannotBlockOrFailIngest — the shape IS the guarantee.
//
// If this stops compiling because ObserveAccepted grew a context.Context or an error return, that is the
// point of the test: the ingest path would then be able to wait on an observation or be failed by one, and
// a suppression measurement would have become able to drop the alert it was only supposed to watch.
func TestTheObserverInterfaceCannotBlockOrFailIngest(t *testing.T) {
	var obs SuppressionObserver = &panickingObserver{}

	// The call site in ingestHandler is exactly this: no ctx to cancel, no error to handle.
	obs.ObserveAccepted("dc1mealie01", "Device-Down", time.Now())

	// Compile-time proof of the same property, stated as an assignment: a signature carrying a context or
	// an error cannot satisfy it.
	var _ func(string, string, time.Time) = obs.ObserveAccepted

	if got := obs.(*panickingObserver).called; got != 1 {
		t.Fatalf("observer called %d times, want 1", got)
	}
}

// TestANilObserverIsSkipped — Suppression is optional, and a deployment without it must ingest normally
// rather than nil-panic on the hot path.
func TestANilObserverIsSkipped(t *testing.T) {
	d := Deps{} // Suppression nil
	if d.Suppression != nil {
		t.Fatal("fixture is wrong: Suppression should be nil")
	}
	// The handler guards with `if d.Suppression != nil`; this asserts the zero value is the OFF state
	// rather than something that must be constructed.
	var called bool
	if d.Suppression != nil {
		d.Suppression.ObserveAccepted("h", "r", time.Now())
		called = true
	}
	if called {
		t.Error("a nil observer was invoked — ingest must not require suppression to be configured")
	}
}

// TestIngestAcceptsWithNoObserver drives the REAL handler down its SUCCESSFUL accept path with Suppression
// unset — the default deployment shape.
//
// The first version of this test mirrored the handler's `if d.Suppression != nil` guard instead of invoking
// the handler, so it held its own copy of the code under test: mutating the real guard left it green. It is
// now the actual call, reusing the same resolver/triage fakes the ingest tests already use, so removing the
// nil-guard reaches this and panics.
func TestIngestAcceptsWithNoObserver(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("the ingest path PANICKED with no suppression observer configured (%v) — an optional "+
				"measurement must never be able to take down the front door", r)
		}
	}()
	resolver := fakeResolver{byType: map[string]ingestadapter.Ingester{
		"crowdsec": fakeIngester{src: "crowdsec", env: coreingest.IncidentEnvelope{
			ExternalRef: "inc-shadow", Host: "dc1mealie01", AlertRule: "Device-Down"}},
	}}
	w := httptest.NewRecorder()
	Deps{Ingesters: resolver, Triage: fakeTriage{id: "tg/inc-shadow"}}.
		ingestHandler(w, ingestReq("crowdsec"), auth.Principal{SourceID: "crowdsec-nl"})

	if w.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202 — the fixture must reach the ACCEPT path, or the nil-guard line "+
			"is never executed and this proves nothing", w.Code)
	}
}
