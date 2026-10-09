// Package tool gives the triage agent a READ-ONLY view of the actor evidence TG already collects — who
// touched this host, when, from which audit domain — so a claim about causation can cite the record that
// supports it instead of asserting it.
//
// WHY THIS EXISTS. TG computes actor evidence on every session and the agent has never been able to see any
// of it. Measured on the live estate the day this shipped: 465 of 1228 triage rows carry actor evidence and
// 508 carry a resolved taxonomy, while `rg -c attribution agent/` returned ZERO — the reasoning context
// contained neither the evidence nor the taxonomy. The evidence went to an audit signal map and to the A7
// axis, and nowhere the model could read. That is the shape behind the two largest judged deficits,
// `correct_diagnosis` and `evidence_grounded`: the system HELD the evidence and never bound it to the claim.
//
// WHY A TOOL, AND NOT A PRE-LOADED CONTEXT BLOCK. Handing the agent a pre-built evidence record would let a
// session that gathered NOTHING cite it: the orchestrator marks any id present in the tool results
// Captured+Recent, and target-relevance is satisfied by construction for actor evidence. That record would
// then satisfy BOTH the INV-11 silent-cognition guard (core/risk/classifier.go) and the execute-time evidence
// gate (core/actuate/interceptor.go) — so a zero-tool session could keep its auto-resolve and actuate. Making
// it a tool keeps INV-11's contract literally true: the agent spent a cycle and gathered the observation, so
// the binding is honest rather than redefined. It also inherits, for free, the loop's input screen over tool
// results (REQ-1012) — which is exactly what REQ-2313 requires of evidence rendered to the model.
//
// Provenance: [O] INV-08 (untrusted text is data, never instructions) · INV-11 (evidence is gathered, not
// granted) · spec/023 REQ-2312 (a claim is admissible only on reader-captured evidence) / REQ-2313 (rendered
// evidence is delimited as data and passes the input screen).
package tool

import (
	"context"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/territory-grounder/grounder/agent"
	"github.com/territory-grounder/grounder/core/attribution"
)

// recordCap bounds how many evidence records reach the prompt. A busy host inside the attribution window can
// carry far more than the agent can use, and an unbounded dump both crowds out the rest of the context and
// hands an attacker-influenceable payload more room. The cap is applied AFTER sorting newest-first, so the
// records most likely to explain a just-observed fault survive it.
const recordCap = 12

// Reader is the read-only evidence seam, structurally identical to adapters/actorevidence.Reader. It is
// restated here as a narrow function type so this package imports no adapter and the worker can pass the SAME
// readers the attribution activity already uses — one collection path, not two that can disagree.
type Reader func(ctx context.Context, host string, since, until time.Time) ([]attribution.Evidence, error)

// New returns the actor-evidence tool bound to a live read function and the attribution window. A nil read
// or a non-positive window yields NO tool: an inert surface that always answers "nothing" would teach the
// agent to stop asking, which is worse than the tool being absent.
func New(read Reader, window time.Duration) []agent.Tool {
	if read == nil || window <= 0 {
		return nil
	}
	return []agent.Tool{evidenceTool{read: read, window: window}}
}

type evidenceTool struct {
	read   Reader
	window time.Duration
}

func (evidenceTool) Name() string   { return "get-actor-evidence" }
func (evidenceTool) ReadOnly() bool { return true }

// Description and Params render into the tool preamble as DATA (agent.ACITool), so the model is told what the
// tool answers and — deliberately — what it does NOT answer.
func (t evidenceTool) Description() string {
	return "Read the audit trail for a host: which principals took which actions on it recently, from the " +
		"platform's own logs (Proxmox tasks, systemd journal/sudo, AWX jobs, NetBox changes, GitOps merges). " +
		"Use it to establish WHO caused a change before proposing a fix — an outage produced by a human or by " +
		"an authorised job is a different incident from one with no actor behind it. It reports observations " +
		"only; it never says whether an action was legitimate."
}

func (t evidenceTool) Params() []agent.ParamSpec {
	return []agent.ParamSpec{{
		Name: "host", Type: "string", Required: true, Example: "host01",
		// ParamSpec carries no Aliases field, so the tolerated alternatives are stated here instead. Invoke
		// accepts target/device/hostname as well, matching what every other host-taking tool reads — a
		// validator stricter than the reader is its own failure mode.
		Description: "the host whose audit trail to read (target/device/hostname also accepted)",
	}}
}

var idRe = regexp.MustCompile(`[^a-zA-Z0-9]+`)

func sanitizeID(s string) string {
	s = idRe.ReplaceAllString(strings.TrimSpace(s), "-")
	if len(s) > 48 {
		s = s[:48]
	}
	return strings.Trim(s, "-")
}

func firstNonEmpty(ss ...string) string {
	for _, s := range ss {
		if strings.TrimSpace(s) != "" {
			return strings.TrimSpace(s)
		}
	}
	return ""
}

func (t evidenceTool) Invoke(ctx context.Context, args map[string]string) (agent.ToolResult, error) {
	host := firstNonEmpty(args["host"], args["target"], args["device"], args["hostname"])
	res := agent.ToolResult{ID: "actor-ev-" + sanitizeID(host), Tool: t.Name()}
	if host == "" {
		res.Output = `no host given — call with {"host": "<name>"}`
		return res, nil
	}

	now := time.Now()
	ev, err := t.read(ctx, host, now.Add(-t.window), now)
	if err != nil {
		// ADVISORY, exactly as the attribution activity treats a reader failure (REQ-2307): report the gap
		// honestly so the agent knows the silence is UNMEASURED rather than evidence of absence. Conflating
		// "no actor found" with "could not look" is how a reader outage becomes a false causal claim.
		res.Output = fmt.Sprintf("the audit trail for %q could NOT be read (%v) — treat this as UNKNOWN, not as "+
			"evidence that nobody acted; do not claim an actor either way", host, err)
		return res, nil
	}
	if len(ev) == 0 {
		res.Output = fmt.Sprintf("no actor evidence for %q in the last %s. That means no covered audit domain "+
			"recorded an action on it in the window — it does NOT prove nobody acted (a domain may be "+
			"uncovered, or the action may predate the window).", host, t.window)
		return res, nil
	}

	// Newest first: a fault is usually explained by the most recent action, and the cap must not drop those.
	//
	// COPY BEFORE SORTING. sort.SliceStable reorders IN PLACE, and a Go slice shares its backing array with
	// the caller — sorting `ev` directly would silently reorder the reader's own return value under it. A
	// read-only tool must not have side effects on its input, and an ordering change is exactly the kind that
	// stays invisible until something downstream depends on the original order.
	shown := make([]attribution.Evidence, len(ev))
	copy(shown, ev)
	sort.SliceStable(shown, func(i, j int) bool { return shown[i].ObservedAt.After(shown[j].ObservedAt) })
	if len(shown) > recordCap {
		shown = shown[:recordCap]
	}

	var sb strings.Builder
	fmt.Fprintf(&sb, "actor evidence for %q — %d record(s) in the last %s", host, len(ev), t.window)
	if len(shown) < len(ev) {
		// No silent truncation: a partial list that reads as complete invites "no other actor" conclusions.
		fmt.Fprintf(&sb, " (showing the %d most recent)", len(shown))
	}
	sb.WriteString(":")
	for _, e := range shown {
		// EVERY field here is EXTERNAL text — an actor name, a verb and a ref straight out of another
		// system's log. %q keeps a hostile value (newlines, forged section headers, injected instructions)
		// visibly inert as data rather than letting it forge structure in the prompt (INV-08, REQ-2313). The
		// loop additionally screens this whole payload before it re-enters the model.
		fmt.Fprintf(&sb, "\n  - %s: actor=%q action=%q target=%q at=%s ref=%q covered=%v",
			e.Domain, e.Actor, e.ActionKind, e.Target,
			e.ObservedAt.UTC().Format(time.RFC3339), e.Ref, e.Covered)
	}
	// The agent decides WHAT the evidence means; the mechanical taxonomy is derived elsewhere and is not the
	// model's to set (REQ-2312). Say so, so a cited record is not mistaken for a licence to label.
	sb.WriteString("\n(observations only — cite the record id in evidence_ids if you rely on it. " +
		"Whether an action was authorised is decided mechanically, not here.)")
	res.Output = sb.String()
	res.Success = true
	return res, nil
}
