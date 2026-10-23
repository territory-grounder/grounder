package kubernetes

import (
	"context"
	"strings"
	"testing"

	actuation "github.com/territory-grounder/grounder/adapters/actuation"
)

type fakeRunner struct{ argv []string }

func (f *fakeRunner) Run(_ context.Context, argv []string, _ []byte) (actuation.Result, error) {
	f.argv = argv
	return actuation.Result{}, nil
}

func TestDeleteAndDrainClampedToFloor(t *testing.T) {
	m := New("prod", &fakeRunner{})
	for _, op := range []string{"delete", "drain"} {
		if _, err := m.Operation(op, "pod", "x"); err == nil || !strings.Contains(err.Error(), "never-auto floor") {
			t.Errorf("%q must be clamped to the never-auto floor, got %v", op, err)
		}
	}
	// clamped through Exec too (defense in depth), regardless of confidence/policy.
	f := &fakeRunner{}
	m2 := New("prod", f)
	if _, err := m2.Exec(context.Background(), []string{"delete", "deployment", "web"}, nil); err == nil {
		t.Fatal("delete via Exec must be clamped")
	}
	if f.argv != nil {
		t.Fatalf("a clamped op must never reach the runner, got %v", f.argv)
	}
}

// A capitalized/whitespaced floor op must still be clamped (never reach the runner) — regression for the
// case-sensitivity bypass an adversarial review found.
func TestFloorClampIsCaseInsensitive(t *testing.T) {
	f := &fakeRunner{}
	m := New("prod", f)
	for _, op := range []string{"Delete", "DRAIN", "dElEtE", " delete "} {
		if _, err := m.Operation(op, "pod", "x"); err == nil || !strings.Contains(err.Error(), "never-auto floor") {
			t.Errorf("floor op %q must be clamped, got %v", op, err)
		}
	}
	if _, err := m.Exec(context.Background(), []string{"Delete", "pod", "x"}, nil); err == nil {
		t.Fatal("capitalized delete via Exec must be clamped")
	}
	if f.argv != nil {
		t.Fatalf("a clamped op must never reach the runner, got %v", f.argv)
	}
}

func TestPermittedOpsBuildTypedArgv(t *testing.T) {
	m := New("prod", &fakeRunner{})
	for _, op := range []string{"get", "describe", "apply", "patch", "rollout", "scale"} {
		cmd, err := m.Operation(op, "pods")
		if err != nil {
			t.Errorf("%q must be permitted: %v", op, err)
			continue
		}
		if cmd[0] != "kubectl" || cmd[1] != "--context" || cmd[2] != "prod" {
			t.Errorf("%q argv malformed: %v", op, cmd)
		}
	}
	helm, err := m.Operation("helm", "upgrade")
	if err != nil || helm[0] != "helm" {
		t.Errorf("helm op malformed: %v %v", helm, err)
	}
	if _, err := m.Operation("teleport"); err == nil {
		t.Error("an unknown operation must be refused")
	}
}

// Destruction-equivalent operations reachable through the permitted verbs — a helm teardown
// (uninstall/delete/rollback) and `kubectl apply --prune` — are clamped to the never-auto floor, like delete;
// non-destructive helm subcommands and a plain apply stay permitted.
func TestDestructiveHelmAndApplyPruneClamped(t *testing.T) {
	m := New("prod", &fakeRunner{})
	// floored teardowns
	for _, args := range [][]string{{"uninstall", "postgres"}, {"delete", "redis"}, {"rollback", "app", "3"}} {
		if _, err := m.Operation("helm", args...); err == nil || !strings.Contains(err.Error(), "never-auto floor") {
			t.Errorf("helm %v must be clamped to the floor, got %v", args, err)
		}
	}
	if _, err := m.Operation("apply", "--prune", "-f", "partial.yaml"); err == nil || !strings.Contains(err.Error(), "never-auto floor") {
		t.Errorf("apply --prune must be clamped to the floor, got %v", err)
	}
	// still permitted: non-teardown helm + a plain apply
	if _, err := m.Operation("helm", "install", "app", "./chart"); err != nil {
		t.Errorf("helm install must be permitted: %v", err)
	}
	if _, err := m.Operation("apply", "-f", "deploy.yaml"); err != nil {
		t.Errorf("a plain apply must be permitted: %v", err)
	}
}
