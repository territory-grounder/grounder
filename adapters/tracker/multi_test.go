package tracker

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// fakeTracker owns exactly the ids in `holds`. Reads for anything else fail, which is how a real
// tracker behaves when handed another system's ref.
type fakeTracker struct {
	name        string
	holds       map[string]Issue
	readErr     error // an OUTAGE: fails for every id, including ones it owns
	transitions []string
	comments    []string
	reads       int
}

func (f *fakeTracker) SourceType() string { return f.name }

func (f *fakeTracker) Read(_ context.Context, id string) (Issue, error) {
	f.reads++
	if f.readErr != nil {
		return Issue{}, f.readErr
	}
	iss, ok := f.holds[id]
	if !ok {
		return Issue{}, errors.New("no such issue")
	}
	return iss, nil
}

func (f *fakeTracker) Open(ctx context.Context, id string) (Issue, error) { return f.Read(ctx, id) }

func (f *fakeTracker) TransitionState(_ context.Context, id string, to State) error {
	f.transitions = append(f.transitions, id+"->"+string(to))
	return nil
}

func (f *fakeTracker) Comment(_ context.Context, id, body string) error {
	f.comments = append(f.comments, id+": "+body)
	return nil
}

func owns(name string, ids ...string) *fakeTracker {
	h := map[string]Issue{}
	for _, id := range ids {
		h[id] = Issue{ID: id, Title: name + " ticket", State: StateOpen}
	}
	return &fakeTracker{name: name, holds: h}
}

// THE SAFETY PROPERTY. A write must reach the tracker that OWNS the ref and no other. Broadcasting a
// TransitionState would resolve an unrelated incident in a second system on nothing but an id-shape
// coincidence — a mutation of someone else's record, made on a guess.
func TestMultiTrackerWritesOnlyToTheOwningTracker(t *testing.T) {
	sn := owns("servicenow", "INC0010023")
	yt := owns("youtrack", "IFR-2198")
	m := NewMultiTracker(map[string]Tracker{"servicenow": sn, "youtrack": yt})

	if err := m.TransitionState(context.Background(), "IFR-2198", StateResolved); err != nil {
		t.Fatalf("TransitionState: %v", err)
	}
	if len(yt.transitions) != 1 {
		t.Fatalf("the owning tracker was not transitioned: %v", yt.transitions)
	}
	if len(sn.transitions) != 0 {
		t.Fatalf("a NON-OWNING tracker was transitioned (%v) — that resolves an unrelated incident in "+
			"another system", sn.transitions)
	}

	if err := m.Comment(context.Background(), "INC0010023", "closed by TG"); err != nil {
		t.Fatalf("Comment: %v", err)
	}
	if len(sn.comments) != 1 || len(yt.comments) != 0 {
		t.Fatalf("the audit comment did not land on the owner alone: sn=%v yt=%v", sn.comments, yt.comments)
	}
}

// A ref no tracker holds must be an ERROR, never a zero Issue. A zero Issue has State "" — neither Open
// nor Resolved — so every consumer switching on state takes a branch chosen by a value that means
// "we could not find out".
func TestMultiTrackerUnknownRefIsAnErrorNotAZeroIssue(t *testing.T) {
	m := NewMultiTracker(map[string]Tracker{
		"servicenow": owns("servicenow", "INC1"),
		"youtrack":   owns("youtrack", "IFR-1"),
	})
	iss, err := m.Read(context.Background(), "JIRA-99")
	if err == nil {
		t.Fatalf("an unheld ref resolved to an issue: %+v", iss)
	}
	if iss != (Issue{}) {
		t.Fatalf("a failed lookup must carry no issue payload, got %+v", iss)
	}
	for _, want := range []string{"servicenow", "youtrack"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the error does not say which trackers were asked (%q missing): %v", want, err)
		}
	}
	// And a close-out that cannot find its ticket must FAIL, not report success — a silent success here
	// is a session that believes it closed a ticket nobody transitioned.
	if err := m.TransitionState(context.Background(), "JIRA-99", StateResolved); err == nil {
		t.Fatal("transitioning an unheld ref reported SUCCESS")
	}
	if err := m.Comment(context.Background(), "JIRA-99", "x"); err == nil {
		t.Fatal("commenting on an unheld ref reported SUCCESS")
	}
}

// Ownership resolution finds the holder wherever it sits in the order, not just first.
func TestMultiTrackerFindsTheOwnerInAnyPosition(t *testing.T) {
	for _, id := range []string{"AAA-1", "MMM-1", "ZZZ-1"} {
		m := NewMultiTracker(map[string]Tracker{
			"aaa": owns("aaa", "AAA-1"),
			"mmm": owns("mmm", "MMM-1"),
			"zzz": owns("zzz", "ZZZ-1"),
		})
		iss, err := m.Read(context.Background(), id)
		if err != nil {
			t.Fatalf("owner of %s not found: %v", id, err)
		}
		if iss.ID != id {
			t.Fatalf("wrong issue resolved for %s: %+v", id, iss)
		}
	}
}

// One tracker being DOWN must not hide a ticket the others hold. An outage in the ITSM system cannot be
// allowed to make an engineering ticket unfindable.
func TestMultiTrackerOneDownTrackerDoesNotHideAnothersTicket(t *testing.T) {
	down := &fakeTracker{name: "servicenow", readErr: errors.New("instance 503")}
	up := owns("youtrack", "IFR-2198")
	m := NewMultiTracker(map[string]Tracker{"servicenow": down, "youtrack": up})

	iss, err := m.Read(context.Background(), "IFR-2198")
	if err != nil {
		t.Fatalf("a healthy tracker holds this ref: %v", err)
	}
	if iss.ID != "IFR-2198" {
		t.Fatalf("wrong issue: %+v", iss)
	}
}

// Resolution stops at the first hit. Consulting the rest afterwards is requests an operator pays for
// and, worse, would make "who owns this" depend on which answer arrived last.
func TestMultiTrackerStopsAtTheFirstOwner(t *testing.T) {
	first := owns("aaa", "SHARED-1")
	second := owns("zzz", "SHARED-1")
	m := NewMultiTracker(map[string]Tracker{"aaa": first, "zzz": second})

	if _, err := m.Read(context.Background(), "SHARED-1"); err != nil {
		t.Fatalf("Read: %v", err)
	}
	if first.reads != 1 {
		t.Fatalf("the first tracker was read %d times, want 1", first.reads)
	}
	if second.reads != 0 {
		t.Fatalf("resolution continued past the owner: the second tracker was read %d times", second.reads)
	}
	// Deterministic order across boots — otherwise a shared id would route differently per restart.
	if got := strings.Join(m.Sources(), ","); got != "aaa,zzz" {
		t.Fatalf("resolution order is not deterministic: %q", got)
	}
}

// Nothing configured is an error, not a quiet zero answer.
func TestMultiTrackerWithNoMembersIsAnError(t *testing.T) {
	if _, err := NewMultiTracker(nil).Read(context.Background(), "X-1"); err == nil {
		t.Fatal("an empty router resolved an issue")
	}
	m := NewMultiTracker(map[string]Tracker{"servicenow": nil})
	if m.Len() != 0 {
		t.Fatalf("a nil tracker must not be counted as a member, Len()=%d", m.Len())
	}
	if err := m.TransitionState(context.Background(), "X-1", StateResolved); err == nil {
		t.Fatal("an empty router reported a successful transition")
	}
	// An empty id cannot own anything and must not be searched for.
	if _, err := NewMultiTracker(map[string]Tracker{"a": owns("a", "A-1")}).Read(context.Background(), "  "); err == nil {
		t.Fatal("an empty ref resolved to an issue")
	}
}

// The router is not one of its members: an audit line attributing a transition to "youtrack" when the
// router performed it would misattribute the act.
func TestMultiTrackerSourceTypeIsNotAMember(t *testing.T) {
	m := NewMultiTracker(map[string]Tracker{"servicenow": owns("servicenow", "A"), "youtrack": owns("youtrack", "B")})
	if got := m.SourceType(); got == "servicenow" || got == "youtrack" {
		t.Fatalf("SourceType() reports a member (%q) — every routed act would be misattributed", got)
	}
}
