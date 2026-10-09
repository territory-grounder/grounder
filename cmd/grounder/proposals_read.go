package main

import (
	"context"
	"time"

	"github.com/territory-grounder/grounder/core/auth"
	"github.com/territory-grounder/grounder/core/db"
	"github.com/territory-grounder/grounder/core/httpapi"
)

// shadowProposalLister is the narrow slice of db.TriageStore the proposals read surface consumes.
// Declared here (consumer-side interface, repo idiom) so the composition oracle can drive the adapter
// with a fake and stay always-on — the Stage-1 review proved the route can be green in every unit test
// while DEAD in the shipped binary (Deps.Proposals never wired); this file + its oracle close that gap.
type shadowProposalLister interface {
	ListShadowProposals(ctx context.Context, limit int) ([]db.ShadowProposalRow, error)
	CountShadowProposals(ctx context.Context) (int, error)
	CounterfactualSince(ctx context.Context, since time.Time) (int, int, int, error)
}

// proposalsReadStore adapts the triage store's shadow-proposal reads to the console read surface
// (spec/026 REQ-2607). Same shape as groundingReadStore: the principal is accepted and ignored — the
// surface is read-only and auth happens at the router; rows are already-screened persisted records.
type proposalsReadStore struct{ s shadowProposalLister }

func (r proposalsReadStore) ShadowProposals(ctx context.Context, _ auth.Principal, limit int) ([]httpapi.ShadowProposal, int, error) {
	rows, err := r.s.ListShadowProposals(ctx, limit)
	if err != nil {
		return nil, 0, err
	}
	total, err := r.s.CountShadowProposals(ctx)
	if err != nil {
		return nil, 0, err
	}
	out := make([]httpapi.ShadowProposal, 0, len(rows))
	for _, row := range rows {
		out = append(out, httpapi.ShadowProposal{
			ExternalRef: row.ExternalRef,
			Host:        row.Host,
			AlertRule:   row.AlertRule,
			Op:          row.Op,
			OpClass:     row.OpClass,
			Band:        row.Band,
			Rationale:   row.Rationale,
			UndoSketch:  row.UndoSketch,
			Confidence:  row.Confidence,
			Attribution: row.Attribution,
			CreatedAt:   row.CreatedAt,
			// TG-307: carry the derived diagnosis signal through to the console DTO (the text stays gated).
			DiagnosisRecorded:     row.DiagnosisRecorded,
			DiagnosisContradicted: row.DiagnosisContradicted,
			DiagnosisUncited:      row.DiagnosisUncited,
		})
	}
	return out, total, nil
}

// Counterfactual answers the headline over a window. Same principal handling as the list: accepted and
// ignored, because the surface is read-only and auth happens at the router.
func (r proposalsReadStore) Counterfactual(ctx context.Context, _ auth.Principal, window time.Duration) (int, int, int, error) {
	return r.s.CounterfactualSince(ctx, time.Now().Add(-window))
}
