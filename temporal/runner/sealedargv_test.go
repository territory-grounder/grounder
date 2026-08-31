package runner

import (
	"encoding/json"
	"testing"

	"github.com/territory-grounder/grounder/core/actuate/opschema"
	"github.com/territory-grounder/grounder/core/manifest"
	"github.com/territory-grounder/grounder/core/regime"
	awxjob "github.com/territory-grounder/grounder/modules/actuation/awxjob"
)

func jsonUnmarshal(b []byte, v any) error { return json.Unmarshal(b, v) }

func eqArgv(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// (b) restart-service WITH the structured unit builds exactly [systemctl restart nginx] — never split from
// the free-text Op.
func TestSealedArgvRestartServiceWithUnit(t *testing.T) {
	got := sealedArgv(manifest.Action{OpClass: "restart-service", Op: "restart", Params: map[string]string{"unit": "nginx"}})
	if !eqArgv(got, []string{"systemctl", "restart", "nginx"}) {
		t.Fatalf("sealedArgv(restart-service, unit=nginx) must be [systemctl restart nginx], got %v", got)
	}
}

// restart-service with NO unit yields a nil argv (fail closed) — the runner never fabricates a program.
func TestSealedArgvRestartServiceNoUnitFailsClosed(t *testing.T) {
	if got := sealedArgv(manifest.Action{OpClass: "restart-service", Op: "restart"}); got != nil {
		t.Fatalf("sealedArgv(restart-service, no unit) must be nil (fail closed), got %v", got)
	}
	if got := sealedArgv(manifest.Action{OpClass: "kubectl-get", Op: "get"}); got != nil {
		t.Fatalf("sealedArgv(unregistered op_class) must be nil (fail closed), got %v", got)
	}
}

// (c) The ONE-place proof at the runner boundary: sealedArgv does not construct the argv itself — it returns
// EXACTLY what the op-class schema registry builds from the same params. If the two ever diverged, the runner
// would be defining a second, drift-prone argv (the bug this change removes).
func TestSealedArgvDelegatesToRegistry(t *testing.T) {
	for _, params := range []map[string]string{
		{"unit": "nginx"},
		{"unit": "nginx.service"},
	} {
		act := manifest.Action{OpClass: "restart-service", Op: "restart", Params: params}
		want, err := opschema.Argv(act.OpClass, act.Params)
		if err != nil {
			t.Fatalf("registry must build the argv for %v: %v", params, err)
		}
		if got := sealedArgv(act); !eqArgv(got, want) {
			t.Fatalf("sealedArgv must equal the registry-built argv (single source of truth): got %v want %v", got, want)
		}
	}
}

// --- sealEffect: the effect-kind seam (ssh-argv | awx-launch) — TG-139 -----------------------------------

func TestSealEffectSSHArgvIsUnchanged(t *testing.T) {
	argv, stdin := sealEffect(Deps{}, manifest.Action{OpClass: "restart-service", Op: "restart", Params: map[string]string{"unit": "nginx"}}, "web01")
	if stdin != nil {
		t.Fatalf("an ssh-argv effect must carry no stdin, got %q", stdin)
	}
	if want := []string{"systemctl", "restart", "nginx"}; !eqArgv(argv, want) {
		t.Fatalf("ssh-argv effect = %v, want %v", argv, want)
	}
}

func TestSealEffectAWXLaunchEncodesLaunchSpec(t *testing.T) {
	d := Deps{AWXTemplateForOpClass: func(op string) (int, bool) {
		if op == "disk-grow" {
			return 42, true
		}
		return 0, false
	}}
	argv, stdin := sealEffect(d, manifest.Action{OpClass: "disk-grow", Op: "grow", Params: map[string]string{"filesystem": "/var", "grow_by": "10G"}}, "db01")
	if !eqArgv(argv, []string{awxjob.LaunchVerb}) {
		t.Fatalf("awx-launch argv = %v, want [%s]", argv, awxjob.LaunchVerb)
	}
	var spec awxjob.LaunchSpec
	if err := jsonUnmarshal(stdin, &spec); err != nil {
		t.Fatalf("awx-launch stdin must decode to a LaunchSpec: %v", err)
	}
	if spec.TemplateID != 42 {
		t.Fatalf("LaunchSpec.TemplateID = %d, want 42 (from the op-class→template config)", spec.TemplateID)
	}
	if spec.OpClass != "disk-grow" {
		t.Fatalf("LaunchSpec.OpClass = %q, want disk-grow (cross-checked at the leaf)", spec.OpClass)
	}
	if spec.Limit != "db01" {
		t.Fatalf("LaunchSpec.Limit = %q, want the incident target host db01", spec.Limit)
	}
	if spec.ExtraVars["filesystem"] != "/var" || spec.ExtraVars["grow_by"] != "10G" {
		t.Fatalf("LaunchSpec.ExtraVars = %v, want the params as extra_vars {filesystem:/var, grow_by:10G}", spec.ExtraVars)
	}
}

func TestSealEffectAWXLaunchFailsClosedWithoutTemplate(t *testing.T) {
	// No resolver wired (no AWX config) ⇒ empty effect ⇒ the leaf refuses.
	if argv, stdin := sealEffect(Deps{}, manifest.Action{OpClass: "disk-grow", Op: "grow", Params: map[string]string{"filesystem": "/var", "grow_by": "10G"}}, "db01"); argv != nil || stdin != nil {
		t.Fatalf("awx-launch with NO template config must fail closed (nil,nil), got argv=%v stdin=%q", argv, stdin)
	}
	// A resolver that binds no template for this op-class ⇒ fail closed.
	d := Deps{AWXTemplateForOpClass: func(string) (int, bool) { return 0, false }}
	if argv, stdin := sealEffect(d, manifest.Action{OpClass: "disk-grow", Op: "grow", Params: map[string]string{"filesystem": "/var", "grow_by": "10G"}}, "db01"); argv != nil || stdin != nil {
		t.Fatalf("awx-launch with ok=false must fail closed, got argv=%v stdin=%q", argv, stdin)
	}
	// A non-positive template id ⇒ EncodeLaunch fails ⇒ fail closed.
	d2 := Deps{AWXTemplateForOpClass: func(string) (int, bool) { return 0, true }}
	if argv, _ := sealEffect(d2, manifest.Action{OpClass: "disk-grow", Op: "grow", Params: map[string]string{"filesystem": "/var", "grow_by": "10G"}}, "db01"); argv != nil {
		t.Fatalf("awx-launch with a non-positive template id must fail closed, got argv=%v", argv)
	}
}

// TestEffectKindRegime covers the runner's effect-kind → regime routing (spec/017): an awx-launch class routes
// to the awx-job regime BY KIND; a ssh-argv or unregistered class routes by the TARGET (ok=false).
func TestEffectKindRegime(t *testing.T) {
	if reg, byKind := effectKindRegime("disk-grow"); !byKind || reg != regime.RegimeAWXJob {
		t.Fatalf("disk-grow (awx-launch) must route to %q by kind, got reg=%q byKind=%v", regime.RegimeAWXJob, reg, byKind)
	}
	if _, byKind := effectKindRegime("restart-service"); byKind {
		t.Fatal("restart-service (ssh-argv) must route by target, not by effect kind")
	}
	if _, byKind := effectKindRegime("nonexistent-op-xyz"); byKind {
		t.Fatal("an unregistered op-class must not route by kind (SelectLane then fails closed)")
	}
}

// TestStartGuestRoutesToProxmoxByKind: start-guest (proxmox-lifecycle) routes to the proxmox regime BY KIND
// and seals a fixed [start, <guest>] argv with no stdin (argv-encoded, like ssh-argv).
func TestStartGuestRoutesToProxmoxByKind(t *testing.T) {
	reg, byKind := effectKindRegime("start-guest")
	if !byKind || reg != regime.RegimeProxmox {
		t.Fatalf("start-guest must route to %q by kind, got reg=%q byKind=%v", regime.RegimeProxmox, reg, byKind)
	}
	argv, stdin := sealEffect(Deps{}, manifest.Action{OpClass: "start-guest", Op: "start", Params: map[string]string{"guest": "librespeed01"}}, "librespeed01")
	if stdin != nil {
		t.Fatalf("proxmox-lifecycle (argv-encoded) must carry no stdin, got %q", stdin)
	}
	if want := []string{"start", "librespeed01"}; !eqArgv(argv, want) {
		t.Fatalf("start-guest effect argv = %v, want %v", argv, want)
	}
}
