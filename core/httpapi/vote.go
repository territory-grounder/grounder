package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/territory-grounder/grounder/core/auth"
)

// The vote intake (spec/006 REQ-518): the authenticated human decision on a POLL_PAUSE proposal, bound
// to exactly the decision AND action it answers (INV-12). A session-authenticated operator POSTs
// {external_ref, action_id, approve}; the handler signals the waiting Runner workflow keyed by the ref.
// The Runner accepts the vote only when action_id names its sealed gated action — a blind, premature, or
// stale vote is ledger-recorded and ignored there, so this surface can never release an action the human
// did not see. The voter identity is the SERVER-authenticated session principal, never a client claim.
//
// Hardening (adversarial review of the first cut):
//   - action_id is REQUIRED — the vote binds an action, not merely a session ref.
//   - same-origin enforcement: a cross-origin POST is rejected even with a valid cookie (second CSRF
//     layer on top of SameSite=Strict).
//   - a small per-voter rate limit — a runaway client cannot mass-decide held sessions.
//   - honest errors: 409 ONLY for a genuinely closed decision window; a transient signal failure is 503
//     (retryable), never disguised as a closed window.

// ErrNoWaitingDecision is returned by a VoteSignaler when no session is waiting on the ref (completed,
// timed out, or never existed) — the decision window is closed. Any other signaler error is transient.
var ErrNoWaitingDecision = errors.New("httpapi: no waiting decision for that ref")

// VoteRequest is the operator's decision. ActionID must name the sealed action the poll presented.
type VoteRequest struct {
	ExternalRef string `json:"external_ref"`
	ActionID    string `json:"action_id"`
	Approve     bool   `json:"approve"`
}

// VoteSignaler delivers an authenticated vote to the waiting Runner workflow. The composition root
// injects the Temporal-backed implementation; nil = the surface fails closed to 503.
type VoteSignaler interface {
	SignalVote(ctx context.Context, externalRef, actionID string, approve bool, voter string) error
}

// voteLimiter is a small fixed-window per-voter rate limit (defense in depth — the edge rate-limits too).
type voteLimiter struct {
	mu     sync.Mutex
	window map[string][]time.Time
}

var votesPerMinute = 10

func (l *voteLimiter) allow(voter string, now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.window == nil {
		l.window = map[string][]time.Time{}
	}
	cut := now.Add(-time.Minute)
	kept := l.window[voter][:0]
	for _, t := range l.window[voter] {
		if t.After(cut) {
			kept = append(kept, t)
		}
	}
	if len(kept) >= votesPerMinute {
		l.window[voter] = kept
		return false
	}
	l.window[voter] = append(kept, now)
	return true
}

var voteLimits voteLimiter

// sameOrigin reports whether the request's Origin (when present) matches the request host — browsers
// always send Origin on cross-site POSTs, so a mismatch is a forged/cross-site submission. A missing
// Origin (curl, same-origin GET-initiated fetch in older agents) falls through to the cookie's
// SameSite=Strict protection.
func sameOrigin(r *http.Request) bool {
	o := r.Header.Get("Origin")
	if o == "" {
		return true
	}
	u, err := url.Parse(o)
	if err != nil {
		return false
	}
	return strings.EqualFold(u.Host, r.Host)
}

// voteHandler serves POST /v1/vote (AuthSession — an authenticated operator session; machine principals
// have their own lanes and can never reach it).
func (d Deps) voteHandler(w http.ResponseWriter, r *http.Request, p auth.Principal) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if d.Votes == nil {
		http.Error(w, "vote intake unavailable", http.StatusServiceUnavailable)
		return
	}
	if !sameOrigin(r) {
		http.Error(w, "cross-origin vote rejected", http.StatusForbidden)
		return
	}
	// The voter is the authenticated principal — the session's operator identity, server-derived.
	voter := strings.TrimPrefix(p.SourceID, "operator:")
	if !voteLimits.allow(voter, time.Now()) {
		http.Error(w, "vote rate limit exceeded", http.StatusTooManyRequests)
		return
	}
	var req VoteRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&req); err != nil {
		http.Error(w, "malformed vote", http.StatusBadRequest)
		return
	}
	req.ExternalRef = strings.TrimSpace(req.ExternalRef)
	req.ActionID = strings.TrimSpace(req.ActionID)
	if req.ExternalRef == "" || len(req.ExternalRef) > 256 {
		http.Error(w, "external_ref required", http.StatusBadRequest)
		return
	}
	if req.ActionID == "" || len(req.ActionID) > 128 {
		http.Error(w, "action_id required — a vote binds the sealed action it approves", http.StatusBadRequest)
		return
	}
	if err := d.Votes.SignalVote(r.Context(), req.ExternalRef, req.ActionID, req.Approve, voter); err != nil {
		if errors.Is(err, ErrNoWaitingDecision) {
			// Closed decision window (already decided, timed out, or never existed) — idempotent-safe: a
			// duplicate or late vote cannot double-release.
			http.Error(w, "no waiting decision for that ref", http.StatusConflict)
			return
		}
		// Transient delivery failure (e.g. the workflow backend is unreachable) — retryable, and NOT
		// disguised as a closed window.
		http.Error(w, "vote delivery unavailable — retry", http.StatusServiceUnavailable)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	// "accepted" = delivered to the waiting session. The Runner decides on the FIRST vote naming its
	// sealed action; there is no revocation, and a vote naming a different action is counted+summarized on
	// the ledger, never a per-vote record.
	_ = json.NewEncoder(w).Encode(map[string]any{
		"accepted": true, "external_ref": req.ExternalRef, "action_id": req.ActionID, "approve": req.Approve,
		"note": "the first vote naming the sealed action decides; there is no revocation",
	})
}
