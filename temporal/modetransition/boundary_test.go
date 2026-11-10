package modetransition

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"go.temporal.io/sdk/testsuite"

	"github.com/territory-grounder/grounder/core/audit"
	"github.com/territory-grounder/grounder/core/db"
	"github.com/territory-grounder/grounder/core/policy"
	"github.com/territory-grounder/grounder/temporal/wfauth"
	"github.com/territory-grounder/grounder/temporal/wfauth/wfauthtest"
)

// TG-660 — THE AUTHORIZATION BOUNDARY, DRIVEN THROUGH THE WORKFLOW (owner prompt 2026-11-07 §2.C).
//
// Every row below starts ModeTransitionWorkflow — the most consequential admin write: it raises the live mode —
// through the Temporal SDK test environment (the harness `make all` runs every workflow oracle under), with the
// activity registered the way cmd/worker registers it and a REAL signer/verifier (HMAC, nonce store, clock), not a
// mocked boolean. For each refused row the test proves THREE things: the workflow failed with the refusal, the
// refusal is on the governance ledger with its class (`auth:refused-unsigned`, `why=<class>`), and NOTHING
// changed — the controller's mode is what it was and its own ledger saw no transition. The accepted rows prove
// the inverse: the grounder's signed start flips the mode, and the operator's emergency REDUCTION of authority
// (a signed lower) still works.
//
// Not in this file, by design:
//   - the vote boundary (an unauthenticated VoteSignal, a vote signed for another session, a replayed vote):
//     temporal/runner/vote_auth_test.go, driven through RunnerWorkflow;
//   - the autonomous fallback approver as a positive control: temporal/runner/autonomous_resolver_test.go
//     (TestArmedFallbackApproverActsOnACanaryPinWithoutPollingOrWaiting) executes through ExecuteActivity's
//     durable re-derivation on the approver's `fallback:autonomous-approve` ledger row;
//   - a rogue WORKER on the task queue: on this stack that is the transport boundary (Temporal frontend mTLS,
//     TG-660 part a, a separate MR) — the envelope makes a rogue worker's forged STARTS worthless but cannot stop a
//     process the frontend admits from polling tasks. Nothing here claims otherwise.
//
// KILLING MUTATION (executed 2026-11-07): deleting the wfauth.Require call in ApplyModeTransitionActivity turned
// every refused row of TestModeTransitionBoundaryMatrix RED — the unsigned, forged, stale, replayed and mis-scoped
// starts all FLIPPED THE MODE.

type boundaryRig struct {
	ctl      *policy.ModeController
	led      *audit.Ledger
	refusals *audit.Ledger
	signer   *wfauth.Signer
	verifier wfauth.EnvelopeVerifier
}

func newBoundaryRig(t *testing.T, verifier wfauth.EnvelopeVerifier) *boundaryRig {
	t.Helper()
	s, v := wfauthtest.Pair(t)
	if verifier == nil {
		verifier = v
	}
	led := audit.NewLedger()
	return &boundaryRig{
		ctl:      newCtl(t, led, policy.NewModeTransitionAuthority(nil), greenPreflight{}),
		led:      led,
		refusals: audit.NewLedger(),
		signer:   s,
		verifier: verifier,
	}
}

// start runs the workflow end to end in the SDK test environment and returns its result or error.
func (r *boundaryRig) start(t *testing.T, req Request) (Result, error) {
	t.Helper()
	var ts testsuite.WorkflowTestSuite
	env := ts.NewTestWorkflowEnvironment()
	acts := &Activities{D: Deps{Controller: r.ctl, Auth: r.verifier, Ledger: r.refusals}}
	env.RegisterWorkflow(ModeTransitionWorkflow)
	env.RegisterActivity(acts.ApplyModeTransitionActivity)
	env.ExecuteWorkflow(ModeTransitionWorkflow, req)
	if !env.IsWorkflowCompleted() {
		t.Fatal("the workflow must complete")
	}
	if err := env.GetWorkflowError(); err != nil {
		return Result{}, err
	}
	var res Result
	if err := env.GetWorkflowResult(&res); err != nil {
		t.Fatalf("read result: %v", err)
	}
	return res, nil
}

// raise is the forged intent every refused row carries: an admin-claimed raise to Full-auto.
func raise() Request {
	return Request{To: "Full-auto", ExpectedFrom: "Shadow", Actor: "operator:intruder", Reason: "boundary matrix", AdminAuthorized: true}
}

func (r *boundaryRig) signed(t *testing.T, req Request) Request {
	t.Helper()
	env, err := r.signer.Sign(AuthKind, req.AuthFields()...)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	req.Auth = env
	return req
}

// expectRefused asserts the three proofs of a refused start: the failed workflow, the ledgered class, no change.
func (r *boundaryRig) expectRefused(t *testing.T, err error, class string, modeBefore policy.Mode, ledgerBefore int) {
	t.Helper()
	if err == nil {
		t.Fatalf("the start must FAIL (class %s), it succeeded — mode now %v", class, r.ctl.Current())
	}
	if !strings.Contains(err.Error(), wfauth.RefusedMarker) {
		t.Fatalf("the workflow error must carry the refusal marker %q: %v", wfauth.RefusedMarker, err)
	}
	if got := r.ctl.Current(); got != modeBefore {
		t.Fatalf("STATE CHANGED on a refused start: mode %v -> %v", modeBefore, got)
	}
	if r.led.Len() != ledgerBefore {
		t.Fatalf("the controller's ledger must see nothing on a refused start: %+v", r.led.Entries()[ledgerBefore:])
	}
	last := r.refusals.Entries()[r.refusals.Len()-1]
	if last.Decision != wfauth.RefusalDecision || !strings.Contains(last.Reason, "why="+class) || !strings.Contains(last.Reason, "kind="+AuthKind) {
		t.Fatalf("the refusal must be ledgered as %s with why=%s, got %+v", wfauth.RefusalDecision, class, last)
	}
	if strings.Contains(last.Reason, "MAC") || strings.Contains(last.Reason, "nonce=") {
		t.Fatalf("the ledger must never carry the envelope's secrets: %q", last.Reason)
	}
}

func TestModeTransitionBoundaryMatrix(t *testing.T) {
	t.Run("authorized start is accepted and flips the mode", func(t *testing.T) {
		r := newBoundaryRig(t, nil)
		good := r.signed(t, Request{To: "Full-auto", ExpectedFrom: "Shadow", Actor: "operator:kyriakos", Reason: "owner canary GO", AdminAuthorized: true})
		res, err := r.start(t, good)
		if err != nil || res.Mode != "Full-auto" || r.ctl.Current() != policy.ModeFullAuto {
			t.Fatalf("the grounder's signed start must flip the controller: res=%+v err=%v mode=%v", res, err, r.ctl.Current())
		}
		if r.led.Len() != 1 || r.led.Entries()[0].Decision != "mode-transition:Shadow->Full-auto" {
			t.Fatalf("the transition must be audited exactly once on the controller's ledger: %+v", r.led.Entries())
		}
		if r.refusals.Len() != 0 {
			t.Fatalf("an accepted start ledgers no refusal: %+v", r.refusals.Entries())
		}
	})
	t.Run("unsigned envelope", func(t *testing.T) {
		r := newBoundaryRig(t, nil)
		_, err := r.start(t, raise())
		r.expectRefused(t, err, "unsigned", policy.ModeShadow, 0)
	})
	t.Run("forged signature: another key", func(t *testing.T) {
		r := newBoundaryRig(t, nil)
		other, err := wfauth.NewSigner(append([]byte("not-the-grounder-key-"), wfauthtest.Key[:11]...))
		if err != nil {
			t.Fatalf("other signer: %v", err)
		}
		req := raise()
		req.Auth, err = other.Sign(AuthKind, req.AuthFields()...)
		if err != nil {
			t.Fatalf("sign: %v", err)
		}
		_, err = r.start(t, req)
		r.expectRefused(t, err, "key-mismatch", policy.ModeShadow, 0)
	})
	t.Run("forged signature: the right key id, a made-up MAC", func(t *testing.T) {
		r := newBoundaryRig(t, nil)
		req := r.signed(t, raise())
		req.Auth.MAC = strings.Repeat("ab", 32)
		_, err := r.start(t, req)
		r.expectRefused(t, err, "tampered", policy.ModeShadow, 0)
	})
	t.Run("stale envelope: a captured start presented after its window", func(t *testing.T) {
		r := newBoundaryRig(t, nil)
		req := r.signed(t, raise())
		req.Auth.IssuedAt -= int64((2 * time.Hour).Seconds())
		req.Auth.ExpiresAt -= int64((2 * time.Hour).Seconds())
		_, err := r.start(t, req)
		r.expectRefused(t, err, "expired", policy.ModeShadow, 0)
	})
	t.Run("replayed envelope: the same signed start twice", func(t *testing.T) {
		r := newBoundaryRig(t, nil)
		// a legitimate signed LOWER is accepted once ...
		lower := r.signed(t, Request{To: "Shadow", Actor: "operator:kyriakos", Reason: "stand down", AdminAuthorized: true})
		if _, err := r.start(t, lower); err != nil {
			t.Fatalf("first presentation must be accepted: %v", err)
		}
		mode, n := r.ctl.Current(), r.led.Len()
		// ... and the identical bytes presented again (a captured Temporal payload) are refused.
		_, err := r.start(t, lower)
		r.expectRefused(t, err, "replayed", mode, n)
	})
	t.Run("mis-scoped envelope: a valid signature over another workflow kind", func(t *testing.T) {
		r := newBoundaryRig(t, nil)
		req := raise()
		env, err := r.signer.Sign(wfauth.VoteKind, req.AuthFields()...)
		if err != nil {
			t.Fatalf("sign: %v", err)
		}
		req.Auth = env
		_, err = r.start(t, req)
		r.expectRefused(t, err, "tampered", policy.ModeShadow, 0)
	})
	t.Run("mis-scoped envelope: a valid signature over another actor's request", func(t *testing.T) {
		r := newBoundaryRig(t, nil)
		alice := raise()
		alice.Actor = "operator:alice"
		signedForAlice := r.signed(t, alice)
		req := raise()
		req.Auth = signedForAlice.Auth
		_, err := r.start(t, req)
		r.expectRefused(t, err, "tampered", policy.ModeShadow, 0)
	})
	t.Run("mis-scoped envelope: a signed lower spliced onto a raise", func(t *testing.T) {
		r := newBoundaryRig(t, nil)
		lower := r.signed(t, Request{To: "Shadow", Actor: "operator:kyriakos", Reason: "stand down", AdminAuthorized: true})
		req := raise()
		req.Actor, req.Reason = lower.Actor, lower.Reason
		req.Auth = lower.Auth
		_, err := r.start(t, req)
		r.expectRefused(t, err, "tampered", policy.ModeShadow, 0)
	})
	t.Run("a worker with no key refuses even the grounder's own start", func(t *testing.T) {
		r := newBoundaryRig(t, (*wfauth.Verifier)(nil))
		good := r.signed(t, Request{To: "Full-auto", ExpectedFrom: "Shadow", Actor: "operator:kyriakos", Reason: "owner canary GO", AdminAuthorized: true})
		_, err := r.start(t, good)
		r.expectRefused(t, err, "no-key", policy.ModeShadow, 0)
	})
	t.Run("positive control: the operator's emergency reduction of authority still works", func(t *testing.T) {
		r := newBoundaryRig(t, nil)
		if _, err := r.start(t, r.signed(t, Request{To: "Full-auto", ExpectedFrom: "Shadow", Actor: "operator:kyriakos", Reason: "GO", AdminAuthorized: true})); err != nil {
			t.Fatalf("arm: %v", err)
		}
		res, err := r.start(t, r.signed(t, Request{To: "Shadow", ExpectedFrom: "Full-auto", Actor: "operator:kyriakos", Reason: "kill", AdminAuthorized: true}))
		if err != nil || res.Mode != "Shadow" || r.ctl.Current() != policy.ModeShadow {
			t.Fatalf("a signed lower must still be accepted: res=%+v err=%v mode=%v", res, err, r.ctl.Current())
		}
	})
}

// The replay guard against a REAL PostgreSQL (TG_TEST_DSN, the migrated fixture): the nonce table, not an
// in-memory map, is what makes a captured start single-use across worker restarts and across both planes.
func TestModeTransitionReplayIsRefusedByTheDurableNonceTable(t *testing.T) {
	dsn := os.Getenv("TG_TEST_DSN")
	if dsn == "" {
		t.Skip("set TG_TEST_DSN to a migrated database to run the TG-660 durable replay-guard boundary test")
	}
	ctx := context.Background()
	pool, err := db.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	t.Cleanup(pool.Close)
	v, err := wfauth.NewVerifier(wfauthtest.Key, db.NewWorkflowAuthNonceStore(pool))
	if err != nil {
		t.Fatalf("verifier: %v", err)
	}
	r := newBoundaryRig(t, v)
	good := r.signed(t, Request{To: "Full-auto", ExpectedFrom: "Shadow", Actor: "operator:kyriakos", Reason: "owner canary GO", AdminAuthorized: true})
	t.Cleanup(func() { _, _ = pool.Exec(ctx, "DELETE FROM workflow_auth_nonce WHERE nonce = $1", good.Auth.Nonce) })
	if _, err := r.start(t, good); err != nil || r.ctl.Current() != policy.ModeFullAuto {
		t.Fatalf("first presentation must be accepted: err=%v mode=%v", err, r.ctl.Current())
	}
	// a SECOND worker process (fresh verifier, same key, same table) sees the same captured payload
	v2, err := wfauth.NewVerifier(wfauthtest.Key, db.NewWorkflowAuthNonceStore(pool))
	if err != nil {
		t.Fatalf("verifier 2: %v", err)
	}
	r.verifier = v2
	mode, n := r.ctl.Current(), r.led.Len()
	_, err = r.start(t, good)
	r.expectRefused(t, err, "replayed", mode, n)
	var seen int
	if qerr := pool.QueryRow(ctx, "SELECT count(*) FROM workflow_auth_nonce WHERE nonce = $1", good.Auth.Nonce).Scan(&seen); qerr != nil || seen != 1 {
		t.Fatalf("the nonce must be claimed exactly once in the table: n=%d err=%v", seen, qerr)
	}
}
