package observeprobe

import (
	"fmt"
	"sort"

	"github.com/territory-grounder/grounder/tools/faultinjector"
)

// ProbeState is the complete, EXPLICIT input to a probe-planning decision — pure in, pure out, so the part
// that decides which production guest to perturb is exhaustively testable without touching a machine (the same
// discipline faultinjector.PlanNext follows, and for the same reason: this eventually breaks a live estate).
type ProbeState struct {
	// Unobservable is the census-UNOBSERVABLE host set (core/observe: never-fired-ever), the population this
	// probe exists to test. A host is a probe CANDIDATE only if it is ALSO a sanctioned guinea-pig.
	Unobservable []string
	// Pool is the guinea-pig injection pool (faultinjector's NL-only, Tier-B, oas-excluded pool). A census-
	// unobservable host that is NOT in this pool is UNCOVERABLE — the probe reports it, never guesses at it.
	Pool []faultinjector.PoolGuest
	// Allowlist is TG_PROXMOX_ALLOWED_GUESTS — the guests TG itself may actuate. Probing a host TG cannot heal
	// would risk stranding it, so a non-allowlisted candidate is uncoverable too.
	Allowlist map[string]bool
	// Status is vmid -> "running"|"stopped" from the LIVE cluster snapshot. Never probe blind: a snapshot too
	// short even to describe the pool refuses the whole cycle.
	Status map[string]string
	// Outstanding is faultinjector's durable restore ledger — the NO-STACKING input. A host that already owes a
	// restore is never probed (faultinjector INVARIANT 2 carried across the two engines, which share one pool:
	// stacking a probe onto a pending fault is how an in-guest cleanup gets destroyed).
	Outstanding []faultinjector.Outstanding
	// AlreadyProbed is the set of hosts that already carry a PENDING probe or a TERMINAL verdict — do not
	// re-probe them (a pending host is mid-window; a confirmed host is answered). A host whose only probe was
	// INCONCLUSIVE is NOT here: that probe never ran, so the question is still open and it is re-probeable.
	AlreadyProbed map[string]bool
	// Classes is the operator-preferred probe fault-class order. The planner picks the first class in this list
	// the chosen guest can host (faultinjector.GuestSupportsClass). Configuration, not code.
	Classes []faultinjector.Class

	BreakerOpen bool
	KillSwitch  bool
}

// ProbeDecision is the planner's output. Act=false ALWAYS carries a Reason — a silent no-op is
// indistinguishable from a stuck orchestrator, and this runs unattended. Uncoverable is ALWAYS populated: the
// census-unobservable hosts that structurally cannot be probed (not a guinea-pig, or not TG-actuatable). It is
// the ticket's "no silent sampling caps" — the uncovered remainder is surfaced explicitly, never dropped, so a
// coverage number can never quietly exclude the entities it could not reach.
type ProbeDecision struct {
	Act         bool
	Guest       faultinjector.PoolGuest
	Class       faultinjector.Class
	Reason      string
	Uncoverable []string
}

// PlanProbe selects, from the census-unobservable set, ONE guinea-pig to probe next and the fault class to
// inject — or refuses with a reason. It is FAIL-CLOSED at every step (kill switch, breaker open, blind
// snapshot, pool/allowlist disagreement, no-stacking, already-probed): refusing to probe is always safe;
// probing wrongly perturbs a production guest. It NEVER injects — it returns the plan for the (default-OFF)
// orchestrator to act on or merely log.
func PlanProbe(st ProbeState) ProbeDecision {
	// Uncoverable is computed over the WHOLE unobservable set, independent of which candidate is picked, so the
	// remainder is reported even on a cycle that also injects.
	uncoverable := uncoverableHosts(st)

	switch {
	case st.KillSwitch:
		return ProbeDecision{Reason: "kill-switch engaged", Uncoverable: uncoverable}
	case st.BreakerOpen:
		return ProbeDecision{Reason: "TG mutation breaker is OPEN — the estate is already unhappy; not adding probe load", Uncoverable: uncoverable}
	case len(st.Unobservable) == 0:
		return ProbeDecision{Reason: "no census-unobservable entities to probe", Uncoverable: uncoverable}
	case len(st.Pool) == 0:
		return ProbeDecision{Reason: "empty guinea-pig pool", Uncoverable: uncoverable}
	case len(st.Classes) == 0:
		return ProbeDecision{Reason: "no probe class configured", Uncoverable: uncoverable}
	case len(st.Status) < len(st.Pool):
		return ProbeDecision{Reason: fmt.Sprintf("cluster snapshot too short (%d entries for a %d-guest pool) — refusing to probe blind", len(st.Status), len(st.Pool)), Uncoverable: uncoverable}
	}

	byName := make(map[string]faultinjector.PoolGuest, len(st.Pool))
	for _, g := range st.Pool {
		byName[g.Name] = g
	}
	busy := make(map[string]bool, len(st.Outstanding))
	for _, o := range st.Outstanding {
		busy[o.Host] = true
	}

	// Deterministic order so a given state always probes the same next host (stable tests, even pool sweep).
	cands := append([]string(nil), st.Unobservable...)
	sort.Strings(cands)

	seen := map[string]bool{}
	for _, h := range cands {
		if h == "" || seen[h] {
			continue
		}
		seen[h] = true
		g, inPool := byName[h]
		switch {
		case !inPool || !st.Allowlist[h]:
			continue // UNCOVERABLE (already recorded) — not a guinea-pig we may safely probe; never guessed at
		case busy[h]:
			continue // no-stacking: it already owes a restore
		case st.AlreadyProbed[h]:
			continue // pending or already-confirmed — the question is open elsewhere or answered
		case st.Status[g.VMID] != "running":
			continue // a stopped guest cannot be perturbed into an alert; probe it once it is back
		}
		if c, ok := firstSupportedClass(st.Classes, g); ok {
			return ProbeDecision{Act: true, Guest: g, Class: c, Uncoverable: uncoverable}
		}
		// In pool + allowlisted + free + running, but hosts no configured probe class — skip to the next.
	}
	return ProbeDecision{
		Reason: fmt.Sprintf("no probeable candidate (%d distinct unobservable, %d uncoverable — not a guinea-pig or not TG-actuatable; the remainder busy, stopped, already-probed, or hosting no configured class)",
			distinctNonBlank(st.Unobservable), len(uncoverable)),
		Uncoverable: uncoverable,
	}
}

// uncoverableHosts is the census-unobservable set the probe structurally cannot reach: a host that is not a
// pool guinea-pig, or is a guinea-pig TG may not actuate. Distinct, sorted, blanks dropped.
func uncoverableHosts(st ProbeState) []string {
	inPool := make(map[string]bool, len(st.Pool))
	for _, g := range st.Pool {
		inPool[g.Name] = true
	}
	seen := map[string]bool{}
	var out []string
	for _, h := range st.Unobservable {
		if h == "" || seen[h] {
			continue
		}
		seen[h] = true
		if !inPool[h] || !st.Allowlist[h] {
			out = append(out, h)
		}
	}
	sort.Strings(out)
	return out
}

// firstSupportedClass returns the first class in the operator's preference order that this guest can host
// without the injector guessing a victim.
func firstSupportedClass(classes []faultinjector.Class, g faultinjector.PoolGuest) (faultinjector.Class, bool) {
	for _, c := range classes {
		if guestHostsClass(g, c) {
			return c, true
		}
	}
	return "", false
}

// guestHostsClass reports whether a guest can host a fault class without the injector guessing a victim — the
// same CONFIG-NOT-CODE eligibility the campaign planner (faultinjector.PlanNext) enforces inline: the three
// declaration-gated classes are ineligible unless the operator NAMED the target on this guest (a Unit for
// service-down, a Container for container-down, a valid LogPath for log-fill), because scraping a victim from
// `systemctl list-units` or `docker ps` could stop sshd, the log shipper or a database. The path rule is not
// re-implemented — it delegates to faultinjector.ValidLogPath, the single source of what a log-fill target may
// be. The self-targeting classes need no declaration, so every guest hosts them; an unknown class is refused.
func guestHostsClass(g faultinjector.PoolGuest, c faultinjector.Class) bool {
	switch c {
	case faultinjector.ClassServiceDown:
		return g.Unit != ""
	case faultinjector.ClassContainerDown:
		return g.Container != ""
	case faultinjector.ClassLogFill:
		return faultinjector.ValidLogPath(g.LogPath) == nil
	case faultinjector.ClassDeviceDown, faultinjector.ClassDiskFill, faultinjector.ClassMemPressure:
		return true
	default:
		return false
	}
}

// distinctNonBlank counts distinct non-blank host names — the honest denominator for a diagnostic message.
func distinctNonBlank(hosts []string) int {
	seen := map[string]bool{}
	for _, h := range hosts {
		if h != "" {
			seen[h] = true
		}
	}
	return len(seen)
}
