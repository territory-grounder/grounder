package ledgeranchor

// TG-80 P1.1 (verbatim ranked adoption #1): "Anchor governance_ledger HEAD HASH into Temporal history
// (periodic activity where tg_runtime has no cred) -> tamper-EVIDENCE becomes tamper-RESISTANCE."
//
// WHAT job.go/verify.go ALREADY DELIVER, AND WHAT THEY DON'T. The recorder (Job) and its consumer (VerifyJob)
// witness the governance_ledger HEAD into ledger_anchor (migration 0092) — an append-only Postgres table
// tg_runtime holds INSERT+SELECT but not UPDATE/DELETE on, same REVOKE as the spine (0015). That is real
// tamper-EVIDENCE, but job.go's own docstring names its residual: ledger_anchor lives in the SAME Postgres
// INSTANCE as governance_ledger, so an actor who can reverse ONE REVOKE (0015.down, a mis-scoped retention
// job, a compromised superuser) can reverse the OTHER identically — one escalation defeats both tables. This
// file is the deferred piece: land the SAME anchor in a credential domain that escalation cannot reach at all.
//
// THE CREDENTIAL-DOMAIN BOUNDARY THIS RESTS ON (verified against this deployment, not assumed):
//   - tg_runtime's Postgres role writes governance_ledger + ledger_anchor in the `tg` application database.
//   - Temporal's OWN persistence (the durable store behind every workflow's event history) runs in a
//     SEPARATE container + database + role: deploy/docker-compose.yml's `temporal-postgres` service, credentialed
//     by TEMPORAL_PG_PASSWORD — a secret tg_runtime's own process env never holds (grep TEMPORAL_PG_PASSWORD:
//     it reaches temporal-postgres and the temporal auto-setup container only).
//   - tg_runtime's ONLY reach into Temporal is a gRPC client to the frontend service (TG_TEMPORAL_HOSTPORT,
//     client.Dial with a bare HostPort — cmd/worker/main.go). That client can start/signal/query/terminate
//     workflows, but no RPC on that surface lets a caller EDIT the content of an event already durably
//     appended to a workflow's history — only append new events to an OPEN run, or fork a new RunID (Reset).
//     A CLOSED witness workflow's recorded result is therefore not rewritable through any exposed client call,
//     regardless of which process holds the client — REWRITE is what VerifyAgainstAnchors's residual is about,
//     and REWRITE is what this closes.
//
// THE RESIDUAL (report honestly, do not oversell — revised after a fresh-eyes review caught the first cut of
// this doc under-stating it). TG_TEMPORAL_HOSTPORT is dialed with no TLS and no API key
// (client.Dial(client.Options{HostPort: hostPort}), confirmed at cmd/worker/main.go) — this is a self-hosted
// OSS Temporal server with no authorizer configured, so ANYTHING with tg_runtime's network reach has the SAME
// broad frontend RPC surface tg_runtime does. That has TWO distinct consequences, not one:
//
//  1. ERASURE of an ALREADY-witnessed anchor: a frontend-reaching actor could TERMINATE or (on servers
//     exposing it) DELETE a closed witness execution outright. Deletion is erasure, not rewrite — Temporal's
//     execution model structurally has no "edit history in place" operation — which is why TemporalVerifyJob
//     (temporal_verify.go) fails SAFE, never a silent "clean", when an expected witness cannot be found.
//
//  2. THE SQUAT RESIDUAL (the severe one — NOT fully closed by this package, unlike (1) which the fail-safe
//     verify design handles cleanly). A frontend-reaching actor can also call ExecuteWorkflow on
//     WitnessWorkflowID(domain, seq) for a seq that has NOT been witnessed yet — i.e. PRE-REGISTER a forged
//     witness before the legitimate recorder's next periodic pass reaches that seq. Record() now (after the
//     review) reads back and compares an already-existing witness's content on AlreadyStarted rather than
//     treating it as unconditional success, which closes the SLOPPY version of this attack: a squatter who
//     guesses the wrong content, or who squats without also controlling the DB, gets caught the moment the
//     legitimate recorder tries to witness the real value and finds a mismatch.
//
//     It does NOT close the COORDINATED version. Work through the timeline: an actor who (a) can rewrite
//     governance_ledger + ledger_anchor in TG's Postgres to a self-consistent forged chain AND (b) can also
//     reach the unauthenticated Temporal frontend can compute the forged anchor BEFORE tampering the DB,
//     pre-register (squat) WitnessWorkflowID(domain, seq) with that EXACT forged content, THEN tamper the DB
//     to match, all before the periodic recorder's next tick. When the recorder runs, Head() reads the
//     ALREADY-tampered DB and computes the SAME forged anchor the squatter planted — Record()'s new
//     content-compare passes, because both sides genuinely agree (both are the same lie). TemporalVerifyJob
//     then finds DB anchor, live chain, and Temporal witness all agreeing on the forged content and reports
//     clean. This is NOT a hole in the compare fix specifically — no compare-at-record-time logic can catch
//     it, because from the recorder's vantage point there is no internal contradiction to see: the ONLY way
//     to catch it is an independent source of truth for "what this seq's content SHOULD be", which does not
//     exist once both the DB and the not-yet-witnessed Temporal slot are attacker-controlled.
//
//     What DOES survive, and is the real, substantial value this still delivers: every seq that was ALREADY
//     witnessed before the attack began. Squatting only works on a not-yet-used Workflow ID — REJECT_DUPLICATE
//     makes an already-witnessed seq's id un-squattable regardless of DB access, so rewriting PAST, already-
//     witnessed history (the tail-truncation and wholesale-re-link tampers core/audit/anchor.go's own doc
//     names as VerifyChain's blind spot, and the scenario the DB-only ledger_anchor witness cannot itself
//     survive against a compromised superuser) is still caught. The residual is narrowed to a real-time race
//     against RECENTLY-appended, not-yet-witnessed rows — bounded by TG_LEDGER_ANCHOR_INTERVAL (periodic, up
//     to that interval wide today) — not eliminated for that narrow, fresh-boundary window.
//
//     Two paths would close it further, both architectural, neither built here:
//       - Authenticate the Temporal frontend (mTLS/API key) so an unauthenticated actor cannot reach
//         ExecuteWorkflow/GetWorkflow AT ALL — this removes the attacker's capability at the root, regardless
//         of timing, and is general estate hardening beyond this one feature.
//       - Witness SYNCHRONOUSLY at ledger-append time instead of periodically, shrinking the not-yet-witnessed
//         window from "up to an hour" to "a real-time race of one network round trip" (not providably zero,
//         but the attacker would need to predict a row's content before it exists rather than rewrite one that
//         already does). This requires coupling core/audit/ledger.go — a spec/006 lockstep-governed file — to
//         Temporal's availability on the SAME path every governance decision writes through; this codebase has
//         already been burned once by exactly that coupling shape against a slower dependency (TG-277,
//         ledger.go's own AppendContext doc), so this is an owner call, not an AFK build.
//
// This is also why the mechanism is additive alongside, not a replacement for, the DB-REVOKE anchor: the two
// remaining failure modes (same-Postgres-instance rewrite of ALREADY-witnessed rows; a coordinated fresh-row
// squat) are independent and both must hold for a tamper to go fully undetected, which is strictly more than
// either layer alone provides — but it is not the airtight guarantee "tamper-RESISTANCE" can be read to imply
// for the freshest slice of the ledger, and this doc says so on purpose.
//
// This is NOT the cousin's (h-apache-stack) full airgap: their agent box holds ZERO Temporal creds and can
// only POST one-way signals to a separate service. TG's worker is a first-class Temporal WORKER with full RPC
// reach (start/signal/terminate/query/pre-register) over the same UNAUTHENTICATED frontend it records anchors
// through — narrower than a zero-cred agent, and (per the squat residual above) narrower than "closes the
// compromised-superuser threat outright". The full zero-cred airgap (a separate service + network boundary,
// or at minimum an authenticated Temporal frontend) is an infra/architecture decision, not this slice.

import (
	"context"
	"errors"
	"fmt"
	"time"

	enumspb "go.temporal.io/api/enums/v1"
	"go.temporal.io/api/serviceerror"
	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/workflow"

	"github.com/territory-grounder/grounder/core/audit"
)

// WitnessWorkflowID returns the deterministic Temporal Workflow ID for one (domain, seq) HEAD witness. Every
// seq gets its own permanently addressable execution, so a verifier can look one up directly via
// client.GetWorkflow — never needing Temporal's Visibility/List API, whose availability/query semantics vary
// by deployment. domain matches core/audit's anchor-store domain (audit.DomainGovernanceLedger for the
// governance ledger; a second witnessed chain, e.g. TG-510's knowledge corpus, uses its own).
func WitnessWorkflowID(domain string, seq int64) string {
	return fmt.Sprintf("tg/ledger-anchor/%s/seq-%d", domain, seq)
}

// WitnessAnchorActivity durably lands one HEAD anchor in Temporal's own event history. It performs no I/O and
// no logic of its own — it returns its input unchanged. The witnessing is not something this function's CODE
// does; it is what EXECUTING it durably records: the Temporal SERVER appends the ActivityTaskCompleted event
// (and, once WitnessAnchorWorkflow returns, WorkflowExecutionCompleted) carrying this exact payload to
// Temporal's persistence store — a database tg_runtime's own Postgres role cannot reach (see the package
// doc). A plain function, not a method: nothing here needs injected state.
func WitnessAnchorActivity(_ context.Context, a audit.Anchor) (audit.Anchor, error) {
	return a, nil
}

// WitnessAnchorWorkflow exists only to give WitnessAnchorActivity a durable, individually addressable
// execution to run inside (a bare activity task has no Workflow ID a verifier could look up later). One
// attempt is enough — the activity is a pure, always-succeeding echo, so a retry would buy nothing a first
// attempt does not already give.
func WitnessAnchorWorkflow(ctx workflow.Context, a audit.Anchor) (audit.Anchor, error) {
	opts := workflow.ActivityOptions{
		StartToCloseTimeout: 30 * time.Second,
		RetryPolicy:         &temporal.RetryPolicy{MaximumAttempts: 2},
	}
	var out audit.Anchor
	err := workflow.ExecuteActivity(workflow.WithActivityOptions(ctx, opts), WitnessAnchorActivity, a).Get(ctx, &out)
	return out, err
}

// temporalClient is the minimal slice of client.Client TemporalWitness needs. Narrow deliberately: the real
// SDK Client interface has ~30 methods, which would make Record's AlreadyStarted branch (added for the
// squat finding below) untestable without a heavy hand-rolled implementation of all of them. client.Client
// already has both these exact methods, so a real *client.Client value satisfies this interface with no
// adapter — only the TEST fake needs to implement just these two.
type temporalClient interface {
	ExecuteWorkflow(ctx context.Context, options client.StartWorkflowOptions, workflow interface{}, args ...interface{}) (client.WorkflowRun, error)
	GetWorkflow(ctx context.Context, workflowID string, runID string) client.WorkflowRun
}

// TemporalWitness is an AnchorSink (the SAME interface Job.Sink already takes — job.go) that durably witnesses
// each recorded HEAD anchor into Temporal's event history, and a TemporalAnchorReader (temporal_verify.go)
// that reads one back. Wired ALONGSIDE (never instead of) the existing DB-backed AnchorStore — see
// cmd/worker/ledger_anchor_wiring.go — as a second, independent witness in a different credential domain.
type TemporalWitness struct {
	Client    temporalClient
	TaskQueue string
	Domain    string
}

// Record starts the witness workflow for a.Seq and waits for it to complete — the wait is what proves the
// witness actually landed, matching the durability guarantee AnchorSink callers already rely on for the
// DB-backed sink (job.go's Run treats a Record error as a failed pass, retried next tick).
//
// THE SQUAT FINDING (caught in review, NOT closed by REJECT_DUPLICATE alone — see the package doc's "THE
// SQUAT RESIDUAL"). WorkflowIDReusePolicy REJECT_DUPLICATE means Temporal refuses a SECOND execution under an
// already-used (domain, seq) id, but "already used" says nothing about WHO used it first or WHAT it recorded.
// Because TG_TEMPORAL_HOSTPORT is unauthenticated (package doc), anything with tg_runtime's network reach can
// call ExecuteWorkflow on WitnessWorkflowID(domain, seq) for a seq that has not been witnessed YET — i.e. can
// PRE-REGISTER (squat) a forged witness before the legitimate recorder gets there. The original version of
// this method treated AlreadyStarted as unconditional success — exactly the blind spot a squatted forgery
// needs: the legitimate recorder's later attempt would silently no-op, and the forged witness would stand
// unchallenged. Fixed: on AlreadyStarted, the existing witness is READ BACK and its content COMPARED to `a`.
// A mismatch is surfaced (never silent success) — it means either a genuine content-changing race (which
// must never happen: two legitimate recorders witnessing the same seq always compute the same anchor from
// the same chain) or a pre-registered forgery. Only a content MATCH is treated as idempotent success — the
// same convention cmd/grounder/deps.go's temporalTriage.StartTriage and cmd/worker's cron arms use for
// AlreadyStarted, but never applied blind here again.
func (t *TemporalWitness) Record(ctx context.Context, a audit.Anchor) error {
	if t.Client == nil {
		return errors.New("ledgeranchor: TemporalWitness has no Temporal client")
	}
	id := WitnessWorkflowID(t.Domain, a.Seq)
	run, err := t.Client.ExecuteWorkflow(ctx, client.StartWorkflowOptions{
		ID:                    id,
		TaskQueue:             t.TaskQueue,
		WorkflowIDReusePolicy: enumspb.WORKFLOW_ID_REUSE_POLICY_REJECT_DUPLICATE,
	}, WitnessAnchorWorkflow, a)
	if err != nil {
		var already *serviceerror.WorkflowExecutionAlreadyStarted
		if errors.As(err, &already) {
			existing, found, rerr := t.ReadWitness(ctx, t.Domain, a.Seq)
			if rerr != nil {
				return fmt.Errorf("ledgeranchor: witness %s already exists but could not be read back to verify its content: %w", id, rerr)
			}
			if !found {
				// AlreadyStarted but GetWorkflow reports no such execution: an inconsistent read (e.g. the
				// existing run has not completed yet). Unverifiable is not the same as clean — surface it.
				return fmt.Errorf("ledgeranchor: witness %s reported already-started but could not be read back — cannot confirm it is not a forgery", id)
			}
			if existing.Hash != a.Hash || existing.Digest != a.Digest || existing.WindowSize != a.WindowSize {
				return fmt.Errorf("%w: seq %d — a witness already exists under %s but its content disagrees with the anchor being recorded (existing hash %s, intended hash %s): either a legitimate recorder computed two different anchors for the same seq (must never happen) or this Workflow ID was PRE-REGISTERED with forged content before this recorder reached it",
					ErrTemporalWitnessMismatch, a.Seq, id, shortHash(existing.Hash), shortHash(a.Hash))
			}
			return nil // content genuinely matches what we intended to witness — truly idempotent, not a forgery
		}
		return fmt.Errorf("ledgeranchor: start temporal witness %s: %w", id, err)
	}
	var out audit.Anchor
	if err := run.Get(ctx, &out); err != nil {
		return fmt.Errorf("ledgeranchor: temporal witness %s did not complete: %w", id, err)
	}
	return nil
}

// ReadWitness looks up the Temporal-recorded anchor for (domain, seq) via its deterministic Workflow ID.
// found=false (nil error) means no such execution exists — never witnessed, or aged out of the Temporal
// namespace's retention window — which TemporalVerifyJob (temporal_verify.go) treats as "cannot verify this
// seq", NEVER as "clean". A namespace's retention must exceed the verify cadence for cross-checks to have
// continuous coverage; that is an operator/ops knob (Temporal namespace config), not something this code can
// enforce.
func (t *TemporalWitness) ReadWitness(ctx context.Context, domain string, seq int64) (audit.Anchor, bool, error) {
	if t.Client == nil {
		return audit.Anchor{}, false, errors.New("ledgeranchor: TemporalWitness has no Temporal client")
	}
	var out audit.Anchor
	err := t.Client.GetWorkflow(ctx, WitnessWorkflowID(domain, seq), "").Get(ctx, &out)
	if err != nil {
		var notFound *serviceerror.NotFound
		if errors.As(err, &notFound) {
			return audit.Anchor{}, false, nil
		}
		return audit.Anchor{}, false, err
	}
	return out, true, nil
}
