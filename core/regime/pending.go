package regime

// pending.go — the durable pending-verification record and its repository seam (REQ-1711/REQ-1712). An
// async lane's launch is a PREDICTION, not a success (design.md, the AsyncVerify component): the launch
// returns a job handle and the effect is only VERIFIED when a deferred verify polls the job to a terminal
// outcome. Until then the launch lives here in a `pending-verification` state and counts as NO clean run.
//
// This file owns the STATE the async-verify channel (asyncverify.go) keeps: one immutable-once-terminal
// record per launched `action_id`. The record is the durable idempotency guard (REQ-1712) — because a record
// is keyed by `action_id` and Insert fails closed on a duplicate, a retry / re-poll / redelivery can never
// reserve a SECOND launch for an `action_id` that already carries one (live OR terminal), so the action never
// double-actuates. It is ALSO the human/escalation-visible fail-safe (REQ-1711): a launch whose deferred
// verify never terminates stays a VISIBLE pending (then unverified) record — never a silent success.
//
// Persistence is a REPOSITORY INTERFACE with an in-memory fake for the oracles (CI has no Postgres). The pgx
// implementation + the append-only `deferred_verdict` table are a sibling task (T-017-6); this file defines
// only the interface the channel needs and NEVER imports that store. main.go (a later wave) binds the pgx
// impl in place of the fake — the channel is unchanged.
//
// Provenance: [O] INV-07 (bind each launch to its action_id; never double-fire) · [O] INV-10/INV-11
// (a launched effect is a prediction until a deterministic deferred verify reaches a terminal outcome), spec/017
// REQ-1709/1710/1711/1712 (TG-110). Phase 2: this is a VERIFY/observe record — it launches nothing and
// actuates nothing (the launch is the T-017-3 actuator through the spec/013 interceptor); mutation stays OFF.

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/territory-grounder/grounder/core/safety"
	"github.com/territory-grounder/grounder/core/verify"
)

// VerifyState is the CLOSED lifecycle of a launched action's deferred verification (REQ-1711). Its ZERO value
// is StatePending (fail-safe): an un-initialised record is treated as still-awaiting-verification and NEVER as
// a clean verified run.
type VerifyState string

const (
	// StatePending — the launch was reserved and (once bound) a job handle recorded; the deferred verify is
	// polling and has NOT reached a terminal AWX outcome. It counts as NO clean verified run (REQ-1711) and is
	// visible in the pending-verification queue. This is the zero value — fail-safe by construction.
	StatePending VerifyState = "pending"
	// StateVerified — the deferred verify reached a TERMINAL AWX status (successful / failed / error / canceled)
	// and produced the mechanical verdict fed to the graduation ladder (REQ-1709/1710). "Verified" names the
	// deferred-verify LIFECYCLE (a terminal was adjudicated); whether the run was a clean match, a deviation, or
	// indeterminate is carried by the record's Verdict + Verified fields.
	StateVerified VerifyState = "verified"
	// StateUnverified — the operator-declared verification bound elapsed BEFORE the job reached a terminal
	// status (REQ-1711). The launch is recorded unverified, counts toward NO graduation, and stays visible for a
	// human / escalation to see — the fail-safe for a deferred verify that never terminates. Never a silent
	// success.
	StateUnverified VerifyState = "unverified"
)

// Valid reports whether s is one of the three closed states. An unknown value is not valid and callers treat
// it fail-safe as pending (never as a clean verified run).
func (s VerifyState) Valid() bool {
	switch s {
	case StatePending, StateVerified, StateUnverified:
		return true
	default:
		return false
	}
}

// String renders the state slug the console and the ledger record. An invalid state renders "pending"
// (fail-safe) so a corrupt value can never read as a clean verified run.
func (s VerifyState) String() string {
	if !s.Valid() {
		return string(StatePending)
	}
	return string(s)
}

// PendingVerification is ONE launched action's durable deferred-verification record. It binds the launch to
// its `action_id` (the manifest lifecycle key, INV-07) and carries exactly what the deferred verify needs to
// reach a verdict later — the committed launch Prediction (REQ-1710) and the async JobID handle (REQ-1709) —
// plus the terminal outcome once reached. No secret value is ever stored here (INV-13): the JobID is a
// non-secret AWX handle, and the token that launched the job is a `SecretRef` held elsewhere.
type PendingVerification struct {
	// ActionID is the manifest action_id — the idempotency key (REQ-1712) AND the graduation binding. Exactly
	// one PendingVerification may exist per action_id; Insert fails closed on a duplicate.
	ActionID string
	// OpClass is the policy op-class the launched action belongs to — the earned-trust bucket the deferred
	// verdict is fed to on the graduation ladder (REQ-1710, spec/015 REQ-1514).
	OpClass string
	// Lane is the async effect lane whose launch deferred (awx-job today; gitops-mr later). The deferred-verify
	// channel is GLOBAL: any lane whose effect is not synchronously observable records here.
	Lane Regime
	// JobID is the async job handle the launch returned (REQ-1709) — a PREDICTION, not a success. Empty from
	// Reserve until BindHandle records the one sanctioned launch's handle.
	JobID string
	// State is the deferred-verify lifecycle position (pending / verified / unverified). Zero value = pending.
	State VerifyState
	// Prediction is the committed launch prediction the terminal outcome is diffed against by the deterministic
	// verifier (REQ-1710, spec/002 verify.ComputeVerdictDetail). The acting model never authors the verdict.
	Prediction verify.Prediction
	// LaunchedAt is when Reserve claimed the action_id; the verification bound (REQ-1711) is measured from here.
	LaunchedAt time.Time
	// TerminalStatus is the terminal AWX status once the deferred verify reaches one; empty while pending or on
	// a bound-elapsed timeout (which never reaches a terminal).
	TerminalStatus JobStatus
	// Verdict is the mechanical verdict the terminal outcome produced (spec/002). Empty until a terminal
	// success/failure adjudicates it; a timeout / indeterminate terminal leaves it empty (no clean run).
	Verdict safety.Verdict
	// Verified reports whether a terminal let the deterministic verifier ADJUDICATE the post-state — true for a
	// successful or failed/error terminal, false for a canceled (indeterminate) terminal or a timeout. It is the
	// verify-on-auto bit the graduation ladder consumes (spec/015 REQ-1515): an unverified run never counts as
	// clean.
	Verified bool
	// ResolvedAt is when the record reached a terminal (verified) or bound-elapsed (unverified) state; zero
	// while pending.
	ResolvedAt time.Time
}

// CleanRun reports whether this record is the ONE thing that counts as a clean verified run for graduation: a
// deferred verify that reached a terminal, ADJUDICATED the post-state, and found a mechanical `match`. A
// pending record, a timeout, a failed/error run (a deviation), or an indeterminate terminal is NOT a clean run
// — the launch-as-prediction discipline (REQ-1710/1711) made structural: nothing is clean until the deferred
// verify says so.
func (r PendingVerification) CleanRun() bool {
	return r.State == StateVerified && r.Verified && r.Verdict == safety.VerdictMatch
}

// ---------------------------------------------------------------------------------------------------------
// Repository seam + in-memory fake. The durable pgx store + the append-only `deferred_verdict` table are a
// sibling task (T-017-6); this file ships ONLY the interface the channel needs and the in-memory fake the
// oracles run against (CI has no Postgres). main.go binds the pgx impl in place of the fake — the channel is
// unchanged.
// ---------------------------------------------------------------------------------------------------------

var (
	// ErrDuplicatePending is the fail-closed refusal a PendingStore returns when a record already exists for an
	// action_id — the durable idempotency guard (REQ-1712). It is what makes a second launch for the same
	// action_id impossible: the reservation cannot be created twice.
	ErrDuplicatePending = errors.New("regime: a pending-verification record already exists for this action_id — refusing (idempotency)")
	// ErrNoPending is returned by Get/Update for an action_id that carries no pending-verification record.
	ErrNoPending = errors.New("regime: no pending-verification record for this action_id")
)

// PendingStore persists pending-verification records keyed by action_id. Insert is the ATOMIC idempotency
// gate: it MUST fail closed with ErrDuplicatePending if a record for the action_id already exists (in ANY
// state — live or terminal), so a retry / re-poll / redelivery can never reserve a second launch (REQ-1712).
// The durable pgx impl + migration is T-017-6; this seam is all the channel depends on.
type PendingStore interface {
	// Insert durably records a NEW pending-verification. It fails closed with ErrDuplicatePending if a record
	// for rec.ActionID already exists — the atomic no-double-launch guard (REQ-1712).
	Insert(ctx context.Context, rec PendingVerification) error
	// Get returns the record for actionID, or ErrNoPending if none exists.
	Get(ctx context.Context, actionID string) (PendingVerification, error)
	// Update writes back a record whose state advanced (pending → verified / unverified). It fails with
	// ErrNoPending if the action_id was never inserted (a transition can never invent a launch).
	Update(ctx context.Context, rec PendingVerification) error
	// List returns records in the given states (all records when no state is passed), deterministically
	// ordered — the visible pending-verification / unverified queues the console and escalation read (REQ-1711).
	List(ctx context.Context, states ...VerifyState) ([]PendingVerification, error)
}

// MemPendingStore is the in-memory PendingStore fake for the oracles. It is concurrency-safe and enforces the
// SAME fail-closed idempotency as the durable store: Insert refuses a duplicate action_id (ErrDuplicatePending).
type MemPendingStore struct {
	mu   sync.Mutex
	recs map[string]PendingVerification
}

// NewMemPendingStore returns an empty in-memory pending store.
func NewMemPendingStore() *MemPendingStore {
	return &MemPendingStore{recs: map[string]PendingVerification{}}
}

// Insert records a new pending-verification, failing closed on a duplicate action_id (REQ-1712).
func (s *MemPendingStore) Insert(_ context.Context, rec PendingVerification) error {
	if rec.ActionID == "" {
		return fmt.Errorf("regime: pending-verification requires an action_id")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.recs[rec.ActionID]; ok {
		return fmt.Errorf("%w: %s", ErrDuplicatePending, rec.ActionID)
	}
	s.recs[rec.ActionID] = rec
	return nil
}

// Get returns the record for actionID or ErrNoPending.
func (s *MemPendingStore) Get(_ context.Context, actionID string) (PendingVerification, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rec, ok := s.recs[actionID]
	if !ok {
		return PendingVerification{}, fmt.Errorf("%w: %s", ErrNoPending, actionID)
	}
	return rec, nil
}

// Update writes back an existing record, failing with ErrNoPending if it was never inserted.
func (s *MemPendingStore) Update(_ context.Context, rec PendingVerification) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.recs[rec.ActionID]; !ok {
		return fmt.Errorf("%w: %s", ErrNoPending, rec.ActionID)
	}
	s.recs[rec.ActionID] = rec
	return nil
}

// List returns the records in the requested states (all when none given), sorted by action_id for a
// deterministic queue.
func (s *MemPendingStore) List(_ context.Context, states ...VerifyState) ([]PendingVerification, error) {
	want := map[VerifyState]bool{}
	for _, st := range states {
		want[st] = true
	}
	s.mu.Lock()
	out := make([]PendingVerification, 0, len(s.recs))
	for _, rec := range s.recs {
		if len(want) == 0 || want[rec.State] {
			out = append(out, rec)
		}
	}
	s.mu.Unlock()
	sort.Slice(out, func(i, j int) bool { return out[i].ActionID < out[j].ActionID })
	return out, nil
}

// Compile-time proof the fake satisfies the seam.
var _ PendingStore = (*MemPendingStore)(nil)
