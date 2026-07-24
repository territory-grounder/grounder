package runner

import (
	"context"
	"regexp"
	"sync"
	"time"

	"github.com/territory-grounder/grounder/core/audit"
	"github.com/territory-grounder/grounder/core/suppression"
)

// rebootClassRE matches an alert rule that names a host reboot/restart/down condition — the class the
// scheduled-reboot stage applies to. Deliberately narrow so a NON-reboot alert on a host under a reboot
// schedule (a real incident during the window) is never swept up by that schedule.
var rebootClassRE = regexp.MustCompile(`(?i)reboot|restart|host.?down|node.?down|unreachable|power.?cycle`)

// isRebootClass reports whether an alert rule names a reboot-class condition (see rebootClassRE).
func isRebootClass(alertRule string) bool { return rebootClassRE.MatchString(alertRule) }

// suppressor decides whether an incident is suppressed before a session is spent (spec/005). A
// *suppression.Chain satisfies it directly (static stages only); the LiveSuppressGate below wraps a chain
// with a LIVE recent-triage log so the dedup stage sees the incidents this worker recently triaged. Deps
// carries the interface so the oracle can inject a plain chain and production the live gate.
type suppressor interface {
	Decide(ctx context.Context, a suppression.Alert, now time.Time) (suppression.Decision, error)
}

// RecentTriageLog is the worker's in-memory, concurrency-safe, time-windowed memory of recently triaged
// (host, alert_rule) incidents — the anchor set the dedup stage scans so a re-fire of an OPEN incident within
// the window does not spawn a second session. It is best-effort by design: entries live at most `retention`
// and are evicted lazily on read, and the log is per-worker (a restart or a second worker simply forgets some
// recent triages). Dedup is fail-open — forgetting an anchor costs at most one extra session, never a missed
// real incident — so an in-memory single-worker log is a sound default; a durable shared log can replace it
// behind the same seam without touching the gate.
type RecentTriageLog struct {
	mu        sync.Mutex
	entries   []suppression.TriageEntry
	retention time.Duration
}

// NewRecentTriageLog returns a log that retains entries for the given window.
func NewRecentTriageLog(retention time.Duration) *RecentTriageLog {
	return &RecentTriageLog{retention: retention}
}

// Record appends a triage entry (best-effort; a retried activity may double-record, which dedup tolerates).
func (l *RecentTriageLog) Record(e suppression.TriageEntry) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.entries = append(l.entries, e)
}

// Recent returns the entries within `window` of now and, in the same pass, evicts everything older than the
// log's retention — so the slice cannot grow without bound. A copy is returned; callers never alias the log.
func (l *RecentTriageLog) Recent(now time.Time, window time.Duration) []suppression.TriageEntry {
	l.mu.Lock()
	defer l.mu.Unlock()
	kept := l.entries[:0]
	var out []suppression.TriageEntry
	for _, e := range l.entries {
		age := now.Sub(e.LoggedAt)
		if age < 0 || age > l.retention {
			continue // future-dated or past retention — drop
		}
		kept = append(kept, e)
		if age <= window {
			out = append(out, e)
		}
	}
	// zero the tail so evicted entries can be GC'd, then truncate.
	for i := len(kept); i < len(l.entries); i++ {
		l.entries[i] = suppression.TriageEntry{}
	}
	l.entries = kept
	return out
}

// LiveSuppressGate is the production suppressor: it assembles a suppression.Chain PER INCIDENT from the
// static operator-curated config (freeze windows, active-memory rules) PLUS a live dedup stage backed by the
// recent-triage log, runs it, and records the triage back into the log. Assembling per incident is what makes
// the dedup stage see live state (the DedupStage's anchor set is a fixed slice, so it must be re-supplied each
// time). Stage order matches spec/005: freeze (in Chain) → severity floor → dedup → operator rule.
type LiveSuppressGate struct {
	Freeze          *suppression.FreezeGate
	Folds           []suppression.SuppressionPolicy
	FoldFreshness   time.Duration
	Schedules       []suppression.Schedule
	RebootPreBuffer time.Duration // how long BEFORE a scheduled fire a reboot alert still matches (default 5m)
	RebootWindow    time.Duration // how long AFTER  a scheduled fire a reboot alert still matches (default 10m)
	Patterns        []suppression.TransientPattern
	Rules           []suppression.SuppressRule
	Window          time.Duration
	OpenIssue       func(issueRef string) bool
	Ledger          *audit.Ledger
	Log             *RecentTriageLog

	countMu sync.Mutex
	counts  map[string]int // decision outcome (escalate/suppressed/notice) → running count, for telemetry
}

// Counts returns a snapshot of the gate's decision counts by outcome (for observability). Concurrency-safe.
func (g *LiveSuppressGate) Counts() map[string]int {
	g.countMu.Lock()
	defer g.countMu.Unlock()
	out := make(map[string]int, len(g.counts))
	for k, v := range g.counts {
		out[k] = v
	}
	return out
}

func (g *LiveSuppressGate) record(outcome string) {
	g.countMu.Lock()
	defer g.countMu.Unlock()
	if g.counts == nil {
		g.counts = map[string]int{}
	}
	g.counts[outcome]++
}

// Decide runs the assembled chain and records the incident into the live log.
func (g *LiveSuppressGate) Decide(ctx context.Context, a suppression.Alert, now time.Time) (suppression.Decision, error) {
	// spec/005 stage order: dedup → known-pattern → active-memory (blast-radius and scheduled join with their
	// stateful backing). First non-escalate wins.
	stages := []suppression.Stage{
		&suppression.DedupStage{Recent: g.Log.Recent(now, g.Window), Window: g.Window, OpenIssue: g.OpenIssue},
	}
	if len(g.Folds) > 0 {
		stages = append(stages, &suppression.BlastRadiusStage{Policies: g.Folds, Freshness: g.FoldFreshness})
	}
	if len(g.Schedules) > 0 {
		stages = append(stages, &suppression.ScheduledStage{Schedules: g.Schedules, Window: suppression.WindowEvaluator{PreBuffer: g.RebootPreBuffer, PostWindow: g.RebootWindow}})
	}
	if len(g.Patterns) > 0 {
		stages = append(stages, &suppression.KnownPatternStage{Patterns: g.Patterns})
	}
	if len(g.Rules) > 0 {
		stages = append(stages, &suppression.ActiveMemoryStage{Rules: g.Rules})
	}
	chain := &suppression.Chain{Freeze: g.Freeze, Stages: stages, Ledger: g.Ledger}
	d, err := chain.Decide(ctx, a, now)
	// Record this triage as a future dedup anchor. Suppressed is carried so a silenced alert is not itself a
	// valid anchor (you dedup a re-fire against a still-open INCIDENT, not against another suppressed alert),
	// and IssueRef carries the incident key so a re-fire is deduped only WHILE that incident is still open
	// (OpenIssue) — a re-fire after it resolved is a genuine new incident.
	g.Log.Record(suppression.TriageEntry{Host: a.Host, AlertRule: a.AlertRule, LoggedAt: now, Suppressed: d.Outcome.Suppressing(), IssueRef: a.ExternalRef})
	g.record(d.Outcome.String())
	return d, err
}
