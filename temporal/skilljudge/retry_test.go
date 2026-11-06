package skilljudge

import (
	"testing"
	"time"
)

// The batch retry waits out the model breaker's default cooldown (60s) so the second attempt is the half-open
// probe that re-closes a circuit a transient (a deploy in the cron's second, TG-593) opened — three attempts, flat.
func TestJudgeRetryOutlivesTheBreakerCooldown(t *testing.T) {
	opts := judgeActivityOptions() // the value JudgeWorkflow runs under, not a constant beside it
	p := opts.RetryPolicy
	if p == nil || p.MaximumAttempts < 3 {
		t.Fatalf("want at least three attempts, got %+v", p)
	}
	if p.InitialInterval <= DefaultBreakerCooldown {
		t.Fatalf("the retry interval must exceed the default breaker cooldown of %s, got %s", DefaultBreakerCooldown, p.InitialInterval)
	}
	if worst := time.Duration(p.MaximumAttempts)*opts.StartToCloseTimeout + time.Duration(p.MaximumAttempts-1)*p.InitialInterval; worst >= 2*time.Hour {
		t.Fatalf("three attempts must fit inside the two-hour cron period, worst case %s", worst)
	}
	if p.BackoffCoefficient != 1.0 {
		t.Fatalf("a flat interval keeps the third attempt inside the activity budget, got coefficient %v", p.BackoffCoefficient)
	}
}
