package mcp

import (
	"context"
	"errors"
	"testing"

	actuation "github.com/territory-grounder/grounder/adapters/actuation"
)

type fakeRunner struct{ ran string }

func (f *fakeRunner) Run(_ context.Context, tool string, _ []string, _ []byte) (actuation.Result, error) {
	f.ran = tool
	return actuation.Result{}, nil
}

func TestUnregisteredToolHasNoExecutionPath(t *testing.T) {
	f := &fakeRunner{}
	m := New(f)
	m.RegisterTool("k8s.get", false)
	if _, err := m.Exec(context.Background(), []string{"unregistered.tool"}, nil); !errors.Is(err, ErrNoExecutionPath) {
		t.Fatalf("an unregistered tool must have no execution path, got %v", err)
	}
	if f.ran != "" {
		t.Fatalf("an unregistered tool must never reach the runner, ran %q", f.ran)
	}
}

func TestMutatingToolBehindEnableFlag(t *testing.T) {
	f := &fakeRunner{}
	m := New(f)
	m.RegisterTool("k8s.get", false)    // read-only
	m.RegisterTool("k8s.rollout", true) // mutating
	// read-only tool runs.
	if _, err := m.Exec(context.Background(), []string{"k8s.get", "pods"}, nil); err != nil {
		t.Fatalf("a registered read-only tool must run: %v", err)
	}
	// mutating tool is withheld while the enable flag is unset.
	if _, err := m.Exec(context.Background(), []string{"k8s.rollout"}, nil); err == nil {
		t.Fatal("a mutating tool must be withheld behind the disabled enable flag")
	}
	// once enabled, the mutating tool runs.
	m2 := New(f, WithMutationEnabled())
	m2.RegisterTool("k8s.rollout", true)
	if _, err := m2.Exec(context.Background(), []string{"k8s.rollout"}, nil); err != nil {
		t.Fatalf("an enabled mutating tool must run: %v", err)
	}
}

func TestRegisteredManifestSorted(t *testing.T) {
	m := New(&fakeRunner{})
	m.RegisterTool("b.tool", false)
	m.RegisterTool("a.tool", false)
	got := m.Registered()
	if len(got) != 2 || got[0] != "a.tool" || got[1] != "b.tool" {
		t.Fatalf("registered manifest must be sorted, got %v", got)
	}
	if m.Capability() != "mcp" || !m.ReadOnly() {
		t.Errorf("capability/read-only wrong")
	}
}
