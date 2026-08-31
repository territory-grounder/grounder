package runner

// C-3b drills — the base prompt's guidance half served store-first. The oracle set covers all four
// compose postures (store row / trial arm / absent / broken), the leak law (a ClassPrompt row must never
// reach the <behavioral_guidance> skill block), and the agent-side override render.
//
// EXECUTED KILLING MUTATIONS (2026-08-15, each witnessed red then restored green):
//   1. compose_seed.go composeBasePrompt: the store-row return replaced with the fallback return (the
//      seam unwired — every session silently composes the embed while the store row versions in the
//      console) → TestComposeBasePromptServesTheStoreRow red.
//   2. agent/loop.go renderPreamble: the override parameter ignored (guidance always the embed) →
//      TestRenderPreambleComposesTheOverride red.
//   3. compose_seed.go: the content-hash re-check dropped (the review's MEDIUM — a row written around
//      the API composes into the system prompt) → the tampered-row arm red.

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/territory-grounder/grounder/agent/skills"
	"github.com/territory-grounder/grounder/core/execclass"
	"github.com/territory-grounder/grounder/core/skillstore"
)

func basePromptRow(body string) skillstore.ProductionRow {
	return skillstore.ProductionRow{
		VersionID: 71, SkillName: basePromptRowName, Version: "1.0.0", Body: body,
		Class: skillstore.ClassPrompt, Position: 999,
		ContentHash: skillstore.ContentHash(body, skillstore.AppliesWhen{}),
	}
}

func TestComposeBasePromptServesTheStoreRow(t *testing.T) {
	body := "GUIDANCE FROM THE STORE — versioned, graduating"
	a := &Activities{D: Deps{SkillRows: func(context.Context) ([]skillstore.ProductionRow, error) {
		return []skillstore.ProductionRow{basePromptRow(body)}, nil
	}}}
	g, entry := a.composeBasePrompt(context.Background(), "TG-c3b-1")
	if g != body {
		t.Fatalf("the production ClassPrompt row must compose, got %q", g)
	}
	if entry != basePromptRowName+"@1.0.0#71:store" {
		t.Fatalf("the load entry must bind the exact version+row id (the judge spine's key), got %q", entry)
	}
}

func TestComposeBasePromptFallsBackToTheEmbed(t *testing.T) {
	cases := []struct {
		name string
		deps Deps
		want string
	}{
		{"no store wired", Deps{}, basePromptRowName + "@compiled"},
		{"store read fails", Deps{SkillRows: func(context.Context) ([]skillstore.ProductionRow, error) {
			return nil, errors.New("db down")
		}}, basePromptRowName + "@compiled:fallback"},
		{"row absent", Deps{SkillRows: func(context.Context) ([]skillstore.ProductionRow, error) {
			return []skillstore.ProductionRow{{SkillName: "proving-your-work", Class: skillstore.ClassSkill, Body: "x", VersionID: 1, Version: "1.0.0"}}, nil
		}}, basePromptRowName + "@compiled"},
		{"oversized body refused", Deps{SkillRows: func(context.Context) ([]skillstore.ProductionRow, error) {
			return []skillstore.ProductionRow{basePromptRow(strings.Repeat("x", skillstore.MaxBodyBytes(skillstore.ClassPrompt)+1))}, nil
		}}, basePromptRowName + "@compiled:fallback"},
		{"tampered row refused (content-hash mismatch)", Deps{SkillRows: func(context.Context) ([]skillstore.ProductionRow, error) {
			r := basePromptRow("BODY EDITED AROUND THE API")
			r.ContentHash = skillstore.ContentHash("the body the hash was stamped for", skillstore.AppliesWhen{})
			return []skillstore.ProductionRow{r}, nil
		}}, basePromptRowName + "@compiled:fallback"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			a := &Activities{D: c.deps}
			g, entry := a.composeBasePrompt(context.Background(), "TG-c3b-2")
			if g != "" {
				t.Fatalf("every fallback posture must return \"\" (the agent renders the embed), got %q", g)
			}
			if entry != c.want {
				t.Fatalf("the record must state the posture honestly: want %q got %q", c.want, entry)
			}
		})
	}
}

// The C-6 half: a trial on the prompt row composes its CANDIDATE for the drawn arm — through the same
// deterministic assignment as skill trials — so a booked arm is an arm that ran (TG-218).
func TestComposeBasePromptComposesTheTrialArm(t *testing.T) {
	mem := skillstore.NewMemTrialStore(100)
	tr, err := mem.CreateTrial(context.Background(), skillstore.Trial{
		SkillName: basePromptRowName, CandidateIDs: []int64{88}, ControlVersionID: 71,
		Dimension: "correct_diagnosis", MinSamplesPerArm: 5, MinLift: 0.1, PThreshold: 0.05,
		EndsAt: time.Now().Add(72 * time.Hour), Status: "active",
	})
	if err != nil {
		t.Fatalf("fixture trial: %v", err)
	}
	candBody := "CANDIDATE GUIDANCE — the trial arm"
	a := &Activities{D: Deps{
		SkillRows: func(context.Context) ([]skillstore.ProductionRow, error) {
			return []skillstore.ProductionRow{basePromptRow("PRODUCTION GUIDANCE — the control")}, nil
		},
		SkillTrials: mem,
		SkillVersionByID: func(_ context.Context, id int64) (skillstore.Version, error) {
			return skillstore.Version{ID: 88, SkillName: basePromptRowName, Version: "1.1.0-cand1",
				Status: skillstore.StatusTrial, Body: candBody,
				ContentHash: skillstore.ContentHash(candBody, skillstore.AppliesWhen{})}, nil
		},
	}}
	// Find a ref that deterministically draws arm 0 (not control) so the drill is stable.
	ref := ""
	for _, cand := range []string{"TG-c3b-a", "TG-c3b-b", "TG-c3b-c", "TG-c3b-d", "TG-c3b-e", "TG-c3b-f"} {
		if arm, aerr := skillstore.AssignArm(context.Background(), mem, cand, tr); aerr == nil && arm == 0 {
			ref = cand
			break
		}
	}
	if ref == "" {
		t.Fatal("fixture: no candidate ref drew arm 0 in six tries (1-in-64 odds — the fixture set needs widening)")
	}
	g, entry := a.composeBasePrompt(context.Background(), ref)
	if g != candBody {
		t.Fatalf("the drawn arm's candidate must compose (the booked arm must RUN — TG-218), got %q", g)
	}
	if !strings.Contains(entry, ":store:trial") || !strings.Contains(entry, "/arm0") {
		t.Fatalf("the load entry must book the arm it composed, got %q", entry)
	}
}

// The leak law from the OTHER side: a ClassPrompt row in the row set must never surface in the skill
// guidance block or its load record (NewFromStore's REQ-1316 filter — this pins the pair with
// TestRunbookNeverComposes for the prompt class specifically).
func TestPromptRowNeverLeaksIntoSkillGuidance(t *testing.T) {
	a := &Activities{D: Deps{SkillRows: func(context.Context) ([]skillstore.ProductionRow, error) {
		return []skillstore.ProductionRow{basePromptRow("PROMPT BODY MUST NOT LEAK")}, nil
	}}}
	guidance, loads := a.composeGuidance(context.Background(), "TG-c3b-3", execclass.StandardAgent, skills.DomainUnknown, nil)
	if strings.Contains(guidance, "PROMPT BODY MUST NOT LEAK") {
		t.Fatal("a ClassPrompt row composed into <behavioral_guidance> — the class law (REQ-1316) is broken")
	}
	for _, l := range loads {
		if strings.HasPrefix(l, basePromptRowName+"@") && strings.Contains(l, ":store") {
			t.Fatalf("the skill load record must not book the prompt row (it did not reach the skill block): %v", loads)
		}
	}
}
