package reconcile

import (
	"context"
	"errors"

	"github.com/territory-grounder/grounder/core/audit"
	"github.com/territory-grounder/grounder/core/schema"
)

// TicketTransitioner transitions an incident's ticket through the tracker module (YouTrack / Jira /
// GitHub Issues / ServiceNow / native). Implementations are per-source, capability-scoped modules; the
// in-memory fake drives the oracle.
type TicketTransitioner interface {
	Transition(ctx context.Context, externalRef string, to TicketState) error
}

// CloseoutRecord is one immutable, append-only close-out row (REQ-203), stamped schema_version and
// chained into the governance ledger.
type CloseoutRecord struct {
	SessionID     string
	ExternalRef   string
	ActionID      string
	Resolution    ResolutionType
	Ticket        TicketState
	Closed        bool
	SchemaVersion schema.Version
}

// ErrIncompleteCloseout fails closed on a close-out missing a load-bearing field.
var ErrIncompleteCloseout = errors.New("reconcile: close-out record missing a required field (session_id, external_ref)")

// CloseOut applies a reconcile decision: it transitions the ticket through the tracker module, stamps
// and returns the append-only close-out record, and appends the governance decision to the hash-chained
// ledger (INV-19). It is the single writer of a close-out. A close is recorded only for a decision that
// closed (REQ-201/202/204 are decided in Reconcile).
func CloseOut(ctx context.Context, l *audit.Ledger, tickets TicketTransitioner, s FinishedSession, d ReconcileDecision) (CloseoutRecord, error) {
	if s.SessionID == "" || s.ExternalRef == "" {
		return CloseoutRecord{}, ErrIncompleteCloseout
	}
	if err := tickets.Transition(ctx, s.ExternalRef, d.Ticket); err != nil {
		return CloseoutRecord{}, err
	}
	ver, err := schema.Stamp(schema.TableSessionRiskAudit) // close-out shares the audit-spine schema family
	if err != nil {
		return CloseoutRecord{}, err
	}
	rec := CloseoutRecord{
		SessionID:     s.SessionID,
		ExternalRef:   s.ExternalRef,
		ActionID:      s.ActionID,
		Resolution:    d.Resolution,
		Ticket:        d.Ticket,
		Closed:        d.Close,
		SchemaVersion: ver,
	}
	actionID := s.ActionID
	if actionID == "" {
		actionID = "session:" + s.SessionID // a close-out without a bound action still needs a ledger key
	}
	if _, err := l.Append(audit.GovDecision{
		Decision: "close-out:" + string(d.Resolution),
		Reason:   d.Reason,
		ActionID: actionID,
		Withheld: !d.Close, // leaving the incident open is withholding autonomous closure
	}); err != nil {
		return CloseoutRecord{}, err
	}
	return rec, nil
}
