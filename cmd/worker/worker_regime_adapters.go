package main

// The actuation-regime RUNTIME ADAPTERS, carved out of main()'s composition root (TG-501 LOC-debt paydown):
// regimeVerifyLoop drains the async-verify queue on a cadence; deferredVerdictRow renders a resolved deferred
// verification as its audit row; regimeGradSink feeds a completed deferred verdict back to the policy graduation
// ladder; and awxJobPoller adapts the AWX job client to the regime's job-status poll. Pure relocation — the
// regime_wiring (call-based) and graduation-async (package-wide walk) guards pin their behaviour, and nothing
// here is pinned to main.go. Behaviour is unchanged by the move.

import (
	_ "time/tzdata" // embed the IANA zoneinfo DB so time.LoadLocation resolves on distroless (no OS tzdata)

	"context"
	"fmt"
	"log"
	"strconv"
	"strings"
	"time"

	"github.com/territory-grounder/grounder/core/policy"
	"github.com/territory-grounder/grounder/core/regime"
	"github.com/territory-grounder/grounder/core/safety"
	"github.com/territory-grounder/grounder/modules/actuation/awxjob"
	"github.com/territory-grounder/grounder/modules/actuation/gitopsmr"
)

// gitOpsMRJobPoller adapts the gitops-mr sensor (modules/actuation/gitopsmr.Poller) to the regime's
// job-status poll seam (TG-122 slice 2), mirroring awxJobPoller: the module speaks strings so it never
// imports core/regime; this adapter is the one place the slugs are typed. The sensor's contract holds here
// unchanged — it emits running/failed only (merge is NOT convergence; successful is withheld until a real
// convergence reader exists), and every read failure propagates so the deferred verify retries.
type gitOpsMRJobPoller struct{ poller *gitopsmr.Poller }

func (p gitOpsMRJobPoller) PollJob(ctx context.Context, handle string) (regime.JobStatus, error) {
	s, err := p.poller.PollJob(ctx, handle)
	return regime.JobStatus(s), err
}

// refusingJobPoller is the channel-wide fallback when NO lane-specific poller matches a record (e.g. an
// awx-job record on a deployment with no AWX client): it errors, so the record stays pending (transient) and
// resolves `unverified` at its bound — never a fabricated terminal from an unobservable job.
type refusingJobPoller struct{}

func (refusingJobPoller) PollJob(_ context.Context, handle string) (regime.JobStatus, error) {
	return "", fmt.Errorf("no poller registered for job %q's lane on this deployment — unobservable (stays pending, resolves unverified at the bound)", handle)
}

// asyncLaunchSeam nil-guards the deferred-verify PRODUCER seam for the runner Deps (TG-122 slice 0). A nil
// *regime.AsyncVerify assigned straight into the Deps interface field would be a NON-nil interface holding a
// typed nil — defeating the execute activity's `AsyncLaunch != nil` arming check and panicking at Reserve
// (the exact db.NewPendingStore(nil) trap poll_queue.go documents). nil stays UNTYPED nil, so the
// handle-returning lanes keep LaneEffect's structural refusal (fail closed).
func asyncLaunchSeam(av *regime.AsyncVerify) interface {
	Reserve(ctx context.Context, intent regime.LaunchIntent) error
	BindHandle(ctx context.Context, actionID, jobID string) error
} {
	if av == nil {
		return nil
	}
	return av
}

// regimeVerifyLoop drives the GLOBAL deferred-verify channel on an interval (REQ-1709..1712). Each tick lists
// the pending-verification queue (empty at Shadow) and single-steps Verify per action; on the ONE transition
// to a terminal outcome it appends the append-only deferred_verdict audit row (REQ-1715). It launches nothing
// and never crashes the worker — a poll/persist error is logged and retried next tick.
func regimeVerifyLoop(av *regime.AsyncVerify, aud *regime.Audit, interval time.Duration) {
	if interval <= 0 {
		interval = time.Minute
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for range t.C {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		pending, err := av.PendingQueue(ctx)
		if err != nil {
			log.Printf("regime async-verify: list pending failed: %v (retried next tick)", err)
			cancel()
			continue
		}
		for _, rec := range pending {
			res, verr := av.Verify(ctx, rec.ActionID)
			if verr != nil {
				log.Printf("regime async-verify: verify action %s: %v (still pending)", rec.ActionID, verr)
				continue
			}
			// Persist the append-only deferred_verdict row ONCE — on the transition to a terminal (GradFed is
			// true only on that call). A timeout (StateUnverified) has no terminal AWX status, so it never
			// writes a deferred_verdict row (the 0020 status CHECK requires a terminal); the pending record +
			// graduation feed already handle it.
			if res.GradFed && res.State == regime.StateVerified && res.TerminalStatus.Valid() {
				if werr := aud.RecordDeferredVerdict(ctx, deferredVerdictRow(res)); werr != nil {
					log.Printf("regime async-verify: persist deferred verdict for %s: %v", res.ActionID, werr)
				}
			}
		}
		cancel()
	}
}

// deferredVerdictRow maps a completed deferred-verify resolution onto the append-only deferred_verdict row
// vocabulary (REQ-1715): the mechanical verdict slug and the earned-trust graduation outcome, both non-secret.
func deferredVerdictRow(res regime.DeferredResolution) regime.DeferredVerdictRow {
	var v regime.Verdict
	switch res.Verdict {
	case safety.VerdictMatch:
		v = regime.VerdictMatch
	case safety.VerdictDeviation:
		v = regime.VerdictDeviation
	default:
		v = regime.VerdictUnverified
	}
	var g regime.GraduationOutcome
	switch {
	case res.CleanRun:
		g = regime.GraduationVerifiedClean
	case res.Verified && res.Verdict == safety.VerdictDeviation:
		g = regime.GraduationDeviated
	default:
		g = regime.GraduationNoCredit
	}
	return regime.DeferredVerdictRow{
		ActionID:   res.ActionID,
		JobID:      res.JobID,
		Status:     res.TerminalStatus,
		Verdict:    v,
		Graduation: g,
	}
}

// regimeGradSink adapts the regime deferred-verify channel's decoupled GraduationSink onto the spec/015
// policy graduation ladder WITHOUT core/regime importing core/policy (REQ-1710): it maps a deferred verdict
// to the ladder's RunOutcome (OutcomeFromVerdict) and records it against the op-class. An unverified run is
// fed as not-clean, so a launch we could not confirm within the bound never earns autonomy.
type regimeGradSink struct{ ladder *policy.Ladder }

// RecordDeferredVerdict feeds one completed deferred verify to the graduation ladder.
//
// FAIL-CLOSED AGAINST PROMOTION (TG-436). Unlike the session-terminus promote path (RecordGraduation,
// ~L4614), this async feed has NO credits.Claim gate — the ON CONFLICT DO NOTHING exactly-once key
// (REQ-2804) — and NO external_ref to ground migration-0064's graduation_credit_grounded trigger against an
// action_execution row: the GraduationSink seam (core/regime/asyncverify.go) carries only opClass/verdict/
// verified, and the async terminal writes a deferred_verdict AUDIT row, not an action_execution row. So a
// PROMOTING outcome recorded here would credit autonomy ungrounded AND un-deduplicated — the exact fail-open
// the terminus Claim closes, and the inverse of the earn-path (REQ-1514: graduation is EARNED; crediting a
// non-grounded run inverts it). Until the async producer writes an action_execution row and this seam carries
// an external_ref (the remaining TG-436 live work, gated on a live async observer + producer that do not yet
// exist), the async feed MUST NOT promote.
//
// Demotions and streak-breaks STILL flow: a safety outcome is never withheld by a bookkeeping key (the same
// rule the terminus states for its Claim). Only the promoting OutcomeVerifiedClean is refused, and a refused
// promote leaves the streak unchanged (return nil) — exactly as the terminus does on an already-claimed run.
func (s regimeGradSink) RecordDeferredVerdict(ctx context.Context, opClass string, v safety.Verdict, verified bool) error {
	if s.ladder == nil {
		return nil
	}
	outcome := policy.OutcomeFromVerdict(v, verified)
	if outcome == policy.OutcomeVerifiedClean {
		log.Printf("graduation: async deferred-verify produced a CLEAN outcome for %q but the async feed has no "+
			"exactly-once claim and no action_execution grounding — PROMOTION REFUSED, streak unchanged, autonomy "+
			"NOT credited (TG-436); route the async producer through credits.Claim before this may promote", opClass)
		return nil
	}
	// GRADUATION-WRITER: fail-closed — the promoting outcome is refused above (TG-436); only a demote or a
	// streak-break (a safety signal never withheld) reaches the ladder here.
	_, err := s.ladder.Record(ctx, opClass, outcome)
	return err
}

// awxJobPoller adapts the AWX-job launch client's read-only GetJob into the regime deferred-verify JobPoller
// (REQ-1709). It OBSERVES only (GET /api/v2/jobs/{id}/) — it launches nothing. A non-numeric handle or an
// unreadable read leaves the deferred verify pending (the channel retries) rather than fabricating a terminal.
type awxJobPoller struct{ client *awxjob.Client }

// PollJob reads the current AWX job status by its handle.
func (p awxJobPoller) PollJob(ctx context.Context, jobID string) (regime.JobStatus, error) {
	id, err := strconv.Atoi(strings.TrimSpace(jobID))
	if err != nil || id <= 0 {
		return "", fmt.Errorf("regime poll: non-numeric AWX job handle %q", jobID)
	}
	j, err := p.client.GetJob(ctx, id)
	if err != nil {
		return "", err
	}
	return regime.JobStatus(strings.TrimSpace(j.Status)), nil
}
