package db

import (
	"context"
	"fmt"
	"time"

	"github.com/territory-grounder/grounder/core/evidencepack"
	"github.com/territory-grounder/grounder/core/judge"
	"github.com/territory-grounder/grounder/core/skillstore"
)

// EvidenceSource builds the evidence packs the flywheel's proposers read (TG-582): for a production version and a
// judge dimension, the version's judged sessions that scored LOWEST on that dimension under the current rubric and
// that carry a recorded trajectory — rendered through core/evidencepack exactly as `tools/evidencepack` renders them
// (one assembler, so the improver reads the same record an operator would). Answer-free by construction: the pack
// carries the record the judge adjudicated, never the score that chose the session.
type EvidenceSource struct{ p *Pool }

// NewEvidenceSource returns the Postgres-backed pack source.
func NewEvidenceSource(p *Pool) *EvidenceSource { return &EvidenceSource{p: p} }

var _ skillstore.EvidenceSource = (*EvidenceSource)(nil)

// evidenceWindow is the FALLBACK window when a caller passes none (zero): the flywheel's default generation window.
// The flywheel itself passes CreationConfig.Window, so the packs come from the pool the trigger measured.
const evidenceWindow = 14 * 24 * time.Hour

// PacksFor returns up to limit packs, the lowest-scoring session on dimension first. A session without steps or
// evidence rows is skipped (there is no record to read, so nothing a proposer could cite); the caller treats zero
// packs as "fall back to lens generation".
func (s *EvidenceSource) PacksFor(ctx context.Context, versionID int64, dimension string, window time.Duration, limit int) ([]evidencepack.Pack, error) {
	if window <= 0 {
		window = evidenceWindow
	}
	if limit <= 0 {
		limit = skillstore.DefaultProposerPacks
	}
	loadPattern := fmt.Sprintf("%%#%d:store%%", versionID)
	// THE SAME POOL THE TRIGGER MEASURED (review of TG-582; TG-599): the pool is flywheelPoolCTE — the one constant
	// DimensionMeans (the trigger) also prepends, bound to the same first four parameters — so the proposers read
	// exactly the sessions whose judged mean fired the regression. The first cut of this query carried its own copy
	// of the CTEs with one extra clause (every trial-flagged session dropped from the pool, where the trigger drops
	// them only from the anchor grouping); with three trials live every judged session carried another skill's arm,
	// the pool was empty on every cycle and the proposers fell back to lens generation without a word (found by the
	// first live run of tools/proposerdryrun). On top of the shared pool this query keeps what is specific to a
	// pack: the dimension ($5), the stand-down N/A rule for falsifiable_prediction (TG-61, as the trigger's outer
	// SELECT does), a recorded trajectory (a session with no steps has nothing a proposer could cite), the limit ($6).
	rows, err := s.p.Query(ctx, flywheelPoolCTE+`
		SELECT p.external_ref
		FROM pool p
		JOIN session_judgment j ON j.external_ref = p.external_ref
		WHERE j.dimension = $5 AND j.score > 0 AND j.rubric_version = $3
		  AND NOT (j.dimension = 'falsifiable_prediction' AND p.proposed = false)
		  AND EXISTS (SELECT 1 FROM agent_step st WHERE st.external_ref = p.external_ref)
		ORDER BY j.score ASC, (SELECT c.created_at FROM composing c WHERE c.external_ref = p.external_ref) DESC
		LIMIT $6`,
		loadPattern, fmt.Sprintf("%d seconds", int(window.Seconds())), judge.RubricVersion(), skillstore.DefaultGenMinPoolSessions, dimension, limit)
	if err != nil {
		if isUndefinedTable(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("db: evidence sessions for version %d: %w", versionID, err)
	}
	var refs []string
	for rows.Next() {
		var ref string
		if err := rows.Scan(&ref); err != nil {
			rows.Close()
			return nil, fmt.Errorf("db: scan evidence session: %w", err)
		}
		refs = append(refs, ref)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("db: evidence sessions for version %d: %w", versionID, err)
	}
	triage, steps, evidence, skills := NewTriageStore(s.p), NewAgentStepStore(s.p), NewAgentStepEvidenceStore(s.p), NewSkillStore(s.p)
	var out []evidencepack.Pack
	for _, ref := range refs {
		row, ok, err := triage.Row(ctx, ref)
		if err != nil {
			return nil, err
		}
		if !ok {
			continue
		}
		st, err := steps.Steps(ctx, ref)
		if err != nil {
			return nil, err
		}
		ev, err := evidence.EvidenceFor(ctx, ref)
		if err != nil {
			return nil, err
		}
		if len(st) == 0 && len(ev) == 0 {
			continue
		}
		harness := map[string]string{}
		for name, id := range evidencepack.HarnessLoads(row.SkillLoads) {
			if v, err := skills.GetVersion(ctx, id); err == nil {
				harness[name] = v.Body
			}
			// An unreadable body is simply absent from the pack: the proposer edits the production body it is
			// handed by the generator, not this map; the map is context.
		}
		out = append(out, evidencepack.Assemble(ref, evidencepack.Source{Row: row, Steps: st, Evidence: ev, Harness: harness}))
	}
	return out, nil
}
