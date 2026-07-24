package governance

import (
	"errors"
	"testing"

	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/temporal"
)

// CreateSchedules is genuinely idempotent: an already-existing schedule is skipped, and the loop still
// reaches the LATER (judge-liveness dead-man) schedule. The naive abort-on-first-error shape silently dropped
// judge-liveness whenever governance-metrics already existed, leaving the judge-death control uninstalled.
func TestCreateSchedulesIdempotent(t *testing.T) {
	// governance-metrics already exists (ErrScheduleAlreadyRunning); judge-liveness does not.
	var created []string
	err := createSchedules(func(opts client.ScheduleOptions) error {
		created = append(created, opts.ID)
		if opts.ID == GovernanceMetricsScheduleID {
			return temporal.ErrScheduleAlreadyRunning
		}
		return nil
	})
	if err != nil {
		t.Fatalf("an already-existing schedule must not be fatal: %v", err)
	}
	// BOTH schedules must have been attempted — the judge-liveness one is not skipped by the first's already-exists.
	if len(created) != 2 || created[1] != JudgeLivenessScheduleID {
		t.Fatalf("the loop must reach the judge-liveness schedule after the first already exists, got %v", created)
	}
	// a genuine (non-already-exists) error still aborts and surfaces.
	boom := errors.New("boom")
	if err := createSchedules(func(client.ScheduleOptions) error { return boom }); !errors.Is(err, boom) {
		t.Fatalf("a real create error must surface, got %v", err)
	}
}
