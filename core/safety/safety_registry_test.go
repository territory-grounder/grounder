package safety_test

import (
	"testing"

	"github.com/territory-grounder/grounder/core/actuate/opschema"
	"github.com/territory-grounder/grounder/core/safety"
)

// lifecycleFamilies are the op-class families whose verbs can start, stop or replace a running thing — and
// can therefore ORPHAN THE SESSION ISSUING THEM when the target happens to host the control plane. Every
// op-class in one of these families must be recognised by IsRestartClass, or the self-protected control-plane
// veto silently does not apply to it.
var lifecycleFamilies = map[string]bool{
	opschema.FamilyServiceLifecycle:   true,
	opschema.FamilyContainerLifecycle: true,
	opschema.FamilyGuestLifecycle:     true,
}

// TestEveryLifecycleOpClassIsRecognisedAsRestartClass closes the hole that shipped: restartClassRE was a
// HAND-MAINTAINED list of slugs, its own comment said it "MUST list every op-class the effect leaves can
// actuate", and it did not — `start-guest` was absent, the class with 219 hands-off heals on this estate.
//
// A hand-maintained list next to a growing registry rots silently and in the permissive direction. This
// asserts the list against the LIVE registry, so the next lifecycle verb either matches or reds CI.
func TestEveryLifecycleOpClassIsRecognisedAsRestartClass(t *testing.T) {
	t.Parallel()
	specs := opschema.Specs()
	if len(specs) == 0 {
		t.Fatal("registry is empty — this test would pass vacuously")
	}
	checked := 0
	for _, s := range specs {
		if !lifecycleFamilies[s.Family] {
			continue
		}
		checked++
		if !safety.IsRestartClass(s.OpClass) {
			t.Errorf("op-class %q (family %s) is a lifecycle verb but IsRestartClass says NO — the "+
				"self-protected control-plane veto does not apply to it", s.OpClass, s.Family)
		}
	}
	if checked == 0 {
		t.Fatal("no lifecycle-family op-class was checked — the test must not pass by finding nothing")
	}
}

// TestStatefulDenyMatchesThisEstatesHostnames pins the fix for a control that was INERT in production.
// The ported regex carried a leading \b tuned to the predecessor's naming; this estate's hostnames are
// unbroken (dc1cl01mariadb01), so the clamp never fired on a real target.
func TestStatefulDenyMatchesThisEstatesHostnames(t *testing.T) {
	t.Parallel()
	for _, h := range []string{
		"dc1cl01mariadb01", "dc2cl01mariadb01", "dc1cl01postgres01",
		"dc1redis01", "dc1k8s-openbao01",
	} {
		if !safety.IsStatefulWorkload(h) {
			t.Errorf("IsStatefulWorkload(%q) = false — a stateful host on THIS estate must clamp", h)
		}
	}
	// and it must not clamp everything: a false positive is cheap, but a blanket match would make the
	// signal meaningless and every action a poll.
	for _, h := range []string{"dc1mealie01", "dc1ghostfolio01", "dc1pve01", "dc1librespeed01"} {
		if safety.IsStatefulWorkload(h) {
			t.Errorf("IsStatefulWorkload(%q) = true — a non-stateful host must not clamp", h)
		}
	}
}

// TestDestructiveBackstopCatchesUnadornedDeletes closes the third inert-control hole found in the same
// review. IsDestructiveOp is the server-side backstop for "a plan cannot hide a mutation": it reads the
// ACTUAL op string, independent of whatever op_class the model declared. It covered `rm -rf` and nothing
// plainer, so a proposal declaring op_class="restart-service" whose op is `rm /var/log/x` was NOT caught —
// and a plain `rm <path>` is the third most common mutation in the predecessor's history (153 executions).
func TestDestructiveBackstopCatchesUnadornedDeletes(t *testing.T) {
	t.Parallel()
	for _, op := range []string{
		"rm /var/log/big.log",        // the unadorned delete that shipped uncaught
		"rm -rf /var/tmp/x",          // the dramatic one already covered — regression guard
		"rmdir /tmp/emptydir",        //
		"unlink /var/lib/thing.sock", //
		"truncate -s 0 /var/log/y",   // emptying destroys contents and leaves the path looking intact
		"docker system prune -af",    // pre-existing cover — regression guard
		"mkfs.ext4 /dev/sdb",         //
	} {
		if !safety.IsDestructiveOp(op) {
			t.Errorf("IsDestructiveOp(%q) = false — a destructive op must be caught even when the model "+
				"declares a benign op_class", op)
		}
	}
	// The word boundary must still DISCRIMINATE. Without it "confirm", "transform" and "performance" all
	// contain "rm" and every action would be floored — which makes the signal useless rather than safe.
	for _, op := range []string{
		"systemctl restart nginx", "docker restart mealie", "start-guest", "restart-service",
		"confirm the change", "transform data", "performance check",
	} {
		if safety.IsDestructiveOp(op) {
			t.Errorf("IsDestructiveOp(%q) = true — a non-destructive op must not be floored", op)
		}
	}
}
