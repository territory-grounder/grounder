// Package incidenthistory gives the triage agent a READ-ONLY view of TG's OWN prior sessions on the
// alerting host — the predecessor's single biggest correct_diagnosis lever, unported until now: it
// RECOGNIZED recurring incidents ("this host has filled its disk five times; every time the journal was
// the consumer; twice a human approved a prune") while TG re-derives every recurrence from scratch as if
// the estate had no past.
//
// The tool answers "has TG seen this host fail this way before, and how did that end?" from the durable
// session_triage record: per prior incident — when, which rule, how the session ended (proposed / stood
// down / escalated), the op-class if one was proposed, whether the condition was later CONFIRMED clear,
// and the session's own conclusion. Same-condition matching is by rule FAMILY through the ONE family
// authority (core/knowledge.CanonicalRule — the same map the novelty gate, the verdict's REQ-108 sibling
// check and the recovery belt key on), so a recurrence under another source's spelling of the same fault
// still matches, and a genuinely different rule never does.
//
// It is a TOOL, not pre-loaded context, for the same reason get-actor-evidence is (INV-11): the agent
// spends a cycle and genuinely gathers the observation, so citing it is honest. Everything it returns is
// an OBSERVATION about prior sessions (INV-08 — prior conclusions are prior model text, rendered quoted
// and inert, never instructions): history suggests where to look; it never proves the current fault has
// the same cause, and the tool says so.
//
// Provenance: [F] the predecessor triage-researcher's incident-history recall, re-expressed over TG's own
// durable judge spine · [O] INV-08 (returned text is data) · INV-11 (evidence is gathered, not granted).
package incidenthistory

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/territory-grounder/grounder/agent"
	"github.com/territory-grounder/grounder/core/knowledge"
)

const (
	// fetchBound is how many of the host's newest sessions are read before the family fold. It bounds the
	// DB read AND the honesty of the aggregate line: when a host has more history than this, the counts
	// are disclosed as "over the most recent N sessions" rather than presented as all-time.
	fetchBound = 200
	// showCap bounds how many prior incidents are rendered (newest first). Recognition needs the recent
	// pattern, not the full archive — and an unbounded dump of prior conclusions both crowds the context
	// and hands prior-session text more room than an observation deserves.
	showCap = 8
	// conclusionCap bounds each rendered conclusion. The first ~200 chars carry the diagnosis ("journal
	// was the consumer, stood down: loopback rootfs"); the rest is prose the agent can re-derive live.
	conclusionCap = 200
)

// PriorIncident is one prior triage session on the host, as the durable record captured it. It is
// restated here as a narrow local type (mirroring the actor-evidence tool's Reader seam) so this package
// imports no DB code and the worker adapts db.IncidentHistoryStore rows into it — the formatting logic
// stays unit-testable against a fake.
type PriorIncident struct {
	ExternalRef    string    // the incident's correlation ref
	Rule           string    // the alert rule that fired (family-folded via knowledge.CanonicalRule)
	Outcome        string    // the orchestrator's terminal outcome string (proposed / stood down / escalated…)
	OpClass        string    // the canonical op-class proposed, "" for a no-proposal stop
	Proposed       bool      // did the session propose an action?
	Mutated        bool      // did TG actually actuate a mutation?
	ConfirmedClear bool      // was the condition re-observed CLEAR afterwards (the fail-closed heal signal)?
	Conclusion     string    // the session's own conclusion (prior model text — rendered quoted, inert)
	At             time.Time // when the session was recorded
}

// Reader is the read-only prior-session seam: the newest sessions recorded for a host, newest first,
// bounded by limit. The worker passes an adapter over db.IncidentHistoryStore.PriorSessions.
type Reader func(ctx context.Context, host string, limit int) ([]PriorIncident, error)

// New returns the incident-history tool bound to a live reader. A nil reader yields NO tool (an inert
// surface that always answers "no history" would teach the agent to stop asking — worse than absent).
func New(read Reader) []agent.Tool {
	if read == nil {
		return nil
	}
	return []agent.Tool{historyTool{read: read, now: time.Now}}
}

type historyTool struct {
	read Reader
	now  func() time.Time // clock seam so the relative-age rendering is deterministic under test
}

func (historyTool) Name() string   { return "get-incident-history" }
func (historyTool) ReadOnly() bool { return true }

// Description and Params render into the tool preamble as DATA (agent.ACITool) — what the tool answers,
// and deliberately what it does not.
func (t historyTool) Description() string {
	return "Read TG's own record of PRIOR incidents on a host: when it alerted before, under which rule, how " +
		"each session ended (proposed / stood down / escalated), whether a proposed fix was later confirmed to " +
		"clear, and each session's conclusion. Use it to RECOGNIZE a recurring incident before re-deriving it " +
		"from scratch — a fault this host has had five times, with the same consumer named every time, is a " +
		"different investigation from a first occurrence. Observations about past sessions only: history never " +
		"proves the current fault has the same cause — confirm against the live host before proposing."
}

func (t historyTool) Params() []agent.ParamSpec {
	return []agent.ParamSpec{{
		Name: "host", Type: "string", Required: true, Example: "host01",
		Description: "the alerting host whose prior incidents to read (target/device/hostname also accepted)",
	}, {
		Name: "rule", Type: "string", Required: false, Example: "Devices-up/down",
		Description: "optional alert rule: scope the history to this rule's FAMILY (same condition under any " +
			"source's spelling); omit for all prior incidents on the host",
	}}
}

func (t historyTool) Invoke(ctx context.Context, args map[string]string) (agent.ToolResult, error) {
	host := firstNonEmpty(args["host"], args["target"], args["device"], args["hostname"])
	rule := firstNonEmpty(args["rule"], args["alert_rule"], args["alert"])
	res := agent.ToolResult{ID: "incident-history-" + sanitizeID(host), Tool: t.Name()}
	if host == "" {
		res.Output = `no host given — call with {"host": "<name>"}`
		return res, nil
	}

	all, err := t.read(ctx, host, fetchBound)
	if err != nil {
		// Honest gap, exactly as the actor-evidence tool treats a reader failure: an unreadable history is
		// UNKNOWN, not "no prior incidents" — conflating the two turns a DB blip into a false novelty claim.
		res.Output = fmt.Sprintf("the incident history for %q could NOT be read (%v) — treat it as UNKNOWN, "+
			"not as evidence this fault is new; investigate the live host as usual", host, err)
		return res, nil
	}

	// Fold to the requested rule FAMILY through the one family authority (core/knowledge.CanonicalRule):
	// the same physical fault surfaces under several source rule spellings, and matching the raw string
	// would hide exactly the recurrences recognition exists for. No rule ⇒ the host's full history.
	scope := fmt.Sprintf("host %q (all rules)", host)
	matched := all
	if rule != "" {
		fam := knowledge.CanonicalRule(rule)
		scope = fmt.Sprintf("host %q + rule family of %q", host, rule)
		matched = nil
		for _, p := range all {
			if knowledge.CanonicalRule(p.Rule) == fam {
				matched = append(matched, p)
			}
		}
	}

	if len(matched) == 0 {
		res.Success = true
		res.Output = fmt.Sprintf("no prior incident recorded for %s. TG's history begins at its own "+
			"deployment, so absence here means TG has not handled this before — it does not prove the fault "+
			"never happened. Investigate as a first occurrence.", scope)
		return res, nil
	}

	// The one-line aggregate first — the recognition signal the agent must not skim past. An "auto-heal"
	// is the fail-closed pairing: TG actuated AND the condition was re-observed clear (never asserted).
	heals := 0
	for _, p := range matched {
		if p.Mutated && p.ConfirmedClear {
			heals++
		}
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "incident history for %s: %d prior incident(s) / %d confirmed auto-heal(s) / last %s ago",
		scope, len(matched), heals, ago(t.now().Sub(matched[0].At)))
	if len(all) == fetchBound {
		// The fetch window was full, so older history exists beyond it — a silently-capped count reads as
		// all-time when it is not.
		fmt.Fprintf(&sb, " (counted over the host's most recent %d sessions — older history exists)", fetchBound)
	}

	shown := matched
	if len(shown) > showCap {
		shown = shown[:showCap]
		fmt.Fprintf(&sb, "\nshowing the %d most recent of %d:", showCap, len(matched))
	}
	for _, p := range shown {
		// EVERY free-text field here is a PRIOR session's text (rule labels from ingest, outcome strings,
		// the prior model's own conclusion). %q keeps a hostile or malformed value visibly inert as data
		// (INV-08) — a prior conclusion must never be able to forge structure in this prompt.
		fmt.Fprintf(&sb, "\n  - %s (%s ago): rule=%q outcome=%q",
			p.At.UTC().Format("2006-01-02"), ago(t.now().Sub(p.At)), p.Rule, p.Outcome)
		if p.OpClass != "" {
			fmt.Fprintf(&sb, " op_class=%s", p.OpClass)
		}
		fmt.Fprintf(&sb, " confirmed_clear=%t", p.ConfirmedClear)
		if c := strings.TrimSpace(p.Conclusion); c != "" {
			fmt.Fprintf(&sb, "\n    conclusion: %q", truncate(c, conclusionCap))
		}
	}
	sb.WriteString("\n(observations about PRIOR sessions — recognition, not proof. A past cause suggests where " +
		"to look first; confirm it on the live host before proposing, and cite this id in evidence_ids if you " +
		"rely on it.)")
	res.Success = true
	res.Output = sb.String()
	return res, nil
}

// truncate bounds a prior conclusion to cap runes with a visible ellipsis — never a silent cut that reads
// as the whole text.
func truncate(s string, cap int) string {
	r := []rune(s)
	if len(r) <= cap {
		return s
	}
	return string(r[:cap]) + "…"
}

// ago renders a duration as a compact human age ("3d", "5h", "12m"); sub-minute (and any clock-skewed
// negative) renders as "<1m" rather than a nonsense negative.
func ago(d time.Duration) string {
	switch {
	case d < time.Minute:
		return "<1m"
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	}
}

func firstNonEmpty(ss ...string) string {
	for _, s := range ss {
		if strings.TrimSpace(s) != "" {
			return strings.TrimSpace(s)
		}
	}
	return ""
}

// sanitizeID keeps the observation id printable and stable for the citation gate (mirrors the estate
// tool's convention: lowercased, unexpected runes collapsed to '-').
func sanitizeID(h string) string {
	h = strings.ToLower(strings.TrimSpace(h))
	var b strings.Builder
	for _, r := range h {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '.', r == '-', r == '_':
			b.WriteRune(r)
		default:
			b.WriteRune('-')
		}
	}
	if b.Len() == 0 {
		return "unnamed"
	}
	return b.String()
}
