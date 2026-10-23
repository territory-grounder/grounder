package worldmodel

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/territory-grounder/grounder/core/audit"
	"github.com/territory-grounder/grounder/core/estate"
)

// fakeLedger records appends and can be made to fail, so the ledger-before-row ordering is provable.
type fakeLedger struct {
	appended []audit.GovDecision
	fail     error
	seq      int64
}

func (f *fakeLedger) Append(d audit.GovDecision) (audit.LedgerEntry, error) {
	if f.fail != nil {
		return audit.LedgerEntry{}, f.fail
	}
	f.appended = append(f.appended, d)
	f.seq++
	return audit.LedgerEntry{Seq: f.seq, Decision: d.Decision, Reason: d.Reason}, nil
}

type fakeStore struct {
	updated  []Entry
	approved []Entry
	fail     error
}

func (f *fakeStore) UpdateEntry(_ context.Context, e Entry) error {
	if f.fail != nil {
		return f.fail
	}
	f.updated = append(f.updated, e)
	return nil
}
func (f *fakeStore) ApprovedEntries(context.Context) ([]Entry, error) { return f.approved, nil }

func draftEntry() Entry {
	return Entry{
		EntityType: estate.TypeService,
		Name:       "nginx.service",
		Host:       "dc1mealie01",
		Source:     estate.SourcePVE,
		Confidence: 0.95,
		Status:     StatusDraft,
	}
}

// TestAdoptAppendsTheLedgerBeforeTheRow is O-2702: a grant that widened the allowlist with no chain entry
// is the audit hole the ordering exists to make impossible. Proven by failing the ledger and asserting the
// row was never updated.
func TestAdoptAppendsTheLedgerBeforeTheRow(t *testing.T) {
	lg := &fakeLedger{}
	st := &fakeStore{}
	got, err := Transition(context.Background(), st, lg, draftEntry(), StatusApproved, "operator@estate", "reviewed the diff; unit is ours")
	if err != nil {
		t.Fatalf("adopt: %v", err)
	}
	if len(lg.appended) != 1 {
		t.Fatalf("exactly one ledger append expected, got %d", len(lg.appended))
	}
	if lg.appended[0].Decision != DecisionAdopt {
		t.Fatalf("decision must be %s, got %s", DecisionAdopt, lg.appended[0].Decision)
	}
	if lg.appended[0].Withheld {
		t.Fatal("adoption is the one decision that WIDENS — it must not be marked withheld")
	}
	if got.LedgerSeq != 1 || len(st.updated) != 1 {
		t.Fatalf("row must carry the ledger seq and be persisted once: seq=%d updates=%d", got.LedgerSeq, len(st.updated))
	}
	if got.Approver != "operator@estate" {
		t.Fatalf("approver must be persisted server-derived, got %q", got.Approver)
	}

	// The ordering proof: a failing ledger must leave the row UNTOUCHED.
	lg2 := &fakeLedger{fail: errors.New("chain unavailable")}
	st2 := &fakeStore{}
	if _, err := Transition(context.Background(), st2, lg2, draftEntry(), StatusApproved, "op", "why"); err == nil {
		t.Fatal("a failing ledger must fail the transition")
	}
	if len(st2.updated) != 0 {
		t.Fatalf("ledger-before-row violated: the row changed with no chain entry (%d updates)", len(st2.updated))
	}
}

// TestEveryTransitionRequiresARationale — an unexplained grant or revocation is refused before anything is
// written, so the chain reads as a decision record and never as a bare diff.
func TestEveryTransitionRequiresARationale(t *testing.T) {
	for _, blank := range []string{"", "   ", "\t\n"} {
		lg := &fakeLedger{}
		st := &fakeStore{}
		if _, err := Transition(context.Background(), st, lg, draftEntry(), StatusApproved, "op", blank); !errors.Is(err, ErrRationaleRequired) {
			t.Fatalf("rationale %q must be refused with ErrRationaleRequired, got %v", blank, err)
		}
		if len(lg.appended) != 0 || len(st.updated) != 0 {
			t.Fatal("a refused transition must write NOTHING")
		}
	}
}

// TestDriftCanNeverRetireAnApprovedEntry is the safe-direction law (REQ-2705). Discovery losing sight of a
// unit marks it stale — an entry that KEEPS materializing — and no path from the drift lane reaches
// retired. Only an explicit operator act ends an entry's life.
func TestDriftCanNeverRetireAnApprovedEntry(t *testing.T) {
	approved := draftEntry()
	approved.Status = StatusApproved

	// The drift transition an automated pass is allowed to make.
	lg := &fakeLedger{}
	st := &fakeStore{}
	stale, err := Transition(context.Background(), st, lg, approved, StatusStale, "", "discovery stopped seeing it")
	if err != nil {
		t.Fatalf("approved -> stale must be allowed: %v", err)
	}
	if stale.Status != StatusStale {
		t.Fatalf("want stale, got %s", stale.Status)
	}
	if !lg.appended[0].Withheld || lg.appended[0].Decision != DecisionDrift {
		t.Fatalf("drift must be a withheld %s decision, got %+v", DecisionDrift, lg.appended[0])
	}

	// And a stale entry STILL materializes — the store's contract includes it.
	st.approved = []Entry{stale}
	rows, _ := st.ApprovedEntries(context.Background())
	if len(rows) != 1 {
		t.Fatal("a stale entry must keep materializing — narrowing a grant is an operator act")
	}

	// The structural half: from a DRAFT there is no path to retired at all, and stale->retired requires
	// the explicit operator verb (it is legal, but it is not something the drift lane emits).
	if transitionAllowed(StatusDraft, StatusRetired) {
		t.Fatal("draft must never reach retired")
	}
	if transitionAllowed(StatusRetired, StatusApproved) || transitionAllowed(StatusRejected, StatusApproved) {
		t.Fatal("terminal states must never be resurrected — a rework is a NEW draft row")
	}
}

// TestUnknownEntityTypeIsLoudRejected — a corrupted or typo'd source must fail the transition, never seed a
// phantom actuation target that later reads as operator-adopted truth.
func TestUnknownEntityTypeIsLoudRejected(t *testing.T) {
	e := draftEntry()
	e.EntityType = estate.EntityType("k8s_pod") // plausible, and NOT in the closed vocabulary
	lg := &fakeLedger{}
	st := &fakeStore{}
	if _, err := Transition(context.Background(), st, lg, e, StatusApproved, "op", "looks fine to me"); !errors.Is(err, ErrUnknownEntityType) {
		t.Fatalf("unknown entity type must be loud-rejected, got %v", err)
	}
	if len(st.updated) != 0 {
		t.Fatal("a rejected vocabulary must write nothing")
	}
}

// TestVocabularyMatchesEstatesOwnRejection is the LOAD-BEARING equivalence oracle for the replicated
// vocabulary (the spec/028 normalization precedent). worldmodel cannot import estate's private
// knownEntityTypes, so it mirrors the set; this test drives estate's PUBLIC parser and fails loudly if the
// two ever diverge in EITHER direction — a type we accept that estate rejects (phantom target), or a type
// estate accepts that we reject (silent coverage gap).
func TestVocabularyMatchesEstatesOwnRejection(t *testing.T) {
	decl := func(fromType string) string {
		return `[{"from":"a","from_type":"` + fromType + `","to":"b","to_type":"host","rel":"depends_on"}]`
	}
	// Every type we claim to know must be accepted by estate's own declared-edge parser.
	for typ := range knownEntityTypes {
		if _, err := estate.ParseDeclared(strings.NewReader(decl(string(typ)))); err != nil {
			t.Fatalf("worldmodel accepts %q but estate REJECTS it — vocabularies diverged: %v", typ, err)
		}
	}
	// And a type outside the set must be rejected by BOTH.
	const bogus = "k8s_pod"
	if KnownEntityType(estate.EntityType(bogus)) {
		t.Fatalf("%q must not be in the worldmodel vocabulary", bogus)
	}
	if _, err := estate.ParseDeclared(strings.NewReader(decl(bogus))); err == nil {
		t.Fatalf("estate accepts %q but worldmodel rejects it — vocabularies diverged", bogus)
	}
}

// TestAdoptionNeverLowersConfidence is REQ-2706's MAX-ratchet: a later sighting from a weaker source, or an
// adoption itself, may only ever raise an entry's confidence.
func TestAdoptionNeverLowersConfidence(t *testing.T) {
	if got := RatchetConfidence(0.95, 0.75); got != 0.95 {
		t.Fatalf("ratchet must keep the higher confidence, got %v", got)
	}
	if got := RatchetConfidence(0.75, 0.95); got != 0.95 {
		t.Fatalf("ratchet must raise to the stronger source, got %v", got)
	}
	// A source outside the fixed table contributes at the learned cap — hard-capped below the 0.80
	// suppression cutoff, so an unrecognised contributor can never outrank ground truth.
	c, known := SourceConfidence(estate.Source("some-new-scanner"))
	if known {
		t.Fatal("an unlisted source must not report as table-known")
	}
	if c >= 0.80 {
		t.Fatalf("an unlisted source must stay below the 0.80 suppression cutoff, got %v", c)
	}
	if c, known := SourceConfidence(estate.SourcePVE); !known || c != 0.95 {
		t.Fatalf("pve must carry its table confidence 0.95, got %v (known=%v)", c, known)
	}
}
