// Package holds is the durable memory of every HOLD Territory Grounder decides (TG-667, deep-review F5).
//
// Until 2026-11-07 a `fallback:autonomous-hold:*` decision wrote ONE governance-ledger row and ONE chat notice,
// and the sentence that said WHY lived only in Temporal history (24 h retention). Nothing listed open holds,
// nothing alerted on a held incident that stayed actionable, and — the dangerous half — nothing remembered
// the hold: the actor-attribution lookback is a fixed 30 min, so an operator's deliberate stop that TG
// correctly HELD at minute 5 was eligible to be HEALED when the same alert re-fired at minute 40, because the
// evidence that made it a person's change had aged out of the window while the person's intent had not.
//
// This package carries the vocabulary; core/db persists it (hold_decision, migration 0127); the runner writes a
// row beside every hold/deny ledger row and consults it at classify time; core/risk polls `prior-hold-still-open`
// when a STICKY hold is still open for the incident's (host, rule), and the autonomous fallback approver then
// HOLDS again — the resolver clears only the three "no first look yet" inputs, never this one.
//
// STICKY vs NOT. A hold is sticky when its reason describes a fact about the ESTATE or the ACTOR that a later
// re-detection cannot refresh on its own: a person's deliberate change, an unknown or contradictory actor, a
// stateful or destructive target, an operator's explicit denial. It is NOT sticky when the reason was about the
// PROPOSAL (prose naming another host, no committed prediction) or about TG's own first-look inputs (novelty,
// an ungraduated class, a canary pin) — those are re-judged on fresh input every time, and remembering them
// would turn one bad proposal into a permanent hold. The owner's test carve-outs are untouched: an
// `authorized-test` attribution never produces a hold in the first place, so nothing here can remember one.
package holds

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

// DefaultRedetectWindow is how long a sticky hold keeps a (host, rule) held after the decision when the
// deployment sets no TG_HOLD_REDETECT_WINDOW (owner-accepted default, 2026-11-07). Long enough to outlive the
// 30-minute attribution lookback by a wide margin and a night of unattended re-fires; short enough that a
// genuinely new failure on the same subject the next day is judged afresh.
const DefaultRedetectWindow = 24 * time.Hour

// ReasonPriorHold is the poll reason the classifier records when a sticky hold is still open.
const ReasonPriorHold = "prior-hold-still-open"

// ReasonHumanDeny is the reason key recorded for an operator's explicit `human:deny`.
const ReasonHumanDeny = "human-deny"

// HoldDecision is one persisted hold: who held what, why, and until when the hold is remembered.
type HoldDecision struct {
	Seq              int64
	ExternalRef      string
	Host             string // the incident SUBJECT (env.Host) — the same key the classifier's novelty read uses
	AlertRule        string
	OpClass          string
	ActionID         string
	ReasonKey        string // the poll reason that decided (the resolver's Key), or ReasonHumanDeny
	Why              string // the resolver's one-sentence rationale, persisted so it outlives Temporal history
	ActorAttribution string // the attribution taxonomy recorded on the classification, if any
	DecidedBy        string // tg:fallback-approver, or the operator login that denied
	DecidedAt        time.Time
	ExpiresAt        time.Time
}

// Open reports whether the hold is still remembered at now.
func (h HoldDecision) Open(now time.Time) bool {
	return !h.ExpiresAt.IsZero() && now.Before(h.ExpiresAt)
}

// FailedInvestigation is a session that ended `failed:investigate` and has not since been followed by a
// non-failed session for the same (host, rule) — an incident TG tried to look at and never got back to.
type FailedInvestigation struct {
	ExternalRef string
	Host        string
	AlertRule   string
	StopReason  string
	FailedAt    time.Time
}

// stickyReasons are the poll reasons that survive re-detection (see the package comment). The value is the
// plain-language reading the console shows.
var stickyReasons = map[string]string{
	"actor-attributed-authorized":          "a person made this change deliberately",
	"actor-attribution-escalate":           "who made this change is unknown or contradictory",
	"actor-attributed-suspicious":          "an unsanctioned actor made this change",
	"stateful-workload-mutation":           "the target is a stateful workload",
	"server-derived-destructive-op":        "the action is destructive or irreversible",
	"irreversible-or-never-auto-floor":     "the action is on the never-auto floor",
	"irreversible-op-class":                "the op class cannot be undone (the registry declares it irreversible)",
	"self-protected-control-plane-restart": "the action restarts TG's own control plane",
	ReasonHumanDeny:                        "an operator denied it",
}

// ReasonPriorHold is deliberately NOT sticky and is NEVER recorded as a hold of its own (RecordVoteActivity
// skips it): a re-hold is the ORIGINAL decision being honoured, and recording it again with a fresh expiry
// would renew the hold on every re-fire — a flapping alert would then stay held forever (fresh-eyes review of
// this change, 2026-11-07). The original row's expires_at is the whole lifetime of the memory.

// Sticky reports whether a hold with this reason keeps the (host, rule) held on re-detection.
func Sticky(reasonKey string) bool {
	_, ok := stickyReasons[strings.TrimSpace(reasonKey)]
	return ok
}

// StickyReasons returns the sticky reason keys, sorted — the store filters in SQL on exactly this set, so the
// judgement lives here once and the database never has to know what "sticky" means.
func StickyReasons() []string {
	out := make([]string, 0, len(stickyReasons))
	for k := range stickyReasons {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// Reading renders a sticky reason in plain language; a non-sticky or unknown reason renders as itself.
func Reading(reasonKey string) string {
	if strings.TrimSpace(reasonKey) == ReasonPriorHold {
		return "a prior hold on this incident is still open"
	}
	if r, ok := stickyReasons[strings.TrimSpace(reasonKey)]; ok {
		return r
	}
	return reasonKey
}

const autonomousHoldPrefix = "fallback:autonomous-hold:"

// ReasonFromDecision maps a governance-ledger decision string onto a hold reason key. It answers (key, true)
// for the two decisions that ARE holds — `fallback:autonomous-hold:<reason>` and `human:deny` — and ("",
// false) for everything else (approvals, timeouts, vote bookkeeping), so a caller never records a hold for a
// decision that released or merely abandoned an action.
func ReasonFromDecision(decision string) (string, bool) {
	switch {
	case strings.HasPrefix(decision, autonomousHoldPrefix):
		key := strings.TrimSpace(strings.TrimPrefix(decision, autonomousHoldPrefix))
		if key == "" {
			key = "unrecorded"
		}
		return key, true
	case decision == "human:deny":
		return ReasonHumanDeny, true
	}
	return "", false
}

// Detail renders the signal the classifier records beside a prior-hold poll, so the audit row says WHICH
// prior decision is being honoured and who made it — a poll that names its evidence can be adjudicated.
func Detail(h HoldDecision) string {
	return fmt.Sprintf("%s@%s by %s at %s until %s", h.ReasonKey, h.ExternalRef, h.DecidedBy,
		h.DecidedAt.UTC().Format(time.RFC3339), h.ExpiresAt.UTC().Format(time.RFC3339))
}
