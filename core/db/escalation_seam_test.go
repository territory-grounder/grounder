package db

import (
	"github.com/territory-grounder/grounder/core/escalation"
)

// The durable pgx store must satisfy the SAME requeue seam the in-memory oracle does, so the escalation
// controller (spec/003) runs over either unchanged: an operator with TG_DB_DSN gets a requeue lane that
// survives a restart behind the same controller CI exercises against the in-memory twin. A signature drift
// on either side stops this compiling — the interchangeability is enforced at the type level, not asserted.
var _ escalation.Store = (*EscalationStore)(nil)
