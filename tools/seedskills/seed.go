package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/territory-grounder/grounder/core/skillstore"
	skillcorpus "github.com/territory-grounder/grounder/skills"
)

// seedAuthor is stamped on every row this tool creates — provenance the chain binds forever.
const seedAuthor = "tool:seedskills"

// ---------------------------------------------------------------------------------------------------
// The store surface.
// ---------------------------------------------------------------------------------------------------

// StoredVersion is the slice of an existing skill_version row the idempotency and occupancy
// decisions read: which version strings exist, at which content hashes, written by whom.
type StoredVersion struct {
	Version     string
	ContentHash string
	Source      string
}

// StoreState is everything the planner needs to know about one artifact name.
type StoreState struct {
	Found    bool // the identity row exists
	Identity skillstore.Skill
	Versions []StoredVersion
}

// Store is the narrow surface the seeder drives — the EXISTING skill-store API, nothing else.
// The pgx-backed db.SkillStore satisfies it via dbStore (main.go); tests drive the in-memory
// skillstore.MemStore via a thin adapter. CreateVersion is the store's own governed draft gate
// (skillstore.ValidateDraft: mandatory rationale, per-class body caps, closed predicate
// vocabulary) AND — on the pgx path — the TG-489 chain-linked append: chain participation is
// INTERNAL to CreateVersion (head locked FOR UPDATE, link computed, head advanced, one
// transaction; core/db/skillstore.go), so this tool never touches a chain primitive on the
// write path. It initializes/verifies the chain around the writes instead (main.go).
type Store interface {
	State(ctx context.Context, name string) (StoreState, error)
	PutSkill(ctx context.Context, sk skillstore.Skill) error
	CreateVersion(ctx context.Context, v skillstore.Version) (skillstore.Version, error)
}

// ---------------------------------------------------------------------------------------------------
// The corpus: skills/ tree + manifest, loaded and cross-validated.
// ---------------------------------------------------------------------------------------------------

// Artifact is one distilled SKILL.md joined with its manifest entry — everything a draft row needs.
type Artifact struct {
	Name        string
	Class       skillstore.ArtifactClass
	Version     string // the frontmatter version (prosedistill's artifactVersion stamp)
	Source      string // "distill:<source_path>" — frontmatter and manifest agree, the row stores it
	Description string
	Body        string // the authored body WITHOUT frontmatter (frontmatter maps onto row fields)
	AppliesWhen skillstore.AppliesWhen
	ContentHash string
	Rel         string // repo-relative SKILL.md path, for messages
	Position    int
}

// manifest mirrors tools/prosedistill/manifest.json — the 67-entry inventory truth (TG-36
// re-baseline). Decoded closed-world (DisallowUnknownFields), the same posture as prosedistill's
// own loader: a manifest field this tool does not understand refuses the run rather than being
// silently ignored at a write path.
type manifest struct {
	Readme  []string        `json:"readme"`
	Entries []manifestEntry `json:"entries"`
}

type manifestEntry struct {
	SourcePath  string `json:"source_path"`
	Disposition string `json:"disposition"`
	TargetName  string `json:"target_name,omitempty"`
	Class       string `json:"class,omitempty"`
	Description string `json:"description,omitempty"`
	Notes       string `json:"notes,omitempty"`
	// Predicate is the OPTIONAL manifest-borne applies_when for one artifact (the TG-36 seeding
	// policy's escape hatch). Today NO entry carries one — and prosedistill's own closed-world
	// loader would refuse a manifest that grew the field before prosedistill learns it too — so
	// every artifact seeds with the EMPTY predicate: an unscoped draft. That is deliberate and
	// safe: drafts never compose (only production rows reach the seed), and scoping is decided
	// where the eval evidence lives — per-artifact admission/trial, or an operator in the console
	// — not invented at the seeding wire. When an entry does carry the field it is validated
	// against the store's closed vocabulary (skillstore.ValidatePredicate) and rides the draft.
	Predicate *skillstore.AppliesWhen `json:"predicate,omitempty"`
}

var targetNameRe = regexp.MustCompile(`^[a-z][a-z0-9-]*$`)

// classFor maps a manifest/frontmatter class token onto the store's closed vocabulary. The
// seeder admits ONLY the two classes the distillation batches produce; anything else — including
// store classes this corpus must never mint, like prompt or rubric — refuses (fail closed).
func classFor(s string) (skillstore.ArtifactClass, bool) {
	switch s {
	case "skill":
		return skillstore.ClassSkill, true
	case "runbook":
		return skillstore.ClassRunbook, true
	}
	return "", false
}

// kindFor maps the artifact class onto skill.kind's CHECK vocabulary (migration 0009:
// behavioral | catalog): skill-class artifacts are agent behavioral competence; runbook-class
// artifacts are library content — the same split the boot importer draws ("behavioral" compiled
// skills, "catalog" for the judge-rubric mirror).
func kindFor(c skillstore.ArtifactClass) string {
	if c == skillstore.ClassSkill {
		return "behavioral"
	}
	return "catalog"
}

// positionBase spaces the seeded identities out of the compiled registry's range (the boot
// importer stamps 0..n on compiled skills and 1000 on the rubric mirror): skill drafts sort
// after the compiled library, runbooks after those. Position is compose order and drafts never
// compose — this matters only if an artifact graduates, and then a stable deterministic order
// beats an accidental one.
func positionBase(c skillstore.ArtifactClass) int {
	if c == skillstore.ClassSkill {
		return 100
	}
	return 500
}

// frontmatterKeys is the exact set prosedistill generates — nothing more, nothing less.
var frontmatterKeys = []string{"name", "class", "version", "source", "description"}

// loadCorpus reads the manifest and the skills/ tree under root and cross-validates them into
// the seedable artifact set. EVERY disagreement refuses the whole run — a seeding wire that
// writes around drift is how two sources of truth are minted:
//
//   - a skills/ file no distilled manifest entry produces (stray) — refuse;
//   - a distilled manifest entry whose SKILL.md is absent (missing) — refuse;
//   - frontmatter name/class/source/description differing from the manifest entry — refuse;
//   - a class outside the closed {skill, runbook} set — refuse;
//   - a body outside its class's byte cap — refused HERE with the store's OWN cap
//     (skillstore.MaxBodyBytes; the store re-enforces it at CreateVersion), so a dry-run
//     already reds on what the write path would refuse;
//   - a manifest predicate outside the closed vocabulary — refused via the store's own
//     skillstore.ValidatePredicate.
func loadCorpus(root string) ([]Artifact, error) {
	raw, err := os.ReadFile(filepath.Join(root, "tools", "prosedistill", "manifest.json"))
	if err != nil {
		return nil, fmt.Errorf("manifest: %w", err)
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var m manifest
	if err := dec.Decode(&m); err != nil {
		return nil, fmt.Errorf("manifest: %v", err)
	}

	var problems []string
	fail := func(format string, a ...any) { problems = append(problems, fmt.Sprintf(format, a...)) }

	// The distilled entries, keyed by their SKILL.md path.
	type target struct {
		entry manifestEntry
		class skillstore.ArtifactClass
	}
	targets := map[string]target{}
	seenName := map[string]bool{}
	for i, e := range m.Entries {
		if e.Disposition != "distilled" {
			continue
		}
		at := fmt.Sprintf("manifest entry %d (%s)", i+1, e.SourcePath)
		if !targetNameRe.MatchString(e.TargetName) {
			fail("%s: target_name %q must match %s", at, e.TargetName, targetNameRe)
			continue
		}
		if seenName[e.TargetName] {
			fail("%s: target_name %q claimed twice — one artifact per name", at, e.TargetName)
			continue
		}
		seenName[e.TargetName] = true
		class, ok := classFor(e.Class)
		if !ok {
			fail("%s: class %q is outside the closed {skill, runbook} set — refused, never guessed", at, e.Class)
			continue
		}
		if strings.TrimSpace(e.Description) == "" {
			fail("%s: distilled entry carries no description", at)
			continue
		}
		rel := "skills/" + e.Class + "/" + e.TargetName + "/SKILL.md"
		targets[rel] = target{entry: e, class: class}
	}

	// The tree: every file must be either the generated README or a SKILL.md a distilled entry
	// produces. A stray is drift between the tree and the inventory truth — refuse the run.
	onDisk := map[string]bool{}
	base := filepath.Join(root, "skills")
	walkErr := filepath.WalkDir(base, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, rerr := filepath.Rel(root, path)
		if rerr != nil {
			return rerr
		}
		rel = filepath.ToSlash(rel)
		onDisk[rel] = true
		if rel == "skills/README.md" {
			return nil
		}
		// The embed package's own declared residents (TG-529): Go's embed cannot cross package
		// boundaries, so skills/corpus.go lives beside the data it embeds. Declared BY the package —
		// an undeclared newcomer is still a stray.
		for _, pf := range skillcorpus.PackageFiles {
			if rel == pf {
				return nil
			}
		}
		if _, ok := targets[rel]; !ok {
			fail("%s: STRAY — present in the tree but produced by no distilled manifest entry; a stray is drift, and seeding around drift mints a second source of truth", rel)
		}
		return nil
	})
	if walkErr != nil && !os.IsNotExist(walkErr) {
		return nil, walkErr
	}

	var rels []string
	for rel := range targets {
		rels = append(rels, rel)
	}
	sort.Strings(rels)

	var arts []Artifact
	for _, rel := range rels {
		tg := targets[rel]
		if !onDisk[rel] {
			fail("%s: MISSING — the manifest distills it but the tree does not carry it (run prosedistill?)", rel)
			continue
		}
		raw, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel)))
		if err != nil {
			return nil, err
		}
		fm, body, err := splitArtifact(rel, raw)
		if err != nil {
			fail("%v", err)
			continue
		}
		e := tg.entry
		for _, check := range []struct{ key, got, want string }{
			{"name", fm["name"], e.TargetName},
			{"class", fm["class"], e.Class},
			{"source", fm["source"], "distill:" + e.SourcePath},
			{"description", fm["description"], e.Description},
		} {
			if check.got != check.want {
				fail("%s: frontmatter %s %q disagrees with the manifest's %q — the tree and the inventory truth have split", rel, check.key, check.got, check.want)
			}
		}
		aw := skillstore.AppliesWhen{}
		if e.Predicate != nil {
			aw = *e.Predicate
			if err := skillstore.ValidatePredicate(aw); err != nil {
				fail("%s: manifest predicate refused by the store's closed vocabulary: %v", rel, err)
			}
		}
		if maxB := skillstore.MaxBodyBytes(tg.class); len(body) < 1 || len(body) > maxB {
			fail("%s: body is %d bytes; class %s admits 1..%d (the store's own cap, skillstore.MaxBodyBytes — CreateVersion would refuse this identically)", rel, len(body), tg.class, maxB)
		}
		arts = append(arts, Artifact{
			Name:        e.TargetName,
			Class:       tg.class,
			Version:     fm["version"],
			Source:      "distill:" + e.SourcePath,
			Description: e.Description,
			Body:        body,
			AppliesWhen: aw,
			ContentHash: skillstore.ContentHash(body, aw),
			Rel:         rel,
		})
	}
	if len(problems) > 0 {
		return nil, fmt.Errorf("corpus refused — nothing will be seeded:\n  %s", strings.Join(problems, "\n  "))
	}
	if len(arts) == 0 {
		return nil, fmt.Errorf("corpus refused: zero distilled artifacts — an empty seeding run is a broken input, not a no-op")
	}

	// Deterministic positions: class-sorted, then name-sorted (rels are already name-sorted per
	// class because the class is the path segment before the name).
	sort.SliceStable(arts, func(i, j int) bool {
		if arts[i].Class != arts[j].Class {
			return arts[i].Class == skillstore.ClassSkill
		}
		return arts[i].Name < arts[j].Name
	})
	idx := map[skillstore.ArtifactClass]int{}
	for i := range arts {
		arts[i].Position = positionBase(arts[i].Class) + idx[arts[i].Class]
		idx[arts[i].Class]++
	}
	return arts, nil
}

// splitArtifact is parseFrontmatter with its real return contract (map, body, error).
func splitArtifact(rel string, raw []byte) (map[string]string, string, error) {
	lines := strings.Split(string(raw), "\n")
	if len(lines) == 0 || lines[0] != "---" {
		return nil, "", fmt.Errorf("%s: does not open with a frontmatter block", rel)
	}
	fm := map[string]string{}
	end := -1
	for i := 1; i < len(lines); i++ {
		if lines[i] == "---" {
			end = i
			break
		}
		k, v, ok := strings.Cut(lines[i], ": ")
		if !ok {
			return nil, "", fmt.Errorf("%s: frontmatter line %d is not `key: value`", rel, i+1)
		}
		if _, dup := fm[k]; dup {
			return nil, "", fmt.Errorf("%s: duplicate frontmatter key %q", rel, k)
		}
		fm[k] = v
	}
	if end < 0 {
		return nil, "", fmt.Errorf("%s: unterminated frontmatter block", rel)
	}
	for k := range fm {
		known := false
		for _, want := range frontmatterKeys {
			known = known || k == want
		}
		if !known {
			return nil, "", fmt.Errorf("%s: unknown frontmatter key %q (generated frontmatter carries exactly %v)", rel, k, frontmatterKeys)
		}
	}
	for _, want := range frontmatterKeys {
		if strings.TrimSpace(fm[want]) == "" {
			return nil, "", fmt.Errorf("%s: frontmatter key %q is missing or empty", rel, want)
		}
	}
	body := strings.Join(lines[end+1:], "\n")
	return fm, strings.TrimPrefix(body, "\n"), nil
}

// ---------------------------------------------------------------------------------------------------
// Plan and apply.
// ---------------------------------------------------------------------------------------------------

// Action is one artifact's planned disposition.
type Action string

const (
	ActionCreate Action = "create"         // PutSkill (identity) + CreateVersion (draft)
	ActionSkip   Action = "skip-identical" // a version of this name already stores this exact ContentHash
	ActionRefuse Action = "refuse"         // store state contradicts the corpus — a human decides
)

// PlanItem is one artifact's decided action with its reason.
type PlanItem struct {
	Artifact Artifact
	Action   Action
	Reason   string
}

// Plan is the decided run. StoreAware is false for an offline dry-run (no DSN): tree and
// manifest are fully validated, but idempotency/occupancy against the store are decided at
// --execute time.
type Plan struct {
	Items      []PlanItem
	StoreAware bool
}

// Counts is the run's honest arithmetic.
type Counts struct{ Created, Skipped, Refused int }

func (p Plan) counts() Counts {
	var c Counts
	for _, it := range p.Items {
		switch it.Action {
		case ActionCreate:
			c.Created++ // planned creates; apply converts plan to fact
		case ActionSkip:
			c.Skipped++
		case ActionRefuse:
			c.Refused++
		}
	}
	return c
}

// buildPlan decides every artifact against the store (st == nil: offline).
func buildPlan(ctx context.Context, arts []Artifact, st Store) (Plan, error) {
	p := Plan{StoreAware: st != nil}
	for _, a := range arts {
		it := PlanItem{Artifact: a, Action: ActionCreate, Reason: "offline plan — store state decided at --execute"}
		if st != nil {
			state, err := st.State(ctx, a.Name)
			if err != nil {
				return Plan{}, fmt.Errorf("read store state for %s: %w", a.Name, err)
			}
			it.Action, it.Reason = decide(a, state)
		}
		p.Items = append(p.Items, it)
	}
	return p, nil
}

// decide is the idempotency + occupancy law for one artifact:
//
//   - any stored version of this name at the SAME ContentHash → skip-identical (re-run = no-op);
//   - no identity row → create;
//   - identity PINNED → refuse (a pin is REQ-1305 law; PutSkill's upsert would clobber the flag
//     BEFORE ValidateDraft could refuse the draft, so the check lives here, ahead of any write);
//   - identity class differs → refuse (a class flip is drift, not seeding);
//   - a stored version carries the SAME version string at a DIFFERENT hash → refuse (the tree
//     changed without a version bump — re-running prosedistill after editing a distilled body
//     bumps nothing, and silently minting a sibling row would hide that);
//   - versions exist but NONE carries distill:* provenance → refuse (the name belongs to another
//     writer — the compiled importer or the flywheel; grafting a distillate onto it is a
//     collision, not an upgrade);
//   - otherwise → create (a fresh identity resume, or a revised distillate at a bumped version).
func decide(a Artifact, s StoreState) (Action, string) {
	for _, v := range s.Versions {
		if v.ContentHash == a.ContentHash {
			return ActionSkip, fmt.Sprintf("version %s already stores this exact content (hash %.12s…)", v.Version, a.ContentHash)
		}
	}
	if !s.Found {
		return ActionCreate, "new identity + draft"
	}
	if s.Identity.Pinned {
		return ActionRefuse, "identity is PINNED (REQ-1305) — seeding would rewrite the pin flag via the identity upsert; never touched"
	}
	if got := skillstore.DefaultClass(s.Identity.Class); got != a.Class {
		return ActionRefuse, fmt.Sprintf("identity class %s differs from the corpus class %s — a class flip is drift, not seeding", got, a.Class)
	}
	for _, v := range s.Versions {
		if v.Version == a.Version {
			return ActionRefuse, fmt.Sprintf("version %s already exists at a DIFFERENT content hash — the tree changed without a version bump; bump the artifact version (prosedistill) or reconcile the store first", a.Version)
		}
	}
	if len(s.Versions) > 0 {
		distilled := false
		for _, v := range s.Versions {
			distilled = distilled || strings.HasPrefix(v.Source, "distill:")
		}
		if !distilled {
			return ActionRefuse, "name collision — every stored version of this name was written by another writer (no distill:* provenance); seeding would graft onto a foreign artifact"
		}
	}
	return ActionCreate, "new draft on an existing distilled identity"
}

// rationaleFor is the fixed provenance sentence every seeded draft carries (the append-only
// transition log's first line). "source" (not "predecessor source") deliberately: batches 1-3
// (TG-477/478/479) distill predecessor prose, but batches 4 (TG-85) and 5 (TG-78) ground fresh
// content directly in vendor documentation — "source" is honest for all without asserting a
// predecessor lineage those rows do not have (each artifact's own manifest entry notes and Doc
// basis section carry the specific distinction).
func rationaleFor(a Artifact) string {
	scope := "unscoped draft — applies_when is EMPTY by seeding policy (drafts never compose; scoping is decided at per-artifact admission/trial or by an operator, where the eval evidence lives)"
	if !predicateEmpty(a.AppliesWhen) {
		scope = "applies_when carried from the manifest entry's predicate field"
	}
	return fmt.Sprintf("[draft] seeded by %s (TG-36 seeding rail; distillate corpus TG-477/TG-478/TG-479/TG-85/TG-78): re-authored per ADR-0012 from source %s; %s.",
		seedAuthor, a.Source, scope)
}

func predicateEmpty(aw skillstore.AppliesWhen) bool {
	return len(aw.Phases) == 0 && len(aw.ExecClasses) == 0 && len(aw.Domains) == 0
}

// apply executes a plan: identity upsert + draft insert per created artifact, through the
// store's own write path (which on the pgx side is the TG-489 chained writer). It writes
// NOTHING when the plan carries any refusal — drift is reconciled by a human, never written
// around. A mid-run store error aborts with the artifacts already written left in place: every
// write is individually durable and the run is idempotent, so the re-run resumes exactly where
// this one stopped (created rows re-decide as skip-identical).
func apply(ctx context.Context, p Plan, st Store) (Counts, error) {
	var c Counts
	if pc := p.counts(); pc.Refused > 0 {
		return Counts{Refused: pc.Refused}, fmt.Errorf("apply refused: %d artifacts refused in the plan — nothing written", pc.Refused)
	}
	for _, it := range p.Items {
		if it.Action != ActionCreate {
			c.Skipped++
			continue
		}
		a := it.Artifact
		if err := st.PutSkill(ctx, skillstore.Skill{
			Name:        a.Name,
			Kind:        kindFor(a.Class),
			Pinned:      false,
			Class:       a.Class,
			Position:    a.Position,
			Description: a.Description,
		}); err != nil {
			return c, fmt.Errorf("%s: identity upsert: %w", a.Name, err)
		}
		if _, err := st.CreateVersion(ctx, skillstore.Version{
			SkillName:   a.Name,
			Version:     a.Version,
			Body:        a.Body,
			AppliesWhen: a.AppliesWhen,
			ContentHash: a.ContentHash,
			Author:      seedAuthor,
			Source:      a.Source,
			Rationale:   rationaleFor(a),
		}); err != nil {
			return c, fmt.Errorf("%s: draft refused by the store: %w", a.Name, err)
		}
		c.Created++
	}
	return c, nil
}
