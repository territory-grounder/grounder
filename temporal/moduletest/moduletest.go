// Package moduletest is the TEST button: it exercises a configured module for real.
//
// WHY IT IS A WORKFLOW AND NOT AN HTTP HANDLER. The modules live in the WORKER — the grounder's registry
// holds no notifiers, and `grep ListenAndServe cmd/worker/main.go` finds nothing, so the process with the
// module cannot receive the request and the process receiving the request has no module. A handler-side
// "just call Notify" is not implementable. This mirrors configwrite, which crosses the same gap for the
// same reason.
//
// NO MOCKS. A test that checked whether the credentials were non-empty would be a mock wearing a test's
// name: it would pass against a revoked token, a room the bot was kicked from, and a homeserver that has
// been offline for a week — the three things an operator presses Test to rule out. So the notifier probe
// posts a real message to the real room.
//
// WHAT A TEST MAY NOT DO. It must never mutate the estate, never traverse the mode chokepoint, and never
// be mistakable for a governance decision by whoever sees it. The last one is not fussiness: a message
// that reads like a real approval request, arriving because someone opened a settings dialog, is an
// operator acting on a decision TG never made.
package moduletest

import (
	"context"
	"errors"
	"fmt"
	"time"

	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/workflow"

	"github.com/territory-grounder/grounder/core/selftest"
)

// Request names the module to exercise. It carries no configuration: the module under test is the one
// TG has ACTUALLY got wired, because "does the config I just saved work" is the question, and testing a
// config supplied alongside the request would answer a different one.
type Request struct {
	Surface    string
	SourceType string
	// Operator is the authenticated principal, recorded in the probe's own message so the room can see
	// who triggered it.
	Operator string
}

// Result is what the operator is shown. Detail must be actionable: "error" tells them nothing, while
// "the bot is not a member of that room" tells them exactly what to fix.
type Result struct {
	Surface    string `json:"surface"`
	SourceType string `json:"source_type"`
	OK         bool   `json:"ok"`
	// Summary is one line, safe to display.
	Summary string `json:"summary"`
	// Detail names the failure class an operator can act on.
	Detail string `json:"detail,omitempty"`
	// Elapsed is how long the real call took — a Test that passes in 9 seconds is itself a finding.
	ElapsedMS int64 `json:"elapsed_ms"`
}

// TestBodyMarker is the prefix every probe message carries.
//
// It exists so a human reading the room can never mistake a probe for a governance decision. It is a
// CONSTANT rather than a formatting choice because the marking is a safety property: an operator acting
// on a test message is acting on a decision TG never made.
const TestBodyMarker = selftest.BodyMarker

// Prober exercises one module for real. Implemented per surface in the composition root, where the live
// module instances are.
//
// It returns a SUMMARY and a DETAIL rather than a bare error so the surface can be honest about partial
// outcomes ("posted, but the room is not the one you configured").
type Prober interface {
	Probe(ctx context.Context, req Request) (summary, detail string, err error)
}

// Deps is what the activity needs. Probers is keyed "surface/source_type".
type Deps struct {
	Probers map[string]Prober
}

// Activities holds the test activity.
type Activities struct{ D Deps }

// ErrNoProber means the module is configured but nothing knows how to exercise it. It is returned rather
// than faked into a pass: a Test that cannot test must say so, or it certifies a module nobody checked.
var ErrNoProber = errors.New("moduletest: no prober for this module")

// TestModuleActivity runs the real probe.
func (a *Activities) TestModuleActivity(ctx context.Context, req Request) (Result, error) {
	key := req.Surface + "/" + req.SourceType
	res := Result{Surface: req.Surface, SourceType: req.SourceType}
	p, ok := a.D.Probers[key]
	if !ok || p == nil {
		res.Summary = "no test is implemented for this module"
		res.Detail = "the module is configured but TG has no probe for its surface — a pass here would " +
			"certify something nobody checked"
		// NOT an activity error: this is a definitive answer, and retrying it would only repeat it.
		return res, nil
	}
	start := time.Now()
	summary, detail, err := p.Probe(ctx, req)
	res.ElapsedMS = time.Since(start).Milliseconds()
	res.Summary, res.Detail = summary, detail
	if err != nil {
		if res.Summary == "" {
			res.Summary = "the test call failed"
		}
		if res.Detail == "" {
			res.Detail = err.Error()
		}
		res.OK = false
		// A FAILED TEST IS A RESULT, NOT AN ACTIVITY ERROR. Returning an error would make Temporal retry
		// the probe — which for a notifier means posting the same test message repeatedly into an
		// operator's room while they watch a spinner.
		return res, nil
	}
	res.OK = true
	if res.Summary == "" {
		res.Summary = "ok"
	}
	return res, nil
}

// ProbeBody renders the message an emitting probe sends.
//
// It DELEGATES to core/selftest rather than holding its own copy. Two renderers would be two markers the
// day one of them changed, and the marker is a safety property: a probe message a human cannot tell from
// a governance decision is an operator acting on a decision TG never made.
func ProbeBody(operator string) string { return selftest.ProbeBody(operator) }

func activityOpts() workflow.ActivityOptions {
	return workflow.ActivityOptions{
		// A third-party API is on the other side of this. Generous enough for a slow homeserver, bounded
		// so a dialog cannot hang.
		StartToCloseTimeout: 30 * time.Second,
		RetryPolicy: &temporal.RetryPolicy{
			// ONE attempt. The probe has a side effect an operator can see — retrying posts the test
			// message again, and three identical probes in a room look like a malfunction rather than a
			// test.
			MaximumAttempts: 1,
		},
	}
}

// TestModuleWorkflow is the distinctly-named entry point the grounder starts.
//
// The name is distinct at the SYMBOL because Temporal registers by bare function name, and a plain
// TestWorkflow here would collide with another lane's and panic the worker at boot — the 2026-07-17
// boot-loop this repo already paid for once.
func TestModuleWorkflow(ctx workflow.Context, req Request) (Result, error) {
	ctx = workflow.WithActivityOptions(ctx, activityOpts())
	var a *Activities
	var res Result
	if err := workflow.ExecuteActivity(ctx, a.TestModuleActivity, req).Get(ctx, &res); err != nil {
		return Result{Surface: req.Surface, SourceType: req.SourceType, OK: false,
			Summary: "the test could not be run",
			Detail:  fmt.Sprintf("the worker did not complete the probe: %v", err)}, nil
	}
	return res, nil
}
