package ledgeranchor

// THE WITNESS CHECKS ITS OWN HISTORY (TG-674, deep-review F16). TemporalVerifyJob used to START from the
// database's anchor list and ask Temporal "do you remember this seq?" — so a whole-database rollback that
// also truncated ledger_anchor simply offered fewer seqs to ask about, and the cross-check read clean. The
// consuming half has to walk the OTHER way as well: list what Temporal itself remembers witnessing, and demand
// that every one of those seqs is still in the live chain with the same hash and still in ledger_anchor. A
// witnessed seq the chain no longer reaches is a rollback, whatever the DB says.

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"go.temporal.io/api/workflowservice/v1"

	"github.com/territory-grounder/grounder/core/audit"
)

// TemporalAnchorLister lists the anchors Temporal's OWN visibility store says it witnessed for a domain,
// newest first, up to limit — a *TemporalWitness in production, a fake in tests.
type TemporalAnchorLister interface {
	ListWitnesses(ctx context.Context, domain string, limit int) ([]audit.Anchor, error)
}

// temporalLister is the one extra SDK method the own-history walk needs; a real *client.Client has it, and the
// test fake implements only this.
type temporalLister interface {
	ListWorkflow(ctx context.Context, request *workflowservice.ListWorkflowExecutionsRequest) (*workflowservice.ListWorkflowExecutionsResponse, error)
}

// witnessIDPrefix is the Workflow ID prefix every witness of one domain shares (WitnessWorkflowID).
func witnessIDPrefix(domain string) string { return fmt.Sprintf("tg/ledger-anchor/%s/seq-", domain) }

// ListWitnesses implements TemporalAnchorLister over Temporal's visibility store: completed WitnessAnchorWorkflow
// executions, newest first (the server's default order), filtered to this domain's id prefix on the client
// side (a prefix predicate is not guaranteed on standard visibility), each result read back through the same
// GetWorkflow path ReadWitness uses. A client that cannot list is an error — never an empty, clean-looking
// answer — so TemporalVerifyJob fails safe rather than skipping the walk.
func (t *TemporalWitness) ListWitnesses(ctx context.Context, domain string, limit int) ([]audit.Anchor, error) {
	if t.Client == nil {
		return nil, errors.New("ledgeranchor: TemporalWitness has no Temporal client")
	}
	lister, ok := t.Client.(temporalLister)
	if !ok {
		return nil, errors.New("ledgeranchor: the Temporal client cannot list executions — the own-history walk cannot run")
	}
	if limit <= 0 {
		limit = DefaultTemporalVerifyRecent
	}
	prefix := witnessIDPrefix(domain)
	var out []audit.Anchor
	var token []byte
	for pages := 0; len(out) < limit && pages < 16; pages++ {
		resp, err := lister.ListWorkflow(ctx, &workflowservice.ListWorkflowExecutionsRequest{
			Query:         "WorkflowType = 'WitnessAnchorWorkflow' AND ExecutionStatus = 'Completed'",
			PageSize:      int32(limit * 4),
			NextPageToken: token,
		})
		if err != nil {
			return nil, fmt.Errorf("ledgeranchor: list witness executions: %w", err)
		}
		for _, ex := range resp.GetExecutions() {
			id := ex.GetExecution().GetWorkflowId()
			if !strings.HasPrefix(id, prefix) {
				continue
			}
			var a audit.Anchor
			if err := t.Client.GetWorkflow(ctx, id, ex.GetExecution().GetRunId()).Get(ctx, &a); err != nil {
				return nil, fmt.Errorf("ledgeranchor: read witness %s: %w", id, err)
			}
			out = append(out, a)
			if len(out) >= limit {
				break
			}
		}
		token = resp.GetNextPageToken()
		if len(token) == 0 {
			break
		}
	}
	return out, nil
}

// verifyOwnHistory is the second half of TemporalVerifyJob.Run: every anchor Temporal remembers witnessing must
// still be in the live chain with the same hash; and WHERE ledger_anchor also holds that seq, the two witnesses
// must agree. A Temporal-only witness is NOT a contradiction: the DB recorder and the Temporal recorder are two
// independent jobs reading HEAD on their own ticks, so they witness different seqs most of the time (fresh-eyes
// review of this change, 2026-11-07 — demanding presence in ledger_anchor would page "tamper" on every normal
// pass). The DB-list pass (Run) already tolerates the mirror case, a DB anchor with no Temporal witness.
func verifyOwnHistory(witnesses []audit.Anchor, chainBySeq map[int64]audit.RowRef, maxSeq int64, dbAnchors []audit.Anchor) (int, error) {
	dbBySeq := make(map[int64]audit.Anchor, len(dbAnchors))
	for _, a := range dbAnchors {
		dbBySeq[a.Seq] = a
	}
	checked := 0
	for _, w := range witnesses {
		if w.Seq == 0 {
			continue
		}
		row, present := chainBySeq[w.Seq]
		if !present {
			return checked, fmt.Errorf("%w: Temporal's own history holds a witness for seq %d (hash %s) but the live chain stops at seq %d — the database was rolled back or truncated past an already-witnessed row",
				ErrTemporalWitnessMismatch, w.Seq, shortHash(w.Hash), maxSeq)
		}
		if row.Hash != w.Hash {
			return checked, fmt.Errorf("%w: Temporal's own history witnessed seq %d as %s but the live chain carries %s — the row was rewritten after it was witnessed",
				ErrTemporalWitnessMismatch, w.Seq, shortHash(w.Hash), shortHash(row.Hash))
		}
		dbA, inDB := dbBySeq[w.Seq]
		if inDB && (dbA.Hash != w.Hash || dbA.Digest != w.Digest) {
			return checked, fmt.Errorf("%w: seq %d — Temporal's own witness (hash %s) disagrees with the DB-recorded anchor (hash %s)",
				ErrTemporalWitnessMismatch, w.Seq, shortHash(w.Hash), shortHash(dbA.Hash))
		}
		checked++
	}
	return checked, nil
}
