package worlddiscovery

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/territory-grounder/grounder/core/audit"
	"github.com/territory-grounder/grounder/core/estate"
	"github.com/territory-grounder/grounder/core/worldmodel"
)

// The spec/027 REQ-2705 oracles: drift moves in the SAFE direction only.

// fakeSource is a discovery EdgeSource whose observations (and failure) the oracle controls.
type fakeSource struct {
	src   estate.Source
	edges []estate.Edge
	err   error
}

func (f fakeSource) Source() estate.Source { return f.src }
func (f fakeSource) Edges(_ context.Context) ([]estate.Edge, error) {
	return f.edges, f.err
}

func unitEdge(src estate.Source, unit, host string) estate.Edge {
	return estate.Edge{
		From:   estate.Entity{Type: estate.TypeService, Name: unit},
		To:     estate.Entity{Type: estate.TypeHost, Name: host},
		Rel:    estate.RelRunsOn,
		Source: src,
	}
}

// fakeStore records every write so the oracles assert on what actually happened, not on a return value.
type fakeStore struct {
	granted  []worldmodel.Entry
	drafted  []worldmodel.Entry
	updated  []worldmodel.Entry
	draftErr error
}

func (s *fakeStore) DraftEntry(_ context.Context, e worldmodel.Entry) error {
	if s.draftErr != nil {
		return s.draftErr
	}
	s.drafted = append(s.drafted, e)
	return nil
}
func (s *fakeStore) ApprovedEntries(_ context.Context) ([]worldmodel.Entry, error) {
	return s.granted, nil
}
func (s *fakeStore) UpdateEntry(_ context.Context, e worldmodel.Entry) error {
	s.updated = append(s.updated, e)
	return nil
}

// fakeLedger records appends in order — the ledger-before-row ordering is asserted from this.
type fakeLedger struct {
	entries []audit.GovDecision
	seq     int64
}

func (l *fakeLedger) Append(d audit.GovDecision) (audit.LedgerEntry, error) {
	l.seq++
	l.entries = append(l.entries, d)
	return audit.LedgerEntry{Seq: l.seq}, nil
}

func approvedUnit(unit, host string, src estate.Source) worldmodel.Entry {
	return worldmodel.Entry{
		ID: 1, EntityType: estate.TypeService, Name: unit, Host: host,
		Source: src, Confidence: 0.85, Status: worldmodel.StatusApproved,
	}
}

// TestDriftMarksDisappearedStaleAndDraftsNewWithoutRetiring is the spec's named scenario and the whole
// point of the pass: a unit that vanished becomes STALE (never retired, and still granted), a unit that
// appeared becomes a DRAFT, and manifest:drift lands on the ledger.
func TestDriftMarksDisappearedStaleAndDraftsNewWithoutRetiring(t *testing.T) {
	st := &fakeStore{granted: []worldmodel.Entry{approvedUnit("gone.service", "h1", estate.SourceDeclared)}}
	lg := &fakeLedger{}
	j := Job{
		Sources: []estate.EdgeSource{fakeSource{src: estate.SourceDeclared,
			edges: []estate.Edge{unitEdge(estate.SourceDeclared, "arrived.service", "h1")}}},
		Store: st, Ledger: lg,
	}
	res, err := j.Run(context.Background())
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.MarkedStale != 1 {
		t.Fatalf("the disappeared unit must be marked stale, got %d", res.MarkedStale)
	}
	var draftedNew bool
	for _, d := range st.drafted {
		if d.Name == "arrived.service" {
			draftedNew = true
		}
	}
	if !draftedNew {
		t.Fatalf("the newly observed unit must be drafted, got %+v", st.drafted)
	}
	if len(st.updated) != 1 || st.updated[0].Status != worldmodel.StatusStale {
		t.Fatalf("the vanished entry must land in STALE, got %+v", st.updated)
	}
	// The load-bearing assertion: NOTHING in this pass may retire an operator's grant.
	for _, u := range st.updated {
		if u.Status == worldmodel.StatusRetired {
			t.Fatalf("drift retired a grant — absence of evidence is not evidence of absence: %+v", u)
		}
	}
	if len(lg.entries) != 1 || lg.entries[0].Decision != worldmodel.DecisionDrift {
		t.Fatalf("manifest:drift must be ledgered, got %+v", lg.entries)
	}
	if !lg.entries[0].Withheld {
		t.Fatal("a drift-to-stale withholds; the chain must record it as a narrowing act, not a grant")
	}
}

// TestAFailingSourceContributesNothingAndIsReportedLoudly is the spec's second named scenario. The subtle
// half is the one that matters: a broken source's OWN entries must be excluded from the stale diff, or one
// transport error becomes estate-wide drift noise.
func TestAFailingSourceContributesNothingAndIsReportedLoudly(t *testing.T) {
	const broken = estate.Source("systemd-broken")
	st := &fakeStore{granted: []worldmodel.Entry{
		approvedUnit("from-broken.service", "h1", broken),
		approvedUnit("from-healthy.service", "h2", estate.SourceDeclared),
	}}
	st.granted[1].ID = 2
	lg := &fakeLedger{}
	j := Job{
		Sources: []estate.EdgeSource{
			fakeSource{src: broken, err: errors.New("ssh: connection refused")},
			// The healthy source still sees its own unit, so nothing of its should go stale.
			fakeSource{src: estate.SourceDeclared,
				edges: []estate.Edge{unitEdge(estate.SourceDeclared, "from-healthy.service", "h2")}},
		},
		Store: st, Ledger: lg,
	}
	res, err := j.Run(context.Background())
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.SourceErrors != 1 {
		t.Fatalf("the failing source must be reported, got %d", res.SourceErrors)
	}
	if res.MarkedStale != 0 {
		t.Fatalf("a source's transport error must NOT mark its entries stale — that turns one ssh failure into estate-wide drift; got %d stale", res.MarkedStale)
	}
	if len(st.updated) != 0 {
		t.Fatalf("nothing may change status on behalf of a broken source, got %+v", st.updated)
	}
	// And the healthy source's work still happened — isolation, not abort.
	var sawHealthy bool
	for _, d := range st.drafted {
		if d.Name == "from-healthy.service" {
			sawHealthy = true
		}
	}
	if !sawHealthy {
		t.Fatal("a failing source must not stop the healthy ones from contributing")
	}
}

// TestDriftNeverRetiresEvenWhenEverythingDisappears is the extreme case: every source healthy, every
// granted entry gone. The pass may mark them all stale; it must retire none.
func TestDriftNeverRetiresEvenWhenEverythingDisappears(t *testing.T) {
	st := &fakeStore{granted: []worldmodel.Entry{
		approvedUnit("a.service", "h1", estate.SourceDeclared),
		approvedUnit("b.service", "h1", estate.SourceDeclared),
	}}
	st.granted[1].ID = 2
	lg := &fakeLedger{}
	j := Job{Sources: []estate.EdgeSource{fakeSource{src: estate.SourceDeclared}}, Store: st, Ledger: lg}
	if _, err := j.Run(context.Background()); err != nil {
		t.Fatalf("run: %v", err)
	}
	for _, u := range st.updated {
		if u.Status != worldmodel.StatusStale {
			t.Fatalf("an empty estate observation may only ever mark stale, got %s for %s", u.Status, u.Name)
		}
	}
}

// TestAlreadyStaleEntriesAreNotReMarked — a drift feed that repeats itself every pass is a feed nobody
// reads, and each re-mark would append a ledger row for a fact that has not changed.
func TestAlreadyStaleEntriesAreNotReMarked(t *testing.T) {
	e := approvedUnit("gone.service", "h1", estate.SourceDeclared)
	e.Status = worldmodel.StatusStale
	st := &fakeStore{granted: []worldmodel.Entry{e}}
	lg := &fakeLedger{}
	j := Job{Sources: []estate.EdgeSource{fakeSource{src: estate.SourceDeclared}}, Store: st, Ledger: lg}
	res, err := j.Run(context.Background())
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.MarkedStale != 0 || len(lg.entries) != 0 {
		t.Fatalf("an already-stale entry must not be re-marked (stale=%d ledger=%d)", res.MarkedStale, len(lg.entries))
	}
}

// TestUnknownEntityTypesAreDroppedNotDrafted — a corrupted source must never seed a phantom target that
// later reads as operator-adopted truth (REQ-2701's closed vocabulary, enforced in the drift path too).
func TestUnknownEntityTypesAreDroppedNotDrafted(t *testing.T) {
	st := &fakeStore{}
	j := Job{
		Sources: []estate.EdgeSource{fakeSource{src: estate.SourceDeclared, edges: []estate.Edge{{
			From:   estate.Entity{Type: estate.EntityType("k8s_pod"), Name: "phantom"},
			To:     estate.Entity{Type: estate.TypeHost, Name: "h1"},
			Rel:    estate.RelRunsOn,
			Source: estate.SourceDeclared,
		}}}},
		Store: st, Ledger: &fakeLedger{},
	}
	if _, err := j.Run(context.Background()); err != nil {
		t.Fatalf("run: %v", err)
	}
	for _, d := range st.drafted {
		if string(d.EntityType) == "k8s_pod" || d.Name == "phantom" {
			t.Fatalf("an entity type outside the closed vocabulary was drafted: %+v", d)
		}
	}
	// The healthy end of the same edge is still a real observation and must survive.
	var sawHost bool
	for _, d := range st.drafted {
		if d.EntityType == estate.TypeHost && d.Name == "h1" {
			sawHost = true
		}
	}
	if !sawHost {
		t.Fatal("the valid end of a partly-corrupt edge must still be observed")
	}
}

// TestAPassWithNoSourcesRefusesLoudly — a pass over zero sources would see the whole estate as
// disappeared. Refusing is the only honest answer; "succeeded, observed nothing" is not.
func TestAPassWithNoSourcesRefusesLoudly(t *testing.T) {
	st := &fakeStore{granted: []worldmodel.Entry{approvedUnit("a.service", "h1", estate.SourceDeclared)}}
	_, err := Job{Store: st, Ledger: &fakeLedger{}}.Run(context.Background())
	if !errors.Is(err, ErrNoSources) {
		t.Fatalf("a sourceless pass must refuse loudly, got %v", err)
	}
	if len(st.updated) != 0 {
		t.Fatalf("a refused pass must change nothing, got %+v", st.updated)
	}
}

// TestPerItemErrorsNeverAbortThePass — the finalizer contract: one bad row must not stop the other
// nineteen from being observed.
func TestPerItemErrorsNeverAbortThePass(t *testing.T) {
	st := &fakeStore{draftErr: errors.New("constraint violation")}
	j := Job{
		Sources: []estate.EdgeSource{fakeSource{src: estate.SourceDeclared, edges: []estate.Edge{
			unitEdge(estate.SourceDeclared, "a.service", "h1"),
			unitEdge(estate.SourceDeclared, "b.service", "h1"),
		}}},
		Store: st, Ledger: &fakeLedger{},
	}
	res, err := j.Run(context.Background())
	if err != nil {
		t.Fatalf("per-item errors must not abort the pass, got %v", err)
	}
	if res.ItemErrors == 0 {
		t.Fatal("item errors must be counted and visible, not swallowed")
	}
}

// TestLedgerPrecedesRowOnDrift — the ordering law, asserted through the real Transition the pass uses.
func TestLedgerPrecedesRowOnDrift(t *testing.T) {
	st := &fakeStore{granted: []worldmodel.Entry{approvedUnit("gone.service", "h1", estate.SourceDeclared)}}
	lg := &fakeLedger{}
	j := Job{Sources: []estate.EdgeSource{fakeSource{src: estate.SourceDeclared}}, Store: st, Ledger: lg}
	if _, err := j.Run(context.Background()); err != nil {
		t.Fatalf("run: %v", err)
	}
	if len(st.updated) != 1 || st.updated[0].LedgerSeq == 0 {
		t.Fatalf("the row must carry the ledger seq it was written after: %+v", st.updated)
	}
	if !strings.Contains(st.updated[0].Rationale, "still granted") {
		t.Fatalf("the stale rationale must say the grant survives, got %q", st.updated[0].Rationale)
	}
}
