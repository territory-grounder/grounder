// Package authlog is the host AUTHENTICATION ingest module — TG's second witness (TG-315).
//
// WHY THIS EXISTS. TG-169 shipped the correlation stage with two rules. The multi-host rule works today.
// The CROSS-SOURCE rule — two independent ingest sources across two hosts, which is what turns a set of
// symptoms into a compromise hypothesis — has never had a second side to fire on. Measured 2026-08-06, the
// all-time distinct source list in ingest_alert is:
//
//	librenms | pve-liveness | prometheus-alertmanager
//
// Three sources, all availability. CrowdSec is declared and has never delivered a single row (TG-291), so
// the security plane's only source is one that does not exist. worker-RCE -> node-escalation ->
// credential-harvest cannot be fused into one hypothesis — not because the correlator cannot fuse it, but
// because none of those stages produce an admitted alert at all.
//
// The estate already collects the evidence and nothing reads it. On the syslog-ng servers, in the last 36h:
//
//	sshd 6272 · pam_unix 5819 · "Accepted publickey" 987 · "session opened" 2943 · sudo 39
//
// WHAT IS ADMITTED, AND WHY MOST OF THAT IS NOT. Those numbers are the whole design problem. TG's ordinary
// intake is ~500 alerts/day; admitting one event per auth line would be ~9,000/day and would drown the
// correlator's signal, the precedent corpus, and — via the auto->APPROVE clamp — the human approval queue
// this repo has only just learned to measure (TG-173). An ingest source that floods the operator is a
// worse outcome than the blindness it replaces, because the queue it floods is the control everything else
// rests on.
//
// So routine success is NOT an event. A successful publickey login by a known account is the estate working
// and there are a thousand of them. What is admitted is the security-significant minority:
//
//   - authentication FAILURE (wrong password, invalid user, refused key)
//   - privilege ESCALATION (sudo, su) — 39 in 36h, a rate a human can read
//   - ROOT session opened over the network
//
// AND IT IS AGGREGATED, NOT STREAMED. A brute-force burst is a thousand failures and ONE fact. Admitting
// them individually would flood exactly the queues above while telling an operator nothing the first ten
// lines did not. Events are folded by (host, kind, principal) per poll with an occurrence count, so a
// burst arrives as one envelope carrying "247 failures" rather than 247 envelopes.
//
// NOTHING IN A LOG LINE BECOMES CONTROL FLOW (INV-08/INV-20). A syslog line is attacker-authorable — an
// SSH username is chosen by whoever is connecting — so every field is validated against an explicit
// grammar here and the free text rides as a summary that the input screen neutralises downstream, exactly
// as an alert narrative does.
package authlog

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"regexp"
	"sort"
	"strings"
	"time"

	adaptingest "github.com/territory-grounder/grounder/adapters/ingest"
	coreingest "github.com/territory-grounder/grounder/core/ingest"
)

// SourceType is the vendor slug this module serves. It is what the correlator's cross-source rule keys on,
// so it must be distinct from every availability source.
const SourceType = "authlog"

// Kind is the class of auth event. A closed set: a line that matches none of them is not admitted, because
// "some auth thing happened" is not evidence anyone can act on.
type Kind string

const (
	// KindFailure is a rejected authentication attempt.
	KindFailure Kind = "auth-failure"
	// KindEscalation is a privilege escalation (sudo, su).
	KindEscalation Kind = "privilege-escalation"
	// KindRootSession is a root session opened over the network.
	KindRootSession Kind = "root-session"
)

// Event is one folded auth observation: a (host, kind, principal) triple and how many times it occurred in
// the polled window.
type Event struct {
	Host string `json:"host"`
	Kind Kind   `json:"kind"`
	// Principal is the account the event concerns. ATTACKER-CHOSEN on a failure — an invalid-user attempt
	// carries whatever name the client sent — so it is grammar-validated, never trusted.
	Principal string `json:"principal,omitempty"`
	// SourceIP is the remote address where the line carried one. Empty is normal (a local sudo has none).
	SourceIP string `json:"source_ip,omitempty"`
	// Count is how many matching lines folded into this event. 1 is an event; 247 is a burst, and the
	// difference is the whole point of folding.
	Count int `json:"count"`
	// FirstSeen / LastSeen bound the fold. A burst's shape matters: 247 failures in 4 seconds and 247 over
	// six hours are different incidents.
	FirstSeen time.Time `json:"first_seen,omitempty"`
	LastSeen  time.Time `json:"last_seen,omitempty"`
	// DistinctPrincipals > 0 marks this an AGGREGATE enumeration-sweep event (TG-421): the enumeration cap
	// folded a (host,kind) that exceeded its ceiling into ONE envelope carrying the distinct-principal COUNT
	// (this field), the folded total (Count), and the loudest principal (Principal) as the named top offender —
	// so a targeted attack inside a spray is not masked. Zero on an ordinary per-principal event.
	DistinctPrincipals int `json:"distinct_principals,omitempty"`
}

// principalRe is the grammar an account name must satisfy to be carried as a principal.
//
// An SSH username is chosen by the client, so an invalid-user attempt can carry a newline, a shell
// metacharacter, or a whole injection paragraph. A name that does not match is REPLACED by a marker rather
// than dropped: the event is still real and still counts, and losing it would let an attacker suppress
// their own alert by choosing an unparseable username.
var principalRe = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)

// maxPrincipalLen bounds the name before the grammar even runs. Unbounded, a single log line could mint an
// arbitrarily large metric label and a corpus row — the same amplification the egress meter caps
// destinations for.
const maxPrincipalLen = 64

// unparseablePrincipal is what an ungrammatical account name becomes. Deliberately NOT the empty string:
// empty would fold every malformed attempt in with every event that legitimately has no principal, and a
// brute-force run using random binary usernames would then hide inside the local-sudo bucket.
const unparseablePrincipal = "unparseable"

// DefaultFoldWindow is how much time one external_ref covers. See foldBucket for why a window exists at
// all. An hour is chosen to be far longer than any sane collector poll interval — so a re-read of the same
// trailing lines lands in the same bucket and is idempotent — while still being short enough that a
// long-running brute force is not compressed into a single incident for a whole day.
const DefaultFoldWindow = time.Hour

// Module is the auth-log ingest adapter. Construct with New.
type Module struct {
	now        func() time.Time
	foldWindow time.Duration
}

// Option configures a Module.
type Option func(*Module)

// WithClock overrides the wall clock so normalisation is deterministic under test.
func WithClock(now func() time.Time) Option { return func(m *Module) { m.now = now } }

// WithFoldWindow overrides how much time one external_ref covers. A non-positive value is ignored: a zero
// window would truncate every event to the same instant and reinstate the exact defect foldBucket exists
// to close.
func WithFoldWindow(d time.Duration) Option {
	return func(m *Module) {
		if d > 0 {
			m.foldWindow = d
		}
	}
}

// New builds an auth-log ingest module.
func New(opts ...Option) *Module {
	m := &Module{now: time.Now, foldWindow: DefaultFoldWindow}
	for _, o := range opts {
		o(m)
	}
	return m
}

// foldBucket is the time component of the external_ref, and it exists because WITHOUT IT THIS SOURCE CAN
// ADMIT EACH (host, kind, principal) EXACTLY ONCE, EVER.
//
// The ref used to fold on (host, kind, principal) alone, under a comment claiming that "a burst that grows
// between polls updates one incident". The storage layer does not update: core/db/alert_log.go inserts
// `ON CONFLICT (external_ref) DO NOTHING`, deliberately, so the FIRST acceptance is canonical and a
// retrying source cannot accumulate duplicates. Correct for its purpose — and it means the second
// authentication-failure burst on a host, and every one after it for all time, was destined to be silently
// dropped. The source would have looked healthy: rows present, gauge delivered, nothing erroring.
//
// Bucketing the fold's own start time closes that without adding a cursor or any new persistence: a
// collector re-reading the same trailing lines derives the same bucket (idempotent within a window), and
// the next window derives a different one (admissible across windows).
//
// The fallback order is deliberate. FirstSeen is the fold's start; a single-line event carries only
// LastSeen (parse.go sets LastSeen per line and FirstSeen is filled when lines fold); a line whose RFC3164
// stamp did not parse carries neither, and there the INGEST clock is used rather than refusing — an event
// with no readable timestamp is still a real security observation, and bucketing it by arrival keeps it
// separable across windows instead of folding it into one immortal ref.
func (m *Module) foldBucket(e Event) string {
	t := e.FirstSeen
	if t.IsZero() {
		t = e.LastSeen
	}
	if t.IsZero() {
		t = m.now()
	}
	w := m.foldWindow
	if w <= 0 {
		w = DefaultFoldWindow
	}
	return t.UTC().Truncate(w).Format("20060102T150405Z")
}

// SourceType implements adapters/ingest.Ingester.
func (m *Module) SourceType() string { return SourceType }

var _ adaptingest.Ingester = (*Module)(nil)

// Normalize implements adapters/ingest.Ingester: the collector posts one FOLDED event as JSON through the
// same front door every other source uses, so auth telemetry traverses the identical dedup -> flap -> burst
// admission path before any model dispatch (INV-20). Nothing here becomes control flow.
//
// DisallowUnknownFields is deliberate. This payload is produced by TG's own collector, so an unexpected
// field means the two halves have drifted — and silently ignoring it is how a collector starts sending a
// field the parser never reads.
func (m *Module) Normalize(_ context.Context, raw []byte) (coreingest.IncidentEnvelope, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var e Event
	if err := dec.Decode(&e); err != nil {
		return coreingest.IncidentEnvelope{}, fmt.Errorf("authlog: malformed event payload: %w", err)
	}
	return m.ToEnvelope(e)
}

// sanitizePrincipal applies the length bound and the grammar. Exported behaviour is deliberate and tested:
// a hostile name never reaches a label, an ExternalRef, or a metric.
func sanitizePrincipal(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	if len(s) > maxPrincipalLen || !principalRe.MatchString(s) {
		return unparseablePrincipal
	}
	return s
}

// severityFor grades a folded event.
//
// A SINGLE failure is not an incident — people mistype passwords, and a source that pages on that teaches
// an operator to mute it. A burst is. Escalation and root sessions are graded on what they are rather than
// how often: one unexpected root session is the thing you want to hear about, and there are only tens of
// sudo events a day on this estate, so they cannot flood anything.
func severityFor(e Event) string {
	switch e.Kind {
	case KindFailure:
		if e.Count >= burstThreshold {
			return "critical"
		}
		if e.Count > 1 {
			return "warning"
		}
		return "info"
	case KindRootSession:
		return "warning"
	case KindEscalation:
		return "info"
	}
	return "info"
}

// burstThreshold is where repeated failures stop being noise and become an attack shape.
//
// Ten is chosen against the measured estate rather than from a textbook: over 36 hours these hosts logged
// ZERO authentication failures at all, so any double-digit run is far outside normal and there is no
// legitimate traffic for it to false-positive against. If the estate's baseline ever becomes non-zero this
// number has to be re-derived from it — a threshold set against an assumed baseline is the defect REQ-012
// exists to prevent.
const burstThreshold = 10

// ToEnvelope maps one folded auth event to the canonical triage envelope.
//
// The category is set STRUCTURALLY, not by matching content: every event this module emits is an
// authentication failure, an escalation, or a root session, and there is no member of that set which is not
// a security signal. That label is what makes core/risk force a POLL_PAUSE on a containment action
// (classifier.go), so the same reasoning crowdsec.go records applies here — and for the same reason it is
// deliberately NOT done for the availability sources, where the category would be a guess wearing a safety
// control's clothes.
func (m *Module) ToEnvelope(e Event) (coreingest.IncidentEnvelope, error) {
	if strings.TrimSpace(e.Host) == "" {
		return coreingest.IncidentEnvelope{}, fmt.Errorf("authlog: event missing host")
	}
	switch e.Kind {
	case KindFailure, KindEscalation, KindRootSession:
	default:
		return coreingest.IncidentEnvelope{}, fmt.Errorf("authlog: unknown kind %q — the admitted set is "+
			"closed, and 'some auth thing happened' is not evidence anyone can act on", e.Kind)
	}
	if e.Count < 1 {
		return coreingest.IncidentEnvelope{}, fmt.Errorf("authlog: event has count %d — an observation "+
			"that never occurred must not be admitted", e.Count)
	}

	principal := sanitizePrincipal(e.Principal)

	ip := ""
	if e.SourceIP != "" && net.ParseIP(e.SourceIP) != nil {
		ip = e.SourceIP
	}

	// The ref folds by (host, kind, principal) and NOT by count, so a burst that grows WITHIN one window is
	// one incident rather than a new one every poll — that is what lets the dedup and flap stages upstream
	// see it as one ongoing thing. It is additionally bucketed by time (foldBucket), because without a time
	// component the append-only store's ON CONFLICT DO NOTHING admits each triple exactly ONCE FOR ALL TIME.
	ref := SourceType + "-" + slugify(e.Host) + "-" + slugify(string(e.Kind))
	if e.DistinctPrincipals > 0 {
		// AGGREGATE SWEEP (TG-421): fold by (host, kind, window) + a fixed marker, NEVER by principal. A later
		// poll of the same window sees a DIFFERENT subset of the sprayed names — and a different loudest — so
		// keying on any principal would re-mint the incident every poll. The marker keeps it ONE ongoing sweep.
		ref += "-enumeration-sweep"
	} else if principal != "" {
		ref += "-" + slugify(principal)
	}
	ref += "-" + m.foldBucket(e)

	raw := coreingest.NewRawEvent(SourceType, nil)
	raw.ExternalRef = ref
	raw.AlertRule = string(e.Kind)
	raw.Severity = severityFor(e)
	raw.Host = e.Host
	raw.IP = ip
	raw.Summary = summarize(e, principal)
	raw.ObservedAt = e.LastSeen
	raw.Labels = map[string]string{"category": "security-incident"}
	return coreingest.Normalize(raw, m.now())
}

// summarize writes what a human reads first. The COUNT and the WINDOW lead, because "247 failures in 4
// seconds" and "2 failures in six hours" are different incidents and the rule that graded them cannot say
// which one this is.
func summarize(e Event, principal string) string {
	if e.DistinctPrincipals > 0 {
		return summarizeSweep(e, principal)
	}
	who := principal
	if who == "" {
		who = "(no account recorded)"
	}
	var b strings.Builder
	switch e.Kind {
	case KindFailure:
		fmt.Fprintf(&b, "%d authentication failure(s) for %q on %s", e.Count, who, e.Host)
	case KindEscalation:
		fmt.Fprintf(&b, "%d privilege escalation(s) by %q on %s", e.Count, who, e.Host)
	case KindRootSession:
		fmt.Fprintf(&b, "%d root session(s) opened on %s", e.Count, e.Host)
	}
	if e.SourceIP != "" {
		fmt.Fprintf(&b, " from %s", e.SourceIP)
	}
	if !e.FirstSeen.IsZero() && !e.LastSeen.IsZero() {
		if d := e.LastSeen.Sub(e.FirstSeen); d > 0 {
			fmt.Fprintf(&b, " over %s", d.Round(time.Second))
		}
	}
	return b.String()
}

// summarizeSweep renders the AGGREGATE enumeration-sweep incident (TG-421). The distinct-principal count and
// the folded total lead — that is the sweep's shape, and a "N-principal spray" is a higher-signal incident
// than N separate 'one failure for user X' rows. The LOUDEST principal is NAMED so a targeted attack hidden
// inside the spray (a 900-failure account among one-off probes) is not masked by the aggregation — the exact
// signal the pre-aggregate cap kept by admitting the loudest individuals.
func summarizeSweep(e Event, principal string) string {
	verb := "authentication failures"
	switch e.Kind {
	case KindEscalation:
		verb = "privilege escalations"
	case KindRootSession:
		verb = "root sessions"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "user-enumeration sweep: %d distinct principals, %d total %s against %s",
		e.DistinctPrincipals, e.Count, verb, e.Host)
	if principal != "" {
		fmt.Fprintf(&b, " (loudest: %q)", principal)
	}
	if !e.FirstSeen.IsZero() && !e.LastSeen.IsZero() {
		if d := e.LastSeen.Sub(e.FirstSeen); d > 0 {
			fmt.Fprintf(&b, " over %s", d.Round(time.Second))
		}
	}
	return b.String()
}

// slugify reduces a value to the safe identifier grammar used for refs and rule names.
func slugify(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	var b strings.Builder
	lastDash := false
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
			lastDash = false
		default:
			if !lastDash && b.Len() > 0 {
				b.WriteByte('-')
				lastDash = true
			}
		}
	}
	return strings.Trim(b.String(), "-")
}

// Fold collapses raw parsed observations into per-(host, kind, principal) events with counts.
//
// This is the flood control, and it runs BEFORE anything is admitted rather than as a downstream filter: a
// thousand failures are one fact, and the upstream dedup/flap stages should be handed one fact.
// Deterministically ordered so a poll's output is diff-stable and testable.
func Fold(obs []Event) []Event {
	byKey := map[string]*Event{}
	var order []string
	for _, o := range obs {
		p := sanitizePrincipal(o.Principal)
		key := o.Host + "\x00" + string(o.Kind) + "\x00" + p
		cur, ok := byKey[key]
		if !ok {
			cp := o
			cp.Principal = p
			if cp.Count < 1 {
				cp.Count = 1
			}
			cp.FirstSeen, cp.LastSeen = o.LastSeen, o.LastSeen
			if !o.FirstSeen.IsZero() {
				cp.FirstSeen = o.FirstSeen
			}
			byKey[key] = &cp
			order = append(order, key)
			continue
		}
		n := o.Count
		if n < 1 {
			n = 1
		}
		cur.Count += n
		// The IP is kept from the FIRST observation that had one, and a second differing address does not
		// overwrite it — a distributed burst is not one attacker's address, and silently reporting the last
		// one seen would name an arbitrary member of the set as though it were the source.
		if cur.SourceIP == "" {
			cur.SourceIP = o.SourceIP
		}
		if !o.LastSeen.IsZero() && (cur.LastSeen.IsZero() || o.LastSeen.After(cur.LastSeen)) {
			cur.LastSeen = o.LastSeen
		}
		if !o.LastSeen.IsZero() && (cur.FirstSeen.IsZero() || o.LastSeen.Before(cur.FirstSeen)) {
			cur.FirstSeen = o.LastSeen
		}
	}
	sort.Strings(order)
	out := make([]Event, 0, len(order))
	for _, k := range order {
		out = append(out, *byKey[k])
	}
	return out
}
