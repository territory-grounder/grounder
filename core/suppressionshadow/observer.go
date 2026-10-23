// Package suppressionshadow records, for an ACCEPTED alert, whether incident-scoped suppression WOULD have
// dropped it as a repeat of a still-open incident — and drops nothing.
//
// It exists as a shared package rather than a private type in one binary for the reason this codebase already
// applies to RecordFromEnvelope: ONE implementation for every caller, so two intakes cannot drift into
// measuring different things and reporting one number.
//
// ★ WHY THIS NUMBER HAS TO BE COMPLETE. The shadow is the evidence an owner decides on when choosing whether
// suppression may start ACTING. A percentage computed over some intakes and not others is not a conservative
// estimate — it is a figure whose error depends on which sources happened to be wired, and it moves whenever
// traffic shifts between them. Measured 2026-07-29 over seven days: librenms 1,677 alerts (observed, the
// single-envelope HTTP arm), prometheus-alertmanager 27 and pve-liveness 16 (both UNOBSERVED). So the number
// covered 97.5% of volume — better than the standing "measured over a fraction" claim, and still wrong in the
// direction that matters, because the two unobserved intakes are the ones that grow: pve-liveness is TG's own
// fastest detector, and the batch arm carries CrowdSec, a security source.
//
// SHADOW BY CONSTRUCTION: ObserveAccepted takes no context and returns no error, precisely so the ingest path
// can neither be delayed nor failed by an observation. The work happens on its own goroutine with its own
// deadline.
package suppressionshadow

import (
	"context"
	"time"

	coreingest "github.com/territory-grounder/grounder/core/ingest"
)

// History is the slice of durable alert history the shadow needs. Narrow on purpose: a broad store interface
// would let a caller pass something that silently answers a different question.
type History interface {
	KeyHistory(ctx context.Context, host, alertRule string, since time.Time) ([]coreingest.Fire, error)
}

// Observer is the seam the ingest paths depend on. Its shape is the contract: no context, no error.
type Observer interface {
	ObserveAccepted(host, alertRule string, at time.Time)
}

// Shadow is the production Observer.
type Shadow struct {
	hist History
	logf func(string, ...any)
}

// New builds a Shadow. A nil history or logger yields an Observer that does nothing rather than panicking on
// an ingest path — an observation must never be able to take the front door down.
func New(hist History, logf func(string, ...any)) *Shadow { return &Shadow{hist: hist, logf: logf} }

// ObserveAccepted scores one accepted alert against what suppression WOULD have decided.
func (s *Shadow) ObserveAccepted(host, alertRule string, at time.Time) {
	if s == nil || s.hist == nil || s.logf == nil || host == "" || alertRule == "" {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		// Scan back further than the staleness bound: an incident older than the window would look unseen and
		// its repeats would be scored as new, understating what suppression would have caught.
		since := at.Add(-2 * coreingest.MaxOpenIncident)
		h, err := s.hist.KeyHistory(ctx, host, alertRule, since)
		if err != nil {
			s.logf("suppression shadow: history read failed for %s/%s (observation only, ingest unaffected): %v",
				host, alertRule, err)
			return
		}
		// The candidate is the alert just accepted, so evaluate against the history BEFORE it: the reader has
		// already stored it, and counting an alert as a repeat of itself would report suppression of every
		// single alert.
		prior := h
		if n := len(prior); n > 0 && !prior[n-1].Recovered && !prior[n-1].At.After(at) {
			prior = prior[:n-1]
		}
		d := coreingest.DecideSuppress(prior, at)
		if d.Suppress {
			s.logf("suppression shadow: WOULD SUPPRESS %s/%s — %s (incident open since %s); nothing was dropped",
				host, alertRule, d.Reason, d.OpenSince.UTC().Format(time.RFC3339))
			return
		}
		s.logf("suppression shadow: would admit %s/%s — %s", host, alertRule, d.Reason)
	}()
}
