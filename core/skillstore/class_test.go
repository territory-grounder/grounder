package skillstore

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// REQ-1315: the artifact-class vocabulary is CLOSED — exactly four classes validate; the zero value,
// an unknown token, and a case variant are all outside it (the domain never guesses a class).
func TestValidArtifactClassIsAClosedEnumeration(t *testing.T) {
	for _, c := range []ArtifactClass{ClassSkill, ClassPrompt, ClassRunbook, ClassRubric} {
		if !ValidArtifactClass(c) {
			t.Errorf("%q must be a valid artifact class", c)
		}
	}
	for _, c := range []ArtifactClass{"", "bogus", "Skill", "SKILL", "wiki", "skill "} {
		if ValidArtifactClass(c) {
			t.Errorf("%q must NOT be a valid artifact class (closed enumeration)", c)
		}
	}
}

// REQ-1316: per-class caps are the domain-layer law — skill 8 KiB, prompt 16 KiB, runbook/rubric
// 32 KiB. An UNKNOWN class (the empty-input mutation included) gets 0: every body is refused, never
// admitted under a guessed cap (fail closed).
func TestMaxBodyBytesPerClassAndFailClosed(t *testing.T) {
	cases := []struct {
		class ArtifactClass
		want  int
	}{
		{ClassSkill, 8192},
		{ClassPrompt, 16384},
		{ClassRunbook, 32768},
		{ClassRubric, 32768},
		{"", 0},      // the empty-input mutation: an un-normalized zero value refuses everything
		{"bogus", 0}, // unknown class: no cap is guessed
	}
	for _, c := range cases {
		if got := MaxBodyBytes(c.class); got != c.want {
			t.Errorf("MaxBodyBytes(%q) = %d, want %d", c.class, got, c.want)
		}
	}
}

// REQ-1316: flywheel eligibility is a CLOSED predicate over the class — skill and prompt only.
// rubric and runbook are NEVER trial-eligible, and an unknown/empty class is not eligible either
// (the consuming filter lands with the flywheel-generalization task, TG-475; the vocabulary is law now).
func TestFlywheelEligibleIsSkillAndPromptOnly(t *testing.T) {
	eligible := map[ArtifactClass]bool{
		ClassSkill:   true,
		ClassPrompt:  true,
		ClassRunbook: false,
		ClassRubric:  false,
		"":           false,
		"bogus":      false,
	}
	for c, want := range eligible {
		if got := FlywheelEligible(c); got != want {
			t.Errorf("FlywheelEligible(%q) = %v, want %v", c, got, want)
		}
	}
}

// Back-compat: the absent class IS the skill class — every row that predates the class model is a
// skill, exactly as the schema default says. A stated class passes through untouched.
func TestDefaultClassNormalizesAbsentToSkill(t *testing.T) {
	if got := DefaultClass(""); got != ClassSkill {
		t.Fatalf("DefaultClass(\"\") = %q, want %q", got, ClassSkill)
	}
	for _, c := range []ArtifactClass{ClassSkill, ClassPrompt, ClassRunbook, ClassRubric, "bogus"} {
		if got := DefaultClass(c); got != c {
			t.Fatalf("DefaultClass(%q) = %q — a stated class must pass through for validation, never be rewritten", c, got)
		}
	}
}

// REQ-1316 (the pair oracle at the domain layer): the SAME 8193-byte body is refused for a
// skill-class artifact and admitted for a runbook-class one — the cap is the CLASS's, not the
// schema's. A 16385-byte body draws the line between prompt and runbook the same way.
func TestValidateDraftEnforcesTheClassCapNotTheSchemaCeiling(t *testing.T) {
	ctx := context.Background()
	m := NewMemStore()
	m.PutSkill(Skill{Name: "triage-protocol", Kind: "behavioral", Position: 1}) // absent class = skill
	m.PutSkill(Skill{Name: "base-prompt", Kind: "behavioral", Class: ClassPrompt, Position: 2})
	m.PutSkill(Skill{Name: "disk-full-runbook", Kind: "catalog", Class: ClassRunbook, Position: 3})
	m.PutSkill(Skill{Name: "big-runbook", Kind: "catalog", Class: ClassRunbook, Position: 4}) // rubric-class rows refuse ALL drafts since TG-474; runbook shares the 32 KiB cap

	draft := func(name, body string) error {
		aw := AppliesWhen{}
		_, err := m.CreateVersion(ctx, Version{
			SkillName: name, Version: "1.0.0", Body: body, AppliesWhen: aw,
			ContentHash: ContentHash(body, aw), Author: "operator:test", Source: "hand",
			Rationale: "class-cap oracle",
		})
		return err
	}

	over8k := strings.Repeat("r", 8193)
	if err := draft("triage-protocol", over8k); !errors.Is(err, ErrBodyBounds) {
		t.Fatalf("8193 bytes for class skill must be refused by the DOMAIN cap, got %v", err)
	}
	if err := draft("disk-full-runbook", over8k); err != nil {
		t.Fatalf("the SAME 8193 bytes for class runbook must be admitted, got %v", err)
	}

	over16k := strings.Repeat("r", 16385)
	if err := draft("base-prompt", over16k); !errors.Is(err, ErrBodyBounds) {
		t.Fatalf("16385 bytes for class prompt must be refused, got %v", err)
	}
	if err := draft("big-runbook", over16k); err != nil {
		t.Fatalf("16385 bytes for class runbook must be admitted, got %v", err)
	}

	// The 32 KiB schema ceiling is ALSO the domain's largest cap: over it, every class refuses.
	over32k := strings.Repeat("r", 32769)
	if err := draft("big-runbook", over32k); !errors.Is(err, ErrBodyBounds) {
		t.Fatalf("32769 bytes must be refused even for the largest class, got %v", err)
	}
	// An empty body stays refused for every class (the lower bound did not move).
	if err := draft("disk-full-runbook", ""); !errors.Is(err, ErrBodyBounds) {
		t.Fatalf("an empty body must be refused regardless of class, got %v", err)
	}
}

// REQ-1315: a skill row carrying an UNKNOWN class refuses every draft with the named error — the
// domain layer never falls back to a guessed cap (the schema CHECK is the other half of the refusal).
func TestValidateDraftRefusesAnUnknownClass(t *testing.T) {
	ctx := context.Background()
	m := NewMemStore()
	// Simulate an out-of-band row whose class the code does not know (the MemStore analog of a raw
	// SQL write around the CHECK — e.g. a vocabulary added by a newer schema than this binary).
	m.putSkillRaw(Skill{Name: "mystery", Kind: "behavioral", Class: "wiki", Position: 9})
	aw := AppliesWhen{}
	body := "harmless body"
	_, err := m.CreateVersion(ctx, Version{
		SkillName: "mystery", Version: "1.0.0", Body: body, AppliesWhen: aw,
		ContentHash: ContentHash(body, aw), Author: "operator:test", Source: "hand",
		Rationale: "unknown-class oracle",
	})
	if !errors.Is(err, ErrUnknownClass) {
		t.Fatalf("a draft against an unknown-class skill must be refused with ErrUnknownClass, got %v", err)
	}
}
