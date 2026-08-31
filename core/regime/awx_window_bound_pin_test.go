package regime_test

import (
	"testing"
	"time"

	"github.com/territory-grounder/grounder/core/actuate/opschema"
	"github.com/territory-grounder/grounder/core/regime"
)

// The T-029-1 drift guard: opschema.AwxWindowFloorSeconds repeats regime.DefaultVerificationBound as
// a literal (importing regime from opschema would cycle). If either side moves alone, an awx
// commit-confirmed window could validate BELOW the bound the deferred verify may legitimately take —
// firing an inverse against a change whose verdict is still in flight (spec/029 sign-off, TG-488 B5).
func TestAwxWindowFloorMatchesTheDeferredVerifyBound(t *testing.T) {
	if got, want := time.Duration(opschema.AwxWindowFloorSeconds)*time.Second, regime.DefaultVerificationBound; got != want {
		t.Fatalf("opschema.AwxWindowFloorSeconds (%v) drifted from regime.DefaultVerificationBound (%v) — move them together", got, want)
	}
}
