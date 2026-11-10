package ledgeranchor

import (
	"context"
	"errors"
	"strings"
	"testing"

	commonpb "go.temporal.io/api/common/v1"
	workflowpb "go.temporal.io/api/workflow/v1"
	"go.temporal.io/api/workflowservice/v1"
	"go.temporal.io/sdk/client"

	"github.com/territory-grounder/grounder/core/audit"
)

type fakeLister struct {
	witnesses []audit.Anchor
	err       error
}

func (f fakeLister) ListWitnesses(context.Context, string, int) ([]audit.Anchor, error) {
	return f.witnesses, f.err
}

type fakeAnchorsList []audit.Anchor

func (f fakeAnchorsList) Anchors(context.Context) ([]audit.Anchor, error) { return f, nil }

// A WHOLE-DATABASE ROLLBACK (TG-674): the dump restored a chain of 100 rows and an anchor history that stops at
// seq 100, but Temporal's own history remembers witnessing seq 120. The DB-list pass sees nothing to contradict;
// the own-history walk catches it. KILLING MUTATION: drop the Lister pass in Run and the first case passes clean.
func TestOwnHistoryWalkDetectsARollbackTheDBListCannotSee(t *testing.T) {
	entries := chainOfN(t, 100)
	dbAnchors := []audit.Anchor{{Seq: 100, Hash: entries[99].Hash, Digest: "d100", WindowSize: 1}}
	tr := &fakeTemporalReader{witnessed: map[string]audit.Anchor{}}
	tr.witness(audit.DomainGovernanceLedger, dbAnchors[0])

	// Consistent: Temporal remembers exactly what the DB and the chain hold.
	job := TemporalVerifyJob{Anchors: fakeAnchorsList(dbAnchors), Ledger: fakeChain{entries: entries}, Temporal: tr, Domain: audit.DomainGovernanceLedger,
		Lister: fakeLister{witnesses: dbAnchors}}
	if err, ok := job.Run(context.Background()); err != nil || !ok {
		t.Fatalf("consistent: want clean, got err=%v ok=%v", err, ok)
	}

	// Rolled back: Temporal also remembers seq 120, which the chain no longer reaches.
	job.Lister = fakeLister{witnesses: []audit.Anchor{{Seq: 120, Hash: "deadbeef", Digest: "d120"}, dbAnchors[0]}}
	err, ok := job.Run(context.Background())
	if !ok || !errors.Is(err, ErrTemporalWitnessMismatch) {
		t.Fatalf("a witnessed seq past the chain's max is a rollback: err=%v ok=%v", err, ok)
	}

	// Rewritten: the witnessed seq is present but its hash changed.
	job.Lister = fakeLister{witnesses: []audit.Anchor{{Seq: 50, Hash: "not-the-hash", Digest: "d50"}}}
	if err, ok := job.Run(context.Background()); !ok || !errors.Is(err, ErrTemporalWitnessMismatch) {
		t.Fatalf("a witnessed row whose hash changed is a rewrite: err=%v ok=%v", err, ok)
	}

	// A TEMPORAL-ONLY witness (seq 50 in the chain, no ledger_anchor row) is NORMAL, not tamper: the two recorders
	// run on their own ticks (fresh-eyes review). Clean.
	job.Lister = fakeLister{witnesses: []audit.Anchor{{Seq: 50, Hash: entries[49].Hash, Digest: "d50"}, dbAnchors[0]}}
	if err, ok := job.Run(context.Background()); err != nil || !ok {
		t.Fatalf("a Temporal-only witness that is in the chain must read clean: err=%v ok=%v", err, ok)
	}
	// Where BOTH hold the seq they must agree.
	job.Lister = fakeLister{witnesses: []audit.Anchor{{Seq: 100, Hash: entries[99].Hash, Digest: "not-d100"}}}
	if err, ok := job.Run(context.Background()); !ok || !errors.Is(err, ErrTemporalWitnessMismatch) {
		t.Fatalf("a seq both sides witnessed with different digests is a contradiction: err=%v ok=%v", err, ok)
	}

	// A lister that cannot answer is fail-safe, never clean — and the DB-list pass still runs, so a tamper the
	// old pass could see is still reported as tamper (ok=true) rather than hidden behind the list error.
	job.Lister = fakeLister{err: errors.New("visibility down")}
	if err, ok := job.Run(context.Background()); ok || err == nil {
		t.Fatalf("an unlistable history is not a verified one: err=%v ok=%v", err, ok)
	}
	tampered := append([]audit.LedgerEntry(nil), entries...)
	tampered[99].Hash = "rewritten"
	if err, ok := (TemporalVerifyJob{Anchors: fakeAnchorsList(dbAnchors), Ledger: fakeChain{entries: tampered}, Temporal: tr, Domain: audit.DomainGovernanceLedger,
		Lister: fakeLister{err: errors.New("visibility down")}}).Run(context.Background()); !ok || !errors.Is(err, ErrTemporalWitnessMismatch) {
		t.Fatalf("the DB-list pass must still detect tamper while the lister is down: err=%v ok=%v", err, ok)
	}

	// Without a lister the job is exactly what it was (the DB-list pass alone) — and blind to the rollback.
	job.Lister = nil
	if err, ok := job.Run(context.Background()); err != nil || !ok {
		t.Fatalf("control: the DB-list pass alone reads clean on the rolled-back fixture (that blindness is what the lister closes): err=%v ok=%v", err, ok)
	}
}

// fakeListingClient is the temporalClient fake PLUS the one listing method the own-history walk needs: two
// pages of completed executions, some from another domain, each read back through GetWorkflow by id.
type fakeListingClient struct {
	fakeTemporalClient
	pages   [][]string
	results map[string]audit.Anchor
	listErr error
	calls   int
}

func (f *fakeListingClient) ListWorkflow(_ context.Context, req *workflowservice.ListWorkflowExecutionsRequest) (*workflowservice.ListWorkflowExecutionsResponse, error) {
	f.calls++
	if f.listErr != nil {
		return nil, f.listErr
	}
	if !strings.Contains(req.GetQuery(), "WitnessAnchorWorkflow") || !strings.Contains(req.GetQuery(), "Completed") {
		return nil, errors.New("unexpected query: " + req.GetQuery())
	}
	page := 0
	if len(req.GetNextPageToken()) > 0 {
		page = int(req.GetNextPageToken()[0])
	}
	resp := &workflowservice.ListWorkflowExecutionsResponse{}
	if page < len(f.pages) {
		for _, id := range f.pages[page] {
			resp.Executions = append(resp.Executions, &workflowpb.WorkflowExecutionInfo{Execution: &commonpb.WorkflowExecution{WorkflowId: id, RunId: "run-" + id}})
		}
	}
	if page+1 < len(f.pages) {
		resp.NextPageToken = []byte{byte(page + 1)}
	}
	return resp, nil
}

func (f *fakeListingClient) GetWorkflow(_ context.Context, workflowID string, _ string) client.WorkflowRun {
	if a, ok := f.results[workflowID]; ok {
		return &fakeWorkflowRun{result: a, found: true}
	}
	return &fakeWorkflowRun{found: false}
}

func TestListWitnessesWalksPagesFiltersTheDomainAndStopsAtTheLimit(t *testing.T) {
	gov := witnessIDPrefix(audit.DomainGovernanceLedger)
	fc := &fakeListingClient{
		pages: [][]string{
			{gov + "seq-300", "tg/ledger-anchor/knowledge-corpus/seq-9", gov + "seq-290"},
			{gov + "seq-280", gov + "seq-270"},
		},
		results: map[string]audit.Anchor{
			gov + "seq-300": {Seq: 300, Hash: "h300"}, gov + "seq-290": {Seq: 290, Hash: "h290"},
			gov + "seq-280": {Seq: 280, Hash: "h280"}, gov + "seq-270": {Seq: 270, Hash: "h270"},
			"tg/ledger-anchor/knowledge-corpus/seq-9": {Seq: 9, Hash: "other-domain"},
		},
	}
	w := &TemporalWitness{Client: fc, Domain: audit.DomainGovernanceLedger}
	got, err := w.ListWitnesses(context.Background(), audit.DomainGovernanceLedger, 3)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 || got[0].Seq != 300 || got[1].Seq != 290 || got[2].Seq != 280 {
		t.Fatalf("want the newest 3 governance witnesses across two pages, other domains skipped: %+v", got)
	}
	if fc.calls != 2 {
		t.Fatalf("the second page must be fetched to reach the limit, got %d call(s)", fc.calls)
	}
	// A client that cannot list is an error, never an empty clean-looking answer.
	fc.listErr = errors.New("visibility unavailable")
	if _, err := w.ListWitnesses(context.Background(), audit.DomainGovernanceLedger, 3); err == nil {
		t.Fatal("a list failure must surface")
	}
	// A client without the listing method cannot run the walk at all.
	if _, err := (&TemporalWitness{Client: &fakeTemporalClient{}}).ListWitnesses(context.Background(), audit.DomainGovernanceLedger, 3); err == nil {
		t.Fatal("a non-listing client must refuse, so TemporalVerifyJob fails safe rather than skipping the walk")
	}
	// An empty visibility store is an empty list, not an error — the verify job decides what that means.
	empty := &fakeListingClient{pages: nil, results: map[string]audit.Anchor{}}
	if got, err := (&TemporalWitness{Client: empty}).ListWitnesses(context.Background(), audit.DomainGovernanceLedger, 3); err != nil || len(got) != 0 {
		t.Fatalf("an empty store lists nothing: %+v %v", got, err)
	}
}
