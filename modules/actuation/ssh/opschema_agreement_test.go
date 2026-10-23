package ssh

import (
	"testing"

	"github.com/territory-grounder/grounder/core/actuate/opschema"
	"github.com/territory-grounder/grounder/core/safety"
)

// Step-4 agreement: the ssh effect leaf does NOT define its own restart-service argv shape — resolveOp builds
// the FORWARD argv from the SAME op-class schema registry the runner (sealedArgv) and the interceptor read, so
// the `systemctl restart <unit>` shape lives in exactly ONE place. This proves the effect leaf and the
// registry agree (and that guardMutatingArgv's argvEqual re-check is against the registry-built shape).
func TestResolveOpArgvMatchesRegistry(t *testing.T) {
	m := New("web01", "svc-agent", &fakeRunner{}, WithMutation(safety.NewActuatingChokepoint(), []string{"nginx", "sshd"}, nil))
	for _, unit := range []string{"nginx", "sshd"} {
		cmd, rollback, err := m.resolveOp(OpClassRestartService, unit)
		if err != nil {
			t.Fatalf("resolveOp(%q) must succeed for an allowlisted unit: %v", unit, err)
		}
		want, werr := opschema.Argv(OpClassRestartService, map[string]string{opschema.ParamUnit: unit})
		if werr != nil {
			t.Fatalf("registry must build the argv for %q: %v", unit, werr)
		}
		if len(cmd) != len(want) {
			t.Fatalf("effect-leaf argv %v must equal the registry-built argv %v (single source of truth)", cmd, want)
		}
		for i := range want {
			if cmd[i] != want[i] {
				t.Fatalf("effect-leaf argv %v must equal the registry-built argv %v (single source of truth)", cmd, want)
			}
		}
		// the rollback is a re-restart — the same forward argv, but a distinct slice (INV-07 bound inverse).
		if len(rollback) != len(cmd) {
			t.Fatalf("rollback must be the compensating re-restart (same shape), got %v vs %v", rollback, cmd)
		}
	}
}
