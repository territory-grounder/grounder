package manifestwrite

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"go.temporal.io/sdk/testsuite"

	"github.com/territory-grounder/grounder/core/audit"
	"github.com/territory-grounder/grounder/core/estate"
	"github.com/territory-grounder/grounder/core/worldmodel"
	"github.com/territory-grounder/grounder/temporal/skillwrite"
)

type fakeLoader struct {
	e     worldmodel.Entry
	found bool
	err   error
}

func (f fakeLoader) EntryByID(_ context.Context, _ int64) (worldmodel.Entry, bool, error) {
	return f.e, f.found, f.err
}

type fakeStore struct {
	updated []worldmodel.Entry
	err     error
}

func (s *fakeStore) UpdateEntry(_ context.Context, e worldmodel.Entry) error {
	if s.err != nil {
		return s.err
	}
	s.updated = append(s.updated, e)
	return nil
}
func (s *fakeStore) ApprovedEntries(_ context.Context) ([]worldmodel.Entry, error) { return nil, nil }

type fakeLedger struct {
	entries []audit.GovDecision
	seq     int64
}

func (l *fakeLedger) Append(d audit.GovDecision) (audit.LedgerEntry, error) {
	l.seq++
	l.entries = append(l.entries, d)
	return audit.LedgerEntry{Seq: l.seq}, nil
}

func draftUnit() worldmodel.Entry {
	return worldmodel.Entry{
		ID: 7, EntityType: estate.TypeService, Name: "mealie.service", Host: "dc1mealie01",
		Source: estate.SourceDeclared, Confidence: 0.85, Status: worldmodel.StatusDraft,
		LastSeenAt: time.Date(2026, 7, 31, 12, 0, 0, 0, time.UTC),
	}
}

func acts(l Loader, st worldmodel.Store, lg worldmodel.Ledger) *Activities {
	return &Activities{D: Deps{Loader: l, Store: st, Ledger: lg}}
}

// TestAdoptionAppendsToTheLedgerBeforeTheRowAndNamesTheApprover pins the ordering the whole audit story
// rests on: the ledger entry exists before the row says "approved", so a crash between them leaves a
// grant that is EXPLAINED but not yet in force — never in force but unexplained.
func TestAdoptionAppendsToTheLedgerBeforeTheRowAndNamesTheApprover(t *testing.T) {
	st, lg := &fakeStore{}, &fakeLedger{}
	res, err := acts(fakeLoader{e: draftUnit(), found: true}, st, lg).
		ManifestTransitionActivity(context.Background(), Request{
			EntryID: 7, To: worldmodel.StatusApproved, Rationale: "mealie is ours; TG may restart it", Approver: "zoe",
		})
	if err != nil {
		t.Fatalf("adopt: %v", err)
	}
	if len(lg.entries) != 1 {
		t.Fatalf("exactly one ledger append per transition, got %d", len(lg.entries))
	}
	if len(st.updated) != 1 || st.updated[0].Status != worldmodel.StatusApproved {
		t.Fatalf("the row must be updated to approved, got %+v", st.updated)
	}
	if st.updated[0].LedgerSeq != lg.seq {
		t.Errorf("the row must carry the ledger seq that explains it: row %d, ledger %d", st.updated[0].LedgerSeq, lg.seq)
	}
	if st.updated[0].Approver != "zoe" {
		t.Errorf("the SERVER-derived approver must be persisted on the row, got %q", st.updated[0].Approver)
	}
	if !strings.Contains(lg.entries[0].Reason, "zoe") {
		t.Errorf("the ledger must name who ordered the grant, got %q", lg.entries[0].Reason)
	}
	if lg.entries[0].Withheld {
		t.Error("adoption WIDENS — it must not be recorded as withheld")
	}
	if res.EntryID != 7 || res.Status != worldmodel.StatusApproved || res.LedgerSeq != lg.seq {
		t.Errorf("result must echo what was actually written: %+v", res)
	}
}

// TestOnlyAdoptionIsRecordedAsWidening proves reject and retire ride the ledger as WITHHELD. An operator
// auditing "what did we grant?" reads that flag; a retire mis-recorded as widening would read as a grant.
func TestOnlyAdoptionIsRecordedAsWidening(t *testing.T) {
	for _, tc := range []struct {
		to   worldmodel.Status
		from worldmodel.Status
	}{
		{worldmodel.StatusRejected, worldmodel.StatusDraft},
		{worldmodel.StatusRetired, worldmodel.StatusApproved},
	} {
		e := draftUnit()
		e.Status = tc.from
		lg := &fakeLedger{}
		if _, err := acts(fakeLoader{e: e, found: true}, &fakeStore{}, lg).
			ManifestTransitionActivity(context.Background(), Request{
				EntryID: 7, To: tc.to, Rationale: "not ours", Approver: "zoe",
			}); err != nil {
			t.Fatalf("%s: %v", tc.to, err)
		}
		if !lg.entries[0].Withheld {
			t.Errorf("%s narrows or withholds — it must not be recorded as a widening", tc.to)
		}
	}
}

// TestUnknownEntryIsADecisionNotAFault proves a missing row returns the typed ErrNotFound the surface maps
// to 404. Two operators reviewing the same queue is normal; the second must be told the row is gone.
func TestUnknownEntryIsADecisionNotAFault(t *testing.T) {
	st, lg := &fakeStore{}, &fakeLedger{}
	_, err := acts(fakeLoader{found: false}, st, lg).ManifestTransitionActivity(context.Background(),
		Request{EntryID: 404, To: worldmodel.StatusApproved, Rationale: "why", Approver: "zoe"})
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
	if len(lg.entries) != 0 || len(st.updated) != 0 {
		t.Error("a refused transition must touch neither the ledger nor the row")
	}
}

// TestRefusalsSurfaceVerbatimAndWriteNothing pins the state machine's "no" reaching the caller as its own
// typed sentinel — and, critically, that a REFUSED transition leaves no ledger entry. A rejected row that
// nonetheless appended to the chain would make the audit trail describe a grant that never happened.
func TestRefusalsSurfaceVerbatimAndWriteNothing(t *testing.T) {
	terminal := draftUnit()
	terminal.Status = worldmodel.StatusRejected // terminal: a rework is a NEW draft from discovery
	for name, tc := range map[string]struct {
		entry worldmodel.Entry
		req   Request
		want  error
	}{
		"terminal status": {terminal,
			Request{To: worldmodel.StatusApproved, Rationale: "reconsidered", Approver: "zoe"},
			worldmodel.ErrBadTransition},
		"missing rationale": {draftUnit(),
			Request{To: worldmodel.StatusApproved, Rationale: "   ", Approver: "zoe"},
			worldmodel.ErrRationaleRequired},
	} {
		st, lg := &fakeStore{}, &fakeLedger{}
		_, err := acts(fakeLoader{e: tc.entry, found: true}, st, lg).
			ManifestTransitionActivity(context.Background(), tc.req)
		if !errors.Is(err, tc.want) {
			t.Errorf("%s: want %v, got %v", name, tc.want, err)
		}
		if len(lg.entries) != 0 || len(st.updated) != 0 {
			t.Errorf("%s: a refused transition must write nothing (ledger %d, rows %d)",
				name, len(lg.entries), len(st.updated))
		}
	}
}

// TestWorkflowDoesNotRetryARefusal proves the workflow surfaces a state-machine "no" once rather than
// re-attempting it. A retry here would re-run the load against a row the first attempt may already have
// moved — turning one operator click into an unpredictable number of ledger appends.
func TestWorkflowDoesNotRetryARefusal(t *testing.T) {
	var wts testsuite.WorkflowTestSuite
	env := wts.NewTestWorkflowEnvironment()
	// A REAL activity over a counting loader — not a mock. The count is the number of times the worker
	// actually reached the store, which is the thing that must not multiply.
	counter := &countingLoader{e: rejectedUnit(), found: true}
	env.RegisterActivity(acts(counter, &fakeStore{}, &fakeLedger{}))
	env.ExecuteWorkflow(ManifestTransitionWorkflow,
		Request{EntryID: 7, To: worldmodel.StatusApproved, Rationale: "reconsidered", Approver: "zoe"})
	if !env.IsWorkflowCompleted() {
		t.Fatal("workflow did not complete")
	}
	if env.GetWorkflowError() == nil {
		t.Fatal("a refused transition must fail the workflow, not report success")
	}
	if counter.calls != 1 {
		t.Errorf("a DECISION must be attempted exactly once, got %d attempts", counter.calls)
	}
}

type countingLoader struct {
	e     worldmodel.Entry
	found bool
	calls int
}

func (c *countingLoader) EntryByID(_ context.Context, _ int64) (worldmodel.Entry, bool, error) {
	c.calls++
	return c.e, c.found, nil
}

func rejectedUnit() worldmodel.Entry {
	e := draftUnit()
	e.Status = worldmodel.StatusRejected // terminal — the state machine refuses any move off it
	return e
}

// TestManifestActivityNameDoesNotCollideWithSkillwrite is the OTHER half of the 2026-07-17 boot-loop
// guard. temporal/skilltrial's names test covers WORKFLOW names only, but Temporal registers ACTIVITIES
// by bare method name too — and skillwrite.Activities already exports TransitionActivity. Had this
// package kept the obvious name, the real worker would panic at boot with both write lanes wired.
//
// WHY REFLECTION AND NOT A TEST WORKER: the SDK's test environment silently OVERWRITES a duplicate
// activity registration (probed directly on v1.34.0 — struct and method-value forms alike; only
// WORKFLOW duplicates panic there). A testsuite-based activity-collision guard is therefore blind by
// construction, which is exactly how the first draft of this test passed while the property was broken.
// So the property is asserted where it actually lives: the exported method-name sets must be disjoint.
func TestManifestActivityNameDoesNotCollideWithSkillwrite(t *testing.T) {
	names := func(v interface{}) map[string]bool {
		out := map[string]bool{}
		typ := reflect.TypeOf(v)
		for i := 0; i < typ.NumMethod(); i++ {
			out[typ.Method(i).Name] = true
		}
		if len(out) == 0 {
			t.Fatalf("%T exports no activity methods — this guard would pass vacuously", v)
		}
		return out
	}
	mine, theirs := names(&Activities{}), names(&skillwrite.Activities{})
	for n := range mine {
		if theirs[n] {
			t.Errorf("activity name %q is exported by BOTH manifestwrite and skillwrite — Temporal registers "+
				"by bare method name, so the worker panics at boot once both write lanes are wired", n)
		}
	}
}
