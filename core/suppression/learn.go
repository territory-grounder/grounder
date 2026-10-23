package suppression

import (
	"context"
	"fmt"
	"sync"
	"time"
)

// The LEARNED scheduled-reboot lane (spec/005 REQ-409..411, port-fidelity #19/#11/#12/#10).
//
// TG has always honored schedules an operator DECLARED. The predecessor also LEARNED them: a reboot that
// recurs on a deterministic cadence is registered OBSERVING, confirmed across repeated occurrences, and only
// then promoted to LIVE suppression — so an undeclared-but-regular 03:00 reboot stops minting a novel
// incident every night. This file is that lane, ported as LOGIC:
//
//	observe  — a reboot-class alert that the chain did NOT suppress is fed here (the predecessor's REACTIVE
//	           arm, classify-reboot-alert.py, runs at exactly the same point: "invoked at triage when a
//	           reboot-class alert was NOT suppressed by the matcher").
//	verify   — the boot must classify CLEAN (TwoPhaseVerifier.Confirm). A REACTIVE boot — OOM, panic,
//	           watchdog, hung task, emergency/self-heal, thermal — is a SYMPTOM, never a schedule, and is
//	           never recorded; an UNKNOWN reason is treated as not-clean. This is the registration-time gate
//	           the predecessor has and TG lacked (#12): without it an OOM reboot landing near a cron minute
//	           could be learned as "scheduled" and would then suppress the next real incident.
//	promote  — two VERIFIED in-window occurrences promote the row to LIVE through the shared Promote
//	           lifecycle (distinct-boot dedup, 10-cap, accumulation across runs). One occurrence never
//	           promotes; a wrong attribution never accumulates two and stays observing.
//
// Every failure direction ends at INVESTIGATING: an unarmed learner, an unclassifiable boot, a single
// sighting, an irregular gap, or a bad timezone all mean "do not learn", never "suppress".
const (
	// LearnedKind is the registry `kind` learned rows carry, keeping them a distinct family from any
	// operator-declared or discovery-sourced row.
	LearnedKind = "learned-reboot"

	// DefaultLearnValidity is how long a learned row stays valid without renewal (the predecessor's
	// DEFAULT_VALID_UNTIL_DAYS = 90). renew-on-match extends an actively-firing schedule.
	DefaultLearnValidity = 90 * 24 * time.Hour

	// DefaultHistoryAge bounds how far back an occurrence still counts as evidence of a live cadence, and
	// historyCap bounds the retained per-host set. Stale evidence must not promote a schedule that stopped.
	DefaultHistoryAge = 30 * 24 * time.Hour
	historyCap        = 20

	// learnDayPeriod / learnWeekPeriod are the two cadences a reboot schedule is learned at. A gap that is
	// neither (a 2-day gap, an irregular one) is NOT a cadence: rendering it as a daily cron would claim
	// fires on days no reboot was ever observed, and those claims would suppress real incidents.
	learnDayPeriod  = 24 * time.Hour
	learnWeekPeriod = 7 * 24 * time.Hour
)

// RebootObservation is one reboot-class alert offered to the learner: the host it fired on, when it was
// observed, and the RECORDED BOOT REASON that decides whether it may be learned at all.
type RebootObservation struct {
	Host        string
	ExternalRef string
	AlertRule   string
	BootReason  string // the recorded boot reason (journal marker / provider label) — the #12 gate reads this
	Timezone    string // the host's timezone; empty ⇒ the learner's default
	At          time.Time
}

// LearnOutcome is what one Observe call did, for the operator log and the oracles. It is descriptive only —
// nothing here re-enters the suppression decision for the CURRENT alert (that decision was already made).
type LearnOutcome struct {
	Confirmed  bool        // the boot classified CLEAN and was recorded as evidence
	Registered bool        // a schedule row exists for this signature after this call
	Promoted   bool        // this call moved the row to LIVE
	Status     SchedStatus // the row's status after this call
	Key        ScheduleKey // the row's identity
	Reason     string      // why the call did what it did
}

// Learner is the production observe→verify→promote writer for the learned lane. It is concurrency-safe:
// the ingest path calls Observe from many workflow activities at once.
type Learner struct {
	Registry *ScheduleRegistry
	Window   WindowEvaluator
	// Verifier classifies each occurrence's boot. Nil ⇒ a classification-only verifier (same judgement, no
	// reopen/page — learning never pages, because the occurrence it looks at was investigated normally).
	Verifier *TwoPhaseVerifier
	// Timezone is the default host timezone for a learned cron. Empty ⇒ UTC.
	Timezone string
	// ValidFor / MaxHistoryAge default to DefaultLearnValidity / DefaultHistoryAge when zero.
	ValidFor      time.Duration
	MaxHistoryAge time.Duration

	mu   sync.Mutex
	seen map[string][]time.Time // per host: the CONFIRMED-clean reboot times, deduped, bounded
}

// Live returns the learned schedules currently eligible to suppress (a snapshot of copies).
func (l *Learner) Live() []Schedule {
	if l == nil || l.Registry == nil {
		return nil
	}
	return l.Registry.Live()
}

func (l *Learner) validity() time.Duration {
	if l.ValidFor > 0 {
		return l.ValidFor
	}
	return DefaultLearnValidity
}

func (l *Learner) historyAge() time.Duration {
	if l.MaxHistoryAge > 0 {
		return l.MaxHistoryAge
	}
	return DefaultHistoryAge
}

// tolerance is how far an occurrence may drift from an exact cadence and still count as the same schedule.
// It is the matcher's own window width, so what the learner accepts as "the same nightly reboot" is exactly
// what the matcher will later accept as in-window — one number, no second notion of closeness.
func (l *Learner) tolerance() time.Duration {
	tol := l.Window.PreBuffer + l.Window.PostWindow
	if tol <= 0 {
		return time.Minute
	}
	return tol
}

// Observe feeds one reboot-class alert into the learned lane and returns what it did.
//
// Order matters and is the safety argument: the boot-reason gate runs FIRST, so a reactive reboot is never
// even recorded as evidence (it cannot contribute to a later promotion either); the signature is resolved
// SECOND, preferring an existing row's window so ordinary jitter accrues to one identity instead of minting
// a new one each time; registration is ALWAYS observing-or-preserved (never live); and promotion runs LAST
// through the shared lifecycle that dedups boots and needs two of them.
func (l *Learner) Observe(_ context.Context, obs RebootObservation, now time.Time) LearnOutcome {
	if l == nil || l.Registry == nil {
		return LearnOutcome{Reason: "learned lane not armed"}
	}
	if obs.Host == "" || obs.At.IsZero() {
		return LearnOutcome{Reason: "observation lacks a host or an observation time"}
	}
	// GATE (#12): only a CLEAN boot may ever become a schedule. Reactive and unknown reasons stop here and
	// are not recorded — a crash is a symptom, and a symptom that becomes a "schedule" suppresses the next
	// real incident.
	v := l.Verifier
	if v == nil {
		v = &TwoPhaseVerifier{} // Confirm is side-effect free, so a nil verifier still classifies
	}
	if res := v.Confirm(obs.BootReason); !res.Confirmed {
		return LearnOutcome{Reason: fmt.Sprintf("boot reason %q is not a confirmed clean reboot — never registered as a schedule", obs.BootReason)}
	}

	loc, err := time.LoadLocation(l.tz(obs.Timezone))
	if err != nil || loc == nil {
		return LearnOutcome{Confirmed: true, Reason: "unresolvable host timezone — a schedule computed in a guessed zone would fire at the wrong time"}
	}
	history := l.record(obs.Host, obs.At, now)

	// Signature: reuse the identity of an existing learned row whose window already contains this
	// occurrence; otherwise derive one from the cadence between this occurrence and the most recent prior.
	key, ok := l.Registry.MatchWindow(obs.Host, l.Window, obs.At)
	if !ok {
		cron, derived := deriveCron(history, obs.At, loc, l.tolerance())
		if !derived {
			return LearnOutcome{Confirmed: true, Reason: "no daily or weekly cadence confirmed yet — a single or irregular reboot is not a schedule"}
		}
		key = ScheduleKey{Host: obs.Host, Kind: LearnedKind, Cron: cron}
	}

	// Registration is OBSERVING for a new signature and state-preserving for a known one. A shifted schedule
	// is a different signature, so it re-enters observing rather than inheriting the old row's promotion.
	row := l.Registry.RegisterObserving(Schedule{
		Host: key.Host, Kind: key.Kind, Cron: key.Cron, Timezone: l.tz(obs.Timezone),
		Source: SourceLearned, ValidFrom: now, ValidUntil: now.Add(l.validity()), LastVerifiedAt: now,
	})
	wasLive := row.Status == SchLive

	boots := make([]Boot, 0, len(history))
	for _, t := range history {
		boots = append(boots, Boot{At: t})
	}
	// cronStillPresent is TRUE here by construction: the signature was derived from boots that actually
	// happened, so the "schedule was removed from the host" drift signal does not apply to this writer —
	// expiry (valid_until) and the demotion escape are the learned lane's retirement paths.
	status := l.Registry.Promote(key, l.Window, boots, true, now)
	inWindow := 0
	if row, ok := l.Registry.Get(key); ok {
		inWindow = row.ObservedCount // the row's OWN accumulated evidence, not the host's whole history
	}
	return LearnOutcome{
		Confirmed: true, Registered: true, Promoted: status == SchLive && !wasLive,
		Status: status, Key: key,
		Reason: fmt.Sprintf("learned reboot schedule %q on %s: %d confirmed in-window occurrence(s) — %s", key.Cron, key.Host, inWindow, status),
	}
}

// Demote stops a learned row suppressing and clears its evidence (the unlearning half). It exists on the
// Learner so callers hold one handle to the lane.
func (l *Learner) Demote(k ScheduleKey) bool {
	if l == nil || l.Registry == nil {
		return false
	}
	return l.Registry.Demote(k)
}

// DemoteHost demotes every live learned row on a host.
func (l *Learner) DemoteHost(host string) int {
	if l == nil || l.Registry == nil {
		return 0
	}
	return l.Registry.DemoteLearned(host)
}

func (l *Learner) tz(zone string) string {
	if zone != "" {
		return zone
	}
	if l.Timezone != "" {
		return l.Timezone
	}
	return "UTC"
}

// record appends a CONFIRMED occurrence to the host's history and returns the retained set (ascending).
// Duplicates by exact timestamp are dropped — the same alert re-delivered must not look like two
// occurrences — entries older than the history bound are evicted, and the set is capped.
func (l *Learner) record(host string, at, now time.Time) []time.Time {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.seen == nil {
		l.seen = map[string][]time.Time{}
	}
	cutoff := now.Add(-l.historyAge())
	var kept []time.Time
	dup := false
	for _, t := range l.seen[host] {
		if t.Before(cutoff) {
			continue
		}
		if t.Equal(at) {
			dup = true
		}
		kept = append(kept, t)
	}
	if !dup && !at.Before(cutoff) {
		kept = append(kept, at)
	}
	sortTimes(kept)
	if len(kept) > historyCap {
		kept = kept[len(kept)-historyCap:]
	}
	l.seen[host] = kept
	out := make([]time.Time, len(kept))
	copy(out, kept)
	return out
}

// deriveCron turns a confirmed occurrence plus the host's history into a cron signature, or reports that no
// cadence is established. It looks at the gap to the MOST RECENT prior occurrence and accepts only an
// (approximately) exact day or week — a 2-day or ragged gap is deliberately NOT generalized into a daily
// cron, because that cron would claim fires on days nothing was ever observed and would then suppress a real
// incident on one of them. Weekly is tested first: a 7-day gap is also a whole number of days, and the
// weekly form is the more restrictive reading of the same evidence.
func deriveCron(history []time.Time, at time.Time, loc *time.Location, tol time.Duration) (string, bool) {
	var prev time.Time
	for i := len(history) - 1; i >= 0; i-- {
		if history[i].Before(at) {
			prev = history[i]
			break
		}
	}
	if prev.IsZero() {
		return "", false
	}
	gap := at.Sub(prev)
	lt := at.In(loc)
	switch {
	case nearPeriod(gap, learnWeekPeriod, tol):
		return fmt.Sprintf("%d %d * * %d", lt.Minute(), lt.Hour(), int(lt.Weekday())), true
	case nearPeriod(gap, learnDayPeriod, tol):
		return fmt.Sprintf("%d %d * * *", lt.Minute(), lt.Hour()), true
	default:
		return "", false
	}
}

// nearPeriod reports whether a gap is within tol of exactly one period.
func nearPeriod(gap, period, tol time.Duration) bool {
	d := gap - period
	if d < 0 {
		d = -d
	}
	return d <= tol
}

// sortTimes sorts ascending (insertion sort — the slice is capped at historyCap and near-sorted already).
func sortTimes(ts []time.Time) {
	for i := 1; i < len(ts); i++ {
		for j := i; j > 0 && ts[j].Before(ts[j-1]); j-- {
			ts[j], ts[j-1] = ts[j-1], ts[j]
		}
	}
}
