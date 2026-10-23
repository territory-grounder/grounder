package acceptance

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/cucumber/godog"

	"github.com/territory-grounder/grounder/adapters/tracker"
	"github.com/territory-grounder/grounder/modules"
)

// fakeTracker is a minimal concrete adapter used to prove a registration carries a real surface adapter.
type fakeTracker struct{ src string }

func (f fakeTracker) SourceType() string { return f.src }
func (f fakeTracker) Open(context.Context, string) (tracker.Issue, error) {
	return tracker.Issue{}, nil
}
func (f fakeTracker) Read(context.Context, string) (tracker.Issue, error) {
	return tracker.Issue{}, nil
}
func (f fakeTracker) TransitionState(context.Context, string, tracker.State) error { return nil }
func (f fakeTracker) Comment(context.Context, string, string) error                { return nil }

func reg(src string, enabled bool) modules.Registration {
	return modules.Registration{
		Surface:    modules.SurfaceTracker,
		SourceType: src,
		Capability: "tracker." + src,
		Enabled:    enabled,
		Adapter:    fakeTracker{src: src},
	}
}

// moduleStepRegistrars lets each connector module bind its own acceptance scenario in its OWN
// <module>_steps_test.go via an init() append — so parallel module work never edits this shared harness.
// initializeScenario invokes every registrar after wiring the framework (REQ-821) steps.
var moduleStepRegistrars []func(*godog.ScenarioContext)

func TestConnectorsAcceptance(t *testing.T) {
	suite := godog.TestSuite{
		Name:                "spec/008 connectors",
		ScenarioInitializer: initializeScenario,
		Options:             &godog.Options{Format: "pretty", Paths: []string{"."}, Tags: "~@pending", Strict: true, TestingT: t},
	}
	if suite.Run() != 0 {
		t.Fatal("spec/008 acceptance scenarios failed")
	}
}

type world struct {
	reg *modules.Registry

	// INV-17 trace
	unregErr    error
	disabledErr error
	enabledOK   bool

	// INV-18 trace
	dupErr        error
	diffSourceErr error
}

func initializeScenario(sc *godog.ScenarioContext) {
	w := &world{}

	// ---- REQ-821 / INV-17: an unregistered or disabled module has no execution path ----
	sc.Step(`^a capability-scoped module registry$`, func() error {
		w.reg = modules.NewRegistry()
		return nil
	})
	sc.Step(`^a module is resolved before registration and again while registered but disabled$`, func() error {
		// (a) unregistered → no path
		if _, err := w.reg.Resolve(modules.SurfaceTracker, "youtrack"); err != nil {
			w.unregErr = err
		}
		// (b) registered but disabled → still no path
		if err := w.reg.Register(reg("youtrack", false)); err != nil {
			return fmt.Errorf("registering a disabled module must succeed: %w", err)
		}
		if _, err := w.reg.Resolve(modules.SurfaceTracker, "youtrack"); err != nil {
			w.disabledErr = err
		}
		// (c) enable it → path granted
		if err := w.reg.SetEnabled(modules.SurfaceTracker, "youtrack", true); err != nil {
			return fmt.Errorf("enabling a registered module must succeed: %w", err)
		}
		_, err := w.reg.Resolve(modules.SurfaceTracker, "youtrack")
		w.enabledOK = err == nil
		return nil
	})
	sc.Step(`^the registry denies an execution path both times and grants one only once the module is enabled$`, func() error {
		if !errors.Is(w.unregErr, modules.ErrNoExecutionPath) {
			return fmt.Errorf("an unregistered module must have no execution path, got %v", w.unregErr)
		}
		if !errors.Is(w.disabledErr, modules.ErrNoExecutionPath) {
			return fmt.Errorf("a disabled module must have no execution path, got %v", w.disabledErr)
		}
		if !w.enabledOK {
			return fmt.Errorf("an enabled registered module must resolve to an execution path")
		}
		return nil
	})

	// ---- REQ-821 / INV-18: exactly one registered implementation per surface+source ----
	sc.Step(`^a capability-scoped module registry with one module registered for a surface and source type$`, func() error {
		w.reg = modules.NewRegistry()
		return w.reg.Register(reg("youtrack", true))
	})
	sc.Step(`^a second module is registered for the same surface and source type$`, func() error {
		w.dupErr = w.reg.Register(reg("youtrack", true))    // same (tracker, youtrack) → must reject
		w.diffSourceErr = w.reg.Register(reg("jira", true)) // different source type → must accept
		return nil
	})
	sc.Step(`^the duplicate registration is rejected and a different source type is accepted$`, func() error {
		if !errors.Is(w.dupErr, modules.ErrDuplicateSource) {
			return fmt.Errorf("a duplicate (surface, source) registration must be rejected, got %v", w.dupErr)
		}
		if w.diffSourceErr != nil {
			return fmt.Errorf("a different source type must be accepted, got %v", w.diffSourceErr)
		}
		return nil
	})

	// Bind every registered connector module's own scenario steps (each lives in its own *_steps_test.go).
	for _, register := range moduleStepRegistrars {
		register(sc)
	}
}
