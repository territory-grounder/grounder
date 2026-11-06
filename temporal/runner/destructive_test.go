package runner

import (
	"testing"

	"github.com/territory-grounder/grounder/core/manifest"
)

// TG-641, the G3 soak case: a start-guest whose rationale mentions a shutdown that did NOT happen.
func TestDestructiveOf_ProseAboutAShutdownDoesNotHoldAStartGuest(t *testing.T) {
	a := manifest.Action{OpClass: "start-guest", Op: "start", Target: "site01app01", Reversible: true}
	if destructiveOf(a, "the eventlog contains no evidence of a planned shutdown or disabling") {
		t.Fatal("a start-guest must not read destructive because its rationale mentions a shutdown")
	}
	if !destructiveOf(a, "then run shutdown -h now on the node") {
		t.Fatal("a rationale that spells out a shutdown command must still read destructive")
	}
	if !destructiveOf(manifest.Action{OpClass: "restart-service", Op: "dropdb prod", Target: "db01"}, "routine") {
		t.Fatal("a destructive ACTION must read destructive whatever the rationale says")
	}
}
