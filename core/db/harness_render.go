package db

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"sync"
	"time"

	"github.com/territory-grounder/grounder/core/harnessid"
)

// HarnessRenderStore persists the rendering behind each harness fingerprint (TG-600, migration 0117,
// Harness² learnings H2-1c): the four effective components the fingerprint hashes, their SHA-256s, the
// preamble version and the builds that rendered it. One row per configuration, written on its FIRST
// session, INSERT-only (ON CONFLICT DO NOTHING under the 0105 floor). Ensure REFUSES a record whose key
// does not derive from its parts — the integrity property that makes a stored rendering trustworthy.
// Observability only: nothing on the decision path reads it.
type HarnessRenderStore struct {
	p    *Pool
	seen sync.Map // fingerprint ‖ NUL ‖ build → struct{}: one INSERT per configuration per process
}

// NewHarnessRenderStore builds the pgx-backed store.
func NewHarnessRenderStore(p *Pool) *HarnessRenderStore { return &HarnessRenderStore{p: p} }

// HarnessRenderOutcome says what Ensure did: a new rendering row, a new build-lineage row, or nothing
// (already recorded — by this process, Cached, or by an earlier one).
type HarnessRenderOutcome struct {
	RenderInserted bool
	BuildInserted  bool
	Cached         bool
}

// Ensure records the rendering if absent and its build lineage if absent. The key check runs BEFORE the
// cache and before any SQL, so a forged or half-built record is refused on every call, never memoised.
func (s *HarnessRenderStore) Ensure(ctx context.Context, rec harnessid.Record) (HarnessRenderOutcome, error) {
	if err := harnessid.Verify(rec); err != nil {
		return HarnessRenderOutcome{}, fmt.Errorf("db: harness render refused: %w", err)
	}
	key := rec.HarnessFP + "\x00" + rec.BuildSHA
	if _, ok := s.seen.Load(key); ok {
		return HarnessRenderOutcome{Cached: true}, nil
	}
	h := rec.Parts.Hashes()
	var out HarnessRenderOutcome
	tag, err := s.p.Exec(ctx, `
		INSERT INTO harness_render (harness_fp, prompt_version, prose, tool_catalog, opclass_catalog, guidance,
		                            prose_hash, tools_hash, opclass_hash, guidance_hash)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
		ON CONFLICT (harness_fp) DO NOTHING`,
		rec.HarnessFP, rec.PromptVersion, rec.Parts.Prose, rec.Parts.ToolCatalog, rec.Parts.OpClassCatalog, rec.Parts.Guidance,
		h.Prose, h.ToolCatalog, h.OpClassCatalog, h.Guidance)
	if err != nil {
		return out, fmt.Errorf("db: insert harness_render %s: %w", harnessid.Short(rec.HarnessFP), err)
	}
	out.RenderInserted = tag.RowsAffected() == 1
	tag, err = s.p.Exec(ctx, `
		INSERT INTO harness_render_build (harness_fp, build_sha) VALUES ($1, $2)
		ON CONFLICT (harness_fp, build_sha) DO NOTHING`, rec.HarnessFP, rec.BuildSHA)
	if err != nil {
		return out, fmt.Errorf("db: insert harness_render_build %s/%s: %w", harnessid.Short(rec.HarnessFP), rec.BuildSHA, err)
	}
	out.BuildInserted = tag.RowsAffected() == 1
	s.seen.Store(key, struct{}{})
	return out, nil
}

// HarnessRenderSummary is one configuration as the operator list shows it.
type HarnessRenderSummary struct {
	HarnessFP     string
	PromptVersion string
	FirstSeen     time.Time
	Bytes         int      // size of the assembled system half
	Builds        []string // every build that rendered this configuration, oldest first; "" = unrecorded
	Sessions      int      // session_triage rows carrying this fingerprint
}

// List returns every recorded configuration, oldest first, with its build lineage and session count.
func (s *HarnessRenderStore) List(ctx context.Context) ([]HarnessRenderSummary, error) {
	// The parts ride along so Bytes is the ASSEMBLED size through the one assembly (harnessid.Assemble) —
	// a SQL re-implementation of the placeholder substitution would be a second assembly to keep in parity.
	// Rows are few (one per deploy that changes the surface), so the text is cheap to carry.
	rows, err := s.p.Query(ctx, `
		SELECT r.harness_fp, r.prompt_version, r.first_seen, r.prose, r.tool_catalog, r.opclass_catalog, r.guidance,
		       COALESCE((SELECT array_agg(b.build_sha ORDER BY b.first_seen) FROM harness_render_build b WHERE b.harness_fp = r.harness_fp), '{}'),
		       (SELECT count(*) FROM session_triage t WHERE t.harness_fp = r.harness_fp)
		FROM harness_render r
		ORDER BY r.first_seen, r.harness_fp`)
	if err != nil {
		return nil, fmt.Errorf("db: list harness renders: %w", err)
	}
	defer rows.Close()
	var out []HarnessRenderSummary
	for rows.Next() {
		var h HarnessRenderSummary
		var p harnessid.Parts
		var n int64
		if err := rows.Scan(&h.HarnessFP, &h.PromptVersion, &h.FirstSeen, &p.Prose, &p.ToolCatalog, &p.OpClassCatalog, &p.Guidance, &h.Builds, &n); err != nil {
			return nil, fmt.Errorf("db: scan harness render: %w", err)
		}
		h.Bytes = p.Bytes()
		h.Sessions = int(n)
		out = append(out, h)
	}
	return out, rows.Err()
}

// HarnessRenderRow is one configuration in full: the record (parts included), its hashes and lineage.
type HarnessRenderRow struct {
	harnessid.Record
	Hashes    harnessid.Hashes
	FirstSeen time.Time
	Builds    []string
	Sessions  int
}

// ErrHarnessRenderNotFound is returned by Get when no recorded fingerprint has the given prefix.
var ErrHarnessRenderNotFound = errors.New("db: no recorded harness render has that fingerprint prefix")

var fpPrefixRE = regexp.MustCompile(`^[0-9a-f]{4,64}$`)

// Get returns the configuration whose fingerprint starts with prefix (at least 4 hex). An ambiguous
// prefix is refused with the candidates named — a diff against "the wrong one of two" is worse than none.
func (s *HarnessRenderStore) Get(ctx context.Context, prefix string) (HarnessRenderRow, error) {
	if !fpPrefixRE.MatchString(prefix) {
		return HarnessRenderRow{}, fmt.Errorf("db: harness fingerprint prefix %q must be 4-64 lowercase hex", prefix)
	}
	rows, err := s.p.Query(ctx, `
		SELECT r.harness_fp, r.prompt_version, r.prose, r.tool_catalog, r.opclass_catalog, r.guidance,
		       r.prose_hash, r.tools_hash, r.opclass_hash, r.guidance_hash, r.first_seen,
		       COALESCE((SELECT array_agg(b.build_sha ORDER BY b.first_seen) FROM harness_render_build b WHERE b.harness_fp = r.harness_fp), '{}'),
		       (SELECT count(*) FROM session_triage t WHERE t.harness_fp = r.harness_fp)
		FROM harness_render r
		WHERE left(r.harness_fp, length($1::text)) = $1::text
		ORDER BY r.harness_fp
		LIMIT 3`, prefix)
	if err != nil {
		return HarnessRenderRow{}, fmt.Errorf("db: get harness render %s: %w", prefix, err)
	}
	defer rows.Close()
	var found []HarnessRenderRow
	for rows.Next() {
		var r HarnessRenderRow
		var n int64
		if err := rows.Scan(&r.HarnessFP, &r.PromptVersion, &r.Parts.Prose, &r.Parts.ToolCatalog, &r.Parts.OpClassCatalog, &r.Parts.Guidance,
			&r.Hashes.Prose, &r.Hashes.ToolCatalog, &r.Hashes.OpClassCatalog, &r.Hashes.Guidance, &r.FirstSeen, &r.Builds, &n); err != nil {
			return HarnessRenderRow{}, fmt.Errorf("db: scan harness render: %w", err)
		}
		r.Sessions = int(n)
		if len(r.Builds) > 0 {
			r.BuildSHA = r.Builds[0]
		}
		found = append(found, r)
	}
	if err := rows.Err(); err != nil {
		return HarnessRenderRow{}, err
	}
	switch len(found) {
	case 0:
		return HarnessRenderRow{}, ErrHarnessRenderNotFound
	case 1:
		return found[0], nil
	default:
		names := make([]string, 0, len(found))
		for _, f := range found {
			names = append(names, harnessid.Short(f.HarnessFP))
		}
		return HarnessRenderRow{}, fmt.Errorf("db: harness fingerprint prefix %q is ambiguous: %v — give more characters", prefix, names)
	}
}

// MemHarnessRenderStore is the in-memory twin of HarnessRenderStore for tests and the acceptance lattice:
// the SAME key check (harnessid.Verify) and the same insert-if-absent semantics, no SQL.
type MemHarnessRenderStore struct {
	mu      sync.Mutex
	renders map[string]harnessid.Record
	builds  map[string][]string
}

// NewMemHarnessRenderStore builds an empty twin.
func NewMemHarnessRenderStore() *MemHarnessRenderStore {
	return &MemHarnessRenderStore{renders: map[string]harnessid.Record{}, builds: map[string][]string{}}
}

// Ensure mirrors HarnessRenderStore.Ensure.
func (m *MemHarnessRenderStore) Ensure(_ context.Context, rec harnessid.Record) (HarnessRenderOutcome, error) {
	if err := harnessid.Verify(rec); err != nil {
		return HarnessRenderOutcome{}, fmt.Errorf("db: harness render refused: %w", err)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	var out HarnessRenderOutcome
	if _, ok := m.renders[rec.HarnessFP]; !ok {
		m.renders[rec.HarnessFP] = rec
		out.RenderInserted = true
	}
	for _, b := range m.builds[rec.HarnessFP] {
		if b == rec.BuildSHA {
			return out, nil
		}
	}
	m.builds[rec.HarnessFP] = append(m.builds[rec.HarnessFP], rec.BuildSHA)
	out.BuildInserted = true
	return out, nil
}

// Get returns the stored record and its builds, or false.
func (m *MemHarnessRenderStore) Get(fp string) (harnessid.Record, []string, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	r, ok := m.renders[fp]
	return r, append([]string(nil), m.builds[fp]...), ok
}

// Len is the number of recorded configurations.
func (m *MemHarnessRenderStore) Len() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.renders)
}
