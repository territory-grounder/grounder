package main

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	notifier "github.com/territory-grounder/grounder/adapters/notifier"
	"github.com/territory-grounder/grounder/core/config"
)

type signalled struct {
	decisionID, actionID, voter string
	approve                     bool
	n                           int
}

func laneFor(t *testing.T, v notifier.Vote, resolveErr error, openAction string, sig *signalled) *matrixVoteLane {
	t.Helper()
	return &matrixVoteLane{
		resolve: func(context.Context, []byte) (notifier.Vote, error) { return v, resolveErr },
		actionFor: func(context.Context, string) (string, bool) {
			return openAction, openAction != ""
		},
		signal: func(_ context.Context, d, a string, approve bool, voter string) error {
			sig.decisionID, sig.actionID, sig.approve, sig.voter = d, a, approve, voter
			sig.n++
			return nil
		},
	}
}

// THE SECURITY PROPERTY. The sealed action id comes from TG's OWN pending-decision projection, never from
// the inbound event.
//
// runner.VoteSignal requires the action id — "a vote decides ONLY when it names the session's gated
// action" (INV-12) — and the answer id a Matrix client returns is client-supplied data that any approver
// could craft. Taking the action from the event would let a legitimate approver release an action they
// were never shown, by voting with a hand-made answer id.
//
// KILLING MUTATION: make actionFor return the id parsed from the vote instead of the projection lookup. RED.
func TestActionIDComesFromTheProjectionNotTheEvent(t *testing.T) {
	var sig signalled
	l := laneFor(t, notifier.Vote{
		DecisionID: "INC-1", Sender: "@ops:example", Approve: true,
		Choice: "plan|INC-1", // client-supplied; must not be trusted for the action
	}, nil, "sealed-action-777", &sig)

	if l.handle(context.Background(), []byte(`{}`)) != voteDelivered {
		t.Fatal("a valid vote on an open decision was not delivered")
	}
	if sig.actionID != "sealed-action-777" {
		t.Fatalf("the signalled action id was %q — it must come from the pending projection, not from "+
			"anything the client sent", sig.actionID)
	}
	if sig.decisionID != "INC-1" || !sig.approve || sig.voter != "@ops:example" {
		t.Fatalf("vote mis-signalled: %+v", sig)
	}
}

// A vote for a decision with no OPEN action is dropped, not signalled with a guessed action.
func TestVoteWithNoOpenDecisionIsDropped(t *testing.T) {
	var sig signalled
	l := laneFor(t, notifier.Vote{DecisionID: "INC-GONE", Sender: "@ops:example"}, nil, "", &sig)
	// voteAttempted, NOT voteNotAVote: a real approver really did vote, and it did not land. That is the
	// case the yield register must be able to call starvation.
	if got := l.handle(context.Background(), []byte(`{}`)); got != voteAttempted {
		t.Fatalf("a dropped vote reported outcome %v, want voteAttempted — a ballot that reached no "+
			"decision is work offered to this seam and produced nothing, which is exactly what STARVED "+
			"is for. Counting it as not-a-vote hides the broken approval path.", got)
	}
	if sig.n != 0 {
		t.Fatalf("signalled %d time(s) despite no open decision", sig.n)
	}
}

// Room chatter is not a vote. The module's ResolveVote rejects it, and the lane must stay silent rather
// than signalling anything.
func TestNonVoteTrafficIsIgnored(t *testing.T) {
	var sig signalled
	l := laneFor(t, notifier.Vote{}, errors.New("not a vote"), "act-1", &sig)
	// voteNotAVote, so chatter is not counted as work OFFERED. Before this, every timeline event
	// incremented offered, so a chatty approval room with zero ballots scored STARVED — and since !994
	// that is a page.
	if got := l.handle(context.Background(), []byte(`{"content":{"body":"morning all"}}`)); got != voteNotAVote {
		t.Fatalf("ordinary room chatter reported outcome %v, want voteNotAVote", got)
	}
	if sig.n != 0 {
		t.Fatal("chatter reached the signal path")
	}
}

// A vote whose decision id is blank cannot bind to anything and must never be signalled.
func TestUnboundVoteIsNeverSignalled(t *testing.T) {
	var sig signalled
	l := laneFor(t, notifier.Vote{DecisionID: "   ", Sender: "@ops:example"}, nil, "act-1", &sig)
	if l.handle(context.Background(), []byte(`{}`)) != voteNotAVote || sig.n != 0 {
		t.Fatal("a vote with no decision id was delivered")
	}
}

// THE ROOM ALLOWLIST. The bot may sit in rooms that are not approval surfaces, and a vote-shaped message
// in a social room must not decide a governed action.
func TestVoteRoomSetIsAnAllowlistOfApprovalRooms(t *testing.T) {
	got := voteRoomSet("!default:example", "#tg-approvals=!routed:example, #ops=!ops:example")
	for _, want := range []string{"!default:example", "!routed:example", "!ops:example"} {
		if !got[want] {
			t.Errorf("room %q is not in the allowlist: %v", want, got)
		}
	}
	if got["!social:example"] {
		t.Error("an unlisted room is allowed — a vote-shaped message in a social room could decide an action")
	}
	// No rooms configured at all is the documented "every joined room" case, and must be an EMPTY set
	// rather than a set containing "" (which would match nothing and silently disable the reader).
	if len(voteRoomSet("", "")) != 0 {
		t.Error("an unconfigured room set must be empty, not a set of blanks")
	}
}

// A signal failure must not be reported as a delivered vote — the yield pair would then show votes
// flowing while none reached a decision.
func TestSignalFailureIsNotCountedAsDelivered(t *testing.T) {
	l := &matrixVoteLane{
		resolve:   func(context.Context, []byte) (notifier.Vote, error) { return notifier.Vote{DecisionID: "INC-1"}, nil },
		actionFor: func(context.Context, string) (string, bool) { return "act-1", true },
		signal: func(context.Context, string, string, bool, string) error {
			return errors.New("temporal unreachable")
		},
	}
	// voteAttempted: the ballot was real and the delivery failed. Not delivered, but definitely offered —
	// this is the starvation case that matters most, because Temporal being unreachable means EVERY vote
	// is silently dropped.
	if got := l.handle(context.Background(), []byte(`{}`)); got != voteAttempted {
		t.Fatalf("a failed signal reported outcome %v, want voteAttempted", got)
	}
}

// roundTripFunc lets the lane's real poll() run against a canned /sync payload.
type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// THE ROOM ALLOWLIST, ENFORCED — not merely built.
//
// TestVoteRoomSetIsAnAllowlistOfApprovalRooms checks the set BUILDER and would stay green if poll()
// stopped consulting the set entirely. That is the same vacuity that let an allowlist mutation survive
// earlier today in the discovery runner: a control tested at the wrong layer is not tested.
//
// This drives the REAL poll() against a homeserver returning a vote-shaped event in an UNLISTED room.
//
// KILLING MUTATION: drop the `len(l.rooms) > 0 && !l.rooms[roomID]` guard in poll(). RED.
func TestPollIgnoresEventsFromRoomsOutsideTheAllowlist(t *testing.T) {
	t.Setenv("MATRIX_VOTE_TEST_TOKEN", "tok")
	const body = `{"next_batch":"s2","rooms":{"join":{
	  "!social:example":{"timeline":{"events":[{"content":{"body":"approve INC-1"}}]}}
	}}}`
	var signalled int
	l := &matrixVoteLane{
		homeserver: "https://matrix.example",
		tokenRef:   config.SecretRef("env:MATRIX_VOTE_TEST_TOKEN"),
		rooms:      map[string]bool{"!approvals:example": true}, // the social room is NOT listed
		http: &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header)}, nil
		})},
		resolve: func(context.Context, []byte) (notifier.Vote, error) {
			return notifier.Vote{DecisionID: "INC-1", Sender: "@ops:example", Approve: true}, nil
		},
		actionFor: func(context.Context, string) (string, bool) { return "act-1", true },
		signal:    func(context.Context, string, string, bool, string) error { signalled++; return nil },
	}
	if _, err := l.poll(context.Background(), ""); err != nil {
		t.Fatalf("poll: %v", err)
	}
	if signalled != 0 {
		t.Fatalf("a vote-shaped message in an UNLISTED room decided a governed action (%d signalled) — "+
			"the bot sits in rooms that are not approval surfaces", signalled)
	}

	// And the counterweight: the SAME event in a listed room must be delivered, or the guard is simply
	// refusing everything and the assertion above proves nothing.
	l.rooms = map[string]bool{"!social:example": true}
	if _, err := l.poll(context.Background(), ""); err != nil {
		t.Fatalf("poll: %v", err)
	}
	if signalled != 1 {
		t.Fatalf("a vote in an ALLOWED room was not delivered (%d) — the guard rejects everything", signalled)
	}
}

// THE YIELD PAIR COUNTS BALLOTS, NOT ROOM TRAFFIC.
//
// The seam reports offered/produced into core/wiring.YieldRegister, whose STARVED rule is exactly
// offered > 0 && produced == 0. `offered` used to increment for EVERY timeline event, so an approval room
// with ordinary conversation and no ballots scored as a starved seam.
//
// That is not hypothetical. Measured on dc1tg01 2026-08-05, before this change:
//
//	vote.inbound: starved — 10 inbound room events offered, 0 votes delivered
//	              to a waiting decision produced: ... has emitted NOTHING
//
// Ten ordinary messages, read as a broken approval path. Since !994 put WiringSeamStarved on /metrics,
// that reading is a page — so chat in the approvals room would page someone, and the real signal would be
// buried under it within a week.
//
// The lane's own comment already said "a room full of chatter and zero votes is not starvation". The code
// did the opposite. This test pins the semantics the comment always described.
func TestYieldPairCountsBallotsNotChatter(t *testing.T) {
	t.Setenv("MATRIX_VOTE_TEST_TOKEN", "tok")
	sync := `{"next_batch":"s2","rooms":{"join":{"!room:example":{"timeline":{"events":[
		{"content":{"body":"morning all"}},
		{"content":{"body":"anyone looked at the disk alert?"}},
		{"content":{"body":"brb"}}
	]}}}}}`

	var offered, delivered int
	l := &matrixVoteLane{
		homeserver: "https://matrix.example",
		tokenRef:   config.SecretRef("env:MATRIX_VOTE_TEST_TOKEN"),
		rooms:      map[string]bool{"!room:example": true},
		http: &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(sync)),
				Header: make(http.Header)}, nil
		})},
		// Every event is chatter: ResolveVote rejects all three.
		resolve:   func(context.Context, []byte) (notifier.Vote, error) { return notifier.Vote{}, errors.New("not a vote") },
		actionFor: func(context.Context, string) (string, bool) { return "", false },
		signal:    func(context.Context, string, string, bool, string) error { return nil },
		observe:   func(o, d int) { offered, delivered = o, d },
	}
	if _, err := l.poll(context.Background(), ""); err != nil {
		t.Fatalf("poll: %v", err)
	}
	if offered != 0 {
		t.Errorf("three chatter messages reported offered=%d, want 0. offered>0 with produced==0 is the "+
			"register's definition of STARVED, so counting chatter makes an idle approval room look like "+
			"a broken one — and now pages for it.", offered)
	}
	if delivered != 0 {
		t.Errorf("chatter reported delivered=%d, want 0", delivered)
	}
}

// The other half: a real ballot that does not land MUST still register as offered, or the seam can never
// report the failure it exists to catch. This is the vacuity floor for the change above — it would be
// trivial to silence the false positive by never counting anything.
func TestADroppedBallotStillCountsAsOffered(t *testing.T) {
	t.Setenv("MATRIX_VOTE_TEST_TOKEN", "tok")
	sync := `{"next_batch":"s2","rooms":{"join":{"!room:example":{"timeline":{"events":[
		{"content":{"body":"approve INC-1"}}
	]}}}}}`

	var offered, delivered int
	l := &matrixVoteLane{
		homeserver: "https://matrix.example",
		tokenRef:   config.SecretRef("env:MATRIX_VOTE_TEST_TOKEN"),
		rooms:      map[string]bool{"!room:example": true},
		http: &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(sync)),
				Header: make(http.Header)}, nil
		})},
		// A genuine vote from an approver...
		resolve: func(context.Context, []byte) (notifier.Vote, error) {
			return notifier.Vote{DecisionID: "INC-1", Sender: "@ops:example", Approve: true}, nil
		},
		// ...for which no OPEN decision exists. A human clicked a ballot and nothing happened.
		actionFor: func(context.Context, string) (string, bool) { return "", false },
		signal:    func(context.Context, string, string, bool, string) error { return nil },
		observe:   func(o, d int) { offered, delivered = o, d },
	}
	if _, err := l.poll(context.Background(), ""); err != nil {
		t.Fatalf("poll: %v", err)
	}
	if offered != 1 {
		t.Errorf("a real ballot that reached no decision reported offered=%d, want 1. If a dropped vote "+
			"is not counted as offered, this seam can never read STARVED and the approval path can break "+
			"silently — which is the entire point of instrumenting it.", offered)
	}
	if delivered != 0 {
		t.Errorf("a dropped ballot reported delivered=%d, want 0", delivered)
	}
}
