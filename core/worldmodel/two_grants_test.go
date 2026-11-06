package worldmodel_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/territory-grounder/grounder/core/audit"
	"github.com/territory-grounder/grounder/core/estate"
	"github.com/territory-grounder/grounder/core/opclasscat"
	"github.com/territory-grounder/grounder/core/safety"
	"github.com/territory-grounder/grounder/core/worldmodel"
	sshactuation "github.com/territory-grounder/grounder/modules/actuation/ssh"
)

// TestRatifiedClassWithoutAnAdoptedTargetCannotTouchTheHost is spec/027 REQ-2707 — the INTERSECTION
// oracle, and the one the acceptance harness named as its frontier when it shipped with every scenario
// @pending:
//
//	"two independent grants — ratify AND adopt — must both exist before a leaf acts, and with
//	 ratification now live, that intersection is the next property whose absence would be invisible
//	 until it hurts someone."
//
// The two grants are authored in different places by different acts. spec/028 ratification says WHAT
// KIND of operation has earned a verdict; spec/027 adoption says WHICH TARGET the operator has taken
// responsibility for. Neither implies the other, and the failure this pins is the plausible one: a
// ratified class reading as a blanket capability, so the first host discovery ever saw becomes
// actuatable because the CLASS was granted.
//
// The scenario's own words: the poll opens normally and the effect is refused at the leaf default-deny
// gate. Both halves matter. Refusing the poll too would be a different (safer-looking, worse) design —
// the operator would never see the approval and could not learn that the target is unadopted.
func TestRatifiedClassWithoutAnAdoptedTargetCannotTouchTheHost(t *testing.T) {
	const (
		host = "dc1mealie01"
		unit = "mealie.service"
	)
	ctx := context.Background()

	// ── GRANT ONE: the op-class is RATIFIED, through the real state machine ──────────────────────────
	cat := &twoGrantCatalog{}
	catLedger := &twoGrantLedger{}
	ratified, err := opclasscat.Transition(ctx, cat, catLedger, opclasscat.Candidate{
		CandidateKey: "systemctl-restart-unit", OpClass: "service.restart", Op: "systemctl",
		ParamNames: []string{"unit"}, Family: "systemd", Tier: "reversible",
		Status: opclasscat.StatusRatifyReady, FirstSeenAt: time.Unix(0, 0), LastSeenAt: time.Unix(0, 0),
	}, opclasscat.StatusRatified, "earned across 40 clean executions; operator grants the class")
	if err != nil {
		t.Fatalf("ratify: %v", err)
	}
	if ratified.Status != opclasscat.StatusRatified {
		t.Fatalf("precondition: the class must be ratified, got %s", ratified.Status)
	}

	// ── GRANT TWO IS ABSENT: the target unit was DISCOVERED but never adopted ────────────────────────
	store := &memStore{entries: []worldmodel.Entry{{
		EntityType: estate.TypeService, Name: unit, Host: host,
		Source: estate.SourceDeclared, Confidence: 0.85, Status: worldmodel.StatusDraft,
	}}}
	provider := worldmodel.NewAllowlistProvider(store, worldmodel.KindUnit, nil /* no env grant either */)
	if got := provider(ctx); len(got) != 0 {
		t.Fatalf("precondition: nothing is adopted, so the union must be empty, got %v", got)
	}

	// Mutation ARMED, so a refusal below can only be the allowlist gate — not the mode chokepoint,
	// which other oracles already pin. A refusal for the wrong reason would prove nothing about the
	// intersection.
	gate := safety.NewActuatingChokepoint()
	if !gate.MayActuate() {
		t.Fatal("this oracle requires mutation armed so the refusal it observes is the ALLOWLIST gate")
	}

	// ── THE POLL OPENS NORMALLY ─────────────────────────────────────────────────────────────────────
	// Ratification is what opens the approval poll, and it did: the class carries the operator's
	// verdict and its own ledger row. Nothing about the unadopted target suppressed the approval.
	if catLedger.n == 0 {
		t.Error("the ratify decision must be ledgered — the poll's opening is an audited act")
	}
	if !opclasscat.TransitionAllowed(opclasscat.StatusRatifyReady, opclasscat.StatusRatified) {
		t.Error("precondition: ratify_ready -> ratified must be a legal edge")
	}

	// ── AND THE EFFECT IS STILL REFUSED AT THE LEAF ─────────────────────────────────────────────────
	// The REAL ssh leaf, byte-untouched by either plane, with the union it would actually be built
	// from at actuation time.
	runner := &recordingRunner{}
	leaf := sshactuation.New(host, "tg@estate", runner,
		sshactuation.WithMutation(gate, provider(ctx), nil))
	_, execErr := leaf.Exec(ctx, []string{"systemctl", "restart", unit}, nil)
	if execErr == nil {
		t.Fatal("A RATIFIED CLASS IS NOT A TARGET GRANT: the leaf accepted an effect against a unit nobody adopted")
	}
	if !errors.Is(execErr, sshactuation.ErrUnitNotAllowed) {
		t.Fatalf("the refusal must come from the allowlist gate, got %v", execErr)
	}
	if len(runner.ran) != 0 {
		t.Fatalf("a refused effect must never reach the transport, got %+v", runner.ran)
	}

	// ── THE INTERSECTION IS THE POINT: adopting alone completes it ───────────────────────────────────
	// The same class, the same effect, one further operator act — now it passes. This is what makes
	// the refusal above a MISSING SECOND GRANT rather than a broken path.
	if _, err := worldmodel.Transition(ctx, store, &memLedger{}, store.entries[0],
		worldmodel.StatusApproved, "operator@estate", "reviewed the diff; this unit is ours"); err != nil {
		t.Fatalf("adopt: %v", err)
	}
	after := &recordingRunner{}
	leafAfter := sshactuation.New(host, "tg@estate", after,
		sshactuation.WithMutation(gate, provider(ctx), nil))
	if _, err := leafAfter.Exec(ctx, []string{"systemctl", "restart", unit}, nil); err != nil {
		t.Fatalf("with BOTH grants present the effect must pass the leaf gate, got %v", err)
	}
	if len(after.ran) != 1 {
		t.Fatalf("the twice-granted effect must reach the transport exactly once, got %+v", after.ran)
	}
}

// TestAdoptingATargetConfersNoClassGrant is the same intersection from the other side. Adoption says
// "this unit is ours to manage"; it must never imply that any particular OPERATION on it has been
// earned. The two grants are independent in BOTH directions, and a test that only ever checks one
// direction cannot tell an intersection from an implication.
func TestAdoptingATargetConfersNoClassGrant(t *testing.T) {
	ctx := context.Background()
	cat := &twoGrantCatalog{}

	// The target is fully adopted.
	store := &memStore{entries: []worldmodel.Entry{{
		EntityType: estate.TypeService, Name: "mealie.service", Host: "dc1mealie01",
		Source: estate.SourceDeclared, Confidence: 0.85, Status: worldmodel.StatusDraft,
	}}}
	if _, err := worldmodel.Transition(ctx, store, &memLedger{}, store.entries[0],
		worldmodel.StatusApproved, "operator@estate", "ours to manage"); err != nil {
		t.Fatalf("adopt: %v", err)
	}
	adopted, _ := store.ApprovedEntries(ctx)
	if len(adopted) != 1 {
		t.Fatalf("precondition: the entry is adopted, got %d approved", len(adopted))
	}

	// The class is merely OBSERVING — no operator has ratified anything.
	observing := opclasscat.Candidate{
		CandidateKey: "systemctl-restart-unit", OpClass: "service.restart", Op: "systemctl",
		Status: opclasscat.StatusObserving, Family: "systemd", Tier: "reversible",
	}
	// Adoption cannot have moved it, and no edge exists to move it there mechanically.
	if observing.Status != opclasscat.StatusObserving {
		t.Fatalf("adopting a target must not advance a class, got %s", observing.Status)
	}
	if opclasscat.TransitionAllowed(opclasscat.StatusObserving, opclasscat.StatusRatified) {
		t.Fatal("observing -> ratified must NOT be a legal edge: a class is earned through candidacy, never conferred by adopting a host")
	}
	if _, err := opclasscat.Transition(ctx, cat, &twoGrantLedger{}, observing,
		opclasscat.StatusRatified, "the target is adopted, so surely the class is fine"); err == nil {
		t.Fatal("ratifying straight from observing must be refused — adoption is not evidence of earned competence")
	}
}

// ---- the two fakes this file needs (memStore / memLedger / recordingRunner live in materialization_test.go) ----

type twoGrantCatalog struct{ updated []opclasscat.Candidate }

func (c *twoGrantCatalog) RecordOccurrence(context.Context, opclasscat.Occurrence) error { return nil }
func (c *twoGrantCatalog) UpsertObserving(context.Context, string, opclasscat.Occurrence) error {
	return nil
}
func (c *twoGrantCatalog) LiveCandidates(context.Context) ([]opclasscat.Candidate, error) {
	return nil, nil
}
func (c *twoGrantCatalog) Occurrences(context.Context, string, time.Time) ([]opclasscat.Occurrence, error) {
	return nil, nil
}
func (c *twoGrantCatalog) UpdateCandidate(_ context.Context, cand opclasscat.Candidate) error {
	c.updated = append(c.updated, cand)
	return nil
}

type twoGrantLedger struct{ n int }

func (l *twoGrantLedger) Append(audit.GovDecision) (audit.LedgerEntry, error) {
	l.n++
	return audit.LedgerEntry{Seq: int64(l.n)}, nil
}
