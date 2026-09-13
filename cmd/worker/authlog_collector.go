package main

// authlog_collector.go — THE COLLECTOR THAT MAKES TG'S SECOND WITNESS REAL (TG-315).
//
// THE STATE THIS CLOSES. `ingest_alert` holds 3,167 rows across exactly three source types — librenms,
// pve-liveness, prometheus-alertmanager — and ZERO rows carrying `category=security-incident`. Every one
// of those sources answers the same question ("is it up?"), so `core/correlate`'s cross-source rule, which
// keys on DISTINCT source_type, has never had a second KIND of witness to correlate with. The authlog
// parser (!1022), its ingest route, its `sources` row, its OpenBao bearer token and its never-delivered
// gauge are all built and live. Nothing reads the logs.
//
// WHY A PULL COLLECTOR RATHER THAN A SENDER. TG-349 covers pointing the estate's CrowdSec at TG's ingest,
// and its blocker is estate access to the DMZ side — a third-party product on a host TG does not own. That
// does NOT gate authlog: the syslog-ng trees are already readable over an SSH lane TG has provisioned, and
// the host-side forced-command guard already permits the exact `tail -n <n> -- <path>` argv this issues.
// TG can be its own sender here, today, with no new credential and no estate cooperation.
//
// WHAT THIS FILE IS AND IS NOT. It is the READABLE CORE: given a Runner and a host set, produce admitted
// envelopes. It performs no admission itself — `admit` is injected — so the whole path is exercised in CI
// against a fake Runner with no SSH, no database and no workflow engine. That split is deliberate: the
// interesting failures here are path resolution, parse yield and the offered-vs-produced distinction, and
// none of them need a live estate to provoke.

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"sync/atomic"
	"time"

	coreingest "github.com/territory-grounder/grounder/core/ingest"
	"github.com/territory-grounder/grounder/core/metrics"
	"github.com/territory-grounder/grounder/modules/ingest/authlog"
	"github.com/territory-grounder/grounder/modules/observability/syslogng"
)

// authlogTailLines is how many trailing lines each poll reads per host. Bounded for the same reason the
// investigation tool is bounded — a 2.43 GB/day file (measured on dc1fw01, 2026-08-07) is not
// something to stream — and large enough that a burst between polls is not truncated at a normal interval.
const authlogTailLines = 500

// authlogMaxPrincipalsPerHostKind bounds how many DISTINCT-PRINCIPAL envelopes one (host, kind) yields in a
// single poll — the DoS backstop this source's own ticket (TG-315 "watch out for") demands before it is
// armed. Fold deliberately keeps principals distinct (TestDifferentPrincipalsDoNotFoldTogether: a
// user-enumeration sweep must be visible as many usernames, not one), and every distinct principal mints a
// SEPARATE triage session, because the workflow id carries the principal. So a username spray — dozens of
// logins tried against one host in one window — would mint one full agent loop PER username on a
// single-brain deployment (TG-231, no fallback), which is exactly the self-inflicted model-gateway cascade
// TG-376/TG-384 record TG doing to itself on the pve03 storm.
//
// Above the cap, the (host,kind) group folds into ONE aggregate enumeration-sweep incident (TG-421, see
// capEnumeration/aggregateSweep) carrying the distinct-principal count, the folded total, and the loudest
// principal as the named top offender — never a silent drop, because a suppressed sweep is itself the thing
// an operator most wants to see. This const is the bound that keeps a whole spray to one triage session.
//
// Chosen for an internal application estate where a handful of fat-fingered logins per host per window is
// normal and a dozen distinct usernames is not. It is a ceiling on model-consuming work, not a detection
// threshold — the correlator keys on distinct source_type, not on how many principals a host produced.
const authlogMaxPrincipalsPerHostKind = 8

// capEnumeration bounds ONE host's folded events per Kind (ParseLines returns one host's events, already
// folded by host/kind/principal). A Kind AT OR BELOW the cap passes through unchanged. A Kind that EXCEEDS
// the cap is folded into ONE aggregate enumeration-sweep event (TG-421) instead of the loudest `cap`
// individuals plus a silently-dropped tail: the aggregate carries the distinct-principal count, the folded
// total, and the LOUDEST principal as the named top offender. That bounds model-consuming work to a single
// triage session, is higher signal than `cap` arbitrary usernames, and masks no targeted attack (a
// 900-failure account among one-off probes is still named). `suppressed` counts the distinct principals
// folded into an aggregate (0 below the cap). cap <= 0 disables the bound (returns the input unchanged), so
// a test or a deployment can prove the un-capped flood is what the cap prevents.
func capEnumeration(events []authlog.Event, cap int) (kept []authlog.Event, suppressed int) {
	if cap <= 0 {
		return events, 0
	}
	byKind := map[authlog.Kind][]authlog.Event{}
	var kindOrder []authlog.Kind
	for _, e := range events {
		if _, ok := byKind[e.Kind]; !ok {
			kindOrder = append(kindOrder, e.Kind)
		}
		byKind[e.Kind] = append(byKind[e.Kind], e)
	}
	for _, k := range kindOrder {
		grp := byKind[k]
		if len(grp) <= cap {
			kept = append(kept, grp...)
			continue
		}
		kept = append(kept, aggregateSweep(grp))
		suppressed += len(grp)
	}
	return kept, suppressed
}

// aggregateSweep folds one over-the-cap (host, kind) group into a single enumeration-sweep event (TG-421). It
// carries the distinct-principal COUNT (DistinctPrincipals), the folded TOTAL attempts (Count), the widest
// FirstSeen→LastSeen span, and the LOUDEST principal by attempt count (Principal) as the named top offender —
// so a targeted attack hidden inside a spray survives the fold. The loudest is chosen by a stable
// loudest-first sort, so a tie keeps Fold's deterministic order. grp is non-empty (only over-cap groups fold).
func aggregateSweep(grp []authlog.Event) authlog.Event {
	sorted := make([]authlog.Event, len(grp))
	copy(sorted, grp)
	sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].Count > sorted[j].Count })
	agg := authlog.Event{
		Host:               sorted[0].Host,
		Kind:               sorted[0].Kind,
		Principal:          sorted[0].Principal, // loudest = named top offender; ref never keys on it (ToEnvelope)
		DistinctPrincipals: len(sorted),
	}
	for _, e := range sorted {
		agg.Count += e.Count
		if !e.FirstSeen.IsZero() && (agg.FirstSeen.IsZero() || e.FirstSeen.Before(agg.FirstSeen)) {
			agg.FirstSeen = e.FirstSeen
		}
		if e.LastSeen.After(agg.LastSeen) {
			agg.LastSeen = e.LastSeen
		}
	}
	return agg
}

// authlogCollector reads auth events out of the syslog-ng trees on a schedule.
type authlogCollector struct {
	servers []syslogng.Server
	// hosts is the per-server set of hosts to read. Explicit rather than discovered: a collector that
	// enumerated the tree would read every directory syslog-ng has ever created, including the malformed
	// hostnames a parser occasionally mints ("ankh", "DPAA", "I2C" are live examples), and would mint
	// triage sessions keyed on them.
	hosts  []string
	runner syslogng.Runner
	mod    *authlog.Module
	now    func() time.Time
	yield  *authlogYield
}

// authlogCollect is one poll's outcome, kept separate from the act of admitting so the caller decides what
// to do with it and the oracle can assert on it directly.
type authlogCollect struct {
	// Offered is how many (server, host) reads this poll ATTEMPTED. It is the denominator, and without it
	// a zero Produced is ambiguous between "nothing happened on the estate" and "nothing was read".
	Offered int
	// Read is how many of those returned bytes at all.
	Read int
	// Produced is how many envelopes the parse yielded.
	Produced  int
	Envelopes []coreingest.IncidentEnvelope
	// Failures carries per-host read errors. A poll that fails on nine of ten hosts and succeeds on one
	// must not report as a success — the count is what makes that visible.
	Failures []string
	// Suppressed is how many distinct-principal events the enumeration cap dropped this poll. Non-zero means
	// a username spray hit the ceiling — a security signal in its own right, and the reason the single brain
	// was NOT asked to open one investigation per attempted username.
	Suppressed int
}

func newAuthlogCollector(servers []syslogng.Server, hosts []string, runner syslogng.Runner, now func() time.Time) *authlogCollector {
	if now == nil {
		now = time.Now
	}
	return &authlogCollector{
		servers: servers,
		hosts:   hosts,
		runner:  runner,
		mod:     authlog.New(authlog.WithClock(now)),
		now:     now,
		yield:   &authlogYield{},
	}
}

// collectOnce runs one poll across every (server, host) pair.
//
// A read failure on one host NEVER aborts the poll. The syslog-ng trees are per-host and a host that ships
// no logs at all is the normal case — 64 directories exist and 12 wrote on the day this was written — so
// treating an absent file as fatal would mean the collector delivers nothing the moment one host goes
// quiet, which is precisely when the others matter most.
func (c *authlogCollector) collectOnce(ctx context.Context) authlogCollect {
	var out authlogCollect
	if c == nil || c.runner == nil {
		return out
	}
	year := c.now().UTC().Year()
	for _, srv := range c.servers {
		base := srv.BasePath
		if base == "" {
			base = syslogng.DefaultBasePath
		}
		for _, host := range c.hosts {
			out.Offered++
			lines, err := c.readHost(ctx, srv, base, host)
			if err != nil {
				out.Failures = append(out.Failures, fmt.Sprintf("%s/%s: %v", srv.SSHHost, host, err))
				continue
			}
			if len(lines) == 0 {
				continue
			}
			out.Read++
			events, suppressed := capEnumeration(authlog.ParseLines(host, lines, year), authlogMaxPrincipalsPerHostKind)
			out.Suppressed += suppressed
			for _, e := range events {
				env, err := c.mod.ToEnvelope(e)
				if err != nil {
					// A parsed event the module refuses is a parser/module disagreement, not an estate
					// fact. Counted as a failure so it cannot masquerade as a quiet host.
					out.Failures = append(out.Failures, fmt.Sprintf("%s/%s: envelope: %v", srv.SSHHost, host, err))
					continue
				}
				out.Produced++
				out.Envelopes = append(out.Envelopes, env)
			}
		}
	}
	return out
}

// readHost issues the ALREADY-ALLOWLISTED argv against each candidate path in order and returns the first
// non-empty answer. The candidate order (today.log, then today's dated file) is the syslog-ng package's
// own — sites disagree about whether a current-file exists, and resolving it here independently is how the
// two callers would drift.
func (c *authlogCollector) readHost(ctx context.Context, srv syslogng.Server, base, host string) ([]string, error) {
	var lastErr error
	for _, p := range syslogng.ReadPathsFor(base, host, c.now) {
		argv := []string{"tail", "-n", strconv.Itoa(authlogTailLines), "--", p}
		res, err := c.runner.Run(ctx, srv, argv)
		if err != nil {
			lastErr = err
			continue
		}
		// A non-zero exit is the normal answer for "this host has no such file today". It is not an error
		// and must not be reported as one — TG-363 is the record of what it costs when a missing file and
		// a refused request share a status.
		if res.ExitCode != 0 || len(res.Stdout) == 0 {
			continue
		}
		return splitLines(res.Stdout), nil
	}
	return nil, lastErr
}

func splitLines(b []byte) []string {
	var out []string
	start := 0
	for i := 0; i < len(b); i++ {
		if b[i] == '\n' {
			if i > start {
				out = append(out, string(b[start:i]))
			}
			start = i + 1
		}
	}
	if start < len(b) {
		out = append(out, string(b[start:]))
	}
	return out
}

// authlogYield is the offered-vs-produced register, the same shape pve-liveness carries (TG-250).
//
// Four states produce zero admitted events and only one is healthy: the estate is genuinely quiet; every
// read fails; the host set is empty so nothing is offered; the goroutine is dead. A single "events
// admitted" counter cannot tell them apart, and this source's whole value is being a witness — a silent
// witness and an absent one are the same evidence.
type authlogYield struct {
	polls        atomic.Int64
	offered      atomic.Int64
	read         atomic.Int64
	produced     atomic.Int64
	failures     atomic.Int64
	suppressed   atomic.Int64
	lastPollUnix atomic.Int64
}

func (y *authlogYield) record(now time.Time, c authlogCollect) {
	if y == nil {
		return
	}
	y.polls.Add(1)
	y.offered.Add(int64(c.Offered))
	y.read.Add(int64(c.Read))
	y.produced.Add(int64(c.Produced))
	y.failures.Add(int64(len(c.Failures)))
	y.suppressed.Add(int64(c.Suppressed))
	y.lastPollUnix.Store(now.Unix())
}

// samples renders the register. ALWAYS EMITTED, including at zero — a series that appears only once the
// collector delivers makes "quiet" and "never ran" the same observation.
func (y *authlogYield) samples(now time.Time) []metrics.Sample {
	if y == nil {
		return nil
	}
	since := -1.0 // "never", which is a different fact from "a long time ago"
	if last := y.lastPollUnix.Load(); last > 0 {
		since = now.Sub(time.Unix(last, 0)).Seconds()
	}
	return []metrics.Sample{
		{Name: "tg_authlog_polls_total", Kind: metrics.Counter,
			Help:  "authlog collector polls completed. Advances only while the loop lives, so a flat counter is a dead goroutine rather than a quiet estate.",
			Value: float64(y.polls.Load())},
		{Name: "tg_authlog_reads_offered_total", Kind: metrics.Counter,
			Help:  "(server,host) reads ATTEMPTED. The denominator: 0 produced against 0 offered is an unconfigured collector, not a quiet estate.",
			Value: float64(y.offered.Load())},
		{Name: "tg_authlog_reads_answered_total", Kind: metrics.Counter,
			Help:  "reads that returned bytes. Offered-minus-answered is how many hosts ship no auth log at all — a real estate fact, not a fault.",
			Value: float64(y.read.Load())},
		{Name: "tg_authlog_events_produced_total", Kind: metrics.Counter,
			Help:  "auth events parsed into envelopes. Read against reads_answered: answered>0 with produced==0 for long means the log shape stopped matching the parser.",
			Value: float64(y.produced.Load())},
		{Name: "tg_authlog_read_failures_total", Kind: metrics.Counter,
			Help:  "per-host read or envelope failures. A poll that fails on nine hosts and succeeds on one must not read as a success.",
			Value: float64(y.failures.Load())},
		{Name: "tg_authlog_enumeration_suppressed_total", Kind: metrics.Counter,
			Help:  "distinct-principal events FOLDED into an aggregate enumeration-sweep envelope by the per-(host,kind) cap (TG-421) — admitted as ONE incident naming the loudest principal, not dropped. Non-zero is a username-spray that would otherwise have minted one triage session per username on the single brain; a sustained climb is a live enumeration attack worth an alert.",
			Value: float64(y.suppressed.Load())},
		{Name: "tg_authlog_seconds_since_last_poll", Kind: metrics.Gauge,
			Help:  "age of the last completed poll; -1 means the loop has never completed one, which is distinct from 'a long time ago'.",
			Value: since},
	}
}
