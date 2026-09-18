package main

// CAN TG ACTUALLY READ THE HOSTS IT IS ASKED ABOUT? (TG-271)
//
// The agent's host diagnostics (check-host-services / -disk / -memory / -load) SSH the alerting host and
// verify its key against TG_HOSTDIAG_KNOWN_HOSTS. Verification is mandatory and fails closed, which is
// correct — and it means a host missing from that file is a host TG can never diagnose.
//
// That file covered 16 of 38 alerted hosts for weeks and nothing noticed. The operational cost was
// measured: on session librenms-dc1-183957 the agent reached check-host-services, got
// "(host was unreachable or the read errored)", recorded "I cannot yet name the failing unit", and stopped
// after 57 seconds. The host had four failed units at that moment. 478 such calls are recorded
// historically; every one against an uncovered host could not have produced anything.
//
// THE MEASUREMENT IS DONE WITH THE CONSUMER'S OWN PREDICATE. Coverage is decided by the same
// *sshhost.Verifier the diagnostics dial with — never by grepping the file. known_hosts entries are
// HASHED, so `grep <hostname>` returns 0 matches for a host that is perfectly well covered, and that is
// exactly what hid the original gap. Verifier.Algorithms returns nil for an unknown host and a non-empty
// algorithm list for a known one.
//
// THE RESOLVABILITY SPLIT IS LOAD-BEARING, NOT A REFINEMENT. Alertmanager's `host` label carries
// Kubernetes component names — cilium-agent, coredns, kube-etcd, node-exporter, tetragon, seaweedfs-master.
// Measured 2026-08-06: of 86 alert host labels, 52 were uncovered and exactly HALF of those are not hosts
// at all. A naive covered/alerted gauge reads 34/86 and is red forever, and the governance scheduler's own
// comment says what that costs — "an alarm that is always red trains an operator to stop reading
// governance alarms, which is how the real one gets missed". Only the resolvable ones are a defect.

import (
	"context"
	"log"
	"net"
	"os"
	"strings"
	"sync/atomic"
	"time"

	"github.com/territory-grounder/grounder/core/db"
	"github.com/territory-grounder/grounder/core/metrics"
	"github.com/territory-grounder/grounder/core/sshhost"
)

// knownHostsCoverage is one measurement pass.
type knownHostsCoverage struct {
	Alerted             int
	Covered             int
	UncoveredResolvable int
	Entries             int
}

// coverageSamples renders the pass. Every series is emitted UNCONDITIONALLY, including at zero: an absent
// series and a zero are different claims, and the whole point of this gate is that "nothing is uncovered"
// must be distinguishable from "nothing measured".
func coverageSamples(c knownHostsCoverage) []metrics.Sample {
	return []metrics.Sample{
		{
			Name: "tg_hostdiag_hosts_alerted", Kind: metrics.Gauge, Value: float64(c.Alerted),
			Help: "distinct hosts TG has been alerted about in the coverage window — the denominator for " +
				"host-diagnostic reach",
		},
		{
			Name: "tg_hostdiag_hosts_covered", Kind: metrics.Gauge, Value: float64(c.Covered),
			Help: "alerted hosts whose key is in TG_HOSTDIAG_KNOWN_HOSTS, decided by the same verifier the " +
				"diagnostics dial with (NOT by grep — known_hosts entries are hashed)",
		},
		{
			Name: "tg_hostdiag_hosts_uncovered_resolvable", Kind: metrics.Gauge,
			Value: float64(c.UncoveredResolvable),
			Help: "THE NUMBER THAT MATTERS: alerted hosts that resolve in DNS and have no host key, so every " +
				"diagnostic read against them fails closed. Excludes k8s component names (cilium-agent, " +
				"coredns, node-exporter…) which appear in the alert host label and are not hosts TG should SSH",
		},
		{
			Name: "tg_hostdiag_known_hosts_entries", Kind: metrics.Gauge, Value: float64(c.Entries),
			Help: "lines parsed from the known_hosts file. ZERO means the file is empty or unreadable — " +
				"which otherwise looks identical to full coverage of an empty alert set",
		},
	}
}

// measureCoverage runs one pass. Kept free of I/O construction so the whole decision is testable: `known`
// and `resolvable` are injected, which is what lets a killing mutation on the resolvability split fail.
func measureCoverage(hosts []string, known, resolvable func(string) bool, entries int) knownHostsCoverage {
	c := knownHostsCoverage{Entries: entries}
	for _, h := range hosts {
		if h == "" {
			continue
		}
		c.Alerted++
		switch {
		case known(h):
			c.Covered++
		case resolvable(h):
			c.UncoveredResolvable++
		}
	}
	return c
}

// alertedHostReader is the denominator's source: the hosts TG has actually been asked about.
type alertedHostReader interface {
	AlertedHosts(ctx context.Context, window time.Duration) ([]string, error)
}

// startKnownHostsCoverageJob publishes the coverage pass on a cadence.
//
// A nil reader or a nil verifier yields a reader that emits NOTHING and says so at boot. That is
// deliberate: publishing zeros without a verifier would report perfect coverage for a worker that cannot
// verify any host at all, which is the inverse of the truth.
func startKnownHostsCoverageJob(
	ctx context.Context,
	store alertedHostReader,
	v *sshhost.Verifier,
	entries int,
	window, every time.Duration,
	resolvable func(string) bool,
) func() []metrics.Sample {
	var held atomic.Pointer[[]metrics.Sample]
	read := func() []metrics.Sample {
		if s := held.Load(); s != nil {
			return *s
		}
		return nil
	}
	if store == nil || v == nil || resolvable == nil {
		log.Print("known-hosts coverage: not measured — no alert store or no host-key verifier wired. TG " +
			"cannot report which alerted hosts it is able to diagnose, which is the state TG-271 describes")
		return read
	}
	known := func(h string) bool { return len(v.Algorithms(h+":22")) > 0 }
	refresh := func() {
		rctx, cancel := context.WithTimeout(ctx, every)
		defer cancel()
		hosts, err := store.AlertedHosts(rctx, window)
		if err != nil {
			log.Printf("known-hosts coverage: read failed: %v (retry next tick)", err)
			return
		}
		c := measureCoverage(hosts, known, resolvable, entries)
		s := coverageSamples(c)
		held.Store(&s)
		if c.UncoveredResolvable > 0 {
			log.Printf("known-hosts coverage: %d of %d alerted hosts covered; %d resolvable host(s) have NO "+
				"key and cannot be diagnosed at all", c.Covered, c.Alerted, c.UncoveredResolvable)
		}
	}
	refresh() // publish immediately: absent is not zero, and a deploy-shaped blind window is when people look
	go func() {
		t := time.NewTicker(every)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				refresh()
			}
		}
	}()
	log.Printf("known-hosts coverage: measuring host-diagnostic reach every %s over a %s window", every, window)
	return read
}

// knownHostsCoverageInputs builds the coverage job's collaborators from the SAME env var the diagnostic
// tools read, so the gauge can never measure a different file from the one the tools dial with.
//
// A missing/unreadable file yields a nil verifier and entries=0, and the job then reports nothing and says
// so — rather than publishing "0 uncovered", which a reader would take as full coverage.
func knownHostsCoverageInputs(path string) (*sshhost.Verifier, int) {
	if strings.TrimSpace(path) == "" {
		return nil, 0
	}
	v, err := sshhost.New(path)
	if err != nil || v == nil {
		log.Printf("known-hosts coverage: cannot parse %s (%v) — host-diagnostic reach is unmeasured, and "+
			"every diagnostic read is also failing closed for the same reason", path, err)
		return nil, 0
	}
	entries := 0
	if b, rerr := os.ReadFile(path); rerr == nil {
		for _, l := range strings.Split(string(b), "\n") {
			if l = strings.TrimSpace(l); l != "" && !strings.HasPrefix(l, "#") {
				entries++
			}
		}
	}
	return v, entries
}

// dnsResolvable is the split between a real estate host and an Alertmanager component label. It is a
// LOOKUP, not a name pattern: `dc1*` would miss notrf01/dc2 hosts and would happily match a k8s
// service someone names that way. A lookup failure means "not a host TG should hold a key for".
func dnsResolvable(host string) bool {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if addrs, err := net.DefaultResolver.LookupHost(ctx, host); err == nil && len(addrs) > 0 {
		return true
	}
	return false
}

// alertedHostStoreOrNil keeps a TYPED NIL out of the interface — the same hazard pollQueueStoreOrNil
// documents: a nil *db.Pool wrapped in a store yields a NON-nil interface, so the `store == nil` guard in
// startKnownHostsCoverageJob would not fire and the first refresh would panic on a worker with no database.
func alertedHostStoreOrNil(pool *db.Pool) alertedHostReader {
	if pool == nil {
		return nil
	}
	return db.NewAlertHistoryStore(pool)
}
