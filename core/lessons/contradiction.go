package lessons

import (
	"strings"
	"time"

	"github.com/territory-grounder/grounder/core/knowledge"
)

// Contradiction is a recent CAUTION that disagrees with a precedent about to be learned (TG-673, deep-review
// F14): the same (host, rule) was treated with the same action and did NOT resolve cleanly inside the lesson
// horizon. Learning the success anyway would record "this works here" over fresh evidence that it did not —
// one lucky run overwriting the memory of a failure — and the precedent corpus is what the agent leans on and
// what de-novels the incident for the fallback approver.
type Contradiction struct {
	// CautionRef is the caution's external_ref.
	CautionRef string
	// CautionAt is when that caution's incident resolved (zero = undatable).
	CautionAt time.Time
	// Action is the shared attempted action.
	Action string
}

// Contradicted reports whether a resolved incident about to become a precedent is contradicted by a caution in
// `cautions` — the caution lane's rows for the same (host, rule), as the caller selected them. A contradiction
// needs: the SAME host and rule (case-insensitive, trimmed); the SAME action — read from the caution's
// `action:` tag (Caution writes it) or, for a caution written before the tag existed, from the quoted action in
// its reflection prose (`ran "<action>"`); and a caution that is RECENT — its incident resolved within maxAge
// of now. With a horizon set, an undatable caution (zero ResolvedAt) does NOT contradict: nothing prunes the
// caution lane, so an undatable row would otherwise suppress learning for that (host, rule, action) forever.
// With the horizon off (maxAge <= 0) every same-action caution counts. A caution with a different action is
// not a contradiction: "restart failed, start worked" is two facts, not a conflict. Pure over its inputs.
func Contradicted(ri ResolvedIncident, cautions []knowledge.Incident, now time.Time, maxAge time.Duration) (Contradiction, bool) {
	host, rule, action := fold(ri.Host), fold(ri.AlertRule), fold(ri.Action)
	if host == "" || rule == "" || action == "" {
		return Contradiction{}, false
	}
	for _, c := range cautions {
		if c.Source != knowledge.ProvenanceCaution {
			continue
		}
		if fold(c.Host) != host || fold(c.AlertRule) != rule || !cautionNamesAction(c, action) {
			continue
		}
		if maxAge > 0 && (c.ResolvedAt.IsZero() || now.Sub(c.ResolvedAt) > maxAge) {
			continue
		}
		return Contradiction{CautionRef: c.ExternalRef, CautionAt: c.ResolvedAt, Action: action}, true
	}
	return Contradiction{}, false
}

// cautionNamesAction reports whether the caution attempted `action` (already folded): by its action: tag, or by
// the quoted action in a pre-tag reflection.
func cautionNamesAction(c knowledge.Incident, action string) bool {
	for _, t := range c.Tags {
		if strings.HasPrefix(t, ActionTagPrefix) && fold(strings.TrimPrefix(t, ActionTagPrefix)) == action {
			return true
		}
	}
	return strings.Contains(fold(c.Resolution), `ran "`+action+`"`)
}

func fold(s string) string { return strings.ToLower(strings.TrimSpace(s)) }
