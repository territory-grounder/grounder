package docker

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/territory-grounder/grounder/core/audit"
	"github.com/territory-grounder/grounder/core/estate"
	"github.com/territory-grounder/grounder/core/worldmodel"
)

type fakeRunner struct {
	out  map[string][]byte
	err  map[string]error
	seen [][]string
}

func (f *fakeRunner) Run(_ context.Context, host string, argv []string) ([]byte, error) {
	f.seen = append(f.seen, argv)
	if e, ok := f.err[host]; ok {
		return nil, e
	}
	return f.out[host], nil
}

// TestContainersAreDiscoveredWithProvenanceAndFixedConfidence — the docker half of O-2701.
func TestContainersAreDiscoveredWithProvenanceAndFixedConfidence(t *testing.T) {
	f := &fakeRunner{out: map[string][]byte{
		"dc1actualbudget01": []byte("actualbudget-actual_server-1\nnginx-proxy\n\n"),
	}}
	edges, err := New([]string{"dc1actualbudget01"}, WithRunner(f)).Edges(context.Background())
	if err != nil {
		t.Fatalf("edges: %v", err)
	}
	if len(edges) != 2 {
		t.Fatalf("want 2 containers, got %d: %+v", len(edges), edges)
	}
	for _, e := range edges {
		if e.Source != estate.SourceDeclared || e.Confidence != estate.SourceConfidence[estate.SourceDeclared] {
			t.Fatalf("edge must carry source provenance + fixed table confidence, got %+v", e)
		}
		if e.Rel != estate.RelRunsOn {
			t.Fatalf("container must run_on its host, got %q", e.Rel)
		}
	}
	if len(f.seen) != 1 || f.seen[0][0] != "docker" || f.seen[0][1] != "ps" {
		t.Fatalf("exactly one non-mutating docker ps expected, got %+v", f.seen)
	}
	for _, a := range f.seen[0] {
		if a == "restart" || a == "stop" || a == "kill" || a == "rm" {
			t.Fatalf("discovery issued a MUTATING docker verb: %+v", f.seen[0])
		}
	}
}

// TestAnUnknownEntityTypeFromACorruptedSourceIsLoudRejected is O-2701's rejection half (the scenario
// T-027-2 owns). A corrupted source that names a type outside the estate's CLOSED vocabulary must fail the
// adoption LOUDLY — never seed a phantom actuation target that later reads as operator-adopted truth.
//
// The check lives at the manifest chokepoint, which is where a discovered fact becomes an adoptable entry,
// so EVERY source is bound by it rather than each source policing itself.
func TestAnUnknownEntityTypeFromACorruptedSourceIsLoudRejected(t *testing.T) {
	// A source that has been corrupted (or a future source added carelessly) emits a plausible-looking
	// type that the estate vocabulary does not declare.
	corrupted := estate.Edge{
		From:   estate.Entity{Type: estate.EntityType("k8s_pod"), Name: "web-7d9f"},
		To:     estate.Entity{Type: estate.TypeHost, Name: "dc1k8s01"},
		Rel:    estate.RelRunsOn,
		Source: estate.SourceDeclared,
	}
	if worldmodel.KnownEntityType(corrupted.From.Type) {
		t.Fatalf("%q must be outside the declared vocabulary", corrupted.From.Type)
	}

	// And the entry built from it cannot be adopted: the chokepoint refuses before anything is written.
	e := worldmodel.Entry{
		EntityType: corrupted.From.Type,
		Name:       corrupted.From.Name,
		Host:       corrupted.To.Name,
		Source:     estate.SourceDeclared,
		Status:     worldmodel.StatusDraft,
	}
	st, lg := &noopStore{}, &noopLedger{}
	if _, err := worldmodel.Transition(context.Background(), st, lg, e, worldmodel.StatusApproved, "op", "looks fine"); !errors.Is(err, worldmodel.ErrUnknownEntityType) {
		t.Fatalf("a corrupted source's type must be loud-rejected at adoption, got %v", err)
	}
	if st.updates != 0 || lg.appends != 0 {
		t.Fatal("a rejected vocabulary must write NOTHING — no row, no ledger entry")
	}
}

// TestAFailingDockerHostIsReportedAndOthersProceed — the docker half of the per-host isolation law.
func TestAFailingDockerHostIsReportedAndOthersProceed(t *testing.T) {
	f := &fakeRunner{
		out: map[string][]byte{"good01": []byte("app-1\n")},
		err: map[string]error{"bad01": errors.New("docker daemon not reachable")},
	}
	edges, err := New([]string{"good01", "bad01"}, WithRunner(f)).Edges(context.Background())
	if err == nil || !strings.Contains(err.Error(), "bad01") {
		t.Fatalf("the failing host must be named loudly, got %v", err)
	}
	if len(edges) != 1 || edges[0].From.Name != "app-1" {
		t.Fatalf("the healthy host must still contribute, got %+v", edges)
	}
}

// TestGarbageLinesAreNotDraftedAsContainers — a header or an error line leaking into stdout must not become
// an adoptable target.
func TestGarbageLinesAreNotDraftedAsContainers(t *testing.T) {
	got := ParseContainers("app-1\nCONTAINER ID   IMAGE   STATUS\nErro: cannot connect\napp-1\napp-2\n")
	if len(got) != 2 || got[0] != "app-1" || got[1] != "app-2" {
		t.Fatalf("multi-field lines and duplicates must be skipped, got %v", got)
	}
}

type noopStore struct{ updates int }

func (s *noopStore) UpdateEntry(context.Context, worldmodel.Entry) error { s.updates++; return nil }
func (s *noopStore) ApprovedEntries(context.Context) ([]worldmodel.Entry, error) {
	return nil, nil
}

type noopLedger struct{ appends int }

func (l *noopLedger) Append(audit.GovDecision) (audit.LedgerEntry, error) {
	l.appends++
	return audit.LedgerEntry{Seq: 1}, nil
}
