package main

import (
	"log"
	"os"
	"strings"
	"sync"

	"github.com/territory-grounder/grounder/core/config"
	"github.com/territory-grounder/grounder/core/db"
	"github.com/territory-grounder/grounder/temporal/runner"
	"github.com/territory-grounder/grounder/temporal/wfauth"
)

// workflowAuth returns the VERIFIER for the signed authorization envelope on admin-write workflow requests and
// approval votes (TG-660, deep-review F1 / pack 12). The grounder signs; this process checks, FIRST, in every
// admin-write activity and in the vote loop, and refuses (ledger `auth:refused-unsigned`) whatever does not
// verify — absent, tampered, expired, replayed, or signed with another key.
//
// Memoized: main() wires nine activity Deps from it and one log line is the truth, not nine. Built on first use,
// which happens after the plane pool exists (the replay guard, workflow_auth_nonce, is a table).
//
// Read with os.Getenv DIRECTLY (not the console-resolving getter — exempted by name in boot_config_test.go): a
// console-supplied key would let the surface this verifies choose the key it is verified with.
//
// UNSET ⇒ a nil verifier, and the line says so. Every admin write and vote is then refused: a worker with no key
// cannot tell a grounder from an attacker, and "refuse everything" is the only honest posture. The ref is a
// BUSINESS secret for the preflight (biz("TG_WORKFLOW_AUTH_KEY_REF")): under TG_SECRET_POLICY=enforce it must
// resolve from a backend (bao:). No pool with a key set ⇒ refuse to boot: verifying without replay protection is
// not verifying.
func workflowAuth(pool *db.Pool) wfauth.EnvelopeVerifier {
	workflowAuthOnce.Do(func() { workflowAuthVerifier = wireWorkflowAuth(pool) })
	return workflowAuthVerifier
}

var (
	workflowAuthOnce     sync.Once
	workflowAuthVerifier wfauth.EnvelopeVerifier = (*wfauth.Verifier)(nil)
)

// wireWorkflowAuth is the un-memoized construction (tested directly).
func wireWorkflowAuth(pool *db.Pool) wfauth.EnvelopeVerifier {
	ref := strings.TrimSpace(os.Getenv("TG_WORKFLOW_AUTH_KEY_REF"))
	if ref == "" {
		log.Print("workflow auth: NO KEY (TG_WORKFLOW_AUTH_KEY_REF unset) — every admin-write workflow and every vote is " +
			"REFUSED as unsigned (TG-660, fail closed); set the ref to the same bao: secret the grounder signs with")
		return (*wfauth.Verifier)(nil)
	}
	if pool == nil {
		log.Fatalf("workflow auth: TG_WORKFLOW_AUTH_KEY_REF is set but there is no database pool for the replay guard " +
			"(workflow_auth_nonce) — refusing to boot rather than verify without replay protection (TG-660)")
	}
	secret, err := config.SecretRef(ref).Resolve()
	if err != nil {
		log.Fatalf("workflow auth: TG_WORKFLOW_AUTH_KEY_REF is set but does not resolve (%v) — refusing to boot rather than "+
			"run with every admin write refused by misconfiguration (TG-660)", err)
	}
	key, err := wfauth.ParseKey(secret)
	if err != nil {
		log.Fatalf("workflow auth: %v — refusing to boot (TG-660)", err)
	}
	v, err := wfauth.NewVerifier(key, db.NewWorkflowAuthNonceStore(pool))
	if err != nil {
		log.Fatalf("workflow auth: %v (TG-660)", err)
	}
	log.Printf("workflow auth: verifying every admin-write start and vote against key %s; replay guard = workflow_auth_nonce (TG-660)", v.KeyID())
	return v
}

// approvalReader / riskAuditReader are the DURABLE authority records ExecuteActivity re-derives Approved and
// the band from (TG-660 part c, pack item 2): the `human:approve` / `fallback:autonomous-approve` ledger row and
// the classifier's session_risk_audit row, read back from the same tables the triage plane wrote them to (both
// planes may read them). nil pool ⇒ nil readers ⇒ every POLL_PAUSE execution is refused (fail closed) — an
// untyped nil, never a typed-nil-in-interface (the poll_queue.go trap).
func approvalReader(pool *db.Pool) runner.ApprovalReader {
	if pool == nil {
		return nil
	}
	return db.NewLedgerStore(pool)
}

func riskAuditReader(pool *db.Pool) runner.RiskAuditReader {
	if pool == nil {
		return nil
	}
	return db.NewRiskAuditStore(pool)
}

// planStepReader binds a transaction plan's one approval (ledgered against plan:<id>) to the actions the plan
// workflow durably listed in transaction_plan_step (TG-660 part c): a plan id in ExecuteInput.ApprovalActionID
// authorizes an action only when that table names it. nil pool ⇒ nil ⇒ every plan-bound execution is refused.
func planStepReader(pool *db.Pool) runner.PlanStepReader {
	if pool == nil {
		return nil
	}
	return db.NewTransactionPlanStore(pool)
}
