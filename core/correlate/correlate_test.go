package correlate

import (
	"testing"
	"time"
)

var base = time.Date(2026, 8, 4, 12, 0, 0, 0, time.UTC)

func obs(ref, source, host string, sev string, offset time.Duration) Observation {
	return Observation{ExternalRef: ref, SourceType: source, Host: host, AlertRule: "r", Severity: sev, At: base.Add(offset)}
}

// THE DEFECT, BOTH DIRECTIONS (TG-169).
//
// `Correlated` — the flag that decides whether an incident is treated as multi-system — was
// `env.Severity == critical`. This is the oracle for what replaced it, and it asserts the two errors that
// heuristic made, because fixing only one of them would be a different bug wearing the same commit
// message.
//
// KILLING MUTATION (executed): re-point Assess at severity — `v.Correlated = subject.Severity ==
// "critical"` — i.e. restore the line the ticket is about. RED on both halves with the real-world
// consequence named: the three-warning-host cascade is declared isolated and routed to the CHEAPEST
// reasoning available (the HuggingFace-shaped signal: many weak signals, no single critical), and the lone
// critical claims to span multiple systems, which is 81% of TG's live admitted alerts asserting a
// multi-system incident on evidence that does not exist.
func TestCorrelationIsAboutBreadthNotSeverity(t *testing.T) {
	// FALSE NEGATIVE — the expensive one. Three DIFFERENT hosts, all merely "warning", inside one window.
	subject := obs("a1", "librenms", "web01", "warning", 0)
	cascade := Window{Span: 10 * time.Minute, Observations: []Observation{
		obs("a2", "librenms", "web02", "warning", 90*time.Second),
		obs("a3", "librenms", "db01", "warning", 3*time.Minute),
	}}
	v := Assess(subject, cascade)
	if !v.Correlated {
		t.Fatalf("a three-host WARNING cascade was declared isolated (reason=%q hosts=%v) — this is the "+
			"multi-host signal a real compromise makes, and calling it isolated routes it to the cheapest "+
			"reasoning TG has", v.Reason, v.Hosts)
	}
	if v.Reason != ReasonMultiHost {
		t.Fatalf("reason = %q, want %q — the persisted routing record must say WHICH rule fired", v.Reason, ReasonMultiHost)
	}
	if len(v.Hosts) != 3 || v.MemberCount != 3 {
		t.Fatalf("evidence wrong: hosts=%v members=%d, want 3 distinct hosts and 3 members", v.Hosts, v.MemberCount)
	}

	// FALSE POSITIVE — the honesty one. ONE host, CRITICAL, nothing else in the window.
	lone := obs("b1", "librenms", "web01", "critical", 0)
	v = Assess(lone, Window{Span: 10 * time.Minute})
	if v.Correlated {
		t.Fatalf("a LONE critical alert claims to span multiple systems (reason=%q) — 2,434 of 2,995 live "+
			"admitted alerts are critical, so this is 81%% of incidents asserting a cascade TG never saw",
			v.Reason)
	}
	if v.Reason != ReasonIsolated {
		t.Fatalf("reason = %q, want %q", v.Reason, ReasonIsolated)
	}
}

// One noisy host cannot manufacture a cascade. Volume is not breadth: five alerts from one host is one
// system in trouble, and a rule that counted ALERTS instead of HOSTS would send every flapping guest down
// the deep path — the same "everything is correlated" state under a new name.
//
// KILLING MUTATION (executed): count members instead of distinct hosts
// (`len(members) >= MinClusterHosts`). RED — the flapping single host is declared a multi-host cascade.
func TestOneChattyHostIsNotACascade(t *testing.T) {
	subject := obs("c1", "librenms", "web01", "warning", 0)
	noisy := Window{Span: 10 * time.Minute, Observations: []Observation{
		obs("c2", "librenms", "web01", "warning", time.Minute),
		obs("c3", "librenms", "web01", "critical", 2*time.Minute),
		obs("c4", "librenms", "web01", "warning", 3*time.Minute),
		obs("c5", "librenms", "web01", "warning", 4*time.Minute),
	}}
	if v := Assess(subject, noisy); v.Correlated {
		t.Fatalf("five alerts from ONE host were called a cascade (reason=%q hosts=%v) — a single flapping "+
			"guest would then take the deep path forever", v.Reason, v.Hosts)
	}
}

// The CROSS-SOURCE rule: two independent observers seeing trouble on two hosts clears at a lower host
// count than one poller seeing three. This is the rule the host/security telemetry connectors (EDR, auth
// logs, Falco, osquery — a separate ticket) land into: their value is being a SECOND witness.
//
// KILLING MUTATION (executed): drop the cross-source branch. RED — two hosts seen by two independent
// sources fall back to the three-host bar and are declared isolated, so the exact fusion the correlator
// exists for (availability plane + security plane agreeing) buys nothing.
func TestTwoIndependentSourcesOnTwoHostsCorrelate(t *testing.T) {
	subject := obs("d1", "librenms", "web01", "warning", 0)
	crossed := Window{Span: 10 * time.Minute, Observations: []Observation{
		obs("d2", "crowdsec", "db01", "warning", 30*time.Second),
	}}
	v := Assess(subject, crossed)
	if !v.Correlated {
		t.Fatalf("two independent sources on two hosts were declared isolated (sources=%v hosts=%v)", v.Sources, v.Hosts)
	}
	if v.Reason != ReasonCrossSource {
		t.Fatalf("reason = %q, want %q", v.Reason, ReasonCrossSource)
	}

	// ...and TWO SOURCES ON ONE HOST is still one system in trouble, seen twice. Without the host bar the
	// cross-source rule would fire on every alert an estate reports through two collectors.
	sameHost := Window{Span: 10 * time.Minute, Observations: []Observation{
		obs("d3", "crowdsec", "web01", "warning", 30*time.Second),
	}}
	if v := Assess(subject, sameHost); v.Correlated {
		t.Fatalf("two sources reporting the SAME host were called cross-system (hosts=%v)", v.Hosts)
	}
}

// The window is a real bound, and the subject is not double-counted. An observation outside the span
// contributes nothing even if the reader hands it over, so the pure rule and the SQL cannot silently
// disagree about what "inside the window" meant; and the front door has ALREADY written the subject's own
// ingest_alert row, so the subject normally arrives twice.
//
// KILLING MUTATION (executed): delete the span check in admit(). RED — an alert from an hour earlier joins
// the cluster and a quiet estate is retroactively declared a cascade by anything that happened that day.
func TestWindowBoundsAndSubjectDedup(t *testing.T) {
	subject := obs("e1", "librenms", "web01", "warning", 0)
	w := Window{Span: 5 * time.Minute, Observations: []Observation{
		subject, // the front door's own row for this very alert
		obs("e2", "librenms", "web02", "warning", time.Minute),
		obs("e3", "librenms", "db01", "warning", 61*time.Minute), // an hour later: not this incident
	}}
	v := Assess(subject, w)
	if v.MemberCount != 2 {
		t.Fatalf("members = %d (%v), want 2 — the subject must count once and the out-of-window alert not at all",
			v.MemberCount, v.Members)
	}
	if v.Correlated {
		t.Fatalf("two in-window hosts from one source were called a cascade (hosts=%v)", v.Hosts)
	}
}

// "TG could not look" must never be recorded as "TG looked and saw one system". A degraded verdict carries
// the caller's fallback answer and says so, so a broken reader is visible in the record instead of looking
// like a quiet estate.
//
// KILLING MUTATION (executed): return Verdict{Correlated: correlated} without Degraded/Reason. RED — an
// unreadable window is indistinguishable from an isolated incident on the persisted routing record, which
// is how a dead correlation stage ships unnoticed.
func TestUnavailableIsDistinguishableFromIsolated(t *testing.T) {
	u := Unavailable(true)
	if !u.Degraded || u.Reason != ReasonUnavailable {
		t.Fatalf("degraded verdict = %+v, want Degraded with reason %q", u, ReasonUnavailable)
	}
	if !u.Correlated {
		t.Fatal("the caller's fallback answer was dropped — a degraded stage must not silently route DOWN")
	}
	if Assess(obs("f1", "librenms", "web01", "critical", 0), Window{Span: time.Minute}).Reason == u.Reason {
		t.Fatal("an isolated verdict and an unreadable window carry the SAME reason — a reviewer cannot " +
			"tell a quiet estate from a dead reader")
	}
}

// The member list is capped so a wide cascade cannot write an unbounded blob into the append-only
// decision table, and MemberCount stays honest about how many there really were.
//
// KILLING MUTATION (executed): drop the MaxMembers truncation. RED — a 200-host event persists 200 refs
// on a row that can never be deleted.
func TestMemberListIsBoundedButCountIsHonest(t *testing.T) {
	subject := obs("g0", "librenms", "host0", "warning", 0)
	var many []Observation
	for i := 1; i <= MaxMembers*2; i++ {
		many = append(many, Observation{
			ExternalRef: string(rune('A'+i%26)) + time.Duration(i).String(),
			SourceType:  "librenms", Host: "host" + time.Duration(i).String(), At: base.Add(time.Second),
		})
	}
	v := Assess(subject, Window{Span: time.Minute, Observations: many})
	if len(v.Members) != MaxMembers {
		t.Fatalf("members carried = %d, want the %d cap", len(v.Members), MaxMembers)
	}
	if v.MemberCount <= MaxMembers {
		t.Fatalf("MemberCount = %d — the truncated list must not shrink the reported cluster size", v.MemberCount)
	}
}
