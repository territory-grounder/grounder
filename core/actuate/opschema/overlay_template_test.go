package opschema

import (
	"strings"
	"testing"
)

// TestRatificationScreensTheTemplateNotTheName is the TG-669 oracle: the destructiveness check used to read
// the op-class SLUG only, so a benign name over a destructive argv passed every admission rule. The TEMPLATE is
// what executes, so it is what the server screens.
//
// RED CONTROL EXECUTED (2026-11-07): with the template loop removed from ValidateRatification, the destructive
// argv below was ADMITTED and this test failed with "a destructive argv template was ratified under a benign
// class name". The seam is the REAL safety.IsDestructiveOp here (not a stub), so the negative cases also prove
// the legitimate reconvergence verbs stay admissible — a screen that refused `systemctl stop` would brick every
// start-* inverse.
func TestRatificationScreensTheTemplateNotTheName(t *testing.T) {
	destructive := map[string][]string{
		"rm -rf":            {"rm", "-rf", "${path}"},
		"mkfs":              {"mkfs.ext4", "${device}"},
		"dd":                {"dd", "if=/dev/zero", "of=/dev/sda"},
		"terraform destroy": {"terraform", "destroy", "-auto-approve"},
		"qm destroy":        {"qm", "destroy", "${vmid}"},
	}
	for name, argv := range destructive {
		t.Run("argv:"+name, func(t *testing.T) {
			s := sampleOverlaySpec("rotate-appliance-log") // a benign NAME — the slug check alone passes it
			s.ArgvTemplate = argv
			s.Params = []ParamSpec{{Name: "path", Required: true}, {Name: "device", Required: true}, {Name: "vmid", Required: true}}
			if _, err := ValidateRatification(s, nil); err == nil {
				t.Fatalf("a destructive argv template %v was ratified under a benign class name — the server must "+
					"screen what would EXECUTE, not the slug", argv)
			} else if !strings.Contains(err.Error(), "argv_template") || !strings.Contains(err.Error(), "TG-669") {
				t.Errorf("the refusal must name the offending template so the operator re-authors it: %v", err)
			}
		})
	}
	// The rollback template is screened too: a benign forward with a destructive "undo" is the same hole.
	t.Run("rollback template", func(t *testing.T) {
		s := sampleOverlaySpec("rotate-appliance-log")
		s.RollbackTemplate = []string{"rm", "-rf", "${config}"}
		if _, err := ValidateRatification(s, nil); err == nil {
			t.Fatal("a destructive ROLLBACK template was ratified — the inverse executes too")
		} else if !strings.Contains(err.Error(), "rollback_template") {
			t.Errorf("the refusal must name the rollback template: %v", err)
		}
	})
	// The legitimate shapes the registry already ships — including the stop inverses of the start classes —
	// MUST stay admissible, or the screen is a brick rather than a floor.
	admissible := map[string][]string{
		"systemctl restart": {"systemctl", "restart", "${unit}"},
		"systemctl reload":  {"systemctl", "reload", "${unit}"},
		"systemctl start":   {"systemctl", "start", "${unit}"},
		"systemctl stop":    {"systemctl", "stop", "${unit}"},
		"docker restart":    {"docker", "restart", "${container}"},
		"docker stop":       {"docker", "stop", "${container}"},
		"logrotate":         {"logrotate", "--force", "${config}"},
	}
	for name, argv := range admissible {
		t.Run("admits:"+name, func(t *testing.T) {
			s := sampleOverlaySpec("reconverge-appliance")
			s.ArgvTemplate = argv
			s.RollbackTemplate = argv
			s.Params = []ParamSpec{{Name: "unit", Required: true}, {Name: "container", Required: true}, {Name: "config", Required: true}}
			if _, err := ValidateRatification(s, nil); err != nil {
				t.Fatalf("the legitimate reconvergence template %v was refused — the screen must be a floor under "+
					"destructive verbs, never a brick on the registry's own shapes: %v", argv, err)
			}
		})
	}
}

// TestRatificationTemplateScreenIsTheSeam pins that the screen consults the package seam (so the oracle above
// exercises the REAL predicate) and that a seam forced true refuses even a benign template — the mutation that
// proves the loop is wired, not decorative.
func TestRatificationTemplateScreenIsTheSeam(t *testing.T) {
	orig := destructiveTemplate
	t.Cleanup(func() { destructiveTemplate = orig })
	destructiveTemplate = func(...string) bool { return true }
	if _, err := ValidateRatification(sampleOverlaySpec("rotate-appliance-log"), nil); err == nil {
		t.Fatal("the template screen is not wired into ValidateRatification — a seam reading every template as " +
			"destructive still admitted one")
	}
	destructiveTemplate = func(...string) bool { return false }
	if _, err := ValidateRatification(sampleOverlaySpec("rotate-appliance-log"), nil); err != nil {
		t.Fatalf("with the seam reading nothing as destructive the benign sample must be admitted: %v", err)
	}
}
