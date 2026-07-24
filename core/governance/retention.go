package governance

import (
	"context"
	"errors"

	"github.com/territory-grounder/grounder/core/audit"
)

// TranscriptStore is the PURGEABLE operational store for raw judged transcripts and their scores. It is
// governed by a retention TTL and right-to-erasure — distinct from the immutable audit spine.
type TranscriptStore interface {
	PurgeTranscripts(ctx context.Context, sessionIDs []string) (int, error)
	Count(ctx context.Context) int
}

// MemTranscriptStore is the in-memory oracle implementation.
type MemTranscriptStore struct {
	transcripts map[string]string // sessionID -> raw transcript
}

// NewMemTranscriptStore seeds a store from a map of sessionID -> raw transcript.
func NewMemTranscriptStore(seed map[string]string) *MemTranscriptStore {
	cp := make(map[string]string, len(seed))
	for k, v := range seed {
		cp[k] = v
	}
	return &MemTranscriptStore{transcripts: cp}
}

// PurgeTranscripts hard-deletes the named raw transcripts (right-to-erasure) and returns the count removed.
func (s *MemTranscriptStore) PurgeTranscripts(_ context.Context, sessionIDs []string) (int, error) {
	n := 0
	for _, id := range sessionIDs {
		if _, ok := s.transcripts[id]; ok {
			delete(s.transcripts, id)
			n++
		}
	}
	return n, nil
}

// Count returns the number of raw transcripts remaining.
func (s *MemTranscriptStore) Count(_ context.Context) int { return len(s.transcripts) }

// ErrSpineTouched is returned if a raw-transcript purge would alter the audit spine.
var ErrSpineTouched = errors.New("governance: a transcript purge must not touch the audit spine")

// RetentionManager enforces the retention split (paradigm-rule 5): the raw judged transcripts are
// purgeable operational memory, while the demotion decisions and judged-fraction facts live on the
// immutable audit spine. A right-to-erasure purge of raw transcripts SHALL NOT remove any audit-spine
// record (REQ-305). The two stores are drawn separately here and the invariant is checked.
type RetentionManager struct {
	Transcripts TranscriptStore
	Spine       *audit.Ledger
}

// PurgeRawTranscripts hard-deletes the named raw transcripts and asserts the audit spine is untouched
// (its length is unchanged). It returns the number of transcripts purged.
func (r *RetentionManager) PurgeRawTranscripts(ctx context.Context, sessionIDs []string) (int, error) {
	before := r.Spine.Len()
	n, err := r.Transcripts.PurgeTranscripts(ctx, sessionIDs)
	if err != nil {
		return 0, err
	}
	if r.Spine.Len() != before {
		return 0, ErrSpineTouched
	}
	return n, nil
}
