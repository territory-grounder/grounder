package skillstore

import (
	"context"
	"fmt"
	"strings"

	"github.com/territory-grounder/grounder/core/execclass"
)

// knownPhases mirrors agent/skills.Phase — the closed phase vocabulary a predicate may reference.
var knownPhases = map[string]bool{"investigate": true, "execute": true}

// knownDomains mirrors agent/skills.Domain — the closed platform vocabulary (TG-36). DomainUnknown ("")
// is intentionally NOT a writable value: a skill scoped to "unknown" is a contradiction, and the empty
// list already means "not domain-scoped". cisco and linux joined with their compiled lanes (cisco had
// been missed when TG-85 landed — a store-graduated cisco-scoped skill would have been refused as an
// unknown domain while the compiled lane routed it fine).
var knownDomains = map[string]bool{"kubernetes": true, "proxmox": true, "cisco": true, "linux": true, "storage": true}

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
	for _, d := range aw.Domains {
		if !knownDomains[d] {
			return fmt.Errorf("%w: domain %q", ErrBadPredicate, d)
		}
	}
	return nil
}

// ValidateDraft is the single write-time gate for a new version row (REQ-1301/1303/1305/1315/1316):
// per-CLASS body bounds, predicate vocabulary, mandatory rationale, and the pinned-skill refusal. The
// store implementation calls it before any INSERT; the console/API surface gets its errors verbatim.
// The body cap is the CLASS's (MaxBodyBytes), not the schema ceiling's: a 9000-byte skill body refuses
// here even though the 0089 schema admits it for the larger classes.
func ValidateDraft(ctx context.Context, st Store, v Version) error {
	if strings.TrimSpace(v.Rationale) == "" {
		return ErrRationaleRequired
	}
	if err := ValidatePredicate(v.AppliesWhen); err != nil {
		return err
	}
	sk, err := st.GetSkill(ctx, v.SkillName)
	if err != nil {
		return err
	}
	// Class rule BEFORE the pin flag, so the refusal names the LAW (ADR-0017: rubric never graduates)
	// rather than this row's configuration — a rubric row with its pin flag lost by SQL edit still refuses.
	if DefaultClass(sk.Class) == ClassRubric {
		return fmt.Errorf("%w: %s", ErrRubricNeverDrafts, sk.Name)
	}
	if sk.Pinned {
		return fmt.Errorf("%w: %s", ErrPinnedSkill, sk.Name)
	}
	class := DefaultClass(sk.Class)
	if !ValidArtifactClass(class) {
		return fmt.Errorf("%w: %q on skill %s", ErrUnknownClass, string(sk.Class), sk.Name)
	}
	// MaxBodyBytes cannot be 0 past the enumeration check above, but the comparison keeps failing
	// closed even if the two ever drift: an unknown cap admits nothing.
	if maxB := MaxBodyBytes(class); len(v.Body) < 1 || len(v.Body) > maxB {
		return fmt.Errorf("%w: class %s admits 1..%d bytes, got %d", ErrBodyBounds, class, maxB, len(v.Body))
	}
	if v.ContentHash != ContentHash(v.Body, v.AppliesWhen) {
		return fmt.Errorf("skillstore: content hash mismatch for %s v%s", v.SkillName, v.Version)
	}
	return nil
}
