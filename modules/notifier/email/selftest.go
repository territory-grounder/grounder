package email

import (
	"context"
	"strconv"

	notifier "github.com/territory-grounder/grounder/adapters/notifier"
	"github.com/territory-grounder/grounder/core/selftest"
)

// SelfTest proves delivery by DELIVERING. See adapters/notifier.ProbeDelivery for why a notifier is the
// one surface whose probe must emit, and for the marker, attribution and non-ballot properties that make
// an outward-visible probe acceptable.
//
// It runs the real Notify path — the same credential resolution, the same routing, the same transport —
// because a check that the configured values are non-empty passes against a revoked credential, a
// destination this account can no longer post to, and a server that has been down for a week, which are
// the three faults an operator presses TEST to rule out.
func (m *Module) SelfTest(ctx context.Context, operator string) (selftest.Result, error) {
	return notifier.ProbeDelivery(ctx, m, operator, firstRecipient(m.to))
}

// Compile-time proof that the capability is actually implemented. Without this a rename of the interface
// method would silently demote this module to "no test is implemented" — a control quietly wired to
// nothing, which is the defect the whole configuration surface exists to remove.
var _ selftest.Tester = (*Module)(nil)

// firstRecipient names ONE configured recipient for the probe's report. The message goes to every
// configured address; naming the first is enough for an operator to recognise a mis-addressed lane, and
// the count makes a truncated list visible rather than implied.
func firstRecipient(to []string) string {
	if len(to) == 0 {
		return ""
	}
	if len(to) == 1 {
		return to[0]
	}
	return to[0] + " (and " + strconv.Itoa(len(to)-1) + " more)"
}
