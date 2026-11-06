package opclassratify

// TG-177 — op-class lineage with FAIL-CLOSED trust inheritance.
//
// The hole these oracles pin: graduation (earned autonomy) is keyed on a BARE op-class string, while the
// ratified slug is OPERATOR-AUTHORED and unbound to the candidate's cluster slug. So a class boundary can
// move — a rename/split, or a revoked-then-re-ratified name — and carry graduation the new grant never
// earned. The fix resets the ladder to approve on any ratify whose slug already holds trust, and records
// the boundary on the ratify ledger entry. The killing mutation for the whole feature is to delete the
// `if lin.reset { a.D.Ladder.Save(approve) }` block in ratify: TestReuseOfGraduatedSlugReEarnsFromZero
// then goes RED because the seeded auto survives the grant.

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/territory-grounder/grounder/core/actuate/opschema"
	"github.com/territory-grounder/grounder/core/audit"
	"github.com/territory-grounder/grounder/core/opclasscat"
	"github.com/territory-grounder/grounder/core/policy"
)

// --- fakes: the minimal Deps the ratify path drives, in-memory so the whole grant runs without a DB ---

type fakeStore struct{ updated opclasscat.Candidate }

func (f *fakeStore) RecordOccurrence(context.Context, opclasscat.Occurrence) error         { return nil }
func (f *fakeStore) UpsertObserving(context.Context, string, opclasscat.Occurrence) error  { return nil }
func (f *fakeStore) LiveCandidates(context.Context) ([]opclasscat.Candidate, error)        { return nil, nil }
func (f *fakeStore) Occurrences(context.Context, string, time.Time) ([]opclasscat.Occurrence, error) {
	return nil, nil
}
func (f *fakeStore) UpdateCandidate(_ context.Context, c opclasscat.Candidate) error {
	f.updated = c
	return nil
}

type fakeLedger struct {
	seq     int64
	entries []audit.GovDecision
}

func (f *fakeLedger) Append(d audit.GovDecision) (audit.LedgerEntry, error) {
	f.seq++
	f.entries = append(f.entries, d)
	return audit.LedgerEntry{Seq: f.seq, Decision: d.Decision, Reason: d.Reason, ActionID: d.ActionID, Withheld: d.Withheld}, nil
}

// ratifyReason returns the reason recorded on the opclass:ratify entry (the last append in a ratify).
func (f *fakeLedger) ratifyReason(t *testing.T) string {
	t.Helper()
	for i := len(f.entries) - 1; i >= 0; i-- {
		if f.entries[i].Decision == opclasscat.DecisionRatify {
			return f.entries[i].Reason
		}
	}
	t.Fatalf("no opclass:ratify ledger entry was appended (entries: %+v)", f.entries)
	return ""
}

type fakeOverlay struct{ granted *Grant }

func (f *fakeOverlay) Ratify(_ context.Context, g Grant) error { f.granted = &g; return nil }
func (f *fakeOverlay) Revoke(context.Context, string, string, string, int64) error { return nil }
func (f *fakeOverlay) IsLive(context.Context, string) (bool, error)        { return false, nil }

// fakeLadder is the durable graduation store as ratify sees it (Deps.Ladder is the DB store directly, not
// the caching policy.Ladder). `prior` is seeded per op-class; an absent key returns ErrClassAbsent, exactly
// as core/db.PolicyGraduationStore.Load does. `saved` captures every write so a reset is observable.
type fakeLadder struct {
	prior map[string]policy.ClassState
	saved []policy.ClassState
}

func (f *fakeLadder) Load(_ context.Context, opClass string) (policy.ClassState, error) {
	if st, ok := f.prior[opClass]; ok {
		st.OpClass = opClass
		return st, nil
	}
	return policy.ClassState{}, policy.ErrClassAbsent
}
func (f *fakeLadder) Save(_ context.Context, st policy.ClassState) error {
	f.saved = append(f.saved, st)
	return nil
}

func (f *fakeLadder) lastSave() (policy.ClassState, bool) {
	if len(f.saved) == 0 {
		return policy.ClassState{}, false
	}
	return f.saved[len(f.saved)-1], true
}

// validOverlaySpec is a non-embedded, non-destructive, auto-eligible spec that passes ValidateRatification.
// The slug must NOT be an embedded class (the overlay may never shadow one) — these are runtime-ratified
// names, the exact population op-class lineage governs.
func validOverlaySpec(slug string) opschema.OpClassSpec {
	return opschema.OpClassSpec{
		OpClass:      slug,
		Op:           "rotate log",
		Family:       opschema.FamilyServiceLifecycle,
		SafetyTier:   opschema.TierLowReversible,
		EffectKind:   string(opschema.EffectSSHArgv),
		ArgvTemplate: []string{"logrotate", "--force", "${config}"},
		Params:       []opschema.ParamSpec{{Name: "config", Required: true}},
	}
}

// ratifyFixture wires a ready-to-ratify candidate whose CLUSTER slug is candidateSlug, with the ladder
// pre-seeded from `prior`. The returned deps expose the fakes for assertions.
func ratifyFixture(candidateSlug string, prior map[string]policy.ClassState) (*Activities, *fakeLedger, *fakeOverlay, *fakeLadder) {
	led := &fakeLedger{}
	ov := &fakeOverlay{}
	lad := &fakeLadder{prior: prior}
	a := &Activities{D: Deps{
		Loader: fakeLoader{c: opclasscat.Candidate{
			CandidateKey: "cand-key",
			OpClass:      candidateSlug,
			Status:       opclasscat.StatusRatifyReady,
		}},
		Store:   &fakeStore{},
		Ledger:  led,
		Overlay: ov,
		Ladder:  lad,
	}}
	return a, led, ov, lad
}

func ratifyReq(authoredSlug string) Request {
	return Request{
		Verb:         VerbRatify,
		CandidateKey: "cand-key",
		Spec:         validOverlaySpec(authoredSlug),
		Rationale:    "operator ratifies after review",
		Approver:     "kyriakosp",
	}
}

// THE NAMED SAFETY ORACLE (red without the reset). A slug that already holds `auto` graduation is ratified
// again — the exact revoked-then-re-ratified shape. The class MUST fall back to approve and re-earn; it must
// not inherit the autonomy the previous life of that name earned. Vacuity guard: the seed is `auto`, so the
// reset is a genuine auto→approve downgrade, not a class that was already at the floor.
func TestReuseOfGraduatedSlugReEarnsFromZero(t *testing.T) {
	slug := "rotate-log-nightly"
	seededAuto := policy.ClassState{OpClass: slug, Level: policy.LevelAuto, CleanRunCount: 0}
	if seededAuto.Level == policy.LevelApprove {
		t.Fatal("vacuity: the seed must be above approve for the reset to be a real transition")
	}
	a, led, ov, lad := ratifyFixture(slug, map[string]policy.ClassState{slug: seededAuto})

	if _, err := a.OpClassVerbActivity(context.Background(), ratifyReq(slug)); err != nil {
		t.Fatalf("ratify: %v", err)
	}
	saved, ok := lad.lastSave()
	if !ok {
		t.Fatal("graduation was NOT reset on ratify — a reused slug kept its inherited `auto` (the TG-177 hole)")
	}
	if saved.OpClass != slug || saved.Level != policy.LevelApprove || saved.CleanRunCount != 0 || saved.NoticeRunCount != 0 {
		t.Fatalf("reset wrote %+v, want a pristine approve/0/0 for %q", saved, slug)
	}
	if ov.granted == nil || ov.granted.OpClass != slug {
		t.Fatalf("the grant did not go live for %q (grant=%+v)", slug, ov.granted)
	}
	if r := led.ratifyReason(t); !strings.Contains(r, "kind=reuse") || !strings.Contains(r, "reset_from=auto") {
		t.Fatalf("ratify ledger reason %q does not record the reuse boundary + reset_from", r)
	}
}

// A mid-climb approve streak is ALSO inherited progress a reused name did not earn (approve with 4 of 5
// clean runs is one short of auto). Resetting only promoted levels would leave that escalation path open.
func TestMidClimbApproveStreakIsReset(t *testing.T) {
	slug := "rotate-log-weekly"
	a, _, _, lad := ratifyFixture(slug, map[string]policy.ClassState{
		slug: {OpClass: slug, Level: policy.LevelApprove, CleanRunCount: 4},
	})
	if _, err := a.OpClassVerbActivity(context.Background(), ratifyReq(slug)); err != nil {
		t.Fatalf("ratify: %v", err)
	}
	saved, ok := lad.lastSave()
	if !ok || saved.CleanRunCount != 0 || saved.Level != policy.LevelApprove {
		t.Fatalf("a mid-climb approve streak was not reset to 0 (saved=%+v, wrote=%v)", saved, ok)
	}
}

// A rename/split (authored slug ≠ cluster slug) with NO prior trust must record the boundary — "no silent
// rename path remains" — but reset nothing, because there is nothing to reset. A new name defaults to
// ungraduated already; the record is the deliverable, not a reset.
func TestRenameRecordsBoundaryWithoutReset(t *testing.T) {
	parent, child := "rotate-log-broad", "rotate-log-narrow"
	a, led, _, lad := ratifyFixture(parent, nil) // child absent → ErrClassAbsent
	if _, err := a.OpClassVerbActivity(context.Background(), ratifyReq(child)); err != nil {
		t.Fatalf("ratify: %v", err)
	}
	if _, ok := lad.lastSave(); ok {
		t.Fatal("a rename with no prior graduation must not write the ladder — nothing was inherited to reset")
	}
	r := led.ratifyReason(t)
	for _, want := range []string{"kind=rename", "parent=" + parent, "child=" + child} {
		if !strings.Contains(r, want) {
			t.Fatalf("ratify ledger reason %q missing %q — the boundary move is not recorded", r, want)
		}
	}
	if strings.Contains(r, "reset_from=") {
		t.Fatalf("ratify ledger reason %q claims a reset that did not happen", r)
	}
}

// The ordinary case: same slug, no prior trust → kind=new, no reset. Guards against the change firing on
// every ratify (which would reset legitimately-earned trust the moment a class is re-ratified in place with
// its own name — there is no such re-ratify path today, but the `kind=new` arm documents the intent).
func TestNewClassNoPriorNoReset(t *testing.T) {
	slug := "rotate-log-fresh"
	a, led, _, lad := ratifyFixture(slug, nil)
	if _, err := a.OpClassVerbActivity(context.Background(), ratifyReq(slug)); err != nil {
		t.Fatalf("ratify: %v", err)
	}
	if _, ok := lad.lastSave(); ok {
		t.Fatal("a new class with no prior graduation must not write the ladder")
	}
	if r := led.ratifyReason(t); !strings.Contains(r, "kind=new") {
		t.Fatalf("ratify ledger reason %q does not record kind=new", r)
	}
}
