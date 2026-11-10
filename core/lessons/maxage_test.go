package lessons

import (
	"testing"
	"time"

	"github.com/territory-grounder/grounder/core/knowledge"
	"github.com/territory-grounder/grounder/core/safety"
)

// The knob has three states an operator must be able to tell apart; a typo must not silently switch decay off.
func TestResolveMaxAgeThreeStates(t *testing.T) {
	cases := []struct {
		raw  string
		want time.Duration
		how  string
	}{
		{"", DefaultMaxAge, "default"},
		{"  ", DefaultMaxAge, "default"},
		{"0", 0, "off"},
		{"off", 0, "off"},
		{"OFF", 0, "off"},
		{"0s", 0, "off"},
		{"720h", 720 * time.Hour, "set"},
		{"36h", 36 * time.Hour, "set"},
		{"banana", DefaultMaxAge, "invalid-using-default"},
		{"-5h", DefaultMaxAge, "invalid-using-default"},
	}
	for _, c := range cases {
		got, how := ResolveMaxAge(c.raw)
		if got != c.want || how != c.how {
			t.Errorf("ResolveMaxAge(%q) = (%s, %s), want (%s, %s)", c.raw, got, how, c.want, c.how)
		}
	}
	if DefaultMaxAge != 90*24*time.Hour {
		t.Fatalf("the owner-accepted default is 90 days, got %s", DefaultMaxAge)
	}
}

func TestStaleFailsTowardRetention(t *testing.T) {
	now := time.Date(2026, 11, 7, 20, 0, 0, 0, time.UTC)
	old := ResolvedIncident{ResolvedAt: now.Add(-100 * 24 * time.Hour)}
	fresh := ResolvedIncident{ResolvedAt: now.Add(-10 * 24 * time.Hour)}
	undatable := ResolvedIncident{}
	if !Stale(old, now, DefaultMaxAge) {
		t.Fatal("100 days old is stale under a 90-day horizon")
	}
	if Stale(fresh, now, DefaultMaxAge) || Stale(undatable, now, DefaultMaxAge) {
		t.Fatal("a fresh or undatable incident is never stale")
	}
	if Stale(old, now, 0) {
		t.Fatal("maxAge 0 (off) prunes nothing")
	}
}

// A recent caution for the same (host, rule, action) contradicts a success; a different action, a different
// subject, an aged-out caution, or a non-caution row does not.
func TestContradictedBySameActionCautionOnly(t *testing.T) {
	now := time.Date(2026, 11, 7, 20, 0, 0, 0, time.UTC)
	ri := ResolvedIncident{ExternalRef: "INC-9", Host: "App01", AlertRule: "Service-up/down", Action: "restart nginx",
		Verdict: safety.VerdictMatch, ConfirmedClear: true, ResolvedAt: now}
	// The caution is built the way PRODUCTION builds it — through Caution(), whose Resolution is prose and whose
	// action rides the action: tag — never hand-shaped (fresh-eyes review of this change, 2026-11-07).
	caution, ok := Caution(ResolvedIncident{ExternalRef: "INC-4", Host: "app01", AlertRule: "Service-up/down", Action: "restart nginx",
		Verdict: safety.VerdictPartial, ConfirmedClear: false, ResolvedAt: now.Add(-2 * 24 * time.Hour)})
	if !ok || caution.Source != knowledge.ProvenanceCaution {
		t.Fatalf("control: a partial outcome distills to a caution, got %+v %v", caution, ok)
	}
	if c, ok := Contradicted(ri, []knowledge.Incident{caution}, now, DefaultMaxAge); !ok || c.CautionRef != "INC-4" || c.Action != "restart nginx" {
		t.Fatalf("a 2-day-old same-action caution contradicts the success; got %+v %v", c, ok)
	}
	// A pre-tag caution (written before this change) still matches on the quoted action in its reflection.
	legacy := caution
	legacy.Tags = nil
	if _, ok := Contradicted(ri, []knowledge.Incident{legacy}, now, DefaultMaxAge); !ok {
		t.Fatal("a caution written before the action: tag must still match on its reflection's quoted action")
	}
	other, _ := Caution(ResolvedIncident{ExternalRef: "INC-5", Host: "app01", AlertRule: "Service-up/down", Action: "start nginx",
		Verdict: safety.VerdictPartial, ResolvedAt: now.Add(-2 * 24 * time.Hour)})
	if _, ok := Contradicted(ri, []knowledge.Incident{other}, now, DefaultMaxAge); ok {
		t.Fatal("a caution about a DIFFERENT action is two facts, not a conflict")
	}
	elsewhere := caution
	elsewhere.Host = "app02"
	if _, ok := Contradicted(ri, []knowledge.Incident{elsewhere}, now, DefaultMaxAge); ok {
		t.Fatal("another host's caution is not this subject's")
	}
	aged := caution
	aged.ResolvedAt = now.Add(-100 * 24 * time.Hour)
	if _, ok := Contradicted(ri, []knowledge.Incident{aged}, now, DefaultMaxAge); ok {
		t.Fatal("a caution older than the horizon no longer contradicts")
	}
	if _, ok := Contradicted(ri, []knowledge.Incident{aged}, now, 0); !ok {
		t.Fatal("with the horizon off every same-action caution counts")
	}
	precedent := caution
	precedent.Source = knowledge.ProvenanceVerifiedResolution
	if _, ok := Contradicted(ri, []knowledge.Incident{precedent}, now, DefaultMaxAge); ok {
		t.Fatal("only a CAUTION row can contradict — a prior precedent agrees")
	}
	undated := caution
	undated.ResolvedAt = time.Time{}
	if _, ok := Contradicted(ri, []knowledge.Incident{undated}, now, DefaultMaxAge); ok {
		t.Fatal("with a horizon set an undatable caution must NOT contradict — nothing prunes the caution lane, so it would suppress learning forever")
	}
	if _, ok := Contradicted(ri, []knowledge.Incident{undated}, now, 0); !ok {
		t.Fatal("with the horizon off an undatable caution counts like any other")
	}
	if _, ok := Contradicted(ResolvedIncident{}, []knowledge.Incident{caution}, now, DefaultMaxAge); ok {
		t.Fatal("an incident with no subject or action cannot be contradicted")
	}
}
