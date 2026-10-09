package worldmodel_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/territory-grounder/grounder/adapters/actuation"
	"github.com/territory-grounder/grounder/core/audit"
	"github.com/territory-grounder/grounder/core/estate"
	"github.com/territory-grounder/grounder/core/safety"
	"github.com/territory-grounder/grounder/core/worldmodel"
	sshactuation "github.com/territory-grounder/grounder/modules/actuation/ssh"
)

// recordingRunner stands in for the network: it records what WOULD have run, so "the leaf accepted it" is
// observable without touching a host.
type recordingRunner struct{ ran [][]string }

func (r *recordingRunner) Run(_ context.Context, argv []string, _ []byte) (actuation.Result, error) {
	r.ran = append(r.ran, argv)
	return actuation.Result{}, nil
}

type memStore struct{ entries []worldmodel.Entry }

func (m *memStore) UpdateEntry(_ context.Context, e worldmodel.Entry) error {
	for i, ex := range m.entries {
		if ex.EntityType == e.EntityType && ex.Name == e.Name {
			m.entries[i] = e
			return nil
		}
	}
	m.entries = append(m.entries, e)
	return nil
}

// ApprovedEntries is the materialization read: approved AND stale rows both feed the union.
func (m *memStore) ApprovedEntries(context.Context) ([]worldmodel.Entry, error) {
	var out []worldmodel.Entry
	for _, e := range m.entries {
		if e.Status == worldmodel.StatusApproved || e.Status == worldmodel.StatusStale {
			out = append(out, e)
		}
	}
	return out, nil
}

type memLedger struct{ n int }

func (l *memLedger) Append(audit.GovDecision) (audit.LedgerEntry, error) {
	l.n++
	return audit.LedgerEntry{Seq: int64(l.n)}, nil
}

// TestAdoptMaterializesIntoTheRealLeafGate is O-2703 — THE flagship oracle of spec/027.
//
// It drives the REAL ssh actuation leaf (modules/actuation/ssh, byte-untouched by this plane) with mutation
// ARMED, and proves the whole point of the epic's plane 2 in one pass:
//
//	before adopt : the leaf REFUSES the effect          (default-deny holds)
//	after  adopt : the SAME effect passes the leaf gate (the operator's grant materialized)
//
// The enforcement point never moves — only the AUTHORSHIP of the grant does. If this test ever passes its
// "before" branch, discovery has started granting capability on its own, which is the failure this entire
// design exists to make impossible.
func TestAdoptMaterializesIntoTheRealLeafGate(t *testing.T) {
	const (
		host = "dc1mealie01"
		unit = "mealie.service"
	)
	// Mutation ARMED: this test is about the ALLOWLIST gate, so the mode gate above it must be open —
	// otherwise a refusal would prove only that the chokepoint works (which other oracles already pin).
	gate := safety.NewActuatingChokepoint()
	if !gate.MayActuate() {
		t.Fatal("this oracle requires mutation armed so the refusal it observes is the ALLOWLIST gate")
	}

	store := &memStore{entries: []worldmodel.Entry{{
		EntityType: estate.TypeService, Name: unit, Host: host,
		Source: estate.SourceDeclared, Confidence: 0.85, Status: worldmodel.StatusDraft,
	}}}
	provider := worldmodel.NewAllowlistProvider(store, worldmodel.KindUnit, nil /* no env grant */)

	restartArgv := []string{"systemctl", "restart", unit}

	// ── BEFORE ADOPT ────────────────────────────────────────────────────────────────────────────────
	// The entry is DISCOVERED but only a draft. The union is empty, so the real leaf must refuse.
	before := &recordingRunner{}
	leafBefore := sshactuation.New(host, "tg@estate", before,
		sshactuation.WithMutation(gate, provider(context.Background()), nil))
	_, err := leafBefore.Exec(context.Background(), restartArgv, nil)
	if err == nil {
		t.Fatal("DISCOVERY MUST NOT GRANT: a merely-drafted unit was accepted by the leaf")
	}
	if !errors.Is(err, sshactuation.ErrUnitNotAllowed) {
		t.Fatalf("the refusal must come from the allowlist gate, got %v", err)
	}
	if len(before.ran) != 0 {
		t.Fatalf("a refused effect must never reach the transport, got %+v", before.ran)
	}

	// ── THE OPERATOR ADOPTS ─────────────────────────────────────────────────────────────────────────
	adopted, err := worldmodel.Transition(context.Background(), store, &memLedger{},
		store.entries[0], worldmodel.StatusApproved, "operator@estate", "reviewed the diff; this unit is ours")
	if err != nil {
		t.Fatalf("adopt: %v", err)
	}
	if adopted.Status != worldmodel.StatusApproved {
		t.Fatalf("want approved, got %s", adopted.Status)
	}

	// ── AFTER ADOPT ─────────────────────────────────────────────────────────────────────────────────
	// The SAME effect, the SAME leaf code, a leaf constructed from the now-materialized union.
	after := &recordingRunner{}
	leafAfter := sshactuation.New(host, "tg@estate", after,
		sshactuation.WithMutation(gate, provider(context.Background()), nil))
	if _, err := leafAfter.Exec(context.Background(), restartArgv, nil); err != nil {
		t.Fatalf("after adopt the SAME effect must pass the leaf gate, got %v", err)
	}
	if len(after.ran) != 1 {
		t.Fatalf("the adopted effect must reach the transport exactly once, got %+v", after.ran)
	}
	if !strings.Contains(strings.Join(after.ran[0], " "), unit) {
		t.Fatalf("the executed argv must be the operator's own effect, got %+v", after.ran[0])
	}

	// ── AND ONLY THAT TARGET ────────────────────────────────────────────────────────────────────────
	// Adopting one unit must not open the door to a neighbour: the grant is per-target, not a mode.
	if _, err := leafAfter.Exec(context.Background(), []string{"systemctl", "restart", "nginx.service"}, nil); !errors.Is(err, sshactuation.ErrUnitNotAllowed) {
		t.Fatalf("adopting one unit must NOT grant another, got %v", err)
	}
}

// TestTheUnionCanOnlyEverAddToTheOperatorsExistingGrant is REQ-2704's composition law (ADR-0016 OQ-2).
// DB-replaces-env would silently REVOKE every env-typed target the moment the first entry was adopted;
// the union makes that arithmetically impossible.
func TestTheUnionCanOnlyEverAddToTheOperatorsExistingGrant(t *testing.T) {
	env := []string{"nginx.service", "docker.service"}
	store := &memStore{entries: []worldmodel.Entry{{
		EntityType: estate.TypeService, Name: "mealie.service", Host: "h1",
		Status: worldmodel.StatusApproved,
	}}}
	got := worldmodel.NewAllowlistProvider(store, worldmodel.KindUnit, env)(context.Background())

	for _, want := range env {
		if !contains(got, want) {
			t.Fatalf("SILENT NARROWING: env-granted %q vanished after an adopt — got %v", want, got)
		}
	}
	if !contains(got, "mealie.service") {
		t.Fatalf("the adopted entry must materialize, got %v", got)
	}
	if len(got) != 3 {
		t.Fatalf("union must be env + adopted with no duplicates, got %v", got)
	}
}

// TestDiscoveryUnavailableNeverWidensAndNeverNarrows — a nil or failing store falls back to the env grant
// alone: fail-closed toward what the operator already authored, never toward a wider list.
func TestDiscoveryUnavailableNeverWidensAndNeverNarrows(t *testing.T) {
	env := []string{"nginx.service"}
	if got := worldmodel.NewAllowlistProvider(nil, worldmodel.KindUnit, env)(context.Background()); len(got) != 1 || got[0] != "nginx.service" {
		t.Fatalf("a nil store must yield exactly the env grant, got %v", got)
	}
	if got := worldmodel.NewAllowlistProvider(&failingStore{}, worldmodel.KindUnit, env)(context.Background()); len(got) != 1 || got[0] != "nginx.service" {
		t.Fatalf("a failing store must yield exactly the env grant, got %v", got)
	}
}

// TestAdoptingANonActuatableEntityGrantsNothing — adopting a site or a tunnel materializes into no leaf.
func TestAdoptingANonActuatableEntityGrantsNothing(t *testing.T) {
	store := &memStore{entries: []worldmodel.Entry{
		{EntityType: estate.TypeSite, Name: "dc1", Status: worldmodel.StatusApproved},
		{EntityType: estate.TypeTunnel, Name: "vti-gr", Status: worldmodel.StatusApproved},
	}}
	for _, kind := range []worldmodel.AllowlistKind{worldmodel.KindUnit, worldmodel.KindContainer, worldmodel.KindGuest} {
		if got := worldmodel.NewAllowlistProvider(store, kind, nil)(context.Background()); len(got) != 0 {
			t.Fatalf("adopting a site/tunnel must grant no actuation for kind %q, got %v", kind, got)
		}
	}
}

// TestAStaleEntryKeepsMaterializing — REQ-2705's safe direction, at the materialization layer: discovery
// losing sight of an adopted unit must not silently narrow the operator's grant.
func TestAStaleEntryKeepsMaterializing(t *testing.T) {
	store := &memStore{entries: []worldmodel.Entry{{
		EntityType: estate.TypeService, Name: "mealie.service", Status: worldmodel.StatusStale,
	}}}
	got := worldmodel.NewAllowlistProvider(store, worldmodel.KindUnit, nil)(context.Background())
	if len(got) != 1 || got[0] != "mealie.service" {
		t.Fatalf("a stale (still-granted) entry must keep materializing, got %v", got)
	}
}

type failingStore struct{ memStore }

func (failingStore) ApprovedEntries(context.Context) ([]worldmodel.Entry, error) {
	return nil, errors.New("database unavailable")
}

func contains(hay []string, needle string) bool {
	for _, h := range hay {
		if h == needle {
			return true
		}
	}
	return false
}
