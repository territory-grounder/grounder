package db

// plane_roles.go — WHICH TABLES EACH CREDENTIAL PLANE MAY WRITE (TG-164, follow-on to TG-153).
//
// THE MEASURED GAP. TG-153 gave the worker two processes under two OpenBao AppRoles, and OpenBao refuses
// each plane the other's secrets (403/200, both directions, verified on the live box 2026-08-04). Both
// processes then connected to Postgres as the SAME role:
//
//	postgres roles: postgres(super) | tg_migration | tg_runtime
//	worker          -> tg_runtime
//	worker-actuate  -> tg_runtime
//
// The process split therefore bounded what a popped TRIAGE worker could FETCH and nothing about what it
// could WRITE. The triage worker is the one that reads untrusted alert bodies, device syslog, host command
// output and ticket text — the July-2026 HuggingFace intrusion chain in one sentence — and it held the
// authority to append action_verdict, action_execution, interceptor_gate_verdict and policy_decision: to
// manufacture the RECORD of an actuation it could not perform, and to poison state this codebase reads back
// (PriorVerdictsActivity reads action_verdict; the console's audit and the incident history read the rest).
//
// ---------------------------------------------------------------------------------------------------------
// HOW THE TWO LISTS BELOW WERE DERIVED — BY TRACING THE COMPOSITION ROOT, NOT BY READING TABLE NAMES.
//
// Only ONE runner activity is registered on the actuation queue: temporal/runner/register.go
// RegisterActuationActivities registers ExecuteActivity and nothing else. Every other activity — Investigate,
// Reconcile, Graduation, RecordVote, Verify, ObserveCleared, the triage record/mark path — is registered on
// tg.runner, which the actuation plane does not poll. So:
//
//   - a table written only from ExecuteActivity's chain (core/actuate.Interceptor and the regime lane it
//     dispatches through) is written only by the ACTUATION plane;
//   - a table written from any other activity is written by the TRIAGE plane;
//   - a table written from a boot-path goroutine in cmd/worker/main.go is written by BOTH planes, because
//     main() runs in full in both processes and only the plane-scoped ENV keys (credential_plane.go) gate
//     what it constructs.
//
// That third case is why neither list is longer. runtime_posture, estate_snapshot, governance_ledger,
// session_risk_audit, mutation_breaker_state, control_plane_config, module_capability_projection,
// credential_binding_projection, credential_coverage, credential_sync_run, credential_resolution,
// knowledge_embedding, cost_accrual and cost_breaker_state are all written by unconditional boot goroutines
// or by the shared ledger, in BOTH processes. Withholding any of them would take the deployment down.
//
// SAID PLAINLY, BECAUSE IT IS THE PART A REVIEWER WOULD OTHERWISE ASSUME THIS FIXED: the governance ledger
// and the graduation ladder are NOT withheld from triage. The triage plane appends to governance_ledger on
// every vote and every governed decision (RecordVoteActivity), and it writes policy_graduation +
// graduation_credit from GraduationActivity/ReconcileActivity — the ladder's earn path runs at session
// terminus, on the triage queue. Those are load-bearing today, so both planes keep them; the ledger's
// tamper-EVIDENCE (hash chain, plus 0015's REVOKE of UPDATE/DELETE from the runtime role, inherited here) is
// what bounds a popped triage worker there. Moving the ladder write onto the actuation plane is a real
// follow-on and is filed as one — it is a behaviour change to the earn loop, not a grant change.
//
// ---------------------------------------------------------------------------------------------------------
// WHAT THIS BUYS, STATED AS THE ATTACK IT STOPS. A compromised triage worker can still propose, still seal a
// manifest, still append to the ledger. It CANNOT write the row that says an action executed, the row that
// says a gate passed, or the row that says policy authorised it. The sealed manifest it can write is
// content-addressed and re-asserted against the action id at execute time (ExecuteActivity → m.Assert), and
// every gate re-runs in the actuation process, so a forged manifest buys no execution — it buys a refused
// one. The verdict/execution/gate/policy record is the part that could previously be fabricated with no
// counter-signature at all.

import (
	"context"
	"errors"
	"fmt"
	"sort"

	"github.com/jackc/pgx/v5/pgxpool"
)

// The three roles this file reasons about. tg_runtime is the SOURCE: the un-split, already-reviewed posture
// that every plane role's privileges are derived from and can never exceed.
const (
	PlaneRoleTriage  = "tg_triage"
	PlaneRoleActuate = "tg_actuate"
	PlaneRoleRuntime = "tg_runtime"
)

// ActuationAuthorityTables are the tables that RECORD or AUTHORISE an actuation. The triage plane may read
// them and may not write them.
//
// Each entry names the single writer that put it here, so the next person can re-check the trace rather than
// trusting this comment:
//
//	action_verdict           core/actuate.Interceptor via VerdictSink      (INV-10: the verifier is sole writer)
//	action_execution         core/actuate.Interceptor via ExecutionSink    (one row per execution, P2-1)
//	action_prestate          core/actuate.Interceptor via PreStateSink     (TG-58 pre-mutation snapshot, latest-wins)
//	actuation_target_state   core/actuate.Interceptor via TargetAdmission  (TG-81 b2 gate 4h2 claim + cooldown; a
//	                         triage worker that could write it could park targets — denial-of-remediation)
//	transaction_plan(+_step) db.TransactionPlanStore via the spec/030 plan workflow — plan lifecycle is
//	                         actuation authority: a triage writer could mark a plan approved or a step
//	                         compensated, forging the all-or-nothing story
//	interceptor_gate_verdict core/actuate.Interceptor via GateVerdictSink  (spec/020 T-020-7 per-gate trail)
//	policy_decision          policy.AuditedEngine, wired ONLY as the interceptor's authorizer (bPolicyDecider).
//	                         The vote-admission path deliberately uses the UNAUDITED engine, so nothing on the
//	                         triage queue writes this table today.
//	regime_resolution        core/regime.Audit — the lane resolution behind a dispatch (spec/017 REQ-1715)
//	regime_actuation         core/regime.Audit — the launch record
//	deferred_verdict         core/regime.Audit — the async lane's post-hoc verdict
//	pending_verification     core/regime.AsyncVerify — the launch's idempotency claim (0022)
//
// The last four are reachable only when the awx-job launch client exists, which needs
// TG_AWXJOB_LAUNCH_TOKEN_REF — an actuation-plane key credential_plane.go already withholds from triage. The
// grant makes that structural rather than incidental.
var ActuationAuthorityTables = []string{
	"action_execution",
	"action_prestate",
	"action_verdict",
	"actuation_target_state",
	"deferred_verdict",
	"interceptor_gate_verdict",
	"pending_verification",
	"policy_decision",
	"regime_actuation",
	"regime_resolution",
}

// TriageContentTables are the tables that hold ATTACKER-AUTHORED or triage-authored content. The actuation
// plane may read them and may not write them.
//
// This is the direction people forget, and it is the same argument credential_plane.go makes for withholding
// untrusted-content READERS from the actuation process: the evidence gate binds a proposal's cited
// tool-result ids to captured observations, so a process that can FABRICATE those observations can ground a
// mutation in evidence that never happened. The actuation worker must not be able to write its own grounds.
//
//	agent_step           InvestigateActivity via Deps.AgentSteps (tg.runner only)
//	agent_step_evidence  InvestigateActivity via Deps.AgentStepEvidence (tg.runner only). The reaper loop DOES
//	                     run in both processes — it deletes through the 0055 SECURITY DEFINER function, which
//	                     needs EXECUTE, not DELETE, and the mirror carries that EXECUTE across.
//	ingest_alert         the PVE-liveness poller's accepted-envelope record — gated on
//	                     TG_PVE_LIVENESS_POLL_INTERVAL, a triage-plane key, so it never starts here anyway.
//	ingest_alert_occurrence  the per-delivery re-fire log written by the SAME front-door Append as ingest_alert
//	                     (TG-399) — triage-authored content the actuation plane reads but must not write.
//	session_triage       Deps.TriageRecord / TriageMarkCleared / TriageMarkMutated (tg.runner only)
//	conversation_turn    RecordTriageActivity via Deps.ConversationAppend (TG-80 P2-8) — the per-lineage
//	                     terminal digests the next session's seed folds in; TTL-reaped through the 0109
//	                     SECURITY DEFINER function (the runtime role holds no DELETE).
var TriageContentTables = []string{
	"agent_step",
	"agent_step_evidence",
	"conversation_turn",
	"ingest_alert",
	"ingest_alert_occurrence",
	"session_triage",
}

// withheldFor returns the tables a plane role may not write. An unknown role gets nil — it is not this
// function's job to invent a policy for a role nobody declared.
func withheldFor(role string) []string {
	switch role {
	case PlaneRoleTriage:
		return ActuationAuthorityTables
	case PlaneRoleActuate:
		return TriageContentTables
	}
	return nil
}

// PlaneGrantReport is what one ApplyPlaneGrants run did, shaped for a boot log that can be believed.
//
// Absent is the interesting field. "Applied grants to 0 roles" and "granted 214 privileges" read the same in
// a log if you do not print WHICH roles were found, and this repository has paid repeatedly for boot lines
// that reported a control as armed over an empty set.
type PlaneGrantReport struct {
	Granted map[string]int // role → table privileges granted
	Absent  []string       // declared plane roles this database does not have (the opt-out path)
}

// Applied reports whether any plane role actually exists and was granted. False means the deployment is
// un-split at the database and is running exactly as it did before TG-164.
func (r PlaneGrantReport) Applied() bool { return len(r.Granted) > 0 }

// String renders the boot line: which roles were derived, how many privileges each holds, which are absent.
func (r PlaneGrantReport) String() string {
	if len(r.Granted) == 0 {
		return fmt.Sprintf("no plane role present (%v) — the database plane split is NOT in force; both worker "+
			"planes connect as %s and hold identical authority, exactly as before TG-164", r.Absent, PlaneRoleRuntime)
	}
	roles := make([]string, 0, len(r.Granted))
	for role := range r.Granted {
		roles = append(roles, role)
	}
	sort.Strings(roles)
	out := ""
	for _, role := range roles {
		out += fmt.Sprintf("%s=%d privileges (withholding writes on %d off-plane table(s)) ", role, r.Granted[role], len(withheldFor(role)))
	}
	if len(r.Absent) > 0 {
		out += fmt.Sprintf("| ABSENT: %v (that plane still connects as %s if deployed)", r.Absent, PlaneRoleRuntime)
	}
	return out
}

// ErrNoSourcePrivileges is the vacuity refusal: tg_runtime holds no write privilege anywhere, so mirroring it
// would produce plane roles that can read and nothing else — two roles, a green boot line, and a worker that
// dies at its first append. A privilege model derived from an empty set is not a tight privilege model.
var ErrNoSourcePrivileges = errors.New("db: plane grants: tg_runtime holds no write privilege on any table in schema public — nothing to mirror")

// ApplyPlaneGrants derives tg_triage's and tg_actuate's privileges from tg_runtime's, withholding each
// plane's off-plane writes, and returns what it did. It MUST be given the MIGRATION (DDL/owner) DSN: only the
// table owner may grant.
//
// It is idempotent, convergent, and safe to run on every boot — which is the point. The plane roles are
// created by the operator (with passwords, so outside the migration lattice) and that can happen long after
// migration 0059 applied; a one-shot grant would have missed them forever. Running here also picks up tables
// created by later migrations, which is the difference between this control staying true and it decaying into
// a comment.
//
// A database with NEITHER plane role is the default and returns a no-op report with no error: TG-164 is
// opt-in at the database exactly as TG-153 is opt-in at the process.
//
// EVERYTHING RUNS IN ONE TRANSACTION. A half-applied privilege set is worse than none: the worker boots,
// serves, and fails with a permission error deep inside an activity hours later. Either the whole derivation
// commits or the deployment keeps the posture it already had.
func ApplyPlaneGrants(ctx context.Context, migrationDSN string) (PlaneGrantReport, error) {
	rep := PlaneGrantReport{Granted: map[string]int{}}
	p, err := pgxpool.New(ctx, migrationDSN)
	if err != nil {
		return rep, fmt.Errorf("db: plane grants: connect: %w", err)
	}
	defer p.Close()

	conn, err := p.Acquire(ctx)
	if err != nil {
		return rep, fmt.Errorf("db: plane grants: acquire: %w", err)
	}
	defer conn.Release()

	// The same advisory lock the migrator takes: two grounder replicas booting together must not interleave
	// REVOKE/GRANT pairs on the same role, which would leave whichever lost the race holding a torn set.
	if _, err := conn.Exec(ctx, "SELECT pg_advisory_lock($1)", advisoryLockKey); err != nil {
		return rep, fmt.Errorf("db: plane grants: advisory lock: %w", err)
	}
	defer conn.Exec(ctx, "SELECT pg_advisory_unlock($1)", advisoryLockKey)

	// Which of the declared plane roles exist? Nothing else in this function may assume.
	present := map[string]bool{}
	for _, role := range []string{PlaneRoleTriage, PlaneRoleActuate} {
		var ok bool
		if err := conn.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM pg_roles WHERE rolname=$1)", role).Scan(&ok); err != nil {
			return rep, fmt.Errorf("db: plane grants: probe role %s: %w", role, err)
		}
		if ok {
			present[role] = true
			continue
		}
		rep.Absent = append(rep.Absent, role)
	}
	if len(present) == 0 {
		return rep, nil // opt-out: nothing declared, nothing changed
	}

	// THE VACUITY FLOOR, checked BEFORE anything is granted. In a database whose tables were not created by
	// tg_migration (a hand-restored dump, a fixture), tg_runtime holds nothing — and mirroring nothing would
	// hand both planes a read-only role while this function reported success.
	// Probed by OID, not by a name run through ::regclass. Postgres does not promise to evaluate the
	// schemaname filter before the privilege call, so the name form resolves `public.<catalog table>` for
	// pg_catalog rows and dies with 42P01 on a database that is otherwise perfectly healthy.
	var writable int
	if err := conn.QueryRow(ctx, `
		SELECT count(*)
		FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE n.nspname = 'public' AND c.relkind IN ('r', 'p')
		  AND has_table_privilege($1, c.oid, 'INSERT')`,
		PlaneRoleRuntime).Scan(&writable); err != nil {
		return rep, fmt.Errorf("db: plane grants: measure %s: %w", PlaneRoleRuntime, err)
	}
	if writable == 0 {
		return rep, ErrNoSourcePrivileges
	}

	tx, err := conn.Begin(ctx)
	if err != nil {
		return rep, fmt.Errorf("db: plane grants: begin: %w", err)
	}
	defer tx.Rollback(ctx)

	for _, role := range []string{PlaneRoleTriage, PlaneRoleActuate} {
		if !present[role] {
			continue
		}
		var granted int
		if err := tx.QueryRow(ctx, "SELECT tg_apply_plane_grants($1, $2, $3)",
			role, PlaneRoleRuntime, withheldFor(role)).Scan(&granted); err != nil {
			return PlaneGrantReport{Granted: map[string]int{}, Absent: rep.Absent},
				fmt.Errorf("db: plane grants: derive %s from %s: %w", role, PlaneRoleRuntime, err)
		}
		if granted <= 0 {
			// -1 means the function found no such role, which contradicts the probe above (a role dropped
			// between the two statements). Refuse rather than report a grant count nobody produced.
			return PlaneGrantReport{Granted: map[string]int{}, Absent: rep.Absent},
				fmt.Errorf("db: plane grants: %s granted %d privileges — refusing to commit a role that can read and not write", role, granted)
		}
		rep.Granted[role] = granted
	}
	if err := tx.Commit(ctx); err != nil {
		return PlaneGrantReport{Granted: map[string]int{}, Absent: rep.Absent}, fmt.Errorf("db: plane grants: commit: %w", err)
	}
	return rep, nil
}

// PlaneWriteAudit is the BOOT SELF-CHECK a split worker prints about its OWN connection: the role it actually
// authenticated as, and whether that role can write the tables its plane must not.
//
// It exists because the DSN is the weakest link in this design. Everything else here is enforced by Postgres;
// which DSN a process was handed is enforced by an operator editing a .env, and the failure mode of getting
// it wrong — worker-actuate pointed at tg_triage, or a "split" triage worker still pointed at tg_runtime — is
// invisible until an activity fails. TG-153's own boot report exists for exactly this reason: a co-holding
// worker that printed "plane split OK" is how the credential gap survived from TG-157 to TG-153.
type PlaneWriteAudit struct {
	Role     string   // the role this connection authenticated as (current_user)
	Writable []string // off-plane tables this role can STILL write — empty is the goal, non-empty is the finding
	Checked  []string // the tables that were examined (so an empty Writable can never be an empty check)
}

// Split reports whether the connected role is actually denied its off-plane writes.
func (a PlaneWriteAudit) Split() bool { return len(a.Checked) > 0 && len(a.Writable) == 0 }

// AuditPlaneWrites asks the database — not the configuration — what this pool's role may write off-plane.
// withheld names the tables the caller's plane must not write (ActuationAuthorityTables on the triage plane,
// TriageContentTables on the actuation plane). An empty withheld list yields an empty audit, and Split() is
// false for it: "checked nothing, found nothing" must never read as "verified".
func (p *Pool) AuditPlaneWrites(ctx context.Context, withheld []string) (PlaneWriteAudit, error) {
	var a PlaneWriteAudit
	if p == nil || len(withheld) == 0 {
		return a, nil
	}
	if err := p.QueryRow(ctx, "SELECT current_user").Scan(&a.Role); err != nil {
		return a, fmt.Errorf("db: plane audit: current_user: %w", err)
	}
	rows, err := p.Query(ctx, `
		SELECT c.relname,
		       has_table_privilege(c.oid, 'INSERT')
		         OR has_table_privilege(c.oid, 'UPDATE')
		         OR has_table_privilege(c.oid, 'DELETE')
		FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE n.nspname = 'public' AND c.relkind IN ('r', 'p') AND c.relname = ANY ($1)
		ORDER BY c.relname`, withheld)
	if err != nil {
		return a, fmt.Errorf("db: plane audit: probe: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var tbl string
		var canWrite bool
		if err := rows.Scan(&tbl, &canWrite); err != nil {
			return a, fmt.Errorf("db: plane audit: scan: %w", err)
		}
		a.Checked = append(a.Checked, tbl)
		if canWrite {
			a.Writable = append(a.Writable, tbl)
		}
	}
	return a, rows.Err()
}
