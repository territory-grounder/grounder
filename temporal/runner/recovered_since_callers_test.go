package runner

import (
	"context"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"
)

// EVERY CALLER OF RecoveredSinceActivity MUST SUPPLY AlertRule, BECAUSE THE CALLEE FAILS CLOSED WITHOUT IT.
//
// RecoveredSinceActivity returns (false, nil) when AlertRule is empty — deliberately, so an unrelated
// flapping rule on the same host cannot confirm THIS incident's recovery. That fail-closed choice is correct
// and must stay. Its consequence is that a caller which omits the field gets a permanent, silent "not
// recovered": no error, no retry, no log.
//
// That happened. The poll-obsolete branch called it with {Host, Since} only, so the recheck ALWAYS answered
// false, `obsoleted` never became true, and every POLL_PAUSE decision parked until VoteWait expired. The
// sibling call at the clear-confirm belt had always passed AlertRule; the two disagreed and nothing noticed.
//
// Measured live 2026-07-29: 138 open decisions, oldest ~4,000 sessions stale, governance ledger tail entirely
// `human:timeout`, every target guest already running for 6–12h. The self-recovery closure had never once run.
//
// THE CALL SITES ARE READ FROM THE SOURCE, not listed here. A hand-kept list of callers is maintained by
// whoever adds the next caller — the same person who would forget the field.
func TestEveryRecoveredSinceCallerPassesAnAlertRule(t *testing.T) {
	src, err := os.ReadFile("workflow.go")
	if err != nil {
		t.Fatalf("cannot read workflow.go: %v", err)
	}
	body := string(src)

	// Each construction of the input literal, with whatever fields it sets.
	lit := regexp.MustCompile(`RecoveredSinceInput\{[^}]*\}`)
	found := lit.FindAllString(body, -1)
	if len(found) < 2 {
		t.Fatalf("found %d RecoveredSinceInput literal(s) in workflow.go, expected at least 2 (the "+
			"poll-obsolete recheck and the clear-confirm belt). The pattern no longer matches the "+
			"construction form, so this oracle would pass vacuously", len(found))
	}
	for _, l := range found {
		if !strings.Contains(l, "AlertRule:") {
			t.Errorf("a RecoveredSinceInput is built WITHOUT AlertRule:\n  %s\n"+
				"RecoveredSinceActivity fails closed on an empty AlertRule, so this call can only ever answer "+
				"\"not recovered\" — silently, forever. That is what parked 138 decisions until they timed out.", l)
		}
	}
}

// THE CALLEE'S FAIL-CLOSED GUARD MUST STAY. If a future change made an empty AlertRule acceptable, the caller
// bug above would stop mattering and a DIFFERENT, worse one would open: an unrelated rule's recovery
// confirming an incident whose own rule is still firing.
func TestRecoveredSinceRefusesAnUnscopedRead(t *testing.T) {
	called := false
	a := &Activities{D: Deps{
		RecoveredSince: func(_ context.Context, host, rule string, _ time.Time) (bool, error) {
			called = true
			return true, nil // would confirm ANY read that reaches it
		},
	}}

	got, err := a.RecoveredSinceActivity(context.Background(),
		RecoveredSinceInput{Host: "dc1mealie01", Since: time.Now()}) // no AlertRule
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got {
		t.Error("an UNSCOPED read (no AlertRule) reported recovered — an unrelated flapping rule on the same " +
			"host would then close an incident whose own rule is still firing")
	}
	if called {
		t.Error("the belt was consulted despite having no rule to scope it to — the guard must refuse BEFORE " +
			"the read, not filter after it")
	}

	// The converse, so this cannot pass by refusing everything: a scoped read IS performed and honoured.
	called = false
	got, err = a.RecoveredSinceActivity(context.Background(),
		RecoveredSinceInput{Host: "dc1mealie01", AlertRule: "Device-Down", Since: time.Now()})
	if err != nil {
		t.Fatalf("unexpected error on the scoped read: %v", err)
	}
	if !called {
		t.Error("a fully-scoped read never reached the belt — the guard is refusing valid reads, which would " +
			"disable self-recovery closure just as thoroughly as the missing field did")
	}
	if !got {
		t.Error("a scoped read whose belt answered TRUE was reported as not-recovered")
	}
}
