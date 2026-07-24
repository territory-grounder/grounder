package regime

// audit.go is the Actuation Regime Engine's APPEND-ONLY persistence writer (spec/017 T-017-6, REQ-1715).
// The engine's every regime resolution, lane selection, job launch, and deferred verdict is a REQUIRED
// output that must land in two places: a purpose-built append-only table (regime_resolution /
// regime_actuation / deferred_verdict, migration 0020) AND the org-global tamper-evident governance ledger
// (INV-19). This file owns the typed, secret-free row shapes, the persistence PORT (AuditSink), an in-memory
// FAKE the CI oracles drive without a database, and the Audit writer that fans each row to its table + the
// ledger, failing closed on an incomplete row.
//
// NEVER A SECRET (INV-13). Not one row column can receive a plaintext secret. The only credential material a
// launch carries is the AWX API token, and it is persisted ONLY as its config.SecretRef REFERENCE (e.g.
// "store:awx.token") — the reference is safe to log, commit, and export; the value it points to is never
// stored. ActuationRow.validate rejects a token field that is not a reference (fail closed), so a raw token
// can never be appended, and migration 0020's token_ref CHECK enforces the same at the database boundary.
//
// This file is PERSISTENCE only: it actuates nothing, authorizes nothing, and lifts no floor. It uses its OWN
// typed row structs (a decoupled Verdict / JobStatus / outcome vocabulary) and does not depend on the
// async-verify channel (T-017-4) types — the main.go wave adapts the engine's outputs onto these rows.
//
// Provenance: [O] INV-19 (append-only, hash-chained governance ledger; every governance decision a required
// output), INV-13 (secrets are references, never literals), spec/017 (TG-110).

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/territory-grounder/grounder/core/audit"
	"github.com/territory-grounder/grounder/core/config"
)

// ---------------------------------------------------------------------------------------------------------
// Typed, secret-free row vocabularies (REQ-1715). Each is a closed, validated string set so a malformed or
// poisoned value fails closed rather than persisting an undefined outcome — the same discipline the Regime
// enum uses. These are the engine's OWN types; the async-verify channel (T-017-4) is not imported.
// ---------------------------------------------------------------------------------------------------------

// ResolutionOutcome is the resolved-or-refused result of a lane selection recorded on a regime_resolution row.
type ResolutionOutcome string

const (
	// OutcomeResolved records that the engine resolved the target to exactly one regime + lane.
	OutcomeResolved ResolutionOutcome = "resolved"
	// OutcomeRefused records a fail-closed refusal (unknown/ambiguous regime, no wired lane) — no lane returned.
	OutcomeRefused ResolutionOutcome = "refused"
)

// Valid reports whether o is one of the two sanctioned resolution outcomes.
func (o ResolutionOutcome) Valid() bool { return o == OutcomeResolved || o == OutcomeRefused }

// JobStatus is a TERMINAL AWX job status recorded on a deferred_verdict row (REQ-1709 terminal set). A
// deferred verdict is written only once the job reaches one of these; a non-terminal status is never recorded
// as a verdict.
type JobStatus string

const (
	JobSuccessful JobStatus = "successful"
	JobFailed     JobStatus = "failed"
	JobError      JobStatus = "error"
	JobCanceled   JobStatus = "canceled"
)

// Valid reports whether s is one of the four terminal AWX job statuses.
func (s JobStatus) Valid() bool {
	switch s {
	case JobSuccessful, JobFailed, JobError, JobCanceled:
		return true
	default:
		return false
	}
}

// Verdict is the mechanical verdict slug (spec/002 verify.ComputeVerdict, REQ-1710) recorded on a
// deferred_verdict row. It is kept a decoupled slug so the audit layer stays independent of core/verify and
// the async-verify channel (T-017-4) — the writer persists an already-computed non-secret outcome.
type Verdict string

const (
	// VerdictMatch — the terminal outcome matched the launch prediction.
	VerdictMatch Verdict = "match"
	// VerdictDeviation — the terminal outcome deviated from the prediction.
	VerdictDeviation Verdict = "deviation"
	// VerdictUnverified — the job never reached a terminal outcome within the operator bound (REQ-1711).
	VerdictUnverified Verdict = "unverified"
)

// Valid reports whether v is one of the three verdict slugs.
func (v Verdict) Valid() bool {
	switch v {
	case VerdictMatch, VerdictDeviation, VerdictUnverified:
		return true
	default:
		return false
	}
}

// GraduationOutcome is the earned-trust outcome fed to the spec/015 graduation ladder (REQ-1710/1711)
// recorded on a deferred_verdict row.
type GraduationOutcome string

const (
	// GraduationVerifiedClean — a clean verified run that earns trust toward auto for its op-class.
	GraduationVerifiedClean GraduationOutcome = "verified_clean"
	// GraduationDeviated — a deviated run (no clean-run credit).
	GraduationDeviated GraduationOutcome = "deviated"
	// GraduationNoCredit — a pending-verification / unverified run counts as no clean verified run (REQ-1711).
	GraduationNoCredit GraduationOutcome = "no_credit"
)

// Valid reports whether g is one of the three graduation outcomes.
func (g GraduationOutcome) Valid() bool {
	switch g {
	case GraduationVerifiedClean, GraduationDeviated, GraduationNoCredit:
		return true
	default:
		return false
	}
}

// ---------------------------------------------------------------------------------------------------------
// Row shapes — each is a REQUIRED output of its engine step (REQ-1715); a row missing a load-bearing field is
// rejected (fail closed) rather than persisting a partial audit record. Every field is NON-SECRET by
// construction; the only credential material is a config.SecretRef REFERENCE, never a value (INV-13).
// ---------------------------------------------------------------------------------------------------------

// ResolutionRow is one immutable regime_resolution row per lane selection (REQ-1715): the non-secret target
// label, the resolved Regime + selected Lane, the matched RuleID (empty ⇒ the operator default lane was
// used), and the resolved/refused Outcome.
type ResolutionRow struct {
	Target  string            // non-secret target label (host / device-class / group)
	Regime  Regime            // the resolved management regime (empty on a refusal)
	Lane    Regime            // the selected effect lane's identity (empty on a refusal; == Regime when resolved)
	RuleID  string            // the matched rule id ("" ⇒ operator default lane)
	Outcome ResolutionOutcome // resolved | refused
}

func (r ResolutionRow) validate() error {
	if strings.TrimSpace(r.Target) == "" {
		return fmt.Errorf("%w: resolution row missing target", ErrIncompleteAuditRow)
	}
	if !r.Outcome.Valid() {
		return fmt.Errorf("%w: resolution row invalid outcome %q", ErrIncompleteAuditRow, r.Outcome)
	}
	switch r.Outcome {
	case OutcomeResolved:
		// A resolved row MUST name a valid regime + lane; a resolved-but-empty row would be a silent partial.
		if !r.Regime.Valid() || !r.Lane.Valid() {
			return fmt.Errorf("%w: resolved row must carry a valid regime and lane, got regime=%q lane=%q",
				ErrIncompleteAuditRow, r.Regime, r.Lane)
		}
	case OutcomeRefused:
		// A refusal carries no lane by construction (fail closed): the target is not actuatable through any lane.
		if r.Regime != "" || r.Lane != "" {
			return fmt.Errorf("%w: refused row must carry no regime/lane, got regime=%q lane=%q",
				ErrIncompleteAuditRow, r.Regime, r.Lane)
		}
	}
	return nil
}

// ActuationRow is one immutable regime_actuation row per job launch (REQ-1715): the ActionID (the INV-07
// manifest-lifecycle binding), the selected Lane, the AWX JobTemplateID, the authorized OpClass, the launched
// AWX JobID, and the launch's token as a config.SecretRef REFERENCE — never the token value or a secret
// extra_var (INV-13).
type ActuationRow struct {
	ActionID      string           // the content-hashed action this launch is bound to (INV-07); required
	Lane          Regime           // the effect lane launched through; required + valid
	JobTemplateID string           // the AWX job-template id (non-secret)
	OpClass       string           // the authorized op-class (non-secret)
	JobID         string           // the launched AWX job id / async handle (non-secret)
	TokenRef      config.SecretRef // the AWX token as a SecretRef REFERENCE (env:/file:/store:/…), never a value
}

func (r ActuationRow) validate() error {
	if strings.TrimSpace(r.ActionID) == "" {
		return fmt.Errorf("%w: actuation row missing action_id", ErrIncompleteAuditRow)
	}
	if !r.Lane.Valid() {
		return fmt.Errorf("%w: actuation row invalid lane %q", ErrIncompleteAuditRow, r.Lane)
	}
	// INV-13 at the writer boundary: a token field that is NOT a reference (has no scheme prefix) is a
	// suspected plaintext secret — refuse to persist it. An empty token is permitted (the sensor/read-only or
	// not-yet-resolved case); a non-empty token MUST be a SecretRef reference.
	if r.TokenRef != "" && !isSecretRef(r.TokenRef) {
		return fmt.Errorf("%w: actuation token must be a SecretRef reference, never a plaintext value", ErrSecretInAuditRow)
	}
	return nil
}

// DeferredVerdictRow is one immutable deferred_verdict row per completed deferred verify (REQ-1715): the
// ActionID, the AWX JobID, the terminal job Status, the mechanical Verdict, and the Graduation outcome fed to
// the ladder.
type DeferredVerdictRow struct {
	ActionID   string            // the action this verdict closes (INV-07); required
	JobID      string            // the AWX job id that reached terminal (non-secret)
	Status     JobStatus         // the terminal AWX job status; required + valid
	Verdict    Verdict           // the mechanical verdict against the launch prediction; required + valid
	Graduation GraduationOutcome // the earned-trust outcome fed to the graduation ladder; required + valid
}

func (r DeferredVerdictRow) validate() error {
	if strings.TrimSpace(r.ActionID) == "" {
		return fmt.Errorf("%w: deferred-verdict row missing action_id", ErrIncompleteAuditRow)
	}
	if !r.Status.Valid() {
		return fmt.Errorf("%w: deferred-verdict row invalid terminal status %q", ErrIncompleteAuditRow, r.Status)
	}
	if !r.Verdict.Valid() {
		return fmt.Errorf("%w: deferred-verdict row invalid verdict %q", ErrIncompleteAuditRow, r.Verdict)
	}
	if !r.Graduation.Valid() {
		return fmt.Errorf("%w: deferred-verdict row invalid graduation outcome %q", ErrIncompleteAuditRow, r.Graduation)
	}
	return nil
}

// isSecretRef reports whether r is a reference (has a non-empty scheme prefix like "env:"/"file:"/"store:"),
// as opposed to a raw value. It mirrors config.SecretRef.Resolve's scheme model — a reference always carries a
// "scheme:" prefix — without resolving (never touches the secret value).
func isSecretRef(r config.SecretRef) bool {
	scheme, rest, ok := strings.Cut(string(r), ":")
	return ok && scheme != "" && rest != ""
}

// ---------------------------------------------------------------------------------------------------------
// The persistence PORT + the in-memory FAKE (REQ-1715). AuditSink is the append-only durable interface the
// main.go wave wires to the pgx store (core/db); MemAuditSink is the concurrency-safe CI twin the oracles
// drive without a database, recording every appended row so a test can assert exactly which non-secret fields
// were written — and that no plaintext secret ever leaked.
// ---------------------------------------------------------------------------------------------------------

// AuditSink is the append-only persistence port for the three regime audit row kinds. Each method appends ONE
// immutable row to its table. The production impl (core/db.RegimeAuditWriteStore) writes to Postgres with
// bound parameters; the runtime DB role holds no UPDATE/DELETE on these tables (migration 0020), so an
// appended row is tamper-resistant.
type AuditSink interface {
	AppendResolution(context.Context, ResolutionRow) error
	AppendActuation(context.Context, ActuationRow) error
	AppendDeferredVerdict(context.Context, DeferredVerdictRow) error
}

// LedgerAppender is the minimal governance-ledger append surface the Audit writer needs (INV-19). The
// org-global *audit.Ledger satisfies it; a fake satisfies it in tests. Every regime audit row is ALSO chained
// into this tamper-evident ledger (REQ-1715).
type LedgerAppender interface {
	Append(audit.GovDecision) (audit.LedgerEntry, error)
}

// MemAuditSink is the in-memory AuditSink twin for the CI oracles (no Postgres). Concurrency-safe: the worker
// appends regime audit rows from concurrent Temporal activities.
type MemAuditSink struct {
	mu          sync.Mutex
	Resolutions []ResolutionRow
	Actuations  []ActuationRow
	Verdicts    []DeferredVerdictRow
}

// NewMemAuditSink returns an empty in-memory regime-audit twin.
func NewMemAuditSink() *MemAuditSink { return &MemAuditSink{} }

// AppendResolution records a regime_resolution row (append).
func (m *MemAuditSink) AppendResolution(_ context.Context, r ResolutionRow) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.Resolutions = append(m.Resolutions, r)
	return nil
}

// AppendActuation records a regime_actuation row (append).
func (m *MemAuditSink) AppendActuation(_ context.Context, r ActuationRow) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.Actuations = append(m.Actuations, r)
	return nil
}

// AppendDeferredVerdict records a deferred_verdict row (append).
func (m *MemAuditSink) AppendDeferredVerdict(_ context.Context, r DeferredVerdictRow) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.Verdicts = append(m.Verdicts, r)
	return nil
}

// Counts returns the number of each row kind recorded (for oracle assertions).
func (m *MemAuditSink) Counts() (resolutions, actuations, verdicts int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.Resolutions), len(m.Actuations), len(m.Verdicts)
}

// compile-time proof the fake satisfies the port.
var _ AuditSink = (*MemAuditSink)(nil)

// ---------------------------------------------------------------------------------------------------------
// The Audit writer (REQ-1715). It is the ONE place a regime audit row is produced. Each Record* call validates
// the row (fail closed on an incomplete/secret-bearing row), appends it to its append-only table (AuditSink),
// and chains a non-secret summary into the tamper-evident governance ledger (INV-19). Both writes are
// REQUIRED outputs — the ledger append comes first (the authoritative spine), then the durable table; a
// failure of either surfaces loudly and the row is not silently half-persisted.
// ---------------------------------------------------------------------------------------------------------

var (
	// ErrAuditNotWired fails closed when the writer has no durable sink or no ledger — persistence is a required
	// output, so an unwired writer refuses rather than silently dropping the audit record.
	ErrAuditNotWired = errors.New("regime: audit writer not wired (missing sink or ledger) — refuse (fail closed)")
	// ErrIncompleteAuditRow fails closed when a row omits a required field — a partial audit record is never
	// persisted (REQ-1715: every field is a required output).
	ErrIncompleteAuditRow = errors.New("regime: audit row missing a required field")
	// ErrSecretInAuditRow fails closed when a row would carry a plaintext secret rather than a SecretRef
	// reference (INV-13) — the writer refuses to persist it.
	ErrSecretInAuditRow = errors.New("regime: audit row would persist a plaintext secret (only a SecretRef reference is allowed)")
)

// Audit is the append-only regime-audit WRITER: it fans every resolution, launch, and deferred verdict to its
// append-only table (via AuditSink) AND the tamper-evident governance ledger (via LedgerAppender), REQ-1715.
type Audit struct {
	sink   AuditSink
	ledger LedgerAppender
}

// NewAudit builds the writer over a durable sink and the governance ledger. Both are required (fail closed):
// a nil sink or nil ledger yields a writer whose every Record* call returns ErrAuditNotWired, so an
// unwired audit can never silently swallow a required governance output.
func NewAudit(sink AuditSink, ledger LedgerAppender) *Audit {
	return &Audit{sink: sink, ledger: ledger}
}

func (a *Audit) wired() bool { return a != nil && a.sink != nil && a.ledger != nil }

// RecordResolution appends one regime_resolution row and chains it into the governance ledger (REQ-1715).
func (a *Audit) RecordResolution(ctx context.Context, r ResolutionRow) error {
	if !a.wired() {
		return ErrAuditNotWired
	}
	if err := r.validate(); err != nil {
		return err
	}
	dec := audit.GovDecision{
		Decision: "regime:resolution:" + string(r.Outcome),
		Reason:   fmt.Sprintf("regime=%s lane=%s rule=%s", r.Regime, r.Lane, r.RuleID),
		ActionID: "regime-resolution:" + r.Target, // NON-SECRET synthetic binding (a resolution has no action_id)
		Withheld: r.Outcome == OutcomeRefused,     // a refusal withholds actuation
	}
	if _, err := a.ledger.Append(dec); err != nil {
		return fmt.Errorf("regime audit: resolution ledger append: %w", err)
	}
	if err := a.sink.AppendResolution(ctx, r); err != nil {
		return fmt.Errorf("regime audit: resolution persist (target %q): %w", r.Target, err)
	}
	return nil
}

// RecordActuation appends one regime_actuation row and chains it into the governance ledger (REQ-1715). The
// token is persisted only as its SecretRef reference — never a value (INV-13, enforced in validate).
func (a *Audit) RecordActuation(ctx context.Context, r ActuationRow) error {
	if !a.wired() {
		return ErrAuditNotWired
	}
	if err := r.validate(); err != nil {
		return err
	}
	dec := audit.GovDecision{
		Decision: "regime:actuation:launch",
		Reason:   fmt.Sprintf("lane=%s template=%s op_class=%s job=%s", r.Lane, r.JobTemplateID, r.OpClass, r.JobID),
		ActionID: r.ActionID,
		Withheld: false,
	}
	if _, err := a.ledger.Append(dec); err != nil {
		return fmt.Errorf("regime audit: actuation ledger append: %w", err)
	}
	if err := a.sink.AppendActuation(ctx, r); err != nil {
		return fmt.Errorf("regime audit: actuation persist (action %q): %w", r.ActionID, err)
	}
	return nil
}

// RecordDeferredVerdict appends one deferred_verdict row and chains it into the governance ledger (REQ-1715).
func (a *Audit) RecordDeferredVerdict(ctx context.Context, r DeferredVerdictRow) error {
	if !a.wired() {
		return ErrAuditNotWired
	}
	if err := r.validate(); err != nil {
		return err
	}
	dec := audit.GovDecision{
		Decision: "regime:deferred-verdict:" + string(r.Verdict),
		Reason:   fmt.Sprintf("status=%s graduation=%s job=%s", r.Status, r.Graduation, r.JobID),
		ActionID: r.ActionID,
		// A non-clean verdict withholds graduation credit (the "one channel allowed to say no").
		Withheld: r.Graduation != GraduationVerifiedClean,
	}
	if _, err := a.ledger.Append(dec); err != nil {
		return fmt.Errorf("regime audit: deferred-verdict ledger append: %w", err)
	}
	if err := a.sink.AppendDeferredVerdict(ctx, r); err != nil {
		return fmt.Errorf("regime audit: deferred-verdict persist (action %q): %w", r.ActionID, err)
	}
	return nil
}
