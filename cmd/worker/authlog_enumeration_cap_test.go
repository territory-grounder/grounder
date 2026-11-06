package main

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/territory-grounder/grounder/modules/ingest/authlog"
	"github.com/territory-grounder/grounder/modules/observability/syslogng"
)

// THE ENUMERATION CAP → AGGREGATE (TG-315 "watch out for"; TG-421).
//
// authlog Fold keeps principals distinct on purpose, and the workflow id carries the principal, so a
// username-spray mints one full triage session PER username. On the single-brain deployment (TG-231, no
// fallback) that is the self-inflicted model-gateway cascade TG-376/TG-384 record. The cap bounds it — and
// TG-421 makes the bounded case FIRST-CLASS: a (host,kind) over the ceiling folds into ONE aggregate
// enumeration-sweep incident carrying the distinct-principal count, the folded total, and the LOUDEST
// principal as the named top offender, instead of the loudest 8 individuals plus a silently-dropped tail.

// TestCapEnumerationFoldsASprayIntoOneAggregate — the load-bearing case. Fifty usernames sprayed at one host
// in one window collapse to ONE aggregate, carrying the distinct-principal count and folded total; every
// folded principal is counted (never silently), because a suppressed spray is the signal an operator wants.
//
//	Killing mutation: keep the loudest `cap` + drop the tail (the pre-TG-421 behaviour) — len(kept) becomes
//	the cap, not 1, and DistinctPrincipals is unset → RED.
func TestCapEnumerationFoldsASprayIntoOneAggregate(t *testing.T) {
	var spray []authlog.Event
	for i := range 50 {
		spray = append(spray, authlog.Event{Host: "web01", Kind: authlog.KindFailure, Principal: fmt.Sprintf("user%02d", i), Count: 1})
	}

	kept, suppressed := capEnumeration(spray, authlogMaxPrincipalsPerHostKind)

	if len(kept) != 1 {
		t.Fatalf("kept %d events, want exactly 1 aggregate — a %d-username spray must collapse to ONE sweep "+
			"incident, not %d triage sessions on the single brain", len(kept), len(spray), len(kept))
	}
	agg := kept[0]
	if agg.DistinctPrincipals != 50 {
		t.Errorf("aggregate DistinctPrincipals=%d, want 50 — the sweep must carry how many usernames it folded", agg.DistinctPrincipals)
	}
	if agg.Count != 50 {
		t.Errorf("aggregate Count=%d, want 50 — the folded total (sum of attempts) must survive the fold", agg.Count)
	}
	if suppressed != 50 {
		t.Errorf("suppressed=%d, want 50 — every folded principal must be counted so a spray is observable", suppressed)
	}
}

// TestCapEnumerationNamesTheLoudestInTheAggregate — the SAFETY property the old keep-loudest behaviour
// protected, preserved through the fold. A 900-failure root among 20 one-off probes is the account actually
// under attack; the aggregate must NAME it as loudest and carry its attempts in the total, or a targeted
// attack hides inside the sweep.
//
//	Killing mutation: fold the loudest identity away (leave the aggregate Principal empty) → the targeted
//	attack's name vanishes from the incident → RED.
func TestCapEnumerationNamesTheLoudestInTheAggregate(t *testing.T) {
	var ev []authlog.Event
	for i := range 20 {
		ev = append(ev, authlog.Event{Host: "web01", Kind: authlog.KindFailure, Principal: fmt.Sprintf("noise%02d", i), Count: 1})
	}
	ev = append(ev, authlog.Event{Host: "web01", Kind: authlog.KindFailure, Principal: "root", Count: 900})

	kept, _ := capEnumeration(ev, authlogMaxPrincipalsPerHostKind)

	if len(kept) != 1 {
		t.Fatalf("kept %d events, want 1 aggregate", len(kept))
	}
	agg := kept[0]
	if agg.Principal != "root" {
		t.Errorf("aggregate names %q as loudest, want \"root\" — the 900-failure account under attack must not "+
			"be masked by the sweep", agg.Principal)
	}
	if agg.Count != 920 {
		t.Errorf("aggregate Count=%d, want 920 — the loudest's 900 attempts must be in the folded total", agg.Count)
	}
	if agg.DistinctPrincipals != 21 {
		t.Errorf("aggregate DistinctPrincipals=%d, want 21", agg.DistinctPrincipals)
	}
}

// TestCapEnumerationLeavesNormalTrafficUntouched — the negative control. A handful of fat-fingered logins per
// host per window is normal; below the ceiling the cap is invisible — per-principal events pass through and
// NONE is marked an aggregate, or this whole source becomes lossy.
func TestCapEnumerationLeavesNormalTrafficUntouched(t *testing.T) {
	ev := []authlog.Event{
		{Host: "web01", Kind: authlog.KindFailure, Principal: "alice", Count: 2},
		{Host: "web01", Kind: authlog.KindFailure, Principal: "bob", Count: 1},
		{Host: "web01", Kind: authlog.KindEscalation, Principal: "deploy", Count: 1},
	}
	kept, suppressed := capEnumeration(ev, authlogMaxPrincipalsPerHostKind)
	if len(kept) != 3 || suppressed != 0 {
		t.Fatalf("normal traffic altered: kept=%d suppressed=%d, want kept=3 suppressed=0", len(kept), suppressed)
	}
	for _, e := range kept {
		if e.DistinctPrincipals != 0 {
			t.Errorf("below-cap event %q wrongly marked an aggregate (DistinctPrincipals=%d)", e.Principal, e.DistinctPrincipals)
		}
	}
}

// TestCapEnumerationIsPerKind — the ceiling is per (host, KIND), not per host. Exactly `cap` failures AND
// `cap` escalations is 2*cap legitimate events AT the ceiling (not over it), so nothing folds.
func TestCapEnumerationIsPerKind(t *testing.T) {
	var ev []authlog.Event
	for i := range authlogMaxPrincipalsPerHostKind {
		ev = append(ev, authlog.Event{Host: "web01", Kind: authlog.KindFailure, Principal: fmt.Sprintf("f%d", i), Count: 1})
		ev = append(ev, authlog.Event{Host: "web01", Kind: authlog.KindEscalation, Principal: fmt.Sprintf("e%d", i), Count: 1})
	}
	kept, suppressed := capEnumeration(ev, authlogMaxPrincipalsPerHostKind)
	if suppressed != 0 || len(kept) != 2*authlogMaxPrincipalsPerHostKind {
		t.Errorf("per-kind ceiling failed: kept=%d suppressed=%d, want kept=%d suppressed=0 — the cap must be "+
			"per (host,kind), not per host", len(kept), suppressed, 2*authlogMaxPrincipalsPerHostKind)
	}
}

// TestCollectOnceFoldsASprayInThePollPath — the fold must be IN the poll path, not a pure function nobody
// calls (the "built, tested, unwired" defect this source's own oracles exist to prevent). A fake syslog tree
// spraying 40 usernames at one host yields exactly ONE aggregate envelope, with the fold recorded on the
// poll result (and thus the register).
func TestCollectOnceFoldsASprayInThePollPath(t *testing.T) {
	now := authlogFixedClock()
	var b strings.Builder
	for i := range 40 {
		fmt.Fprintf(&b, "Aug  7 10:01:%02d web01 sshd[111]: Failed password for user%02d from 192.0.2.9 port 2222 ssh2\n", i%60, i)
	}
	paths := syslogng.ReadPathsFor("/mnt/logs/syslog-ng", "web01", now)
	f := &fakeAuthlogRunner{byPath: map[string]string{paths[0]: b.String()}}

	c := newAuthlogCollector([]syslogng.Server{authlogServer()}, []string{"web01"}, f, now)
	got := c.collectOnce(context.Background())

	if got.Produced != 1 {
		t.Errorf("collectOnce produced %d envelopes from a 40-username spray — want exactly 1 aggregate (TG-421), "+
			"so arming this source mints ONE triage session, not %d", got.Produced, got.Produced)
	}
	if got.Suppressed == 0 {
		t.Error("collectOnce reported Suppressed=0 on a 40-username spray — the fold must reach the poll result " +
			"(and the register), or a live enumeration attack is invisible")
	}
}
