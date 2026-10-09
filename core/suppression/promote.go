package suppression

import "time"

// PromotionThreshold is the observe-before-live bar: a schedule reaches the live state only after at
// least this many observed in-window boots confirm the observing row (REQ-404).
const PromotionThreshold = 2

// Boot is one recorded boot, used for promotion accounting.
type Boot struct {
	At time.Time
}

// Promote drives a schedule's observe-before-live lifecycle. It is the ONLY transition that lets a row
// suppress:
//   - a cron no longer present on the host (drift) or a row past valid_until (expiry) → disabled;
//   - otherwise it counts the boots that fall inside the row's DST-correct window, and flips the row to
//     live once that count reaches the promotion threshold. A wrong attribution never accumulates two
//     in-window boots and stays observing.
//
// It returns the resulting status. The row is addressed by its FULL identity (host, kind, signature), so
// promotion evidence can never be applied to a different schedule than the one it was observed for.
func (r *ScheduleRegistry) Promote(k ScheduleKey, w WindowEvaluator, boots []Boot, cronStillPresent bool, now time.Time) SchedStatus {
	r.mu.Lock() // hold across the whole read-modify so a concurrent promote can't lose a boot / half-transition
	defer r.mu.Unlock()
	sc, ok := r.getLocked(k)
	if !ok {
		return SchDisabled
	}
	if !cronStillPresent {
		sc.Status = SchDisabled
		r.mirrorLocked(sc)
		return SchDisabled
	}
	if !sc.ValidUntil.IsZero() && now.After(sc.ValidUntil) {
		sc.Status = SchDisabled
		r.mirrorLocked(sc)
		return SchDisabled
	}
	// Accumulate DISTINCT in-window boots across runs, deduped by exact timestamp and capped, so a single
	// boot seen in overlapping journalctl lookbacks cannot be counted twice (which would promote on ONE
	// boot, defeating observe-before-live), and evidence accrues across weekly passes rather than being
	// overwritten by the latest lookback.
	for _, b := range boots {
		if !w.Contains(*sc, b.At) {
			continue
		}
		if containsTime(sc.ObservedBoots, b.At) || len(sc.ObservedBoots) >= observedBootCap {
			continue
		}
		sc.ObservedBoots = append(sc.ObservedBoots, b.At)
	}
	// A row reloaded from the durable store keeps its EARNED ObservedCount but not its ObservedBoots slice
	// (only the count is persisted — TG-225). Recomputing the count from the (now-empty) slice would silently
	// reset a live row's evidence trail downward on the first post-restart Promote (e.g. 2 → 0/1); guard so an
	// already-live row's count can only grow, never regress below what earned it live. Observing rows recompute
	// from the boots they are still accumulating, as before.
	n := len(sc.ObservedBoots)
	if !(sc.Status == SchLive && n < sc.ObservedCount) {
		sc.ObservedCount = n
	}
	if sc.ObservedCount >= PromotionThreshold {
		sc.Status = SchLive
	}
	r.mirrorLocked(sc)
	return sc.Status
}

// observedBootCap bounds the accumulated boot set — evidence beyond this adds nothing and unbounded growth
// is a leak.
const observedBootCap = 10

func containsTime(ts []time.Time, t time.Time) bool {
	for _, x := range ts {
		if x.Equal(t) {
			return true
		}
	}
	return false
}
