package entryfile

// TG-490 drills, two-phase edition (the fresh-eyes finding-#1 fix). The claims: reserve→create→
// complete files each incident once and the reservation blocks every blind re-create; the crash
// window (create ok, complete failed) resolves by SEARCH-ADOPTION, never a copy; an unanswerable
// adopt-question never creates; the pass is structurally dark without a project; recovery
// comments advance the cursor ONLY after the comment lands; the renderer is deterministic pure
// data carrying the incident key (INV-08 — nothing here ever touches a model).

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/territory-grounder/grounder/adapters/tracker"
	"github.com/territory-grounder/grounder/core/db"
)

type tg490FakeStore struct {
	unfiled     []db.UnfiledAlert
	reserved    map[string]string // ref -> issue_id ('' = reserved, uncompleted)
	reserveErr  error
	completeErr error
	recoveries  []db.RecoveryToComment
	commented   []int64
	markErr     error
}

func newTG490FakeStore(unfiled ...db.UnfiledAlert) *tg490FakeStore {
	return &tg490FakeStore{unfiled: unfiled, reserved: map[string]string{}}
}

func (f *tg490FakeStore) Unfiled(_ context.Context, _ time.Duration, _ int) ([]db.UnfiledAlert, error) {
	// Mirrors the real anti-join: a reserved (or completed) incident never re-lists.
	out := []db.UnfiledAlert{}
	for _, u := range f.unfiled {
		if _, taken := f.reserved[u.ExternalRef]; !taken {
			out = append(out, u)
		}
	}
	return out, nil
}
func (f *tg490FakeStore) Reserve(_ context.Context, ref, _, _ string) (bool, error) {
	if f.reserveErr != nil {
		return false, f.reserveErr
	}
	if _, taken := f.reserved[ref]; taken {
		return false, nil
	}
	f.reserved[ref] = ""
	return true, nil
}
func (f *tg490FakeStore) Complete(_ context.Context, ref, issueID string) (bool, string, error) {
	if f.completeErr != nil {
		return false, "", f.completeErr
	}
	if cur := f.reserved[ref]; cur != "" {
		return false, cur, nil
	}
	f.reserved[ref] = issueID
	return true, issueID, nil
}
func (f *tg490FakeStore) StaleReserved(_ context.Context, _ time.Duration, _ int) ([]db.StaleReservation, error) {
	out := []db.StaleReservation{}
	for ref, id := range f.reserved {
		if id == "" {
			out = append(out, db.StaleReservation{ExternalRef: ref, Project: "TGOPS",
				Alert: db.UnfiledAlert{ExternalRef: ref, Host: "web01", AlertRule: "NginxDown", Severity: "critical"}})
		}
	}
	return out, nil
}
func (f *tg490FakeStore) RecoveriesToComment(_ context.Context, _ int) ([]db.RecoveryToComment, error) {
	return f.recoveries, nil
}
func (f *tg490FakeStore) MarkCommented(_ context.Context, _ string, id int64) error {
	if f.markErr != nil {
		return f.markErr
	}
	f.commented = append(f.commented, id)
	return nil
}

type tg490FakeCreator struct {
	created []string // summaries
	err     error
}

func (f *tg490FakeCreator) CreateEntry(_ context.Context, project, summary, _ string) (tracker.Issue, error) {
	if f.err != nil {
		return tracker.Issue{}, f.err
	}
	f.created = append(f.created, summary)
	return tracker.Issue{ID: "TGOPS-" + project + "-" + summary[:3], Title: summary}, nil
}

type tg490FakeCommenter struct {
	bodies []string
	err    error
}

func (f *tg490FakeCommenter) Comment(_ context.Context, _, body string) error {
	if f.err != nil {
		return f.err
	}
	f.bodies = append(f.bodies, body)
	return nil
}

func tg490Alert(ref string) db.UnfiledAlert {
	return db.UnfiledAlert{ExternalRef: ref, SourceType: "librenms", AlertRule: "NginxDown",
		Severity: "critical", Host: "web01", Site: "dc1", Summary: "nginx died", ReceivedAt: time.Unix(1786700000, 0)}
}

func TestFileOnceFilesEachUnfiledIncidentExactlyOnce(t *testing.T) {
	st := newTG490FakeStore(tg490Alert("r-1"), tg490Alert("r-2"))
	cr := &tg490FakeCreator{}
	n, err := FileOnce(context.Background(), Config{Project: "TGOPS", Window: time.Hour, Limit: 10}, st, cr)
	if err != nil || n != 2 {
		t.Fatalf("want 2 filed, got n=%d err=%v", n, err)
	}
	if len(cr.created) != 2 {
		t.Fatalf("one create per incident, got %d", len(cr.created))
	}
	if st.reserved["r-1"] == "" || st.reserved["r-2"] == "" {
		t.Fatalf("both reservations must be COMPLETED with issue ids, got %+v", st.reserved)
	}
}

func TestFileOnceIsDarkWithoutAProject(t *testing.T) {
	st := newTG490FakeStore(tg490Alert("r-1"))
	cr := &tg490FakeCreator{}
	if _, err := FileOnce(context.Background(), Config{Project: " "}, st, cr); err == nil {
		t.Fatal("an empty project must refuse the pass (config-not-code: unset means dark)")
	}
	if len(cr.created) != 0 {
		t.Fatal("nothing may be created when the pass refuses")
	}
}

func TestFileOnceCreateFailureLeavesTheReservationForTheResolver(t *testing.T) {
	st := newTG490FakeStore(tg490Alert("r-1"))
	cr := &tg490FakeCreator{err: errors.New("tracker down")}
	n, err := FileOnce(context.Background(), Config{Project: "TGOPS", Window: time.Hour, Limit: 10}, st, cr)
	if err != nil || n != 0 {
		t.Fatalf("a create failure counts nothing, got n=%d err=%v", n, err)
	}
	if id, taken := st.reserved["r-1"]; !taken || id != "" {
		t.Fatalf("the reservation must remain (uncompleted) for the resolver, got taken=%v id=%q", taken, id)
	}
}

// THE CRASH-WINDOW ORACLE the fresh-eyes review demanded (finding #1): create succeeded, the
// completion write failed. The old design blindly re-created next pass (an orphan ticket); the
// two-phase design must (a) NOT re-list the incident (the reservation holds it), and (b) settle
// it through the RESOLVER by search-adoption — never a second blind create.
//
// KILLING MUTATION (executed 2026-08-14): in ResolveReservedOnce, skip the search and go
// straight to CreateEntry (`found, serr := nil, nil` shape — the pre-review behavior
// reconstructed). The second half goes red: the pass mints a SECOND ticket for an incident whose
// first ticket is findable. Restored, green.
func TestCrashBetweenCreateAndCompleteResolvesByAdoptionNotACopy(t *testing.T) {
	st := newTG490FakeStore(tg490Alert("r-1"))
	cr := &tg490FakeCreator{}
	st.completeErr = errors.New("pg blip")

	// Pass 1: create succeeds, completion fails — the exact reviewed window.
	n, err := FileOnce(context.Background(), Config{Project: "TGOPS", Window: time.Hour, Limit: 10}, st, cr)
	if err != nil || n != 0 {
		t.Fatalf("an uncompleted filing must not count, got n=%d err=%v", n, err)
	}
	if len(cr.created) != 1 {
		t.Fatalf("exactly one ticket exists in the tracker, got %d", len(cr.created))
	}

	// Pass 2 (the DB recovered): the incident must NOT re-enter the unfiled list...
	st.completeErr = nil
	n, err = FileOnce(context.Background(), Config{Project: "TGOPS", Window: time.Hour, Limit: 10}, st, cr)
	if err != nil || n != 0 || len(cr.created) != 1 {
		t.Fatalf("the reservation must block any blind second create (created=%d n=%d err=%v)", len(cr.created), n, err)
	}

	// ...and the RESOLVER settles it by ADOPTING the searchable first ticket.
	sr := &tg490FakeSearcher{found: []string{"TGOPS-adopted-1"}}
	n, err = ResolveReservedOnce(context.Background(), Config{Project: "TGOPS", Limit: 10}, st, cr, sr, 0)
	if err != nil || n != 1 {
		t.Fatalf("the resolver must settle the stale reservation, got n=%d err=%v", n, err)
	}
	if len(cr.created) != 1 {
		t.Fatalf("adoption must NOT create a second ticket (created=%d)", len(cr.created))
	}
	if st.reserved["r-1"] != "TGOPS-adopted-1" {
		t.Fatalf("the reservation must complete with the ADOPTED id, got %q", st.reserved["r-1"])
	}

	// The other resolver arms: provably-none-found creates; an unanswerable search NEVER creates.
	st2 := newTG490FakeStore()
	st2.reserved["r-9"] = ""
	n, err = ResolveReservedOnce(context.Background(), Config{Project: "TGOPS", Limit: 10}, st2, cr, &tg490FakeSearcher{}, 0)
	if err != nil || n != 1 || st2.reserved["r-9"] == "" {
		t.Fatalf("none-found must create-and-complete, got n=%d id=%q err=%v", n, st2.reserved["r-9"], err)
	}
	st3 := newTG490FakeStore()
	st3.reserved["r-8"] = ""
	before := len(cr.created)
	n, err = ResolveReservedOnce(context.Background(), Config{Project: "TGOPS", Limit: 10}, st3, cr, &tg490FakeSearcher{err: errors.New("search down")}, 0)
	if err != nil || n != 0 || len(cr.created) != before || st3.reserved["r-8"] != "" {
		t.Fatalf("an unanswered adopt-question must hold (no create, no completion), got n=%d created=%d", n, len(cr.created))
	}
}

func TestCommentRecoveriesAdvancesTheCursorOnlyOnSuccess(t *testing.T) {
	st := &tg490FakeStore{recoveries: []db.RecoveryToComment{
		{ExternalRef: "r-1", IssueID: "TGOPS-1", TransitionID: 41, Host: "web01", AlertRule: "NginxDown", ReceivedAt: time.Unix(1786700100, 0)},
	}}
	cm := &tg490FakeCommenter{}
	n, err := CommentRecoveriesOnce(context.Background(), st, cm, 10)
	if err != nil || n != 1 || len(st.commented) != 1 || st.commented[0] != 41 {
		t.Fatalf("comment then cursor: n=%d err=%v commented=%v", n, err, st.commented)
	}

	st2 := &tg490FakeStore{recoveries: st.recoveries}
	cm2 := &tg490FakeCommenter{err: errors.New("tracker down")}
	n, err = CommentRecoveriesOnce(context.Background(), st2, cm2, 10)
	if err != nil || n != 0 || len(st2.commented) != 0 {
		t.Fatalf("a failed comment must NOT advance the cursor (retry next pass), got n=%d commented=%v", n, st2.commented)
	}
}

func TestRenderEntryIsDeterministicPureData(t *testing.T) {
	u := tg490Alert("librenms-dc1-999")
	s1, d1 := RenderEntry(u)
	s2, d2 := RenderEntry(u)
	if s1 != s2 || d1 != d2 {
		t.Fatal("the renderer must be deterministic")
	}
	if s1 != "[critical] web01: NginxDown" {
		t.Fatalf("summary shape: %q", s1)
	}
	for _, must := range []string{"librenms-dc1-999", "nginx died", "no model authorship"} {
		if !strings.Contains(d1, must) {
			t.Fatalf("description must carry %q, got:\n%s", must, d1)
		}
	}
	// The empty-input arm: a blank record still renders a filed-able, honest ticket.
	sEmpty, dEmpty := RenderEntry(db.UnfiledAlert{ExternalRef: "bare-ref", ReceivedAt: time.Unix(0, 0)})
	if strings.TrimSpace(sEmpty) == "" || !strings.Contains(dEmpty, "bare-ref") {
		t.Fatalf("blank fields must degrade to placeholders, never an empty summary: %q", sEmpty)
	}
}

type tg490FakeSearcher struct {
	found []string
	err   error
}

func (f *tg490FakeSearcher) SearchEntry(_ context.Context, _, _ string) ([]tracker.Issue, error) {
	if f.err != nil {
		return nil, f.err
	}
	out := make([]tracker.Issue, 0, len(f.found))
	for _, id := range f.found {
		out = append(out, tracker.Issue{ID: id})
	}
	return out, nil
}
