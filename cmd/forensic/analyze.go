package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/territory-grounder/grounder/adapters/model"
	"github.com/territory-grounder/grounder/core/db"
	"github.com/territory-grounder/grounder/core/forensic"
)

// TG-168 part 2 — the model-assisted half. Part 1 (cmd/forensic) reconstructs a deterministic cross-incident
// timeline; this runs the local IR model over it to extract IOCs, separate real impact from decoy activity,
// and map the credentials the window touched — the "AI on defense" the ticket asks for, the way HF ran bulk
// analysis over 17k+ events. The model I/O is a single call behind the narrow completer interface; everything
// around it — the prompt, the strict-JSON parse, the render — is deterministic and fixture-tested, so this
// merges and is exercised WITHOUT a live forensic lane (the lane is a runtime dependency, not a build one).

// completer is the narrow model surface the analysis depends on — the same shape agent/loop.go uses, so
// adapters/model.Gateway satisfies it and a test injects a fake without a live model.
type completer interface {
	Complete(ctx context.Context, user, modelName string, msgs []model.Message) (string, error)
}

// ForensicFindings is the closed, structured account the IR model returns over one reconstructed window —
// parseable and citable, never free narrative.
type ForensicFindings struct {
	Summary            string   `json:"summary"`
	IOCs               []string `json:"iocs"`                // indicators of compromise grounded in the timeline
	RealImpactHosts    []string `json:"real_impact_hosts"`   // hosts with genuine impact, separated from noise
	DecoyActivity      []string `json:"decoy_activity"`      // events assessed as decoy / benign
	CredentialsTouched []string `json:"credentials_touched"` // credential references the window shows were used
}

// forensicSystemPrompt instructs the model as an IR analyst. It demands STRICT JSON matching ForensicFindings
// and forbids invention: the timeline is the only evidence, and an empty field is the honest answer when the
// evidence does not support one.
const forensicSystemPrompt = `You are an incident-response forensic analyst. You are given a deterministic, chronologically-ordered timeline reconstructed from a governed system's own audit corpora (the governance ledger, ingest alerts, agent steps, credential resolutions, exec-class decisions).

Analyse ONLY the evidence in the timeline. Do NOT invent hosts, IOCs, or credentials that do not appear in it. An empty finding is correct when the evidence does not support one — under-claim rather than fabricate.

Return STRICT JSON and nothing else, matching exactly:
{"summary": "...", "iocs": [...], "real_impact_hosts": [...], "decoy_activity": [...], "credentials_touched": [...]}
- summary: 2-4 sentences on what the window shows.
- iocs: indicators of compromise grounded in the timeline (hostnames, refs, patterns), or [].
- real_impact_hosts: hosts with genuine impact, separated from decoy/noise, or [].
- decoy_activity: events you assess as decoy / benign, or [].
- credentials_touched: credential references the timeline shows were resolved or used, or [].`

// buildForensicPrompt renders the timeline into the user turn and pairs it with the IR instruction.
// Deterministic: the same window yields the same prompt (up to the model's own nondeterminism downstream).
func buildForensicPrompt(w forensic.Window, host string, r db.ForensicRead) []model.Message {
	var tl bytes.Buffer
	renderTimeline(&tl, w, host, r)
	return []model.Message{
		{Role: "system", Content: forensicSystemPrompt},
		{Role: "user", Content: "Timeline:\n\n" + tl.String()},
	}
}

// parseForensicFindings extracts the model's JSON, tolerating a ```json-fenced block (models commonly wrap
// it) but never guessing: a reply that is not a JSON object is an ERROR, not an empty result. A forensic tool
// that silently returned nothing on a garbled answer would read as "found nothing" when it in fact failed.
func parseForensicFindings(raw string) (ForensicFindings, error) {
	s := strings.TrimSpace(raw)
	if i := strings.Index(s, "```"); i >= 0 {
		s = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(s[i+3:]), "json"))
		if j := strings.LastIndex(s, "```"); j >= 0 {
			s = s[:j]
		}
		s = strings.TrimSpace(s)
	}
	var f ForensicFindings
	if s == "" || s[0] != '{' {
		return f, fmt.Errorf("model returned no JSON object (%d bytes) — refusing to guess a forensic finding", len(raw))
	}
	if err := json.Unmarshal([]byte(s), &f); err != nil {
		return f, fmt.Errorf("model output was not the expected JSON findings shape: %w", err)
	}
	return f, nil
}

// analyzeTimeline builds the prompt, calls the model, and parses the findings. The model call is the only
// part needing a live forensic lane; everything around it is deterministic and fixture-tested.
func analyzeTimeline(ctx context.Context, m completer, modelName, user string, w forensic.Window, host string, r db.ForensicRead) (ForensicFindings, error) {
	raw, err := m.Complete(ctx, user, modelName, buildForensicPrompt(w, host, r))
	if err != nil {
		return ForensicFindings{}, fmt.Errorf("forensic model call: %w", err)
	}
	return parseForensicFindings(raw)
}

// renderFindings prints the structured findings, each list labelled and an empty one saying so — the same
// under-claim honesty the deterministic timeline uses.
func renderFindings(out io.Writer, f ForensicFindings) {
	fmt.Fprintf(out, "\n## forensic analysis (model-assisted)\n%s\n", strings.TrimSpace(f.Summary))
	section := func(label string, xs []string) {
		if len(xs) == 0 {
			fmt.Fprintf(out, "\n%s: (none)\n", label)
			return
		}
		fmt.Fprintf(out, "\n%s:\n", label)
		for _, x := range xs {
			fmt.Fprintf(out, "  - %s\n", x)
		}
	}
	section("IOCs", f.IOCs)
	section("real-impact hosts", f.RealImpactHosts)
	section("decoy activity", f.DecoyActivity)
	section("credentials touched", f.CredentialsTouched)
}
