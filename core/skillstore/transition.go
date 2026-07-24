package skillstore

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/territory-grounder/grounder/core/audit"
)

// allowedTransitions is the whole state machine (REQ-1301). Anything absent is refused — including
// self-transitions and any path that would resurrect a retired or rejected version (a rework is a NEW
// draft row with parent_version_id set, keeping history append-only).
var allowedTransitions = map[Status][]Status{
	StatusDraft:      {StatusTrial, StatusRejected},
	StatusTrial:      {StatusProduction, StatusRejected, StatusDraft},
	StatusProduction: {StatusRetired},
}

// clipLog bounds the append-only rationale log: pathological transition churn keeps the NEWEST tail
// (the ledger holds the full history anyway — the row log is the console-convenience copy).
func clipLog(s string) string {
	const maxLog = 16384
	if len(s) <= maxLog {
		return s
	}
	return "[… clipped — full history in the governance ledger]" + s[len(s)-maxLog:]
}

func transitionAllowed(from, to Status) bool {
	for _, t := range allowedTransitions[from] {
		if t == to {
			return true
		}
	}
	return false
}

// Ledger is the slice of audit.Ledger the store needs — append-only governance decisions (INV-19).
type Ledger interface {
	Append(d audit.GovDecision) (audit.LedgerEntry, error)
}

// Store is the persistence surface Transition drives. The pgx implementation is compose-tested; the
// in-memory fake backs the CI oracles (D5). Update semantics: persist the version's Status, Rationale,
// LedgerSeq, StatusChangedAt.
type Store interface {
	GetVersion(ctx context.Context, id int64) (Version, error)
	GetSkill(ctx context.Context, name string) (Skill, error)
	// ProductionVersion returns the current production row for a skill (ok=false when none).
	ProductionVersion(ctx context.Context, skillName string) (Version, bool, error)
	UpdateVersion(ctx context.Context, v Version) error
}

// Transition is the ONLY way a skill version changes status (REQ-1301): it enforces the state machine,
// appends the mandatory rationale to the version's append-only rationale log, ledger-records the
// decision, and — for a graduation — retires the incumbent production version first so the structural
// one-production index never trips mid-flight (REQ-1302). The ledger entry is written before the row so
// a crash leaves an over-recorded ledger, never an unrecorded state change.
func Transition(ctx context.Context, st Store, lg Ledger, versionID int64, to Status, rationale string) (Version, error) {
	rationale = strings.TrimSpace(rationale)
	if rationale == "" {
		return Version{}, ErrRationaleRequired
	}
	v, err := st.GetVersion(ctx, versionID)
	if err != nil {
		return Version{}, err
	}
	if !transitionAllowed(v.Status, to) {
		return Version{}, fmt.Errorf("%w: %s -> %s (skill %s v%s)", ErrBadTransition, v.Status, to, v.SkillName, v.Version)
	}

	// Graduation structurally supersedes: the incumbent retires in the same logical step (REQ-1302).
	if to == StatusProduction {
		if cur, ok, perr := st.ProductionVersion(ctx, v.SkillName); perr != nil {
			return Version{}, perr
		} else if ok && cur.ID != v.ID {
			if _, err := Transition(ctx, st, lg, cur.ID, StatusRetired,
				fmt.Sprintf("superseded by v%s (version id %d) graduating", v.Version, v.ID)); err != nil {
				return Version{}, fmt.Errorf("retire incumbent production: %w", err)
			}
		}
	}

	entry, err := lg.Append(audit.GovDecision{
		Decision: "skill:" + string(to),
		Reason:   rationale,
		ActionID: fmt.Sprintf("skill:%s:v%s:%s", v.SkillName, v.Version, v.ContentHash[:12]),
		Withheld: to == StatusRejected || to == StatusRetired,
	})
	if err != nil {
		return Version{}, fmt.Errorf("ledger append: %w", err)
	}

	v.Status = to
	v.Rationale = clipLog(v.Rationale + "\n[" + string(to) + "] " + rationale)
	v.LedgerSeq = entry.Seq
	v.StatusChangedAt = time.Now().UTC()
	if err := st.UpdateVersion(ctx, v); err != nil {
		return Version{}, err
	}
	return v, nil
}
