// Package loadharness is the real-run e2e concurrency/throughput harness (TG-80 P1#2, adopted from the
// h-apache-stack audit's "benchmark rigor" finding: 988 real e2e runs, 0 failures, concurrency sweeps with
// p50/p95/max, a hard 0-failure gate — TG had 2,659 test files and zero latency/throughput lines).
//
// It drives N CONCURRENT synthetic incidents through the REAL ingest→triage pipeline over plain HTTP:
//
//	POST /v1/ingest/{source_type}  →  StartTriage (idempotent by external_ref)  →  Runner  →  audit spine
//	GET  /v1/sessions              ←  polled until every session is VISIBLE on the spine
//
// and reports, per concurrency level, throughput + p50/p95/max end-to-end latency (POST-initiated →
// session first observed on the spine; resolution is bounded by the poll interval), exiting non-zero on
// ANY failed run, with the failing external_refs named.
//
// Correctness invariants verified per run — the closed set observable at the authenticated HTTP surface:
//   - MINTED: every accepted incident's session becomes visible within the per-session timeout (no loss);
//   - EXACTLY ONE: response refs are pairwise distinct; a DUPLICATE re-POST of an in-flight ref mid-run is
//     a 202 carrying the SAME external_ref and the SAME workflow_id (the front door's reject-duplicate
//     contract), and the final sessions page holds each ref at most once;
//   - NO CROSS-CONTAMINATION: each session's host equals the fixture host its incident was minted with;
//   - optionally (ExpectQuietSpine, for a hermetic target): the spine population grew by EXACTLY the
//     number of distinct accepted refs — nothing lost, nothing invented.
//
// The synthetic incidents are Alertmanager-shaped (modules/ingest/prometheus-alertmanager — the natural
// push-source grammar) and live in a fixture namespace that can NEVER collide with real estate hosts:
// every host is under the RFC-2606 reserved TLD ".invalid" (loadharness.invalid), the same discipline as
// the repo's RFC-5737 test addresses. Refs embed a per-run id because the pipeline's REJECT_DUPLICATE is
// forever (a completed workflow's id is never reusable), so a re-run with yesterday's refs would dedup
// into finished sessions and pass vacuously.
//
// The tool targets ANY base URL — the docker-compose stack or the production box is the REAL e2e run; CI
// exercises the harness itself against the in-process rig (rig.go: real auth router + real httpapi
// registration + the real Alertmanager normalizer + a triage fake reproducing the reject-duplicate
// contract), because the CI verify stage has no Temporal/worker to stand up. Fault-injection knobs on the
// rig red-prove every invariant (harness_test.go): a harness that cannot fail proves nothing.
package loadharness

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"sync"
	"time"

	"github.com/territory-grounder/grounder/core/httpapi"
)

const (
	// alertName is the synthetic alert rule every harness incident carries. It names the harness so an
	// operator reading the spine (or the console) sees exactly what these sessions are.
	alertName = "TGLoadHarnessSynthetic"
	// fixtureDomain is the RFC-2606 reserved TLD namespace for synthetic hosts. ".invalid" is guaranteed
	// by the RFC to never resolve and never name a real machine, so a harness incident can never be
	// attributed to — or collide with — an estate host, whatever estate it is pointed at.
	fixtureDomain = "loadharness.invalid"
	// serverPageLimit mirrors core/httpapi's sessionsPageLimit: the largest page one read returns. The
	// harness caps runs-per-level below it so a level's whole cohort is observable in a single page.
	serverPageLimit = 200
)

// Config is one harness invocation. Secrets (HMACSecret, IngestToken) arrive via the environment in the
// CLI (never argv — argv is world-readable in /proc); here they are plain fields so the rig tests can
// inject theirs.
type Config struct {
	// BaseURL is the target deployment's API origin (e.g. the compose stack). Any authenticated TG
	// surface works — the harness is a pure HTTP client with no privileged path.
	BaseURL string
	// SourceType is the ingest slug to POST to. Default "prometheus-alertmanager" — the batch push
	// source whose webhook grammar the synthetic incidents speak.
	SourceType string
	// IngestToken is the per-source static bearer for POST /v1/ingest/{source_type} (AuthIngestPush).
	// Empty ⇒ the POST is HMAC-signed with HMACSource/HMACSecret instead.
	IngestToken string
	// HMACSource/HMACSecret authenticate the read-back polling (GET /v1/sessions is machine-HMAC on the
	// read-only tier) and, when IngestToken is empty, the ingest POSTs too.
	HMACSource string
	HMACSecret []byte
	// RunID discriminates this invocation's fixture namespace. Empty ⇒ a fresh random id, which is what
	// makes re-runs against the same deployment honest (REJECT_DUPLICATE outlives workflow completion).
	RunID string
	// Runs is the incidents driven per concurrency level (default 8; 1..serverPageLimit).
	Runs int
	// Levels is the concurrency sweep: at level L, Runs incidents are driven with at most L in flight
	// (h's sweep shape: 1/4/8/16/32). Default: one level equal to min(Runs, 8).
	Levels []int
	// PollInterval is the spine polling cadence (default 250ms). It bounds latency resolution.
	PollInterval time.Duration
	// SessionTimeout bounds how long one incident may take to become visible (default 2m — a live Runner
	// writes its triage row early, but a busy box is given room). A session not seen in time is a FAILED
	// run naming its ref, never a hang.
	SessionTimeout time.Duration
	// RunTimeout caps the whole invocation (default 15m) — the harness can time out, it can never hang.
	RunTimeout time.Duration
	// ExpectQuietSpine additionally asserts the spine population delta equals the distinct accepted refs.
	// Only meaningful against a hermetic target (the rig, an idle stack): organic traffic on a live box
	// grows the population concurrently and would false-flag, so it is opt-in, never default.
	ExpectQuietSpine bool
	// TerminalStatuses is the set of session statuses that count as a COMPLETED run (TG-80 P1-2).
	// Default: proposed, executed, stopped — i.e. the workflow reached its terminal record. "classified"
	// is deliberately NOT terminal: it is the first-visibility boundary the recent-sessions page shows, and
	// scoring a run complete there scored a session that later wedged in propose/execute as a success.
	TerminalStatuses []string
}

func (c *Config) withDefaults() error {
	if c.BaseURL == "" {
		return fmt.Errorf("loadharness: BaseURL required")
	}
	if c.SourceType == "" {
		c.SourceType = "prometheus-alertmanager"
	}
	if len(c.TerminalStatuses) == 0 {
		c.TerminalStatuses = []string{"proposed", "executed", "stopped"}
	}
	if c.HMACSource == "" || len(c.HMACSecret) == 0 {
		return fmt.Errorf("loadharness: HMAC read credentials required (GET /v1/sessions is authenticated)")
	}
	if c.Runs == 0 {
		c.Runs = 8
	}
	if c.Runs < 1 || c.Runs > serverPageLimit {
		return fmt.Errorf("loadharness: Runs %d outside 1..%d (one sessions page must hold a level's cohort)", c.Runs, serverPageLimit)
	}
	if len(c.Levels) == 0 {
		c.Levels = []int{min(c.Runs, 8)}
	}
	seenLevel := map[int]bool{}
	for _, l := range c.Levels {
		if l < 1 {
			return fmt.Errorf("loadharness: concurrency level %d < 1", l)
		}
		// A repeated level would re-derive the SAME fixture hosts, so its whole cohort would dedup into
		// the first cohort's sessions and measure nothing — refuse rather than report a vacuous pass.
		if seenLevel[l] {
			return fmt.Errorf("loadharness: concurrency level %d repeated in the sweep", l)
		}
		seenLevel[l] = true
	}
	if c.PollInterval <= 0 {
		c.PollInterval = 250 * time.Millisecond
	}
	if c.SessionTimeout <= 0 {
		c.SessionTimeout = 2 * time.Minute
	}
	if c.RunTimeout <= 0 {
		c.RunTimeout = 15 * time.Minute
	}
	if c.RunID == "" {
		var b [4]byte
		if _, err := rand.Read(b[:]); err != nil {
			return fmt.Errorf("loadharness: run id: %w", err)
		}
		c.RunID = hex.EncodeToString(b[:])
	}
	return nil
}

// Failure names one failed run: WHICH ref, on which fixture host, and why. The exit contract requires the
// failing refs named — an anonymous count cannot be investigated.
type Failure struct {
	Ref    string `json:"ref"`
	Host   string `json:"host"`
	Reason string `json:"reason"`
}

// DuplicateProbe records the mid-run idempotency probe: one in-flight ref re-POSTed while the level is
// still running, asserted to join the existing session (same ref, same workflow id, still one row).
type DuplicateProbe struct {
	Ran    bool   `json:"ran"`
	Ref    string `json:"ref,omitempty"`
	Passed bool   `json:"passed"`
	Reason string `json:"reason,omitempty"`
}

// LevelReport is one concurrency level's measurement + verdict.
type LevelReport struct {
	Concurrency      int            `json:"concurrency"`
	Runs             int            `json:"runs"`
	Completed        int            `json:"completed"`
	Failed           int            `json:"failed"`
	P50Ms            int64          `json:"p50_ms"`
	P95Ms            int64          `json:"p95_ms"`
	MaxMs            int64          `json:"max_ms"`
	ThroughputPerSec float64        `json:"throughput_per_s"`
	DuplicateProbe   DuplicateProbe `json:"duplicate_probe"`
	Failures         []Failure      `json:"failures,omitempty"`
}

// Report is the whole invocation's result. ExitCode is derived from it and nothing else — the runner's
// exit code is the verdict (a printed "all passed" proves nothing if the process then wedges).
type Report struct {
	RunID             string        `json:"run_id"`
	BaseURL           string        `json:"base_url"`
	SourceType        string        `json:"source_type"`
	Levels            []LevelReport `json:"levels"`
	PopulationBefore  int           `json:"population_before"`
	PopulationAfter   int           `json:"population_after"`
	QuietSpineChecked bool          `json:"quiet_spine_checked"`
	TotalRuns         int           `json:"total_runs"`
	TotalCompleted    int           `json:"total_completed"`
	TotalFailed       int           `json:"total_failed"`
	// Failures aggregates every level's failures plus run-scoped ones (population drift, read errors).
	Failures []Failure `json:"failures,omitempty"`
}

// ExitCode is the process verdict: 0 only when every run completed and every invariant held.
func (r Report) ExitCode() int {
	if r.TotalFailed > 0 || len(r.Failures) > 0 {
		return 1
	}
	if r.TotalRuns == 0 || r.TotalCompleted != r.TotalRuns {
		return 1 // nothing ran, or completions disagree with the ledger — never a silent 0
	}
	return 0
}

// WriteHuman renders the operator-facing summary: one line per level with the percentiles, then every
// failure BY REF. This is the stdout contract; the JSON report is the machine one.
func (r Report) WriteHuman(w io.Writer) {
	fmt.Fprintf(w, "loadharness run %s against %s (source %s)\n", r.RunID, r.BaseURL, r.SourceType)
	for _, l := range r.Levels {
		probe := "probe=skipped"
		if l.DuplicateProbe.Ran {
			if l.DuplicateProbe.Passed {
				probe = "probe=ok"
			} else {
				probe = "probe=FAILED"
			}
		}
		fmt.Fprintf(w, "  level %2d: %d/%d completed  p50=%dms p95=%dms max=%dms  throughput=%.2f/s  %s\n",
			l.Concurrency, l.Completed, l.Runs, l.P50Ms, l.P95Ms, l.MaxMs, l.ThroughputPerSec, probe)
	}
	if r.QuietSpineChecked {
		fmt.Fprintf(w, "  spine population: %d -> %d (quiet-spine asserted)\n", r.PopulationBefore, r.PopulationAfter)
	}
	if len(r.Failures) == 0 {
		fmt.Fprintf(w, "  RESULT: %d runs, 0 failures\n", r.TotalRuns)
		return
	}
	fmt.Fprintf(w, "  RESULT: %d runs, %d FAILED\n", r.TotalRuns, r.TotalFailed)
	for _, f := range r.Failures {
		fmt.Fprintf(w, "  FAIL %s (host %s): %s\n", f.Ref, f.Host, f.Reason)
	}
}

// incident is one synthetic run's whole life: built, POSTed, tracked to spine visibility, judged.
type incident struct {
	host string
	body []byte

	ref        string
	workflowID string
	postAt     time.Time

	foundCh chan struct{} // closed by the poller when the session is first observed
	seen    httpapi.SessionSummary
	seenAt  time.Time
	// terminalAt is when the session's detail first reported a TERMINAL status; lastStatus is the last
	// status the detail reported (named in the failure when terminal is never reached). TG-80 P1-2: the
	// latency a run pays is POST→terminal, not POST→first-visibility.
	terminalAt time.Time
	lastStatus string

	failure *Failure // nil = completed clean
}

// isTerminal reports whether status is one of the configured terminal statuses (TG-80 P1-2).
func isTerminal(terminal []string, status string) bool {
	for _, t := range terminal {
		if t == status {
			return true
		}
	}
	return false
}

func (in *incident) fail(reason string) {
	in.failure = &Failure{Ref: in.ref, Host: in.host, Reason: reason}
}

// tracker is the poller↔driver rendezvous: drivers register accepted refs, the poller marks them seen.
type tracker struct {
	mu    sync.Mutex
	byRef map[string]*incident
}

func newTracker() *tracker { return &tracker{byRef: map[string]*incident{}} }

// register claims a ref for an incident; a second claim is a ref COLLISION — two incidents that were
// supposed to be distinct normalized onto one correlation key, which would silently halve the load.
func (t *tracker) register(ref string, in *incident) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if prev, ok := t.byRef[ref]; ok {
		return fmt.Errorf("ref collision: %q already minted by host %s", ref, prev.host)
	}
	t.byRef[ref] = in
	return nil
}

// observe marks a polled row against its incident, first observation wins. Fields are written under the
// lock BEFORE the channel close, so the driver's post-close read is ordered.
func (t *tracker) observe(row httpapi.SessionSummary, at time.Time) {
	t.mu.Lock()
	defer t.mu.Unlock()
	in, ok := t.byRef[row.ExternalRef]
	if !ok {
		return // not ours (organic traffic on a live box)
	}
	select {
	case <-in.foundCh: // already observed
	default:
		in.seen, in.seenAt = row, at
		close(in.foundCh)
	}
}

// Run drives the configured sweep and returns the full report. It NEVER hangs: every wait is bounded by
// SessionTimeout and the whole invocation by RunTimeout. The report is always complete enough to name
// what failed; the caller exits with report.ExitCode().
func Run(ctx context.Context, cfg Config) Report {
	rep := Report{}
	if err := cfg.withDefaults(); err != nil {
		rep.Failures = append(rep.Failures, Failure{Reason: err.Error()})
		return rep
	}
	rep.RunID, rep.BaseURL, rep.SourceType = cfg.RunID, cfg.BaseURL, cfg.SourceType
	ctx, cancel := context.WithTimeout(ctx, cfg.RunTimeout)
	defer cancel()
	client := newClient(cfg)

	distinctRefs := 0
	if cfg.ExpectQuietSpine {
		page, err := client.sessions(ctx, 1)
		if err != nil {
			rep.Failures = append(rep.Failures, Failure{Reason: fmt.Sprintf("population read (before): %v", err)})
			return rep
		}
		rep.PopulationBefore = page.Total
	}

	for _, level := range cfg.Levels {
		lr := runLevel(ctx, client, cfg, level)
		rep.Levels = append(rep.Levels, lr)
		rep.TotalRuns += lr.Runs
		rep.TotalCompleted += lr.Completed
		rep.TotalFailed += lr.Failed
		rep.Failures = append(rep.Failures, lr.Failures...)
		distinctRefs += lr.Runs - countRefless(lr.Failures)
	}

	if cfg.ExpectQuietSpine {
		page, err := client.sessions(ctx, 1)
		if err != nil {
			rep.Failures = append(rep.Failures, Failure{Reason: fmt.Sprintf("population read (after): %v", err)})
			return rep
		}
		rep.PopulationAfter = page.Total
		rep.QuietSpineChecked = true
		if got, want := rep.PopulationAfter-rep.PopulationBefore, distinctRefs; got != want {
			rep.Failures = append(rep.Failures,
				Failure{Reason: fmt.Sprintf("quiet-spine population drift: spine grew by %d sessions, %d distinct refs were accepted", got, want)})
		}
	}
	return rep
}

// countRefless counts failures that never obtained a ref (POST refused/unreadable) — those minted no
// session, so the quiet-spine expectation must not count them.
func countRefless(fs []Failure) int {
	n := 0
	for _, f := range fs {
		if f.Ref == "" {
			n++
		}
	}
	return n
}

// runLevel drives cfg.Runs incidents with at most `level` in flight, probes idempotency mid-run on the
// first accepted ref, waits every session onto the spine, then judges the invariants.
func runLevel(ctx context.Context, client *client, cfg Config, level int) LevelReport {
	lr := LevelReport{Concurrency: level, Runs: cfg.Runs}
	incidents := make([]*incident, cfg.Runs)
	now := time.Now().UTC()
	for i := range incidents {
		host := FixtureHost(cfg.RunID, level, i)
		incidents[i] = &incident{host: host, body: webhookFor(host, now), foundCh: make(chan struct{})}
	}
	trk := newTracker()

	// The poller: ONE goroutine polls the spine for the whole level and fans observations out to the
	// drivers. One reader for N waiters keeps the read load constant in N (h's harness polls per run;
	// against a production box a per-run poller at level 32 would be 128 reads/s of pure overhead).
	pollCtx, stopPoll := context.WithCancel(ctx)
	defer stopPoll()
	var pollWG sync.WaitGroup
	pollWG.Add(1)
	go func() {
		defer pollWG.Done()
		tick := time.NewTicker(cfg.PollInterval)
		defer tick.Stop()
		for {
			select {
			case <-pollCtx.Done():
				return
			case <-tick.C:
				page, err := client.sessions(pollCtx, serverPageLimit)
				if err != nil {
					continue // transient read errors are retried next tick; the per-session timeout is the backstop
				}
				at := time.Now().UTC()
				for _, row := range page.Sessions {
					trk.observe(row, at)
				}
			}
		}
	}()

	// The drivers: N incidents, at most `level` in flight. The FIRST accepted incident also runs the
	// duplicate probe — mid-run by construction, because its siblings are still being driven.
	var probe DuplicateProbe
	var probeOnce sync.Once
	sem := make(chan struct{}, level)
	var wg sync.WaitGroup
	levelStart := time.Now().UTC()
	for _, in := range incidents {
		wg.Add(1)
		go func(in *incident) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			in.postAt = time.Now().UTC()
			acc, err := client.postIncident(ctx, in.body)
			if err != nil {
				in.fail(fmt.Sprintf("ingest POST refused: %v", err))
				return
			}
			if acc.Count != 1 || len(acc.Incidents) != 1 {
				in.fail(fmt.Sprintf("ingest accepted %d incidents, want exactly 1 (alert skipped or grammar-dropped)", acc.Count))
				return
			}
			item := acc.Incidents[0]
			if !item.Triggered || item.WorkflowID == "" {
				in.fail("triage not triggered — the target has no triage backend wired; a session will never mint")
				return
			}
			in.ref, in.workflowID = item.ExternalRef, item.WorkflowID
			if err := trk.register(in.ref, in); err != nil {
				in.fail(err.Error())
				return
			}

			probeOnce.Do(func() { probe = runDuplicateProbe(ctx, client, in) })

			deadline := time.After(cfg.SessionTimeout)
			select {
			case <-in.foundCh:
				// Visible. Now POLL TO TERMINAL (TG-80 P1-2): the page shows a session at classification,
				// the earliest boundary; the run completes only when the detail reports a terminal status.
				// The same SessionTimeout bounds the whole wait, and a session that never gets there fails
				// NAMING the last status it reported — never scored complete at first sight.
				for {
					det, derr := client.sessionDetail(ctx, in.ref)
					if derr == nil {
						in.lastStatus = det.Status
						if isTerminal(cfg.TerminalStatuses, det.Status) {
							in.terminalAt = time.Now()
							break
						}
					}
					select {
					case <-time.After(cfg.PollInterval):
						continue
					case <-deadline:
						in.fail(fmt.Sprintf("session never reached a terminal status within %s (last status %q)", cfg.SessionTimeout, in.lastStatus))
					case <-ctx.Done():
						in.fail(fmt.Sprintf("run wall-clock cap exceeded before a terminal status (last status %q)", in.lastStatus))
					}
					break
				}
			case <-deadline:
				in.fail(fmt.Sprintf("session never appeared on the spine within %s (LOST)", cfg.SessionTimeout))
			case <-ctx.Done():
				in.fail("run wall-clock cap exceeded before the session appeared")
			}
		}(in)
	}
	wg.Wait()
	stopPoll()
	pollWG.Wait()

	// Judge: host match per completed run, then per-ref uniqueness over one final page.
	var durations []time.Duration
	var lastSeen time.Time
	for _, in := range incidents {
		if in.failure != nil {
			continue
		}
		if in.seen.Host != in.host {
			in.fail(fmt.Sprintf("cross-contamination: session host %q, incident host %q", in.seen.Host, in.host))
			continue
		}
		// TG-80 P1-2: latency is POST → TERMINAL, the whole path (ingest → gate → Runner → Temporal → DB),
		// not POST → first visibility.
		durations = append(durations, in.terminalAt.Sub(in.postAt))
		if in.terminalAt.After(lastSeen) {
			lastSeen = in.terminalAt
		}
	}
	judgeUniqueness(ctx, client, trk, incidents)

	for _, in := range incidents {
		if in.failure != nil {
			lr.Failed++
			lr.Failures = append(lr.Failures, *in.failure)
		} else {
			lr.Completed++
		}
	}
	lr.DuplicateProbe = probe
	if probe.Ran && !probe.Passed {
		lr.Failures = append(lr.Failures, Failure{Ref: probe.Ref, Reason: "duplicate probe: " + probe.Reason})
	}
	if len(durations) > 0 {
		lr.P50Ms = percentile(durations, 0.50).Milliseconds()
		lr.P95Ms = percentile(durations, 0.95).Milliseconds()
		lr.MaxMs = percentile(durations, 1.00).Milliseconds()
		if span := lastSeen.Sub(levelStart); span > 0 {
			lr.ThroughputPerSec = float64(lr.Completed) / span.Seconds()
		}
	}
	return lr
}

// runDuplicateProbe re-POSTs an ALREADY-ACCEPTED incident while its level is still running and asserts
// the front door's idempotency contract: 202, the SAME external_ref, the SAME workflow id — a re-fire
// joins the in-flight session, it never mints a second one (StartTriage's reject-duplicate semantics).
func runDuplicateProbe(ctx context.Context, client *client, in *incident) DuplicateProbe {
	p := DuplicateProbe{Ran: true, Ref: in.ref}
	acc, err := client.postIncident(ctx, in.body)
	if err != nil {
		p.Reason = fmt.Sprintf("duplicate POST refused: %v", err)
		return p
	}
	if acc.Count != 1 || len(acc.Incidents) != 1 {
		p.Reason = fmt.Sprintf("duplicate POST accepted %d incidents, want 1", acc.Count)
		return p
	}
	dup := acc.Incidents[0]
	if dup.ExternalRef != in.ref {
		p.Reason = fmt.Sprintf("duplicate POST minted a DIFFERENT ref %q (want %q)", dup.ExternalRef, in.ref)
		return p
	}
	if dup.WorkflowID != in.workflowID {
		p.Reason = fmt.Sprintf("duplicate POST minted a SECOND session: workflow %q, original %q", dup.WorkflowID, in.workflowID)
		return p
	}
	p.Passed = true
	return p
}

// judgeUniqueness reads ONE final page and fails any of our refs appearing more than once on it. The
// production list collapses per ref by construction (sessions_read.go GROUPs BY external_ref), so against
// a real deployment this can only fire if that contract regresses; against the rig — whose store
// deliberately surfaces every persisted row — it is the check that catches a double-minted session.
func judgeUniqueness(ctx context.Context, client *client, trk *tracker, incidents []*incident) {
	page, err := client.sessions(ctx, serverPageLimit)
	if err != nil {
		for _, in := range incidents {
			if in.failure == nil {
				in.fail(fmt.Sprintf("final uniqueness read failed: %v", err))
			}
		}
		return
	}
	counts := map[string]int{}
	for _, row := range page.Sessions {
		trk.mu.Lock()
		_, ours := trk.byRef[row.ExternalRef]
		trk.mu.Unlock()
		if ours {
			counts[row.ExternalRef]++
		}
	}
	for _, in := range incidents {
		if in.failure == nil && in.ref != "" && counts[in.ref] > 1 {
			in.fail(fmt.Sprintf("EXACTLY-ONE violated: %d sessions on the spine for this ref", counts[in.ref]))
		}
	}
}

// percentile is nearest-rank over a sorted copy: p in (0,1]; p=1 is the max. Nearest-rank never
// interpolates, so a reported p95 is a latency an actual run PAID, not a synthetic midpoint.
func percentile(durs []time.Duration, p float64) time.Duration {
	if len(durs) == 0 {
		return 0
	}
	s := make([]time.Duration, len(durs))
	copy(s, durs)
	sort.Slice(s, func(i, j int) bool { return s[i] < s[j] })
	rank := int(float64(len(s))*p+0.9999999) - 1
	if rank < 0 {
		rank = 0
	}
	if rank >= len(s) {
		rank = len(s) - 1
	}
	return s[rank]
}

// webhookFor builds the single-alert FIRING Alertmanager webhook for one fixture host. The shape is the
// module's real v4 grammar (modules/ingest/prometheus-alertmanager): alertname+instance drive the
// correlation ref am-<alertname>-<host>; severity "warning" survives the info-noise skip; startsAt must
// sit inside the envelope's clock-skew window, so it is minted at build time. JSON is assembled with
// Marshal (never string concatenation) so a hostile-looking host could not break the frame — but the
// host is ours by construction anyway.
func webhookFor(host string, at time.Time) []byte {
	type alert struct {
		Status      string            `json:"status"`
		Labels      map[string]string `json:"labels"`
		Annotations map[string]string `json:"annotations"`
		StartsAt    string            `json:"startsAt"`
		Fingerprint string            `json:"fingerprint"`
	}
	body := struct {
		Status string  `json:"status"`
		Alerts []alert `json:"alerts"`
	}{
		Status: "firing",
		Alerts: []alert{{
			Status: "firing",
			Labels: map[string]string{
				"alertname": alertName,
				"instance":  host + ":9100",
				"severity":  "warning",
			},
			Annotations: map[string]string{
				"summary": "loadharness synthetic incident on " + host,
			},
			StartsAt:    at.Format(time.RFC3339),
			Fingerprint: fingerprintFor(host),
		}},
	}
	b, err := json.Marshal(body)
	if err != nil {
		// Structurally impossible (fixed shape, string fields); a change that makes it possible must fail loudly.
		panic(fmt.Sprintf("loadharness: webhook marshal: %v", err))
	}
	return b
}

// fingerprintFor derives a stable per-host pseudo-fingerprint (hex, like Alertmanager's), so the
// synthetic alert carries the join key real alerts carry.
func fingerprintFor(host string) string {
	sum := 0
	for _, r := range host {
		sum = sum*31 + int(r)
	}
	return fmt.Sprintf("%016x", uint64(sum))
}

// ExpectedRef reports the external_ref the pipeline will mint for a fixture host — exported for the rig
// tests, which need to aim a fault at a ref BEFORE any request exists. Mirrors the module's
// am-<alertname>-<target> derivation; the harness itself never predicts refs (it reads them from the
// accepted response), so a drift here can only mis-aim a test fault, never mis-judge a run.
func ExpectedRef(runID string, level, index int) string {
	return "am-" + alertName + "-" + FixtureHost(runID, level, index)
}

// FixtureHost reports the fixture host for (runID, level, index) — the same derivation runLevel uses.
func FixtureHost(runID string, level, index int) string {
	return fmt.Sprintf("lh-%s-l%d-%02d.%s", runID, level, index, fixtureDomain)
}
