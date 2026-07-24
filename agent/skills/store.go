package skills

import (
	"fmt"
	"sort"

	"github.com/territory-grounder/grounder/core/skillstore"
)

// This file is the store-backed composition path (spec/014 REQ-1303/1304/1305): production rows from the
// skill store compose the seed instead of the compiled bodies — with the compiled registry as a TOTAL
// fallback on any failure, pinned skills always composed from their compiled body, and a provenance
// record that makes every composed seed reconstructable (which version, which hash, which origin).

// Origin says where a composed skill's body came from.
type Origin string

const (
	// OriginStore: the body is a graduated store row.
	OriginStore Origin = "store"
	// OriginCompiled: the compiled body (no store row for this skill).
	OriginCompiled Origin = "compiled"
	// OriginPinned: a store row exists but the skill is pinned — the compiled body wins (REQ-1305).
	OriginPinned Origin = "pinned"
)

// SkillLoad is one skill's provenance in a composed registry.
type SkillLoad struct {
	Name        string
	Version     string
	ContentHash string
	Origin      Origin
	// VersionID is the store row's skill_version id for store-origin loads (0 otherwise). The
	// skill_load record carries it (name@version#id:store) so a judged session can be bound to the
	// exact graduated version the regression watch tracks (REQ-1310).
	VersionID int64
}

// Provenance records how a registry was built: per-skill origins plus the fallback reason when the
// store could not be used (empty Fallback = the store path succeeded).
type Provenance struct {
	Skills   map[string]SkillLoad
	Fallback string // non-empty ⇒ the compiled registry composed in full (REQ-1304)
}

// declarativeApplies converts a validated store predicate into the same pure-selector semantics the
// compiled registry uses (INV-08): empty predicate lists mean "always", and an EMPTY context value
// matches any list — an unclassified session fails toward MORE guidance, mirroring deepOrStandard.
func declarativeApplies(aw skillstore.AppliesWhen) func(Context) bool {
	phases := append([]string(nil), aw.Phases...)
	classes := append([]string(nil), aw.ExecClasses...)
	return func(ctx Context) bool {
		if len(phases) > 0 && ctx.Phase != "" && !contains(phases, string(ctx.Phase)) {
			return false
		}
		if len(classes) > 0 && ctx.ExecClass != "" && !contains(classes, string(ctx.ExecClass)) {
			return false
		}
		return true
	}
}

func contains(xs []string, want string) bool {
	for _, x := range xs {
		if x == want {
			return true
		}
	}
	return false
}

// NewFromStore builds the session registry from the store's production rows over the compiled registry.
// Semantics (REQ-1303/1304/1305):
//   - a store row overrides the compiled body BY NAME — unless the skill is pinned, where the compiled
//     body always composes (the row is reported with OriginPinned so drift is visible, never silent);
//   - compiled skills without a store row compose unchanged (OriginCompiled);
//   - store-only skills (new names) compose after the compiled set, ordered by Position then name;
//   - ANY invalid row (bad predicate, hash mismatch, empty body) fails the WHOLE store path back to the
//     compiled registry with the reason recorded — a partially-applied store is worse than none.
func NewFromStore(rows []skillstore.ProductionRow, compiled *Registry) (*Registry, Provenance) {
	prov := Provenance{Skills: map[string]SkillLoad{}}
	fallback := func(reason string) (*Registry, Provenance) {
		p := Provenance{Skills: map[string]SkillLoad{}, Fallback: reason}
		for _, s := range compiled.All() {
			p.Skills[s.Name] = SkillLoad{Name: s.Name, Version: s.Version, Origin: OriginCompiled}
		}
		return compiled, p
	}

	byName := map[string]skillstore.ProductionRow{}
	for _, r := range rows {
		if err := skillstore.ValidatePredicate(r.AppliesWhen); err != nil {
			return fallback(fmt.Sprintf("row %s v%s: %v", r.SkillName, r.Version, err))
		}
		if len(r.Body) == 0 || len(r.Body) > 8192 {
			// The write path enforces the 8 KiB cap, but composition re-checks it: a row written around
			// the API (raw SQL) with a correctly-computed hash would otherwise flood the seed.
			return fallback(fmt.Sprintf("row %s v%s: body out of bounds (%d bytes)", r.SkillName, r.Version, len(r.Body)))
		}
		if got := skillstore.ContentHash(r.Body, r.AppliesWhen); got != r.ContentHash {
			return fallback(fmt.Sprintf("row %s v%s: content hash mismatch", r.SkillName, r.Version))
		}
		if _, dup := byName[r.SkillName]; dup {
			return fallback(fmt.Sprintf("row %s: duplicate production row", r.SkillName))
		}
		byName[r.SkillName] = r
	}

	var out []Skill
	for _, c := range compiled.All() {
		row, has := byName[c.Name]
		switch {
		case has && row.Pinned:
			// REQ-1305 — the floor's compiled body always wins; report the shadowed row loudly.
			out = append(out, c)
			prov.Skills[c.Name] = SkillLoad{Name: c.Name, Version: c.Version, Origin: OriginPinned}
			delete(byName, c.Name)
		case has:
			out = append(out, Skill{Name: c.Name, Version: row.Version, AppliesWhen: declarativeApplies(row.AppliesWhen), Body: row.Body})
			prov.Skills[c.Name] = SkillLoad{Name: c.Name, Version: row.Version, ContentHash: row.ContentHash, Origin: OriginStore, VersionID: row.VersionID}
			delete(byName, c.Name)
		default:
			out = append(out, c)
			prov.Skills[c.Name] = SkillLoad{Name: c.Name, Version: c.Version, Origin: OriginCompiled}
		}
	}
	// Store-only skills (flywheel-authored new competence) compose after the compiled set.
	rest := make([]skillstore.ProductionRow, 0, len(byName))
	for _, r := range byName {
		rest = append(rest, r)
	}
	sort.Slice(rest, func(i, j int) bool {
		if rest[i].Position != rest[j].Position {
			return rest[i].Position < rest[j].Position
		}
		return rest[i].SkillName < rest[j].SkillName
	})
	for _, r := range rest {
		out = append(out, Skill{Name: r.SkillName, Version: r.Version, AppliesWhen: declarativeApplies(r.AppliesWhen), Body: r.Body})
		prov.Skills[r.SkillName] = SkillLoad{Name: r.SkillName, Version: r.Version, ContentHash: r.ContentHash, Origin: OriginStore, VersionID: r.VersionID}
	}
	return NewRegistry(out), prov
}
