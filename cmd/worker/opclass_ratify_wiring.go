package main

import (
	"context"

	"github.com/territory-grounder/grounder/core/actuate/opschema"
	"github.com/territory-grounder/grounder/core/db"
	"github.com/territory-grounder/grounder/temporal/opclassratify"
	"github.com/territory-grounder/grounder/tools/embedexport/render"
)

// The composition seams for the earned op-class ratify lane (spec/028 T-028-7).
//
// Both adapters are deliberately THIN, and the thinness is the point. The Stage-1 review's defect class was
// a fully-tested surface that was never wired; the failure mode one step past that is a wired surface whose
// adapter quietly does something different from what the tested code does. So neither of these makes a
// decision: one renames struct fields, the other calls a pure function.

// opClassOverlayBackend adapts the pgx overlay store to the lane's Overlay port.
//
// The rename is real work — the lane speaks Grant, the store speaks RatifiedRow — and it is exactly why
// this is a named type with a compile-time proof rather than an anonymous closure: a transposition here
// would record the wrong approver against the wrong capability, and the compiler is the only reviewer
// guaranteed to read it.
type opClassOverlayBackend struct{ s *db.OpClassRatifiedStore }

func (b opClassOverlayBackend) Ratify(ctx context.Context, g opclassratify.Grant) error {
	_, err := b.s.Ratify(ctx, db.RatifiedRow{
		OpClass:          g.OpClass,
		Spec:             g.Spec,
		EntryHash:        g.EntryHash,
		Family:           g.Family,
		Tier:             g.Tier,
		PromoteThreshold: g.PromoteThreshold,
		CandidateKey:     g.CandidateKey,
		Approver:         g.Approver,
		Rationale:        g.Rationale,
		LedgerSeq:        g.LedgerSeq,
	})
	return err
}

func (b opClassOverlayBackend) Revoke(ctx context.Context, opClass, approver, rationale string, ledgerSeq int64) error {
	return b.s.Revoke(ctx, opClass, approver, rationale, ledgerSeq)
}

func (b opClassOverlayBackend) IsLive(ctx context.Context, opClass string) (bool, error) {
	return b.s.IsLive(ctx, opClass)
}

// embedExporter is the export-embed verb's renderer — the SAME function the `embedexport` CLI runs.
//
// Shared rather than reimplemented because the console verb and the hand-run command must produce the
// identical artifact. If they could drift, the reviewer reading a console-generated MR would be approving a
// document whose checklist nobody tests.
type embedExporter struct{}

func (embedExporter) MRBody(spec opschema.OpClassSpec) (string, error) {
	snippet, err := render.Snippet(spec)
	if err != nil {
		return "", err
	}
	return render.MRBody(spec, snippet), nil
}

// Compile-time proof that the composed types satisfy the ports the lane declares. The Stage-1 defect class
// was a surface that type-checked in isolation and was never reachable from main; these assertions cost
// nothing and make that specific mistake impossible to merge.
var (
	_ opclassratify.Overlay  = opClassOverlayBackend{}
	_ opclassratify.Exporter = embedExporter{}
	_ opclassratify.Loader   = (*db.OpClassCandidateStore)(nil)
	_ opclassratify.Ladder   = (*db.PolicyGraduationStore)(nil)
)
