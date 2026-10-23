// Package evidencepack renders the EVIDENCE PACK an improver reads (TG-581, Harness² learnings H2-6b): one
// session's record laid out as FILES, never truncated to fit a prompt — and ANSWER-FREE.
//
// Harness² (core/context.py) serves evidence to its proposers on disk — task.md, rollouts/<label>.md in a shared
// [ACTION]/[OBSERVATION] grammar, deliverables/ rendered by the SAME extractor the grader uses, harness/ (the tree
// being edited), pool/<candidate>/{harness/,harness.diff} — and keeps the grader's verdict OUT of the improver's
// context entirely: an improver that sees the yardstick optimises the yardstick. TG's skill generator saw only a
// weak dimension's name and the current body (TG-181 measured it saturating). This package is the data half: the
// layout below, produced from what TG already persists (agent_step + agent_step_evidence, session_triage,
// skill_version). No judge score, no judge comment, no label and no expected outcome is ever written here — the
// deliverable is the record the judge adjudicated, never what the judge said about it. The proposers that read the
// pack are TG-582; nothing here reaches a prompt or a gate.
//
// Layout written by Write(dir, pack):
//
//	task.md                      the incident as the agent received it (framing only)
//	rollouts/<label>.md          the full trajectory: [STEP n] [THOUGHT] [ACTION] [OBSERVATION] [OUTCOME]; the
//	                             observation is the RECORDED payload (agent_step_evidence) when one exists, fenced
//	                             as data with its capture-truncation stated, else the step's recorded summary; an
//	                             observation recorded for a cycle with no step is rendered too, under its own heading
//	deliverables/<label>.md      the terminal record EXACTLY as the judge saw it (judge.Record) — and nothing else
//	harness/<skill>.md           the skill bodies (and the guidance row) the session composed with
//	pool/<candidate>/harness/…   a candidate's bodies, and pool/<candidate>/harness.diff vs harness/
//
// Everything written is DATA: trajectories and payloads were screened and scrubbed at capture; this package adds
// no instruction text to them, fences every payload so a payload cannot spoof the grammar, and never edits a body.
package evidencepack

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/pmezard/go-difflib/difflib"

	"github.com/territory-grounder/grounder/core/judge"
	"github.com/territory-grounder/grounder/core/trace"
)

// Incident is the task as the agent received it: framing, never labels or expected outcomes.
type Incident struct {
	ExternalRef string
	Host        string
	AlertRule   string
	Severity    string
	Summary     string
}

// Rollout is one recorded run of the task: the reasoning steps and the recorded observations behind them.
type Rollout struct {
	Label    string
	Steps    []trace.AgentStep
	Evidence []trace.AgentStepEvidence
}

// Deliverable is what the grader graded for one rollout: the terminal record, rendered by judge.Record. It carries
// NO verdict — scores and judge comments stay out of the improver's context by design (see the package note).
type Deliverable struct {
	Label  string
	Record judge.Session
}

// Candidate is one proposed harness: skill name → body. Only the skills it carries are diffed against harness/.
type Candidate struct {
	Label  string
	Bodies map[string]string
}

// Pack is everything an improver may read about one task.
type Pack struct {
	Incident     Incident
	Rollouts     []Rollout
	Deliverables []Deliverable
	Harness      map[string]string // skill name → body the session composed with (the guidance row included)
	Pool         []Candidate
}

// Write lays the pack out under dir (created if absent). Every label and skill name is validated as a safe file
// name BEFORE anything is written, so a bad name refuses the whole pack rather than leaving a partial one.
func Write(dir string, p Pack) error {
	for _, r := range p.Rollouts {
		if err := checkLabel(r.Label); err != nil {
			return err
		}
	}
	for _, d := range p.Deliverables {
		if err := checkLabel(d.Label); err != nil {
			return err
		}
	}
	for _, n := range sortedKeys(p.Harness) {
		if err := checkLabel(n); err != nil {
			return fmt.Errorf("harness skill %w", err)
		}
	}
	for _, c := range p.Pool {
		if err := checkLabel(c.Label); err != nil {
			return err
		}
		for _, n := range sortedKeys(c.Bodies) {
			if err := checkLabel(n); err != nil {
				return fmt.Errorf("candidate %s skill %w", c.Label, err)
			}
		}
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	if err := write(filepath.Join(dir, "task.md"), RenderTask(p.Incident)); err != nil {
		return err
	}
	for _, r := range p.Rollouts {
		if err := write(filepath.Join(dir, "rollouts", r.Label+".md"), RenderRollout(r)); err != nil {
			return err
		}
	}
	for _, d := range p.Deliverables {
		if err := write(filepath.Join(dir, "deliverables", d.Label+".md"), RenderDeliverable(d)); err != nil {
			return err
		}
	}
	for _, n := range sortedKeys(p.Harness) {
		if err := write(filepath.Join(dir, "harness", n+".md"), p.Harness[n]); err != nil {
			return err
		}
	}
	for _, c := range p.Pool {
		for _, n := range sortedKeys(c.Bodies) {
			if err := write(filepath.Join(dir, "pool", c.Label, "harness", n+".md"), c.Bodies[n]); err != nil {
				return err
			}
		}
		if err := write(filepath.Join(dir, "pool", c.Label, "harness.diff"), HarnessDiff(p.Harness, c)); err != nil {
			return err
		}
	}
	return nil
}

// RenderTask is the incident framing only — what the agent was handed. No label, no expected outcome, no score
// ever enters this file (the answer-free rule: an improver that sees the yardstick optimises the yardstick).
func RenderTask(inc Incident) string {
	var b strings.Builder
	b.WriteString("# Task\n\n")
	fmt.Fprintf(&b, "- external_ref: %s\n- host: %s\n- alert_rule: %s\n", oneLine(inc.ExternalRef), oneLine(inc.Host), oneLine(inc.AlertRule))
	if inc.Severity != "" {
		fmt.Fprintf(&b, "- severity: %s\n", oneLine(inc.Severity))
	}
	if strings.TrimSpace(inc.Summary) != "" {
		fmt.Fprintf(&b, "\n## Alert summary (as received)\n\n%s\n", fence(strings.TrimSpace(inc.Summary)))
	}
	return b.String()
}

// RenderRollout renders the trajectory in the shared grammar. The observation is the RECORDED payload when the
// cycle has evidence rows (the full captured body, fenced as data, with any capture-time truncation stated as
// "showing X of Y bytes"); otherwise the step's recorded summary, marked as such. Evidence recorded for a cycle
// that has no step row is rendered after the steps under its own heading — the record is never dropped. Nothing is
// cut here: the file is the record.
func RenderRollout(r Rollout) string {
	byCycle := map[int][]trace.AgentStepEvidence{}
	for _, e := range r.Evidence {
		byCycle[e.Cycle] = append(byCycle[e.Cycle], e)
	}
	steps := append([]trace.AgentStep{}, r.Steps...)
	sort.SliceStable(steps, func(i, j int) bool { return steps[i].Cycle < steps[j].Cycle })
	var b strings.Builder
	fmt.Fprintf(&b, "# Rollout %s\n\n", r.Label)
	if len(steps) == 0 && len(r.Evidence) == 0 {
		b.WriteString("(no recorded steps and no recorded observations — the session ran no reasoning cycle, or it predates capture)\n")
		return b.String()
	}
	rendered := map[int]bool{}
	for _, s := range steps {
		fmt.Fprintf(&b, "[STEP %d]\n", s.Cycle)
		if t := strings.TrimSpace(s.Thought); t != "" {
			fmt.Fprintf(&b, "[THOUGHT] %s\n", fence(t))
		}
		if t := strings.TrimSpace(s.Tool); t != "" {
			fmt.Fprintf(&b, "[ACTION] %s\n", oneLine(t))
		}
		if evs := byCycle[s.Cycle]; len(evs) > 0 && !rendered[s.Cycle] {
			// Two steps sharing a cycle (a retried activity) show the evidence once, on the first.
			rendered[s.Cycle] = true
			for _, e := range evs {
				b.WriteString(renderObservation(e))
			}
		} else if o := strings.TrimSpace(s.Observation); o != "" && len(evs) == 0 {
			fmt.Fprintf(&b, "[OBSERVATION summary — no recorded payload for this cycle]\n%s\n", fence(o))
		}
		if oc := strings.TrimSpace(s.Outcome); oc != "" {
			fmt.Fprintf(&b, "[OUTCOME] %s\n", oc)
		}
		b.WriteString("\n")
	}
	cycles := make([]int, 0, len(byCycle))
	for c := range byCycle {
		if !rendered[c] {
			cycles = append(cycles, c)
		}
	}
	sort.Ints(cycles)
	for _, c := range cycles {
		fmt.Fprintf(&b, "[OBSERVATION without a recorded step — cycle %d]\n", c)
		for _, e := range byCycle[c] {
			b.WriteString(renderObservation(e))
		}
		b.WriteString("\n")
	}
	return b.String()
}

func renderObservation(e trace.AgentStepEvidence) string {
	if e.Truncated {
		return fmt.Sprintf("[OBSERVATION id=%s — showing %d of %d bytes; truncated at capture]\n%s\n", oneLine(e.EvidenceID), len(e.Payload), e.FullBytes, fence(e.Payload))
	}
	return fmt.Sprintf("[OBSERVATION id=%s]\n%s\n", oneLine(e.EvidenceID), fence(e.Payload))
}

// oneLine keeps a HEADER field (a ref, a host, a rule name, a result id) on its own line: a control character in
// an upstream name must not forge a heading or a grammar line. Data bodies are fenced instead (fence).
func oneLine(s string) string {
	return strings.Map(func(r rune) rune {
		if r == '\n' || r == '\r' || r == '\t' {
			return ' '
		}
		return r
	}, s)
}

// RenderDeliverable is the terminal record EXACTLY as the judge saw it (judge.Record — the grader's own
// extractor, so the improver reads what was graded) and nothing else: no score, no judge comment.
func RenderDeliverable(d Deliverable) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# Deliverable %s\n\n## Terminal record (as the judge saw it)\n\n", d.Label)
	b.WriteString(judge.Record(d.Record))
	return b.String()
}

// HarnessDiff is the unified diff of a candidate's bodies against the base harness, one hunk set per skill the
// candidate carries (a skill the candidate does not carry is unchanged by definition and absent from the diff).
func HarnessDiff(base map[string]string, c Candidate) string {
	var b strings.Builder
	for _, n := range sortedKeys(c.Bodies) {
		before, after := base[n], c.Bodies[n]
		if before == after {
			continue
		}
		d := difflib.UnifiedDiff{
			A: difflib.SplitLines(before), B: difflib.SplitLines(after),
			FromFile: "harness/" + n + ".md", ToFile: "pool/" + c.Label + "/harness/" + n + ".md", Context: 3,
		}
		text, err := difflib.GetUnifiedDiffString(d)
		if err != nil {
			text = fmt.Sprintf("--- harness/%s.md\n+++ pool/%s/harness/%s.md\n(diff unavailable: %v)\n", n, c.Label, n, err)
		}
		b.WriteString(text)
	}
	if b.Len() == 0 {
		return "(no differences from harness/)\n"
	}
	return b.String()
}

// fence wraps DATA in a code fence longer than any backtick run inside it, so a payload that happens to contain a
// grammar heading ("[STEP 2]", "[OBSERVATION id=…]") or a fence of its own cannot be mistaken for structure by a
// file-reading improver. Payloads were screened and scrubbed at capture; this adds no text to them.
func fence(s string) string {
	run, longest := 0, 2
	for _, r := range s {
		if r == '`' {
			run++
			if run > longest {
				longest = run
			}
		} else {
			run = 0
		}
	}
	f := strings.Repeat("`", longest+1)
	return f + "\n" + s + "\n" + f
}

func sortedKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func checkLabel(label string) error {
	if label == "" || label == "." || label == ".." || len(label) > 200 || strings.ContainsAny(label, "/\\\x00\n\r\t") {
		return fmt.Errorf("label %q is not a safe file name", label)
	}
	return nil
}

func write(path, content string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(content), 0o644)
}
