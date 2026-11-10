package lessons

import (
	"strings"
	"time"
)

// DefaultMaxAge is the retention horizon a precedent lives under when the deployment sets no TG_LESSONS_MAX_AGE
// (owner-accepted default, 2026-11-07; TG-673, deep-review F14). Until then the knob defaulted to 0 — pruning
// OFF — so a lesson learned once was precedent forever and the recency decay (spec/018) had nothing to act on
// unless an operator remembered to arm it. Ninety days keeps a season of estate behaviour and lets a lesson
// about a topology that has since changed age out on its own.
const DefaultMaxAge = 90 * 24 * time.Hour

// ResolveMaxAge turns the raw TG_LESSONS_MAX_AGE value into the horizon the lessons feed and the live
// writeback both apply, and says how it decided (for the boot log). Three states, stated so an operator can
// tell them apart: blank ⇒ DefaultMaxAge ("default"); an explicit zero or "off" ⇒ 0, pruning disabled ("off");
// a parseable positive duration ⇒ that duration ("set"). An unparseable or negative value falls back to the
// default and says so — a typo must not silently switch the decay off.
func ResolveMaxAge(raw string) (time.Duration, string) {
	v := strings.ToLower(strings.TrimSpace(raw))
	switch v {
	case "":
		return DefaultMaxAge, "default"
	case "0", "0s", "off", "none", "never":
		return 0, "off"
	}
	d, err := time.ParseDuration(v)
	if err != nil || d < 0 {
		return DefaultMaxAge, "invalid-using-default"
	}
	if d == 0 {
		return 0, "off"
	}
	return d, "set"
}

// Stale reports whether a resolved incident is older than maxAge at now — the single predicate both source
// paths (the operator-export feed and the live close-out writeback) apply before learning from it, so neither
// path can re-teach a precedent the reconciliation would prune on its next pass. maxAge <= 0 (off) or an
// undatable incident (zero ResolvedAt) is never stale: pruning fails toward retention, exactly like Reconcile.
func Stale(ri ResolvedIncident, now time.Time, maxAge time.Duration) bool {
	return maxAge > 0 && !ri.ResolvedAt.IsZero() && now.Sub(ri.ResolvedAt) > maxAge
}
