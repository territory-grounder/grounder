package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/territory-grounder/grounder/core/knowledge"
	"github.com/territory-grounder/grounder/core/lessons"
	"github.com/territory-grounder/grounder/core/safety"
	"github.com/territory-grounder/grounder/temporal/runner"
)

// TG-579 review finding 1: compose forwards TG_REFLECT_MODEL as a PRESENT-BUT-EMPTY variable, and getenv's
// LookupEnv semantics return "" for it — the "primary" default must apply to the empty string, or every
// reflection hands the gateway an empty model name it refuses. Finding 2: the reflector is OFF by default.
func TestReflectConfigDefaultsAndOptIn(t *testing.T) {
	t.Setenv("TG_REFLECT_MODEL", "")
	t.Setenv("TG_REFLECT_ENABLE", "")
	t.Setenv("TG_REFLECT_TIMEOUT_SECONDS", "")
	enabled, tier, timeout := reflectConfig()
	if enabled {
		t.Fatal("the reflector must be OFF unless TG_REFLECT_ENABLE is set (ship-inert)")
	}
	if tier != "primary" {
		t.Fatalf("a present-but-empty TG_REFLECT_MODEL must fall back to primary, got %q", tier)
	}
	if timeout != 25*time.Second {
		t.Fatalf("a present-but-empty timeout must fall back to 25s, got %s", timeout)
	}
	t.Setenv("TG_REFLECT_ENABLE", "1")
	t.Setenv("TG_REFLECT_MODEL", " judge ")
	t.Setenv("TG_REFLECT_TIMEOUT_SECONDS", "12")
	if enabled, tier, timeout = reflectConfig(); !enabled || tier != "judge" || timeout != 12*time.Second {
		t.Fatalf("explicit knobs must be honoured, got enabled=%v tier=%q timeout=%s", enabled, tier, timeout)
	}
}

// TG-597: compose forwards TG_SKILL_EXPERIENCE_MIN_CONFIDENCE present-but-EMPTY; the boot line printed
// `confidence >=  join` because getenv kept the empty value. The threshold the log names must be the one the
// source is built with, and an unknown word must land on the source's own default.
func TestExperienceMinConfidenceDefaultsWhenPresentButEmpty(t *testing.T) {
	for _, tc := range []struct{ name, env, want, ignored string }{
		{"present-but-empty (what compose forwards)", "", "medium", ""},
		{"blank", "   ", "medium", ""},
		{"cased and padded", " LOW ", "low", ""},
		{"cased", "High", "high", ""},
		{"exact", "medium", "medium", ""},
		{"unknown word is ignored AND named", "hgih", "medium", "hgih"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bootCfg.Store(nil) // getenv consults the boot-config file first; this test is about the env path
			t.Setenv("TG_SKILL_EXPERIENCE_MIN_CONFIDENCE", tc.env)
			got, ignored := experienceMinConfidence()
			if got != tc.want || ignored != tc.ignored {
				t.Fatalf("TG_SKILL_EXPERIENCE_MIN_CONFIDENCE=%q: got (%q, ignored %q), want (%q, ignored %q)", tc.env, got, ignored, tc.want, tc.ignored)
			}
		})
	}
}

// An unarmed reflector leaves the seam nil even with a DB and a gateway present.
func TestComplementSeamsLeaveTheReflectorNilWhenNotEnabled(t *testing.T) {
	t.Setenv("TG_REFLECT_ENABLE", "")
	var deps runner.Deps
	wireComplementSeams(&deps, nil, "", nil)
	if deps.Reflect != nil {
		t.Fatal("TG_REFLECT_ENABLE unset must leave Deps.Reflect nil")
	}
	if deps.LearnCaution != nil {
		t.Fatal("no caution lane configured must leave Deps.LearnCaution nil")
	}
}

// TG-579 review finding 6: a corrupt caution lane is REFUSED, never rewritten as "one entry"; a healthy lane
// gains the caution and the holder reloads; a clean success is an idempotent no-op.
func TestCautionWritebackRefusesACorruptLaneAndWritesAHealthyOne(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "caution.json")
	if err := knowledge.WriteCorpusFile(path, nil); err != nil {
		t.Fatal(err)
	}
	holder := newCautionHolder(path, func(string, ...any) {})
	if holder == nil {
		t.Fatal("a readable (empty) lane must yield a holder")
	}
	var deps runner.Deps
	wireCautionWriteback(&deps, holder, path)
	if deps.LearnCaution == nil {
		t.Fatal("a configured lane must arm the live writer")
	}
	deviated := lessons.ResolvedIncident{ExternalRef: "TG-579-c1", Host: "web01", AlertRule: "NginxDown", Action: "restart-service",
		Verdict: safety.VerdictDeviation, ResolvedAt: time.Now().UTC()}
	if err := deps.LearnCaution(context.Background(), deviated); err != nil {
		t.Fatalf("a healthy lane must take the caution: %v", err)
	}
	body, _ := os.ReadFile(path)
	if !strings.Contains(string(body), "TG-579-c1") {
		t.Fatalf("the caution must be persisted in the lane file, got %s", body)
	}
	// A clean success is never a caution: nothing changes.
	before := string(body)
	clean := deviated
	clean.ExternalRef, clean.Verdict, clean.ConfirmedClear = "TG-579-ok", safety.VerdictMatch, true
	if err := deps.LearnCaution(context.Background(), clean); err != nil {
		t.Fatal(err)
	}
	if after, _ := os.ReadFile(path); string(after) != before {
		t.Fatal("a confirmed-clean match must leave the caution lane untouched")
	}
	// Corrupt the lane: the writer must refuse rather than overwrite what did not parse.
	if err := os.WriteFile(path, []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	second := deviated
	second.ExternalRef = "TG-579-c2"
	if err := deps.LearnCaution(context.Background(), second); err == nil || !strings.Contains(err.Error(), "refusing to overwrite") {
		t.Fatalf("a corrupt lane must be refused, got %v", err)
	}
	if after, _ := os.ReadFile(path); string(after) != "{not json" {
		t.Fatal("a refused write must leave the corrupt lane byte-identical for the operator to inspect")
	}
}
