package mattermost

import (
	"context"
	"sort"

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
	return notifier.ProbeDelivery(ctx, m, operator, firstChannel(m.channels))
}

// Compile-time proof that the capability is actually implemented. Without this a rename of the interface
// method would silently demote this module to "no test is implemented" — a control quietly wired to
// nothing, which is the defect the whole configuration surface exists to remove.
var _ selftest.Tester = (*Module)(nil)

// firstChannel names ONE configured channel for the probe's report, deterministically.
//
// Mattermost routes by an opaque 26-character channel ID, so the map's key (the human channel name) is
// the only part an operator can recognise. Sorted rather than taken from map order because an operator
// pressing TEST twice and being told two different destinations would reasonably conclude the routing is
// unstable, when only Go's map iteration is.
func firstChannel(chans map[string]string) string {
	names := make([]string, 0, len(chans))
	for name := range chans {
		names = append(names, name)
	}
	if len(names) == 0 {
		return ""
	}
	sort.Strings(names)
	return names[0]
}
