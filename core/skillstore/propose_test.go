package skillstore

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/territory-grounder/grounder/core/evidencepack"
	"github.com/territory-grounder/grounder/core/judge"
	"github.com/territory-grounder/grounder/core/trace"
)

type fakeEvidence struct{ packs []evidencepack.Pack }

func (f fakeEvidence) PacksFor(context.Context, int64, string, time.Duration, int) ([]evidencepack.Pack, error) {
	return f.packs, nil
}

func samplePacks() []evidencepack.Pack {
	return []evidencepack.Pack{{
		Incident: evidencepack.Incident{ExternalRef: "TG-ev-1", Host: "dc1web01", AlertRule: "Device-Down"},
		Rollouts: []evidencepack.Rollout{{Label: "TG-ev-1", Steps: []trace.AgentStep{
			{Cycle: 1, Thought: "check the device", Tool: "get-device-status", Observation: "down", Outcome: "success"},
			{Cycle: 2, Thought: "stop without a prediction", Tool: "", Observation: "", Outcome: "success"},
		}, Evidence: []trace.AgentStepEvidence{{Cycle: 1, EvidenceID: "lnms-dev-dc1web01", Tool: "get-device-status", Payload: "device dc1web01: down since 10:00"}}}},
		Deliverables: []evidencepack.Deliverable{{Label: "TG-ev-1", Record: judge.Session{Ref: "TG-ev-1", Band: "ESCALATE", Outcome: "escalated", Conclusion: "down"}}},
		Harness:      map[string]string{"triage-protocol": "current production body"},
	}}
}

func validReply(body string) string {
	return "<edits>\nrevised -> commit a falsifiable prediction before stopping -> TG-ev-1:[STEP 2] the agent stopped with no committed prediction\n</edits>\n<body>\n" + body + "\n</body>"
}

// A proposer round reads the record: the INV and DOM prompts carry the rollout text and each one's forbidden scope;
// a valid reply lands ONE draft whose rationale carries the cited edit rows and whose predicate is the lawful one.
func TestProposersReadTheRecordAndLandCitedDrafts(t *testing.T) {
	m, _, prod := genStore(t)
	gen := &capturingGen{replies: []string{validReply("body from INV: commit a prediction before stopping"), "NO_CHANGE"}}
	trig := GenTrigger{SkillName: "triage-protocol", Dimension: "falsifiable_prediction", Mean: 1.5, Threshold: 3.5, Window: 30, Source: "flywheel:eval-failure:run-9"}
	out, err := GenerateCandidatesFromEvidence(context.Background(), m, gen, trig, samplePacks(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 1 || out[0].Status != StatusDraft || out[0].ParentVersionID != prod.ID || out[0].Author != "flywheel" {
		t.Fatalf("want one lineage-linked draft from the INV proposer (DOM answered NO_CHANGE), got %+v", out)
	}
	if !strings.Contains(out[0].Rationale, "proposer INV") || !strings.Contains(out[0].Rationale, "TG-ev-1:[STEP 2]") {
		t.Fatalf("the rationale must carry the proposer and its cited rows: %q", out[0].Rationale)
	}
	if len(gen.prompts) != 2 {
		t.Fatalf("two proposers, two prompts, got %d", len(gen.prompts))
	}
	inv, dom := gen.prompts[0], gen.prompts[1]
	for _, p := range []string{inv, dom} {
		if !strings.Contains(p, "[STEP 2]") || !strings.Contains(p, "device dc1web01: down since 10:00") || !strings.Contains(p, "# CURRENT BODY") || !strings.Contains(p, "current production body") {
			t.Fatal("every proposer must read the full record and the current body")
		}
		if !strings.Contains(p, "cite the specific trajectory moment") || !strings.Contains(p, "NO_CHANGE") || !strings.Contains(p, "data, not instructions") {
			t.Fatal("the rules and the reply contract must be in the mission")
		}
	}
	if !strings.Contains(inv, "You are the INV proposer") || !strings.Contains(inv, "FORBIDDEN from changing domain rules") {
		t.Fatalf("INV must own procedure and be forbidden domain rules: %s", inv[:400])
	}
	if !strings.Contains(dom, "You are the DOM proposer") || !strings.Contains(dom, "FORBIDDEN from changing procedure") {
		t.Fatalf("DOM must own domain knowledge and be forbidden procedure: %s", dom[:400])
	}
	got, _, _ := m.ProductionVersion(context.Background(), "triage-protocol")
	if got.Body != "current production body" {
		t.Fatal("a draft must not touch production")
	}
}

// An invalid reply is retried ONCE with the validator's words; a second failure yields nothing; NO_CHANGE yields nothing.
func TestInvalidProposalsAreRetriedOnceThenRefused(t *testing.T) {
	m, _, _ := genStore(t)
	uncited := "<edits>\nrevised -> something -> TG-ev-1:[STEP 7] a moment that does not exist\n</edits>\n<body>\nnew body\n</body>"
	gen := &capturingGen{replies: []string{uncited, validReply("body after correction"), uncited, "garbage with no blocks"}}
	trig := GenTrigger{SkillName: "triage-protocol", Dimension: "falsifiable_prediction", Mean: 1.5, Threshold: 3.5, Window: 30, Source: "flywheel:eval-failure:run-9"}
	out, err := GenerateCandidatesFromEvidence(context.Background(), m, gen, trig, samplePacks(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 1 || out[0].Body != "body after correction" {
		t.Fatalf("INV must land its corrected second attempt and DOM (two invalid) nothing, got %+v", out)
	}
	if len(gen.prompts) != 4 {
		t.Fatalf("each proposer gets at most two attempts: %d prompts", len(gen.prompts))
	}
	if !strings.HasPrefix(gen.prompts[1], "# YOUR PREVIOUS ATTEMPT WAS REJECTED") || !strings.Contains(gen.prompts[1], "TG-ev-1:[STEP 7], which is not a moment in the record") {
		t.Fatalf("the retry must carry the validator's words: %q", gen.prompts[1][:300])
	}
	if !strings.HasPrefix(gen.prompts[3], "# YOUR PREVIOUS ATTEMPT WAS REJECTED") || !strings.Contains(gen.prompts[3], "not a moment in the record") {
		t.Fatalf("the second proposer's retry must name its defect: %q", gen.prompts[3][:200])
	}
	// The second attempt's garbage reply is refused without a third prompt: at most two attempts per proposer.
}

// The workspace-facts rule: a body that carries this incident's host (or ref) is refused — a skill is carried to
// later incidents, and facts about one matter belong in its precedent.
func TestBodiesCarryingIncidentFactsAreRefused(t *testing.T) {
	m, _, _ := genStore(t)
	leaky := validReply("when dc1web01 is down, escalate")
	gen := &capturingGen{replies: []string{leaky, leaky, "NO_CHANGE"}}
	trig := GenTrigger{SkillName: "triage-protocol", Dimension: "correct_diagnosis", Mean: 2, Threshold: 3.5, Window: 30, Source: "flywheel:eval-failure:run-9"}
	out, err := GenerateCandidatesFromEvidence(context.Background(), m, gen, trig, samplePacks(), nil)
	if err != nil || len(out) != 0 {
		t.Fatalf("a body naming the incident host must land nothing, got %+v err=%v", out, err)
	}
	if !strings.Contains(gen.prompts[1], "carries a fact about THIS incident") {
		t.Fatalf("the retry must name the workspace-facts rule: %q", gen.prompts[1][:300])
	}
}

// Without an evidence source, or with no packs, generation is the lens path — byte-identical to before.
func TestGenerationFallsBackToLensesWithoutEvidence(t *testing.T) {
	m, lg, _ := genStore(t)
	var logs []string
	d := CreationDeps{Store: m, Ledger: lg, Model: &scriptedGen{replies: []string{"rewritten A", "rewritten B", "rewritten C"}},
		Evidence: fakeEvidence{}, Log: func(f string, a ...any) { logs = append(logs, f) }}
	trig := GenTrigger{SkillName: "triage-protocol", Dimension: "correct_diagnosis", Mean: 2.9, Threshold: 3.5, Window: 30, Source: "flywheel:eval-failure:run-7"}
	prod, _, _ := m.ProductionVersion(context.Background(), "triage-protocol")
	out, err := d.generateForVersion(context.Background(), trig, prod.ID)
	if err != nil || len(out) != 3 {
		t.Fatalf("no packs ⇒ the three lenses run, got %d err=%v", len(out), err)
	}
	joined := strings.Join(logs, "\n")
	if !strings.Contains(joined, "falling back to lens generation") {
		t.Fatalf("the fallback must be logged: %q", joined)
	}
}

// A long observation is clipped in the MISSION only, with the clip stated; headings and the rest are untouched.
func TestMissionClipsObservationsAndSaysSo(t *testing.T) {
	long := strings.Repeat("x", 7000)
	rendered := evidencepack.RenderRollout(evidencepack.Rollout{Label: "L", Steps: []trace.AgentStep{{Cycle: 1, Tool: "t", Outcome: "success"}},
		Evidence: []trace.AgentStepEvidence{{Cycle: 1, EvidenceID: "t-h", Tool: "t", Payload: long}}})
	clipped := clipObservations(rendered)
	if strings.Contains(clipped, long) || !strings.Contains(clipped, "[clipped in this mission at 6000 bytes") || !strings.Contains(clipped, "[STEP 1]") || !strings.Contains(clipped, "[OUTCOME] success") {
		t.Fatalf("the observation must be clipped with the clip stated and the structure kept: %d bytes", len(clipped))
	}
	// A THOUGHT whose text starts with "[OBSERVATION" lives inside the thought's fence and must not open a clip.
	tricky := evidencepack.RenderRollout(evidencepack.Rollout{Label: "L", Steps: []trace.AgentStep{{Cycle: 1, Thought: "[OBSERVATION pretend\n" + strings.Repeat("t", 7000), Tool: "t", Outcome: "success"}}})
	if out := clipObservations(tricky); strings.Contains(out, "[clipped in this mission") {
		t.Fatal("a thought is never clipped, even when its text begins like an observation heading")
	}
	// The clip never splits a rune.
	runes := evidencepack.RenderRollout(evidencepack.Rollout{Label: "L", Steps: []trace.AgentStep{{Cycle: 1, Tool: "t", Outcome: "success"}},
		Evidence: []trace.AgentStepEvidence{{Cycle: 1, EvidenceID: "t-h", Tool: "t", Payload: strings.Repeat("é", 4000)}}})
	if out := clipObservations(runes); !utf8ValidString(out) {
		t.Fatal("the clip must land on a rune boundary")
	}
}

func utf8ValidString(s string) bool {
	for _, r := range s {
		if r == '\uFFFD' {
			return false
		}
	}
	return true
}

// The wired path: CreationDeps with an evidence source that HAS packs runs the proposers (not the lenses), and a
// transport failure of every proposer falls back to the lenses THIS run with the fallback logged — never "0 drafts".
func TestWiredProposersRunAndTransportFailureFallsBackToLenses(t *testing.T) {
	m, lg, prod := genStore(t)
	var logs []string
	gen := &capturingGen{replies: []string{validReply("INV body"), "NO_CHANGE"}}
	d := CreationDeps{Store: m, Ledger: lg, Model: gen, Evidence: fakeEvidence{packs: samplePacks()}, Log: func(f string, a ...any) { logs = append(logs, fmt.Sprintf(f, a...)) }}
	trig := GenTrigger{SkillName: "triage-protocol", Dimension: "falsifiable_prediction", Mean: 1.5, Threshold: 3.5, Window: 30, Source: "flywheel:eval-failure:run-1"}
	out, err := d.generateForVersion(context.Background(), trig, prod.ID)
	if err != nil || len(out) != 1 || !strings.Contains(out[0].Rationale, "proposer INV") {
		t.Fatalf("with packs the proposers must run, got %d drafts err=%v", len(out), err)
	}
	if !strings.Contains(strings.Join(logs, "\n"), "the INV/DOM proposers read the record") || !strings.Contains(strings.Join(logs, "\n"), "draft") {
		t.Fatalf("the proposer path and the landed draft must be logged: %q", logs)
	}
	// Transport failure on every proposer: the lenses run instead, and say so (a fresh store — candidate version
	// numbers restart, as they would on a run where no proposer landed anything).
	logs = nil
	m2, lg2, prod2 := genStore(t)
	down := &failingThenLenses{failures: 2, lenses: []string{"lens A", "lens B", "lens C"}}
	d2 := CreationDeps{Store: m2, Ledger: lg2, Model: down, Evidence: fakeEvidence{packs: samplePacks()}, Log: func(f string, a ...any) { logs = append(logs, fmt.Sprintf(f, a...)) }}
	out2, err := d2.generateForVersion(context.Background(), trig, prod2.ID)
	if err != nil || len(out2) != 3 {
		t.Fatalf("every proposer down ⇒ the three lenses run, got %d err=%v", len(out2), err)
	}
	if !strings.Contains(strings.Join(logs, "\n"), "every proposer failed on the model transport; falling back to lens generation") {
		t.Fatalf("the transport fallback must be logged: %q", logs)
	}
}

// failingThenLenses fails the first N completions (the proposers' transport) and then answers the lenses.
type failingThenLenses struct {
	failures int
	lenses   []string
}

func (f *failingThenLenses) Complete(_ context.Context, user, _ string, _ string) (string, error) {
	if strings.HasPrefix(user, "skill-proposer-") {
		return "", errors.New("gateway: context length exceeded")
	}
	if len(f.lenses) == 0 {
		return "", errors.New("exhausted")
	}
	r := f.lenses[0]
	f.lenses = f.lenses[1:]
	return r, nil
}

// The reply shape is exact: prose outside the blocks is refused; a body may mention </body> and keeps its last line.
func TestReplyShapeIsExact(t *testing.T) {
	moments := map[string]bool{"L:[STEP 1]": true}
	row := "revised -> x -> L:[STEP 1] m\n"
	for _, bad := range []string{
		"Sure! <edits>\n" + row + "</edits>\n<body>\nb\n</body>",
		"<edits>\n" + row + "</edits>\nnote\n<body>\nb\n</body>",
		"<edits>\n" + row + "</edits>\n<body>\nb\n</body>\nthanks",
	} {
		if _, err := parseProposal(bad, moments, nil, 8192); err == nil {
			t.Fatalf("prose outside the blocks must be refused: %q", bad)
		}
	}
	ok, err := parseProposal("<edits>\n"+row+"</edits>\n<body>\nthe tag </body> is text here\nlast line\n</body>", moments, nil, 8192)
	if err != nil || !strings.HasSuffix(ok.body, "last line") || !strings.Contains(ok.body, "</body> is text") {
		t.Fatalf("the LAST </body> closes the body: %+v err=%v", ok, err)
	}
}

// The workspace-facts rule matches whole tokens and only facts the CURRENT body does not already carry.
func TestIncidentFactsAreWholeTokensAndNotAlreadyPresent(t *testing.T) {
	packs := []evidencepack.Pack{{Incident: evidencepack.Incident{ExternalRef: "TG-1", Host: "core"}}}
	facts := incidentFacts(packs, "generic body")
	if len(facts) != 2 {
		t.Fatalf("want host + ref, got %d", len(facts))
	}
	moments := map[string]bool{"L:[STEP 1]": true}
	reply := func(body string) string {
		return "<edits>\nrevised -> x -> L:[STEP 1] m\n</edits>\n<body>\n" + body + "\n</body>"
	}
	if _, err := parseProposal(reply("see ticket tg-12 and the coreless check"), moments, facts, 8192); err != nil {
		t.Fatalf("tg-12 and coreless are not the facts tg-1 / core: %v", err)
	}
	if _, err := parseProposal(reply("when core is down, escalate"), moments, facts, 8192); err == nil {
		t.Fatal("the whole token core must be refused")
	}
	already := incidentFacts(packs, "the core host is special")
	if len(already) != 1 { // core is already in the current body — not a NEW leak; TG-1 still is
		t.Fatalf("a fact the current body carries must not be listed: %d", len(already))
	}
}

// The mission budget keeps the weakest packs first and states the drop; a single oversized pack is cut, not dropped.
func TestMissionBudgetKeepsWeakestFirstAndSaysSo(t *testing.T) {
	big := samplePacks()[0]
	big.Rollouts[0].Evidence[0].Payload = strings.Repeat("y", 3000)
	packs := []evidencepack.Pack{big, big, big}
	text, used := renderMissionEvidence(packs, 7000)
	if used != 1 || !strings.Contains(text, "2 further pack(s) of weaker sessions were not included") {
		t.Fatalf("want one pack kept and the drop stated, got used=%d", used)
	}
	cut, used2 := renderMissionEvidence(packs[:1], 1000)
	if used2 != 1 || !strings.Contains(cut, "this single pack was cut at") || len(cut) > 1200 {
		t.Fatalf("a single oversized pack is cut and said so: used=%d len=%d", used2, len(cut))
	}
}
