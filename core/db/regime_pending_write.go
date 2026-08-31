package db

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/territory-grounder/grounder/core/regime"
	"github.com/territory-grounder/grounder/core/safety"
	"github.com/territory-grounder/grounder/core/verify"
)

// RegimePendingWriteStore is the pgx-backed, DURABLE regime.PendingStore over the pending_verification table
// (migration 0022, spec/017 T-017-8). It is the cross-restart state that makes a launched AWX job's deferred
// verify SURVIVE a worker restart (a flip-prerequisite): a launch RESERVED here as 'pending' stays a visible
// pending/unverified record across a restart rather than being forgotten, so the launch-as-prediction
// discipline (REQ-1709/1710/1711) holds and a mutation we could not confirm is never silently trusted.
//
// It is the durable twin of regime.MemPendingStore (the CI oracle — CI has no Postgres), and its semantics
// are IDENTICAL to that fake by construction:
//   - Insert fails closed with regime.ErrDuplicatePending on a duplicate action_id. The guard is ATOMIC AT THE
//     DATABASE: action_id is the table's PRIMARY KEY, so a second Insert raises a unique-violation (SQLSTATE
//     23505) which this store maps to ErrDuplicatePending — the no-double-launch idempotency guard (REQ-1712)
//     is enforced by the DB, not by a read-then-write race in the app.
//   - Get returns regime.ErrNoPending for an unknown action_id (pgx.ErrNoRows).
//   - Update overwrites the record and returns regime.ErrNoPending if the action_id was never inserted
//     (RowsAffected == 0) — a transition can never invent a launch.
//   - List returns the records in the requested states (all when none given), ordered by action_id — the same
//     deterministic queue the fake yields.
//
// Concurrency: the single-writer-per-action_id discipline for the terminal transition is the caller's
// contract (regime.AsyncVerify.Verify is driven SERIALLY per action by the worker's single poll loop; see
// asyncverify.go). This store mirrors the mem fake's plain overwrite Update rather than adding a compare-and-
// set the interface does not carry — matching the oracle exactly is the mandate, and the PRIMARY KEY already
// makes the load-bearing idempotency guard (Insert) atomic.
//
// Parameters are always bound ($1) — no string-built SQL (INV-03). NON-SECRET by construction (INV-13): the
// AWX token is a SecretRef held elsewhere, never stored here; the only launch-bound value written is the
// non-secret AWX job_id handle. The committed launch prediction is stored NUL-free as jsonb (Postgres jsonb
// cannot hold a NUL, and verify.RuleKey uses a NUL separator — the same NUL-safe pair encoding the
// infragraph_prediction store uses is reused here). The runtime role holds no DELETE on the table (0022), so
// a claimed action_id is never removed and the idempotency guard holds forever.
type RegimePendingWriteStore struct{ p *Pool }

// NewRegimePendingWriteStore returns the Postgres-backed durable pending-verification store.
func NewRegimePendingWriteStore(p *Pool) *RegimePendingWriteStore { return &RegimePendingWriteStore{p: p} }

// compile-time proof the pgx store satisfies the regime.PendingStore seam the async-verify channel depends on.
var _ regime.PendingStore = (*RegimePendingWriteStore)(nil)

const pendingSelectColumns = `action_id, op_class, lane, job_id, state, prediction, launched_at, terminal_status, verdict, verified, resolved_at`

// Insert durably records a NEW pending-verification. It fails closed with regime.ErrDuplicatePending if a
// record for rec.ActionID already exists (the atomic no-double-launch guard, REQ-1712): action_id is the
// PRIMARY KEY, so a duplicate raises SQLSTATE 23505, which this maps to ErrDuplicatePending — never an
// ON CONFLICT DO NOTHING (that would silently succeed and defeat idempotency). The state slug is canonicalised
// through VerifyState.String() (zero value → 'pending'), the type's own fail-safe, so it can never violate the
// state CHECK; a prediction is stored NUL-free as jsonb.
func (s *RegimePendingWriteStore) Insert(ctx context.Context, rec regime.PendingVerification) error {
	if rec.ActionID == "" {
		return fmt.Errorf("db: pending_verification requires an action_id")
	}
	pred, err := marshalPendingPrediction(rec.Prediction)
	if err != nil {
		return err
	}
	_, err = s.p.Pool.Exec(ctx, `
		INSERT INTO pending_verification
			(action_id, op_class, lane, job_id, state, prediction, launched_at,
			 terminal_status, verdict, verified, resolved_at, schema_version)
		VALUES ($1, $2, $3, $4, $5, $6::jsonb, $7, $8, $9, $10, $11, 1)`,
		rec.ActionID, rec.OpClass, string(rec.Lane), rec.JobID, rec.State.String(), pred,
		rec.LaunchedAt.UTC(), string(rec.TerminalStatus), string(rec.Verdict), rec.Verified,
		nullableTime(rec.ResolvedAt))
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			// action_id already claimed — the fail-closed no-double-launch guard, atomic at the PRIMARY KEY.
			return fmt.Errorf("%w: %s", regime.ErrDuplicatePending, rec.ActionID)
		}
		return fmt.Errorf("db: pending_verification insert (action %q): %w", rec.ActionID, err)
	}
	return nil
}

// Get returns the record for actionID, or regime.ErrNoPending if none exists.
func (s *RegimePendingWriteStore) Get(ctx context.Context, actionID string) (regime.PendingVerification, error) {
	rec, err := scanPending(s.p.Pool.QueryRow(ctx,
		`SELECT `+pendingSelectColumns+` FROM pending_verification WHERE action_id = $1`, actionID))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return regime.PendingVerification{}, fmt.Errorf("%w: %s", regime.ErrNoPending, actionID)
		}
		return regime.PendingVerification{}, fmt.Errorf("db: pending_verification get (action %q): %w", actionID, err)
	}
	return rec, nil
}

// Update writes back a record whose state advanced (pending → verified / unverified, or a bound handle). It
// overwrites every mutable field (identical to the mem fake's whole-record overwrite) and fails with
// regime.ErrNoPending if the action_id was never inserted (RowsAffected == 0) — a transition can never invent
// a launch. It never DELETEs and never touches the PRIMARY KEY, so the idempotency claim is preserved.
func (s *RegimePendingWriteStore) Update(ctx context.Context, rec regime.PendingVerification) error {
	pred, err := marshalPendingPrediction(rec.Prediction)
	if err != nil {
		return err
	}
	tag, err := s.p.Pool.Exec(ctx, `
		UPDATE pending_verification SET
			op_class = $2, lane = $3, job_id = $4, state = $5, prediction = $6::jsonb,
			launched_at = $7, terminal_status = $8, verdict = $9, verified = $10, resolved_at = $11
		WHERE action_id = $1`,
		rec.ActionID, rec.OpClass, string(rec.Lane), rec.JobID, rec.State.String(), pred,
		rec.LaunchedAt.UTC(), string(rec.TerminalStatus), string(rec.Verdict), rec.Verified,
		nullableTime(rec.ResolvedAt))
	if err != nil {
		return fmt.Errorf("db: pending_verification update (action %q): %w", rec.ActionID, err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("%w: %s", regime.ErrNoPending, rec.ActionID)
	}
	return nil
}

// List returns the records in the given states (all records when no state is passed), ordered by action_id —
// the visible pending-verification / unverified queues the console and escalation read (REQ-1711). The state
// filter binds the RAW slugs as a text[] to `state = ANY($1)`, so — exactly like the fake's set membership —
// an unknown filter value simply matches no rows (the stored state column only ever holds valid slugs).
func (s *RegimePendingWriteStore) List(ctx context.Context, states ...regime.VerifyState) ([]regime.PendingVerification, error) {
	var (
		rows pgx.Rows
		err  error
	)
	if len(states) == 0 {
		rows, err = s.p.Pool.Query(ctx,
			`SELECT `+pendingSelectColumns+` FROM pending_verification ORDER BY action_id`)
	} else {
		slugs := make([]string, len(states))
		for i, st := range states {
			slugs[i] = string(st)
		}
		rows, err = s.p.Pool.Query(ctx,
			`SELECT `+pendingSelectColumns+` FROM pending_verification WHERE state = ANY($1) ORDER BY action_id`, slugs)
	}
	if err != nil {
		return nil, fmt.Errorf("db: pending_verification list: %w", err)
	}
	defer rows.Close()
	var out []regime.PendingVerification
	for rows.Next() {
		rec, serr := scanPending(rows)
		if serr != nil {
			return nil, fmt.Errorf("db: pending_verification scan: %w", serr)
		}
		out = append(out, rec)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("db: pending_verification rows: %w", err)
	}
	return out, nil
}

// rowScanner is satisfied by both pgx.Row (QueryRow) and pgx.Rows (Query loop), so Get and List share one scan.
type rowScanner interface {
	Scan(dest ...any) error
}

// scanPending decodes one pending_verification row into the regime record, rehydrating the NUL-free prediction
// jsonb and mapping a NULL resolved_at back to the Go zero time.
func scanPending(row rowScanner) (regime.PendingVerification, error) {
	var (
		rec        regime.PendingVerification
		lane       string
		state      string
		predRaw    []byte
		terminal   string
		verdict    string
		resolvedAt *time.Time
	)
	if err := row.Scan(&rec.ActionID, &rec.OpClass, &lane, &rec.JobID, &state, &predRaw,
		&rec.LaunchedAt, &terminal, &verdict, &rec.Verified, &resolvedAt); err != nil {
		return regime.PendingVerification{}, err
	}
	pred, err := unmarshalPendingPrediction(predRaw)
	if err != nil {
		return regime.PendingVerification{}, err
	}
	rec.Lane = regime.Regime(lane)
	rec.State = regime.VerifyState(state)
	rec.Prediction = pred
	rec.TerminalStatus = regime.JobStatus(terminal)
	rec.Verdict = safety.Verdict(verdict)
	if resolvedAt != nil {
		rec.ResolvedAt = *resolvedAt
	}
	return rec, nil
}

// pendingPredictionJSON is the NUL-free on-disk shape of the committed launch prediction. The two set fields
// are stored as pre-rendered JSON arrays (json.RawMessage) so this reuses the infragraph_prediction store's
// proven NUL-safe encoders (sortedKeys / ruleKeysToJSON and their inverses) rather than re-deriving them.
type pendingPredictionJSON struct {
	ActionID       string          `json:"action_id"`
	PlanHash       string          `json:"plan_hash"`
	TargetHost     string          `json:"target_host"`
	Site           string          `json:"site"`
	PredictedHosts json.RawMessage `json:"predicted_hosts"`
	PredictedRules json.RawMessage `json:"predicted_rules"`
}

// marshalPendingPrediction renders a verify.Prediction as a deterministic, NUL-free jsonb document. The
// PredictedRules keys carry a NUL separator (verify.RuleKey) that Postgres jsonb cannot store, so each is
// split into a [host, rule] pair — the identical treatment predictions.go uses — and round-trips exactly.
func marshalPendingPrediction(p verify.Prediction) (string, error) {
	hosts, err := sortedKeys(p.PredictedHosts)
	if err != nil {
		return "", fmt.Errorf("db: marshal prediction predicted_hosts: %w", err)
	}
	rules, err := ruleKeysToJSON(p.PredictedRules)
	if err != nil {
		return "", fmt.Errorf("db: marshal prediction predicted_rules: %w", err)
	}
	b, err := json.Marshal(pendingPredictionJSON{
		ActionID:       p.ActionID,
		PlanHash:       p.PlanHash,
		TargetHost:     p.TargetHost,
		Site:           p.Site,
		PredictedHosts: json.RawMessage(hosts),
		PredictedRules: json.RawMessage(rules),
	})
	if err != nil {
		return "", fmt.Errorf("db: marshal prediction: %w", err)
	}
	return string(b), nil
}

// unmarshalPendingPrediction reverses marshalPendingPrediction, rejoining each [host, rule] pair into its
// verify.RuleKey. An empty document (a launch reserved with a zero prediction) yields a zero Prediction.
func unmarshalPendingPrediction(raw []byte) (verify.Prediction, error) {
	if len(raw) == 0 {
		return verify.Prediction{}, nil
	}
	var pj pendingPredictionJSON
	if err := json.Unmarshal(raw, &pj); err != nil {
		return verify.Prediction{}, fmt.Errorf("db: unmarshal prediction: %w", err)
	}
	hosts, err := keysToSet(pj.PredictedHosts)
	if err != nil {
		return verify.Prediction{}, fmt.Errorf("db: unmarshal prediction predicted_hosts: %w", err)
	}
	rules, err := jsonToRuleKeys(pj.PredictedRules)
	if err != nil {
		return verify.Prediction{}, fmt.Errorf("db: unmarshal prediction predicted_rules: %w", err)
	}
	return verify.Prediction{
		ActionID:       pj.ActionID,
		PlanHash:       pj.PlanHash,
		TargetHost:     pj.TargetHost,
		Site:           pj.Site,
		PredictedHosts: hosts,
		PredictedRules: rules,
	}, nil
}
