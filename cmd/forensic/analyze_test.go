package main

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/territory-grounder/grounder/adapters/model"
	"github.com/territory-grounder/grounder/core/db"
	"github.com/territory-grounder/grounder/core/forensic"
)

type fakeCompleter struct {
	reply   string
	err     error
	gotMsgs []model.Message
}

func (f *fakeCompleter) Complete(_ context.Context, _, _ string, msgs []model.Message) (string, error) {
	f.gotMsgs = msgs
	return f.reply, f.err
}

func sampleRead() db.ForensicRead {
	return db.ForensicRead{Events: []forensic.Event{
		{At: time.Date(2026, 8, 6, 0, 5, 0, 0, time.UTC), Source: forensic.SourceIngest, Host: "dc1pve03", SubjectRef: "ext-1", Kind: "alert", Detail: "host down"},
	}}
}

// The prompt is a system IR instruction (strict JSON) + a user turn that EMBEDS the rendered timeline.
func TestBuildForensicPrompt_EmbedsTimelineAndDemandsJSON(t *testing.T) {
	w := forensic.Window{From: time.Date(2026, 8, 6, 0, 0, 0, 0, time.UTC)}
	msgs := buildForensicPrompt(w, "dc1pve03", sampleRead())
	if len(msgs) != 2 || msgs[0].Role != "system" || msgs[1].Role != "user" {
		t.Fatalf("expected system+user, got %+v", msgs)
	}
	if !strings.Contains(msgs[0].Content, "STRICT JSON") {
		t.Error("system prompt must demand strict JSON")
	}
	if !strings.Contains(msgs[1].Content, "dc1pve03") || !strings.Contains(msgs[1].Content, "host down") {
		t.Errorf("user turn must embed the timeline; got %q", msgs[1].Content)
	}
}

// Parsing accepts bare JSON and a ```json-fenced block; a non-JSON reply is an ERROR, never a silent empty
// finding (that would read as "found nothing" when the analysis in fact failed).
func TestParseForensicFindings_JSONFencedAndErrors(t *testing.T) {
	for name, raw := range map[string]string{
		"bare":   `{"summary":"s","iocs":["dc1pve03"],"credentials_touched":["store:awx.token"]}`,
		"fenced": "```json\n{\"summary\":\"s\",\"iocs\":[\"dc1pve03\"],\"credentials_touched\":[\"store:awx.token\"]}\n```",
	} {
		t.Run(name, func(t *testing.T) {
			f, err := parseForensicFindings(raw)
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			if len(f.IOCs) != 1 || f.IOCs[0] != "dc1pve03" || len(f.CredentialsTouched) != 1 {
				t.Errorf("findings mis-parsed: %+v", f)
			}
		})
	}
	if _, err := parseForensicFindings("I could not analyse this."); err == nil {
		t.Error("a non-JSON model reply must be an error, never a silent empty finding")
	}
	if _, err := parseForensicFindings("```json\n{not valid}\n```"); err == nil {
		t.Error("malformed JSON inside a fence must still error")
	}
}

// analyzeTimeline wires prompt→model→parse: a fake returning JSON yields parsed findings (and got the
// timeline), and a model error propagates rather than being swallowed.
func TestAnalyzeTimeline_WithFakeModel(t *testing.T) {
	w := forensic.Window{From: time.Date(2026, 8, 6, 0, 0, 0, 0, time.UTC)}
	m := &fakeCompleter{reply: `{"summary":"pve03 cascade","real_impact_hosts":["dc1pve03"]}`}
	f, err := analyzeTimeline(context.Background(), m, "ir-model", "u", w, "", sampleRead())
	if err != nil {
		t.Fatalf("analyze: %v", err)
	}
	if f.Summary != "pve03 cascade" || len(f.RealImpactHosts) != 1 {
		t.Errorf("findings: %+v", f)
	}
	if len(m.gotMsgs) != 2 || !strings.Contains(m.gotMsgs[1].Content, "dc1pve03") {
		t.Error("the model was not sent the reconstructed timeline")
	}
	me := &fakeCompleter{err: context.DeadlineExceeded}
	if _, err := analyzeTimeline(context.Background(), me, "m", "u", w, "", sampleRead()); err == nil {
		t.Error("a model error must propagate, not be swallowed into empty findings")
	}
}
