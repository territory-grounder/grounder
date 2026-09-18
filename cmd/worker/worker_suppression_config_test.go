package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/territory-grounder/grounder/core/suppression"
)

// Phase-0b characterization tests (TG-501): lock the behavior of the suppression-config parsers BEFORE
// they are carved out of main.go, and pay down core/suppression's thin coverage (0.72) at this seam.
// suppressPatterns/suppressSchedules/foldPolicies had NO direct test. Each pins an observable contract +
// its fail-closed guard, so the upcoming file-move is provably behavior-preserving.

func TestSuppressPatternsSkipsRowsWithoutAlertRule(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "patterns.json")
	if err := os.WriteFile(p, []byte(`[{"alert_rule":"HostDown","estate":"nl","confidence":0.9},{"alert_rule":"","estate":"gr"},{"estate":"x","confidence":0.5}]`), 0o644); err != nil {
		t.Fatal(err)
	}
	got := suppressPatterns(p)
	if len(got) != 1 {
		t.Fatalf("expected 1 pattern (rows with an empty alert_rule are skipped), got %d: %+v", len(got), got)
	}
	if got[0].AlertRule != "HostDown" || got[0].Estate != "nl" || got[0].Confidence != 0.9 {
		t.Errorf("pattern did not round-trip: %+v", got[0])
	}
	// Fail-soft: an empty path, an unreadable file, and malformed JSON all yield nil (no suppression), never a panic.
	if suppressPatterns("") != nil {
		t.Error("empty path must yield nil")
	}
	if suppressPatterns(filepath.Join(dir, "does-not-exist.json")) != nil {
		t.Error("unreadable file must yield nil")
	}
	bad := filepath.Join(dir, "bad.json")
	_ = os.WriteFile(bad, []byte("{not json"), 0o644)
	if suppressPatterns(bad) != nil {
		t.Error("malformed JSON must yield nil, not a partial/panic")
	}
}

func TestSuppressSchedulesSkipsRowsWithoutHostOrCron(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "sched.json")
	if err := os.WriteFile(p, []byte(`[{"host":"web01","cron":"0 3 * * *","timezone":"Europe/Athens","valid_from":"2026-01-01T00:00:00Z"},{"host":"","cron":"0 4 * * *"},{"host":"db01","cron":""}]`), 0o644); err != nil {
		t.Fatal(err)
	}
	got := suppressSchedules(p)
	if len(got) != 1 {
		t.Fatalf("expected 1 schedule (rows missing host OR cron are skipped), got %d: %+v", len(got), got)
	}
	s := got[0]
	if s.Host != "web01" || s.Cron != "0 3 * * *" || s.Timezone != "Europe/Athens" {
		t.Errorf("schedule fields wrong: %+v", s)
	}
	// An operator declaration is authorized-at-source: it registers LIVE and declared, never observe-before-live.
	if s.Source != suppression.SourceDeclared || s.Status != suppression.SchLive || s.Kind != "declared" {
		t.Errorf("declared schedule must be Source=declared/Status=live/Kind=declared, got source=%v status=%v kind=%q", s.Source, s.Status, s.Kind)
	}
	if s.ValidFrom.IsZero() {
		t.Error("valid_from RFC3339 must parse into the schedule window")
	}
}

func TestFoldPoliciesRefusesCatchAll(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "folds.json")
	// A real child fold + a catch-all (must be REFUSED) + an empty row (skipped).
	if err := os.WriteFile(p, []byte(`[{"host":"child01","rule":"PodRestart","site":"nl"},{"host":"*","rule":"*"},{"host":"","rule":""}]`), 0o644); err != nil {
		t.Fatal(err)
	}
	got := foldPolicies(p)
	if len(got) != 1 {
		t.Fatalf("catch-all (host=* rule=*) must be REFUSED and empty rows skipped — expected 1 policy, got %d: %+v", len(got), got)
	}
	if got[0].HostScope != "child01" || got[0].RuleScope != "PodRestart" || got[0].Site != "nl" {
		t.Errorf("fold policy fields wrong: %+v", got[0])
	}
	// A declared fold is verified-at-load (LastVerifiedAt = now), so only its valid window gates it.
	if got[0].LastVerifiedAt.IsZero() {
		t.Error("a declared fold policy must be stamped verified-at-load (LastVerifiedAt != zero)")
	}
	// The estate-wide-silence guard: a lone catch-all yields ZERO policies — a config slip cannot fold the whole estate to notices.
	only := filepath.Join(dir, "only.json")
	_ = os.WriteFile(only, []byte(`[{"host":"*","rule":"*"}]`), 0o644)
	if n := len(foldPolicies(only)); n != 0 {
		t.Errorf("a lone catch-all fold must yield ZERO policies, got %d — the estate-wide-silence guard is broken", n)
	}
}
