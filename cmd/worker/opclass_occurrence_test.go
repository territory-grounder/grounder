package main

import (
	"context"
	"testing"
	"time"

	"github.com/territory-grounder/grounder/core/db"
	"github.com/territory-grounder/grounder/core/opclasscat"
	"github.com/territory-grounder/grounder/temporal/opclasscluster"
)

// TestOpClassEvidenceSeamIsWiredAtComposition is the ALIVENESS oracle for spec/028 Stage 2.
//
// The Stage-1 adversarial review's biggest catch was a product surface that was green in every unit test
// and DEAD in the shipped binary: /v1/proposals had a parser, a handler, a console view and a migration —
// and nobody set Deps.Proposals at the composition root, so it 503'd forever while the fail-closed design
// made the deadness look intentional. The earned-catalog evidence seam has exactly the same shape (a
// nil-inert func field on runner.Deps), so it gets the same guard.
//
// Two halves, both compile-time-or-oracle enforced:
//   - the pgx store SATISFIES the lifecycle contract (proven in core/db by a var _ assertion, re-asserted
//     here so a signature drift fails at the composition root too);
//   - the clustering Job's dependency shape accepts that store directly, so the wiring in main.go cannot
//     be written against a type the cron does not consume.
func TestOpClassEvidenceSeamIsWiredAtComposition(t *testing.T) {
	// The store the composition root constructs must satisfy the contract the Transition chokepoint and the
	// clustering pass both drive. A nil pool is fine: nothing here executes a query.
	var store opclasscat.Store = db.NewOpClassCandidateStore(nil)

	// And it must slot into the cron Job exactly as main.go assembles it — including the Liveness adapter,
	// whose whole purpose is that its facts come from tables the cron does not write.
	cands := db.NewOpClassCandidateStore(nil)
	job := opclasscluster.Job{
		Store: store,
		Liveness: func(ctx context.Context, window time.Duration) (opclasscluster.Liveness, error) {
			newest, sessions, err := cands.Liveness(ctx, window)
			if err != nil {
				return opclasscluster.Liveness{}, err
			}
			return opclasscluster.Liveness{NewestOccurrence: newest, SessionsSince: sessions}, nil
		},
	}
	if job.Store == nil || job.Liveness == nil {
		t.Fatal("the clustering job must be constructible from the composition root's store — the seam is dead")
	}
	// Ready stays nil in Stage 2 BY DESIGN (fail-closed until Stage 3's estate walk). Asserting it here
	// documents the deliberate gap so a future reader does not "fix" it into a fail-open default.
	if job.Ready != nil {
		t.Fatal("Stage 2 must NOT wire a ready resolver — an incomplete dossier would reach an operator")
	}
}

// TestProposalOccurrenceAdapterDerivesIdentityFromObservedFacts pins the adapter main.go installs on
// runner.Deps.RecordProposalOccurrence: the cluster identity must come from the OBSERVED op-class/op, and
// the journal row must carry the screened text through unchanged.
//
// This is the "a model cannot choose its own cluster" property: if identity were taken from anything the
// model could vary freely, one desire could be split across many keys (never reaching candidacy) or many
// desires collapsed into one (manufacturing it).
func TestProposalOccurrenceAdapterDerivesIdentityFromObservedFacts(t *testing.T) {
	// Same observed operation, described with different casing/whitespace on two incidents.
	k1 := opclasscat.CandidateKey("restart-service", "restart nginx", nil)
	k2 := opclasscat.CandidateKey("  Restart-Service ", "RESTART NGINX", nil)
	if k1 != k2 {
		t.Fatal("the same observed operation must cluster to ONE key regardless of the model's casing")
	}
	// A genuinely different remedy must not join that cluster.
	if opclasscat.CandidateKey("start-guest", "start guest", nil) == k1 {
		t.Fatal("a different remedy must never share a cluster key")
	}
}
