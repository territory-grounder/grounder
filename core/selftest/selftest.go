// Package selftest is the OPTIONAL capability that lets a module prove it actually works.
//
// WHY IT EXISTS. The console's per-module dialog has a TEST button, and every descriptor declares, in the
// operator's own words, what pressing it will do ("post a test message to the approvals room", "open a
// host-key-verified SSH session to each configured syslog server"). That sentence is a consent contract:
// it is shown BEFORE the press so the operator knows what they are about to cause in a third-party system
// other people watch.
//
// For a while exactly one surface could honour it. The worker built probers from the notifier sinks and
// nothing else, so 28 of 29 dialogs disclosed an action nothing could perform — the worker's own boot log
// said `module test: lane registered over 1 prober(s)`. The outcome was honest (a module with no prober
// reports "no test is implemented" rather than a pass), but the promise was not, and a button that says
// what it will do and then does not do it is the same defect class as a lane that is wired and never
// called.
//
// WHY THE CAPABILITY LIVES NEXT TO THE MODULE. A useful probe is module-specific and read-only, and the
// surface interfaces cannot express it. adapters/tracker.Tracker offers Open/Read/TransitionState/Comment
// — three of those four mutate. adapters/actuation.Actuator offers only Exec, which for the AWX job
// launcher means STARTING A JOB; a probe built from the surface interface would be an unreviewed
// actuation triggered from a settings dialog. adapters/model.Provider offers only Models(), a pure
// accessor over a hardcoded slice that would pass with the network unplugged.
//
// So the module implements its own probe, using its own client, against the endpoint it actually talks
// to. This mirrors adapters/tracker.History: a capability a backend MAY have, detected by type assertion,
// degrading honestly when absent — which keeps a module that genuinely cannot be tested from having to
// invent a test.
//
// WHY IT IS IN core/ AND STDLIB-ONLY. Every module imports core (core/config, core/credential,
// core/estate); not every module imports adapters — modules/credsource/*, modules/discovery/* and
// modules/knowledge/* do not. A capability only half the fleet could implement would recreate the gap it
// exists to close. core never imports modules, and nothing here does.
package selftest

import (
	"context"
	"reflect"
)

// Result is what the operator is shown. Both fields are rendered, so both must be safe to display and
// free of credential material.
type Result struct {
	// Summary is one line stating what happened: "reached NetBox as tg-reader (3 sites visible)".
	//
	// It should name what the probe OBSERVED, not merely that it succeeded. "ok" tells an operator
	// nothing they could act on, and in particular cannot distinguish a correctly configured module from
	// one pointed at the wrong instance — which is the failure a green Test is most likely to hide.
	Summary string
	// Detail carries the actionable remainder: a partial outcome, a warning, or on failure the class of
	// fault an operator can fix ("the token was accepted but the account cannot read job templates").
	Detail string
}

// Tester is implemented by a module that can prove itself against its real backend.
//
// THE CONTRACT, and every clause of it is load-bearing:
//
//  1. IT MUST BE READ-ONLY WITH RESPECT TO THE ESTATE. A probe may authenticate, list, and read. It may
//     not create, transition, delete, or launch anything. desc.TestSpec.Mutating is guarded false for
//     every descriptor; this is the runtime half of that promise. The one deliberate exception is a
//     notifier, which must post to prove delivery — and it posts a message carrying
//     moduletest.TestBodyMarker so no human can mistake it for a governance decision.
//
//  2. IT MUST EXERCISE THE REAL NETWORK PATH, WITH THE REAL CREDENTIAL, RESOLVED THE REAL WAY. The three
//     things an operator presses TEST to rule out are a revoked credential, a permission that was never
//     granted, and an endpoint that has been unreachable for a week. A check that the configured values
//     are non-empty passes all three, and is a mock wearing a test's name.
//
//  3. A FAILED PROBE RETURNS AN ERROR, AND THE ERROR MUST BE ACTIONABLE. "error" tells an operator
//     nothing; "the token authenticated but lacks read access to that project" tells them exactly what to
//     fix. Classify on the shape of the failure — status code, transport class — rather than by parsing
//     vendor prose, and fall through to the raw error rather than inventing a diagnosis.
//
//  4. IT MUST RESPECT ctx. The console holds an operator on a spinner; moduletest bounds the activity at
//     30 seconds with no retry, because a retried probe with a visible side effect posts twice.
//
//  5. NEVER RETURN CREDENTIAL MATERIAL IN Result OR IN AN ERROR. An error string is the most commonly
//     copied text in an incident and must be safe to paste.
//
// operator is the authenticated principal who pressed TEST. Almost every probe ignores it — it exists
// because the one probe with an outward side effect, the notifier, must say WHO caused the message that
// appears in an operations room. Attribution is not decoration there: a marked test message with no named
// author is an unexplained event in a room people watch during incidents. It is a parameter rather than a
// context value so a module cannot silently forget to carry it.
type Tester interface {
	SelfTest(ctx context.Context, operator string) (Result, error)
}

// Operator normalises the principal for display, so every probe that names the actor names it the same
// way and none of them has to decide what to do with an empty string.
func Operator(s string) string {
	for _, r := range s {
		if r > ' ' {
			return s
		}
	}
	return "an operator"
}

// BodyMarker is the prefix every probe message carries when a probe must emit something a human will see.
//
// It is a CONSTANT rather than a formatting choice because the marking is a safety property: an operator
// acting on a test message is acting on a decision TG never made. It lives here, beside the capability,
// so a module that emits a marked artefact does not have to import a Temporal package to spell the marker
// — temporal/moduletest re-exports it for the callers that already used it from there.
const BodyMarker = "[TG CONFIGURATION TEST — not a governance decision, no action required]"

// ProbeBody renders the message an emitting probe sends. Exported so the modules and the oracles use the
// same text: a marker that only the test asserts is a marker production does not send.
func ProbeBody(operator string) string {
	return BodyMarker + "\n" +
		"Triggered by " + Operator(operator) + " from the module configuration dialog to confirm delivery works.\n" +
		"Nothing is proposed and nothing will happen as a result of this message."
}

// Of returns v as a Tester when it implements one AND is not a typed nil.
//
// It exists so composition roots express the detection identically everywhere, and so the typed-nil trap
// is handled in ONE place. A plain `any(v).(Tester)` succeeds for a nil *Module: the interface value is
// non-nil because it carries a type, so `t != nil` is TRUE and the check reads as if it worked. A module
// that was never configured would then be registered as having a probe, and pressing TEST would panic the
// activity — turning "this module is not set up" into a crash, which is strictly worse than the honest
// "no test is implemented".
//
// reflect is the only way to ask the question, because the nilness lives in the value word rather than in
// the interface. This runs a handful of times at boot, so its cost is irrelevant beside being correct.
func Of[T any](v T) (Tester, bool) {
	t, ok := any(v).(Tester)
	if !ok || t == nil {
		return nil, false
	}
	switch rv := reflect.ValueOf(t); rv.Kind() {
	case reflect.Ptr, reflect.Interface, reflect.Map, reflect.Slice, reflect.Func, reflect.Chan:
		if rv.IsNil() {
			return nil, false
		}
	}
	return t, true
}
