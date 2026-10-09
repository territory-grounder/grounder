package policy

import (
	"context"
	"errors"
	"testing"
)

// The in-memory ruleset store fails closed: absent until saved, and a malformed document is REFUSED (never
// persisted as the active policy — the prior known-good ruleset stands, INV-09/REQ-1503).
func TestMemRulesetStore_FailClosed(t *testing.T) {
	ctx := context.Background()
	s := NewMemRulesetStore()

	// Absent until saved.
	if _, _, err := s.Load(ctx); !errors.Is(err, ErrRulesetAbsent) {
		t.Fatalf("empty store Load = %v, want ErrRulesetAbsent", err)
	}

	// A malformed document is refused and NOT persisted (the store stays absent).
	if _, err := s.Save(ctx, []byte(`{"rules":[{"id":"bad","verdict":"nope"}]}`), "admin"); !errors.Is(err, ErrMalformedRule) {
		t.Fatalf("malformed Save = %v, want ErrMalformedRule", err)
	}
	if _, _, err := s.Load(ctx); !errors.Is(err, ErrRulesetAbsent) {
		t.Fatalf("after refused Save, Load = %v, want still ErrRulesetAbsent", err)
	}

	// A valid document round-trips: the parsed RuleSet and the raw document both come back.
	doc := []byte(`{"rules":[{"id":"ok","verdict":"deny","match":{"argv_pattern":"rm -rf"}}]}`)
	rs, err := s.Save(ctx, doc, "admin")
	if err != nil {
		t.Fatalf("valid Save: %v", err)
	}
	if len(rs.Rules) != 1 || rs.Rules[0].ID != "ok" {
		t.Fatalf("Save returned %+v, want one rule 'ok'", rs.Rules)
	}
	got, raw, err := s.Load(ctx)
	if err != nil || len(got.Rules) != 1 || string(raw) != string(doc) {
		t.Fatalf("Load = (%+v, %q, %v), want the persisted rule + raw document", got, raw, err)
	}

	// A primed load error surfaces (fail-closed path).
	boom := errors.New("boom")
	if _, _, err := s.WithLoadError(boom).Load(ctx); !errors.Is(err, boom) {
		t.Fatalf("primed load error = %v, want boom", err)
	}
}
