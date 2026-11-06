package main

import (
	"context"
	"fmt"
	"strings"
	"time"

	enumspb "go.temporal.io/api/enums/v1"
	"go.temporal.io/sdk/client"

	"github.com/territory-grounder/grounder/core/actuate/opschema"
	"github.com/territory-grounder/grounder/core/httpapi"
	"github.com/territory-grounder/grounder/core/opclasscat"
	tg "github.com/territory-grounder/grounder/temporal"
	"github.com/territory-grounder/grounder/temporal/opclassratify"
)

// opClassWriteBackend implements httpapi.OpClassWriter by starting the worker's verb workflow — the
// grounder never appends to the hash chain itself (spec/028 REQ-2813; the manifestWriteBackend precedent).
//
// The grounder holds a READ store for the ledger and nothing else. That is the property this adapter
// preserves: a ratification is the single most consequential write in the system (it authors an argv
// template that runs as root), and it happens in exactly one process, on one chain, through one state
// machine.
type opClassWriteBackend struct{ tc client.Client }

// start runs one verb and returns the worker's result. The workflow id names the verb AND the noun so two
// different verbs on the same class are not deduplicated against each other.
func (b opClassWriteBackend) start(ctx context.Context, idPart string, req opclassratify.Request) (httpapi.OpClassOutcome, error) {
	ctx, cancel := context.WithTimeout(ctx, 40*time.Second)
	defer cancel()
	run, err := b.tc.ExecuteWorkflow(ctx, client.StartWorkflowOptions{
		ID:        fmt.Sprintf("tg/opclassratify/%s/%s", req.Verb, idPart),
		TaskQueue: tg.TaskQueueRunner,
		// A completed same-id run may legitimately repeat (a class revoked, re-ratified, revoked again).
		// An IN-FLIGHT duplicate is a double console click and is rejected by Temporal's running-dedup —
		// which is the guard that matters here, because two concurrent ratifications of one candidate would
		// race the partial unique index rather than the state machine.
		WorkflowIDReusePolicy: enumspb.WORKFLOW_ID_REUSE_POLICY_ALLOW_DUPLICATE,
	}, opclassratify.OpClassVerbWorkflow, req)
	if err != nil {
		return httpapi.OpClassOutcome{}, err
	}
	var res opclassratify.Result
	if err := run.Get(ctx, &res); err != nil {
		return httpapi.OpClassOutcome{}, unwrapOpClassErr(err)
	}
	return httpapi.OpClassOutcome{
		CandidateKey: res.CandidateKey, OpClass: res.OpClass, Status: res.Status, Level: res.Level,
		LedgerSeq: res.LedgerSeq, EntryHash: res.EntryHash, Artifact: res.Artifact,
	}, nil
}

func (b opClassWriteBackend) Ratify(ctx context.Context, key string, spec opschema.OpClassSpec, promoteThreshold int, rationale, approver string) (httpapi.OpClassOutcome, error) {
	return b.start(ctx, key, opclassratify.Request{
		Verb: opclassratify.VerbRatify, CandidateKey: key, OpClass: spec.OpClass, Spec: spec,
		PromoteThreshold: promoteThreshold, Rationale: rationale, Approver: approver,
	})
}

func (b opClassWriteBackend) Dismiss(ctx context.Context, key, rationale, approver string) (httpapi.OpClassOutcome, error) {
	return b.start(ctx, key, opclassratify.Request{
		Verb: opclassratify.VerbDismiss, CandidateKey: key, Rationale: rationale, Approver: approver,
	})
}

func (b opClassWriteBackend) Demote(ctx context.Context, opClass, rationale, approver string) (httpapi.OpClassOutcome, error) {
	return b.start(ctx, opClass, opclassratify.Request{
		Verb: opclassratify.VerbDemote, OpClass: opClass, Rationale: rationale, Approver: approver,
	})
}

func (b opClassWriteBackend) Revoke(ctx context.Context, opClass, rationale, approver string) (httpapi.OpClassOutcome, error) {
	return b.start(ctx, opClass, opclassratify.Request{
		Verb: opclassratify.VerbRevoke, OpClass: opClass, Rationale: rationale, Approver: approver,
	})
}

func (b opClassWriteBackend) ExportEmbed(ctx context.Context, opClass, rationale, approver string) (httpapi.OpClassOutcome, error) {
	return b.start(ctx, opClass, opclassratify.Request{
		Verb: opclassratify.VerbExportEmbed, OpClass: opClass, Rationale: rationale, Approver: approver,
	})
}

// unwrapOpClassErr maps a workflow-wrapped refusal back onto the typed sentinels the surface switches on
// (a Temporal ApplicationError carries only the message). Longest-message-first so no sentinel's text can
// shadow a more specific one — the manifestwrite/skillwrite fragility note applies verbatim; replace with
// typed unwrapping when the SDK propagates activity error chains.
//
// The default is deliberately NOT a sentinel. A ratification refused by the laundering tripwire or by
// ValidateSpec produces a plain message with no sentinel to match, and it must reach the operator as the
// worker's own words: "your argv template matches what the model said" is actionable, and collapsing it
// into a generic 503 would tell an operator to retry the exact request the system will always refuse.
func unwrapOpClassErr(err error) error {
	msg := err.Error()
	for _, known := range []error{opclasscat.ErrRationaleRequired, opclasscat.ErrBadTransition} {
		if strings.Contains(msg, known.Error()) {
			return fmt.Errorf("%w (%s)", known, "worker refused")
		}
	}
	if strings.Contains(msg, opclassratify.ErrNotFound.Error()) {
		return fmt.Errorf("%w (%s)", httpapi.ErrOpClassCandidateNotFound, "worker refused")
	}
	if strings.Contains(msg, opclassratify.ErrNotGranted.Error()) {
		return fmt.Errorf("%w (%s)", httpapi.ErrOpClassNotGranted, "worker refused")
	}
	return err
}

var _ httpapi.OpClassWriter = opClassWriteBackend{}

// opClassReadBackend implements httpapi.OpClassCandidateReader over the pgx candidate store.
//
// It exists to translate ONE thing: the store's "no such row" is (zero, false, nil), and the surface's is a
// typed 404 sentinel. Everything else passes straight through, because a read adapter that reshaped the
// dossier would be a second opinion about what an operator is shown.
type opClassReadBackend struct{ store opClassCandidateStore }

// opClassCandidateStore is the slice of *db.OpClassCandidateStore this adapter needs.
type opClassCandidateStore interface {
	LiveCandidates(ctx context.Context) ([]opclasscat.Candidate, error)
	CandidateByKey(ctx context.Context, key string) (opclasscat.Candidate, bool, error)
	Occurrences(ctx context.Context, key string, since time.Time) ([]opclasscat.Occurrence, error)
	CandidateTallies(ctx context.Context) (map[string]opclasscat.Tally, error)
}

func (b opClassReadBackend) OpClassCandidates(ctx context.Context, limit int) (httpapi.OpClassCandidatePage, error) {
	rows, err := b.store.LiveCandidates(ctx)
	if err != nil {
		return httpapi.OpClassCandidatePage{}, err
	}
	// The total is taken BEFORE the page is cut — that is the whole point of carrying it separately.
	total := len(rows)
	if limit > 0 && len(rows) > limit {
		rows = rows[:limit]
	}
	tallies, err := b.store.CandidateTallies(ctx)
	if err != nil {
		return httpapi.OpClassCandidatePage{}, err
	}
	return httpapi.OpClassCandidatePage{Candidates: rows, Tallies: tallies, Total: total}, nil
}

func (b opClassReadBackend) OpClassDossier(ctx context.Context, key string) (opclasscat.Candidate, []opclasscat.Occurrence, error) {
	c, found, err := b.store.CandidateByKey(ctx, key)
	if err != nil {
		return opclasscat.Candidate{}, nil, err
	}
	if !found {
		return opclasscat.Candidate{}, nil, httpapi.ErrOpClassCandidateNotFound
	}
	// The FULL journal, not a window: the dossier's job is to show an operator every occurrence that argues
	// for this capability, and a windowed read would quietly shrink the evidence a grant rests on.
	occs, err := b.store.Occurrences(ctx, key, time.Time{})
	if err != nil {
		return opclasscat.Candidate{}, nil, err
	}
	return c, occs, nil
}

var _ httpapi.OpClassCandidateReader = opClassReadBackend{}
