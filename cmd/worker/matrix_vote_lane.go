package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"strings"
	"time"

	notifier "github.com/territory-grounder/grounder/adapters/notifier"
	"github.com/territory-grounder/grounder/core/config"
)

// THE INBOUND VOTE LANE — the mailbox for the ballot TG posts.
//
// Every notifier module implements ResolveVote: matrix, slack, teams, email, twilio. NOTHING CALLED ANY
// OF THEM. `grep -rn "\.ResolveVote(" cmd/ temporal/ core/` returned nothing outside the fan-out's own
// helper, which is itself uncalled — six implementations of an interface method with no production
// caller, fully unit-tested the whole time.
//
// What that cost, concretely: on 2026-08-02 TG began posting real MSC3381 approval polls into Matrix. A
// human could see the ballot and click it, and the click went nowhere. The notice even instructed them to
// reply — an instruction pointing at a path nothing read. Votes only ever arrived through the console's
// POST /v1/vote.
//
// This lane closes that. It polls the Matrix /sync API as the bot, hands each room event to the module's
// own ResolveVote (so approver authentication and decision binding stay in the module where they are
// tested), and signals the waiting Runner workflow.
//
// THE ACTION ID IS RESOLVED SERVER-SIDE, NEVER FROM THE EVENT. runner.VoteSignal requires the sealed
// action id — "a vote decides ONLY when it names the session's gated action (INV-12)" — and a vote that
// names the wrong action is ledger-recorded and ignored. The answer id a client returns could be forged
// by any approver, so the action id is looked up from TG's own pending-decision projection by decision
// id. A forged or stale answer therefore cannot release an action the human was never shown.
type matrixVoteLane struct {
	homeserver string
	tokenRef   config.SecretRef
	rooms      map[string]bool // empty ⇒ every room the bot is in
	http       *http.Client

	// resolve is the module's own vote parser (approver authentication + decision binding).
	resolve func(ctx context.Context, raw []byte) (notifier.Vote, error)
	// actionFor returns the sealed action id for an OPEN decision. Absent ⇒ the vote is dropped: a
	// decision with no open action is one already resolved, timed out, or never gated.
	actionFor func(ctx context.Context, decisionID string) (string, bool)
	// signal delivers the vote to the waiting workflow.
	signal func(ctx context.Context, decisionID, actionID string, approve bool, voter string) error
	// normalize maps the PRESENTED voter identity (the MXID this lane reads) to the canonical login the
	// frozen approve_by entries carry (TG-463 / B26 — resolution, never a wider set). nil ⇒ identity.
	// Applied HERE, surface-side, so runner.VoterAdmitted stays a pure frozen-set test.
	normalize func(presented string) string
	// observe reports the lane's yield: events offered, votes delivered.
	observe func(offered, delivered int)
}

// syncResponse is the subset of the Matrix /sync payload this lane reads.
type syncResponse struct {
	NextBatch string `json:"next_batch"`
	Rooms     struct {
		Join map[string]struct {
			Timeline struct {
				Events []json.RawMessage `json:"events"`
			} `json:"timeline"`
		} `json:"join"`
	} `json:"rooms"`
}

// run polls /sync until the context ends. Errors are logged and retried: an unreachable homeserver must
// degrade to "votes arrive through the console" rather than take the worker down.
func (l *matrixVoteLane) run(ctx context.Context, every time.Duration) {
	since := ""
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}
		next, err := l.poll(ctx, since)
		if err != nil {
			log.Printf("matrix votes: sync failed: %v (retrying; the console vote path is unaffected)", err)
		} else if next != "" {
			since = next
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(every):
		}
	}
}

// poll performs one /sync and processes its events, returning the next batch token.
func (l *matrixVoteLane) poll(ctx context.Context, since string) (string, error) {
	token, err := l.tokenRef.Resolve()
	if err != nil {
		return "", fmt.Errorf("resolve token: %w", err)
	}
	q := url.Values{}
	q.Set("timeout", "0") // a SHORT poll: this lane is a cron-style reader, not a long-lived stream
	if since != "" {
		q.Set("since", since)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		l.homeserver+"/_matrix/client/v3/sync?"+q.Encode(), nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := l.http.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("sync: status %d", resp.StatusCode)
	}
	var sr syncResponse
	if derr := json.NewDecoder(resp.Body).Decode(&sr); derr != nil {
		return "", fmt.Errorf("malformed sync: %w", derr)
	}

	offered, delivered := 0, 0
	for roomID, room := range sr.Rooms.Join {
		if len(l.rooms) > 0 && !l.rooms[roomID] {
			continue // a room the operator did not designate for approvals
		}
		for _, ev := range room.Timeline.Events {
			// OFFERED COUNTS VOTES, NOT ROOM TRAFFIC.
			//
			// This used to be `offered++` for every timeline event. The comment below already stated the
			// correct semantics — "a room full of chatter and zero votes is not starvation" — but the
			// register's STARVED rule is exactly offered>0 && produced==0, so a chatty approval room with
			// no ballots scored as a starved seam. Measured on dc1tg01 2026-08-05: "10 inbound room
			// events offered, 0 votes delivered", which read as a broken approval path and was ten
			// ordinary messages. With WiringSeamStarved now alerting (!994), that is a page for chat.
			switch l.handle(ctx, ev) {
			case voteDelivered:
				offered++
				delivered++
			case voteAttempted:
				// A real ballot from an approver that did not reach a waiting decision. This is the only
				// thing that should ever make this seam read starved.
				offered++
			case voteNotAVote:
				// No work offered. Not counted in either half.
			}
		}
	}
	// The pair is now votes-attempted vs votes-delivered, so a room full of chatter reports nothing at all
	// (IDLE, which the register explicitly does not treat as starved) and a genuine ballot that failed to
	// land reports STARVED. The first sync returning a whole backlog is harmless for the same reason: old
	// chatter is not work offered.
	if l.observe != nil && offered > 0 {
		l.observe(offered, delivered)
	}
	return sr.NextBatch, nil
}

// handle resolves one event into a vote and signals it. Returns true only when a vote was delivered.
// voteOutcome separates "this event was never a vote" from "a vote was cast and did not land". The
// distinction is the whole yield pair for this seam: only the second is work OFFERED to it.
type voteOutcome int

const (
	// voteNotAVote — chatter, a join, a reaction, or a message from a non-approver. No work was offered.
	voteNotAVote voteOutcome = iota
	// voteAttempted — a genuine vote from an approver that did NOT reach a waiting decision. This is
	// starvation: a human clicked a ballot and nothing happened.
	voteAttempted
	// voteDelivered — signalled to the waiting decision.
	voteDelivered
)

func (l *matrixVoteLane) handle(ctx context.Context, raw []byte) voteOutcome {
	v, err := l.resolve(ctx, raw)
	if err != nil {
		// Not a vote, or not from an approver. This is the COMMON case — most room traffic is chatter —
		// so it is silent by design; logging every non-vote would bury the ones that matter.
		return voteNotAVote
	}
	if strings.TrimSpace(v.DecisionID) == "" {
		return voteNotAVote
	}
	actionID, ok := l.actionFor(ctx, v.DecisionID)
	if !ok {
		// No OPEN decision for this ref: already decided, timed out, or never gated. Dropping is correct
		// and must be LOUD — a human clicked a ballot and it changed nothing, which they deserve to see
		// explained rather than discover from an unchanged console.
		log.Printf("matrix votes: %s voted on %s but no OPEN decision exists for it (already decided, "+
			"timed out, or never gated) — the vote is not counted", v.Sender, v.DecisionID)
		return voteAttempted
	}
	voter := v.Sender
	if l.normalize != nil {
		if n := l.normalize(v.Sender); n != v.Sender {
			// The normalization is part of the authorization trail — log BOTH spellings so the ledger's
			// voter entry is traceable back to the chat identity that actually clicked.
			log.Printf("matrix votes: voter %s normalized to %s (TG-463 alias)", v.Sender, n)
			voter = n
		}
	}
	if err := l.signal(ctx, v.DecisionID, actionID, v.Approve, voter); err != nil {
		log.Printf("matrix votes: %s voted on %s but the signal failed: %v (the console vote path still works)",
			v.Sender, v.DecisionID, err)
		return voteAttempted
	}
	log.Printf("matrix votes: %s voted approve=%t on %s (action %s) via matrix", voter, v.Approve, v.DecisionID, actionID)
	return voteDelivered
}

// voteRoomSet builds the room allowlist the inbound reader will accept votes from: the default approval
// room plus every explicitly routed room. Empty ⇒ every room the bot has joined.
//
// It is an ALLOWLIST because the bot may sit in rooms that are not approval surfaces, and a vote-shaped
// message in a social room must not decide a governed action.
func voteRoomSet(defaultRoom, routed string) map[string]bool {
	out := map[string]bool{}
	if r := strings.TrimSpace(defaultRoom); r != "" {
		out[r] = true
	}
	for _, pair := range strings.Split(routed, ",") {
		if i := strings.IndexByte(pair, '='); i >= 0 {
			if r := strings.TrimSpace(pair[i+1:]); r != "" {
				out[r] = true
			}
		}
	}
	return out
}
