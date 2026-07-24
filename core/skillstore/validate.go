package skillstore

import (
	"context"
	"fmt"
	"strings"

	"github.com/territory-grounder/grounder/core/execclass"
)

// knownPhases mirrors agent/skills.Phase — the closed phase vocabulary a predicate may reference.
var knownPhases = map[string]bool{"investigate": true, "execute": true}

// knownClasses mirrors core/execclass — the closed execution-class vocabulary.
var knownClasses = map[string]bool{
	string(execclass.Deterministic):     true,
	string(execclass.FastAgent):         true,
	string(execclass.StandardAgent):     true,
	string(execclass.DeepInvestigation): true,
	string(execclass.HumanLed):          true,
}

// ValidatePredicate rejects an applies-when outside the closed vocabulary (REQ-1303). The predicate
// language is deliberately not extensible at runtime: an unknown token is an error at WRITE time, so
// composition never meets a predicate it cannot evaluate purely.
func ValidatePredicate(aw AppliesWhen) error {
	for _, p := range aw.Phases {
		if !knownPhases[p] {
			return fmt.Errorf("%w: phase %q", ErrBadPredicate, p)
		}
	}
	for _, c := range aw.ExecClasses {
		if !knownClasses[c] {
			return fmt.Errorf("%w: execution class %q", ErrBadPredicate, c)
		}
	}
	return nil
}

// ValidateDraft is the single write-time gate for a new version row (REQ-1301/1303/1305): body bounds,
// predicate vocabulary, mandatory rationale, and the pinned-skill refusal. The store implementation
// calls it before any INSERT; the console/API surface gets its errors verbatim.
func ValidateDraft(ctx context.Context, st Store, v Version) error {
	if strings.TrimSpace(v.Rationale) == "" {
		return ErrRationaleRequired
	}
	if l := len(v.Body); l < 1 || l > 8192 {
		return ErrBodyBounds
	}
	if err := ValidatePredicate(v.AppliesWhen); err != nil {
		return err
	}
	sk, err := st.GetSkill(ctx, v.SkillName)
	if err != nil {
		return err
	}
	if sk.Pinned {
		return fmt.Errorf("%w: %s", ErrPinnedSkill, sk.Name)
	}
	if v.ContentHash != ContentHash(v.Body, v.AppliesWhen) {
		return fmt.Errorf("skillstore: content hash mismatch for %s v%s", v.SkillName, v.Version)
	}
	return nil
}
