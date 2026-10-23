package model

import (
	"context"
	"errors"
	"testing"
)

// TG-48: MaxTokens caps ONE call; SessionTokenBudget caps the CUMULATIVE output tokens ONE session spends
// across all of its completions. A looping session (the pve03 cascade shape) can make many individually-in-
// budget calls and still spend without bound; the daily cost breaker only trips globally, after the fact, with
// no per-session attribution. Keyed on the per-session `user` ("runner:"+external_ref, activities.go), once a
// session's cumulative completion tokens reach the ceiling its NEXT completion is refused with
// ErrSessionTokenBudget — fail-safe (an INCOMPLETE investigation, never an empty one).
//
// Killing mutation: delete the sessionOverBudget refuse block in CompleteWithUsage → A's over-budget call
// succeeds → RED.
func TestSessionTokenBudgetRefusesAfterCeiling(t *testing.T) {
	g, closeSrv := gatewayFor(t, 200, okBody(`{"prompt_tokens":10,"completion_tokens":60,"total_tokens":70}`))
	defer closeSrv()
	g.SetSessionTokenBudget(100) // ceiling: 100 cumulative output tokens per session

	msgs := []Message{{Role: "user", Content: "hi"}}
	// Session A: call 1 (0 spent < 100) and call 2 (60 spent < 100) succeed, accruing to 60 then 120.
	if _, err := g.Complete(context.Background(), "runner:A", "fast", msgs); err != nil {
		t.Fatalf("A call 1 must succeed (0 spent), got %v", err)
	}
	if _, err := g.Complete(context.Background(), "runner:A", "fast", msgs); err != nil {
		t.Fatalf("A call 2 must succeed (60 spent < 100), got %v", err)
	}
	// Call 3: A has now accrued 120 >= 100 → refused with the typed budget error.
	if _, err := g.Complete(context.Background(), "runner:A", "fast", msgs); err == nil {
		t.Fatalf("A call 3 must be REFUSED (120 spent >= budget 100) — the session budget did not bind")
	} else if !errors.Is(err, ErrSessionTokenBudget) {
		t.Fatalf("A call 3 error must be ErrSessionTokenBudget, got %v", err)
	}
	// A DIFFERENT session is unaffected — the budget is PER-SESSION, not global.
	if _, err := g.Complete(context.Background(), "runner:B", "fast", msgs); err != nil {
		t.Fatalf("session B (0 spent) must be unaffected by A's exhausted budget, got %v", err)
	}
}

// A 0 budget (unset TG_MODEL_SESSION_TOKEN_BUDGET) is UNBOUNDED — today's behaviour, byte-identical.
func TestSessionTokenBudgetZeroIsUnbounded(t *testing.T) {
	g, closeSrv := gatewayFor(t, 200, okBody(`{"prompt_tokens":10,"completion_tokens":1000,"total_tokens":1010}`))
	defer closeSrv()
	// no SetSessionTokenBudget ⇒ 0 ⇒ unbounded
	msgs := []Message{{Role: "user", Content: "hi"}}
	for i := 0; i < 5; i++ {
		if _, err := g.Complete(context.Background(), "runner:X", "fast", msgs); err != nil {
			t.Fatalf("with no budget set, call %d must succeed (unbounded), got %v", i, err)
		}
	}
}
