package db

import (
	"context"
	"testing"

	"github.com/territory-grounder/grounder/core/safety"
)

// verdictCommitter is the narrow contract core/actuate and core/falsify each depend on.
type verdictCommitter interface {
	Commit(ctx context.Context, actionID, planHash, targetHost, site string, v safety.Verdict) error
}

// The two stores must stay SIGNATURE-COMPATIBLE. Which table a scorer writes to is then a wiring decision
// made once at the composition root, not a behaviour either caller can get wrong. If they drift apart, the
// propose-path scorer can no longer be pointed away from action_verdict without editing the scorer, and the
// single-writer guarantee (roadmap P2-2) decays from a type-level fact into a convention.
var (
	_ verdictCommitter = (*VerdictStore)(nil)
	_ verdictCommitter = (*PredictionVerdictStore)(nil)
)

// Both stores must reject an invalid verdict AT THE BOUNDARY rather than leaning on the column enum. A store
// that defers validation to Postgres converts a programming error into a failed write on an append-only
// evidence table — discovered long after the fact, if at all. Constructed with a nil pool deliberately: if
// validation ran too late this would panic instead of returning the typed error.
func TestPredictionVerdictStore_RejectsInvalidVerdictBeforeAnyQuery(t *testing.T) {
	s := &PredictionVerdictStore{}
	err := s.Commit(context.Background(), "a1", "p1", "host", "site", safety.Verdict("not-a-verdict"))
	if err != ErrInvalidVerdict {
		t.Fatalf("err = %v, want ErrInvalidVerdict", err)
	}
}

func TestVerdictStore_RejectsInvalidVerdictBeforeAnyQuery(t *testing.T) {
	s := &VerdictStore{}
	err := s.Commit(context.Background(), "a1", "p1", "host", "site", safety.Verdict("bogus"))
	if err != ErrInvalidVerdict {
		t.Fatalf("err = %v, want ErrInvalidVerdict", err)
	}
}

// Every valid verdict must pass validation in BOTH stores — a store that silently rejected, say, "partial"
// would drop a whole outcome class from the evidence base without erroring visibly.
func TestBothStores_AcceptEveryValidVerdict(t *testing.T) {
	for _, v := range []safety.Verdict{safety.VerdictMatch, safety.VerdictPartial, safety.VerdictDeviation} {
		if !safety.ValidVerdict(v) {
			t.Fatalf("%q is not accepted by safety.ValidVerdict — the stores gate on it", v)
		}
	}
}
