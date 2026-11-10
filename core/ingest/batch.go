package ingest

// Batch is what one grouped-transport payload normalized to. Every item the payload carried is accounted for:
// admitted as an envelope, skipped as noise, or rejected with the grammar's reason. Truncated is how many items
// the sender says it left out of the payload before sending it.
type Batch struct {
	Envelopes []IncidentEnvelope
	Rejected  []Rejection
	Skipped   int
	Truncated int
}

// Rejection is one item that failed the grammar: its zero-based position in the payload and the field-level
// reason the grammar gave.
type Rejection struct {
	Index  int
	Reason string
}

// Items is how many items the payload itself carried.
func (b Batch) Items() int { return len(b.Envelopes) + len(b.Rejected) + b.Skipped }
