package main

import (
	"context"
	"log"
	"time"

	"go.temporal.io/sdk/client"

	"github.com/territory-grounder/grounder/core/audit"
	"github.com/territory-grounder/grounder/core/db"
	tg "github.com/territory-grounder/grounder/temporal"
	"github.com/territory-grounder/grounder/temporal/ledgeranchor"
)

// wireLedgerAnchor arms the ledger-head anchor tamper-evidence pair, carved out of main()'s composition root
// (TG-501 LOC-debt paydown): the RECORDING half (TG-80 P1#1) and the CONSUMING half (TG-509) — see the
// comments below for each. Both are no-ops without a database pool. Behaviour is unchanged by the move.
func wireLedgerAnchor(dbPool *db.Pool) {
	// THE LEDGER-HEAD ANCHOR (TG-80 P1#1): periodically WITNESS the append-only governance_ledger HEAD to an
	// append-only store the recording principal cannot rewrite (tg_runtime holds INSERT+SELECT but no
	// UPDATE/DELETE on ledger_anchor, migration 0092 — the same REVOKE as the spine). VerifyChain proves the
	// chain is internally consistent but CANNOT see a truncated tail or a fully re-linked history (a shortened
	// prefix still verifies); an anchor recorded at T1 fixes the HEAD at T1, so a later rollback of anything
	// appended before T1 is detectable as a HEAD that regressed below a witness (core/audit.VerifyAgainstAnchors).
	// Armed only with a DSN; OBSERVE-ONLY — a read/write error logs and the loop continues, it never actuates
	// and never crashes the worker. The immediate first pass witnesses the HEAD at boot (a deploy is when a
	// fresh, independent witness is worth most).
	if iv := getenv("TG_LEDGER_ANCHOR_INTERVAL", "1h"); iv != "" && dbPool != nil {
		if d, derr := time.ParseDuration(iv); derr == nil && d > 0 {
			anchorJob := ledgeranchor.Job{
				Head:   db.NewLedgerStore(dbPool),
				Sink:   db.NewAnchorStore(dbPool).Scoped(audit.DomainGovernanceLedger),
				Window: envInt("TG_LEDGER_ANCHOR_WINDOW", audit.DefaultAnchorWindow),
				Now:    func() time.Time { return time.Now().UTC() },
				Emit: func(a audit.Anchor) {
					log.Printf("ledger anchor: witnessed governance_ledger HEAD seq=%d hash=%s digest=%s (window %d) — external tamper-evidence for the audit spine (TG-80)",
						a.Seq, a.Hash[:12], a.Digest[:12], a.WindowSize)
				},
			}
			go ledgeranchor.RunPeriodically(context.Background(), anchorJob, d, func(cerr error) {
				log.Printf("ledger anchor: pass failed: %v (retry next tick)", cerr)
			})
			log.Printf("ledger anchor: witnessing the governance_ledger HEAD every %s into the append-only ledger_anchor store — external tamper-evidence (TG-80 P1#1)", d)
		} else if derr != nil {
			log.Printf("ledger anchor: invalid TG_LEDGER_ANCHOR_INTERVAL %q — HEAD anchoring disabled", iv)
		}
	}
	// TG-509: the CONSUMING half of the anchor mechanism — periodically CHECK the live governance ledger against
	// the recorded witnesses (core/audit.VerifyAgainstAnchors, which the recorder above only ever NAMED). Until
	// this, the witnesses were recorded but never verified, so a HEAD that regressed below a witness (a tamper or
	// rollback of anything appended before it) went undetected — recording without checking. Coarse cadence:
	// full-chain verification is O(chain × anchors), so it runs hourly/daily, not the recorder's minute cadence.
	// OBSERVE-ONLY — it surfaces a detected tamper and does NOT act (the operator response is a separate decision).
	if iv := getenv("TG_LEDGER_VERIFY_INTERVAL", "24h"); iv != "" && dbPool != nil {
		if d, derr := time.ParseDuration(iv); derr == nil && d > 0 {
			verifyJob := ledgeranchor.VerifyJob{
				Ledger:  db.NewLedgerStore(dbPool),
				Anchors: db.NewAnchorStore(dbPool).Scoped(audit.DomainGovernanceLedger),
			}
			go ledgeranchor.RunVerifyPeriodically(context.Background(), verifyJob, d,
				func(terr error) {
					log.Printf("!!! GOVERNANCE LEDGER TAMPER DETECTED: the live governance_ledger contradicts a witness recorded before it could have been tampered: %v — the audit spine can no longer be trusted; investigate (TG-509, core/audit.VerifyAgainstAnchors)", terr)
				},
				func(cerr error) {
					log.Printf("ledger verify: pass could not run: %v (retry next tick) — an unverified spine is not a clean one", cerr)
				})
			log.Printf("ledger verify: checking the governance_ledger against its recorded witnesses every %s (core/audit.VerifyAgainstAnchors) — the consuming half of the anchor loop (TG-509 present-not-reaching fix)", d)
		} else if derr != nil {
			log.Printf("ledger verify: invalid TG_LEDGER_VERIFY_INTERVAL %q — anchor verification disabled", iv)
		}
	}
}

// wireLedgerAnchorTemporalWitness arms the TEMPORAL-NATIVE half of the ledger-HEAD anchor (TG-80 P1.1's
// literal ask, completing what wireLedgerAnchor's DB-only witness above deliberately deferred — see
// temporal/ledgeranchor/witness.go's package doc for the full threat model, revised after a fresh-eyes review
// caught an under-stated residual — read it before touching this). ADDITIVE alongside the DB-backed
// ledger_anchor witness, never instead of it: both run independently, on independent cadences, into
// independent stores, so a defect in either can never disable the other.
//
// WHAT THIS CLOSES, PRECISELY (do not overclaim beyond this): ledger_anchor (migration 0092) lives in the
// SAME Postgres instance as governance_ledger, so an actor who can reverse ONE REVOKE (0015.down, a
// mis-scoped retention job, a compromised superuser) can reverse the OTHER identically — one escalation
// defeats both tables for ANY row, past or present. A Temporal-witnessed anchor lives in Temporal's OWN
// persistence (temporal-postgres, its own database + TEMPORAL_PG_PASSWORD credential —
// deploy/docker-compose.yml) that tg_runtime is never handed, and no exposed RPC can EDIT an ALREADY-RECORDED
// historical event — only append or fork a new run. That closes rewriting of PAST, already-witnessed rows: a
// Postgres-only compromise can no longer rewrite yesterday's governance decisions undetected.
//
// WHAT IT DOES NOT CLOSE: TG_TEMPORAL_HOSTPORT is dialed with no TLS/API key (below), so anything with
// tg_runtime's network reach can also PRE-REGISTER a forged witness for a seq that has not been witnessed
// yet, timed to land before this recorder's next tick — an attacker who controls BOTH the DB and that network
// path can make the periodic recorder itself witness the very lie it just planted, and the cross-check finds
// nothing to contradict. TemporalWitness.Record now detects a MISMATCHED squat (closing the sloppy/
// uncoordinated version of this), and TemporalVerifyJob still fails safe on an ABSENT witness rather than
// reading absence as clean — but a fully-coordinated, correctly-timed squat of a FRESH row is a real,
// documented residual, not a closed one. See temporal/ledgeranchor/witness.go's package doc ("THE SQUAT
// RESIDUAL") for the full analysis and what would actually close it (Temporal frontend auth, or synchronous
// witnessing — both owner-level architectural calls, neither built here).
//
// Gated on dbPool + a live Temporal client — nothing else. Registration happens on w, the SAME tg.runner
// worker runner.RegisterActivities/armGovernanceSchedules already use (TG-153): on the actuation-only
// credential plane w is the no-op stub (see planeWorker/offPlaneWorker in credential_plane.go), so
// registration is harmless there and this NEVER gives an actuation-only process a new reason to poll
// tg.runner. Submission (ExecuteWorkflow) uses the Temporal client c directly and is plane-agnostic — exactly
// like wireLedgerAnchor's existing DB recorder above — because starting a workflow from a process that does
// not itself execute it is ordinary, safe Temporal usage: whichever process's worker actually polls
// tg.runner (always at least one, in any deployment where triage runs at all) picks it up.
//
// OBSERVE-ONLY, fail-soft exactly like wireLedgerAnchor: a Temporal hiccup logs and the loop retries next
// tick: it never crashes the worker, never actuates, and never alters governance_ledger's own write path.
func wireLedgerAnchorTemporalWitness(c client.Client, w workflowRegistrar, dbPool *db.Pool) {
	if dbPool == nil || c == nil {
		return
	}
	w.RegisterWorkflow(ledgeranchor.WitnessAnchorWorkflow)
	w.RegisterActivity(ledgeranchor.WitnessAnchorActivity)

	witness := &ledgeranchor.TemporalWitness{Client: c, TaskQueue: tg.TaskQueueRunner, Domain: audit.DomainGovernanceLedger}

	// The RECORDING half. Deliberately reuses TG_LEDGER_ANCHOR_INTERVAL/TG_LEDGER_ANCHOR_WINDOW (not new
	// knobs): this is the SAME HEAD being witnessed a second time, into a second store — one cadence for one
	// concept, not two operator-facing intervals to keep in sync.
	if iv := getenv("TG_LEDGER_ANCHOR_INTERVAL", "1h"); iv != "" {
		if d, derr := time.ParseDuration(iv); derr == nil && d > 0 {
			recordJob := ledgeranchor.Job{
				Head:   db.NewLedgerStore(dbPool),
				Sink:   witness,
				Window: envInt("TG_LEDGER_ANCHOR_WINDOW", audit.DefaultAnchorWindow),
				Now:    func() time.Time { return time.Now().UTC() },
				Emit: func(a audit.Anchor) {
					log.Printf("ledger anchor (temporal): witnessed governance_ledger HEAD seq=%d hash=%s into Temporal event history — a credential domain tg_runtime's own Postgres role cannot reach (TG-80 P1.1)",
						a.Seq, a.Hash[:12])
				},
			}
			go ledgeranchor.RunPeriodically(context.Background(), recordJob, d, func(cerr error) {
				log.Printf("ledger anchor (temporal): pass failed: %v (retry next tick)", cerr)
			})
			log.Printf("ledger anchor (temporal): witnessing the governance_ledger HEAD every %s into Temporal event history, alongside the DB-backed ledger_anchor witness (TG-80 P1.1)", d)
		} else if derr != nil {
			log.Printf("ledger anchor (temporal): invalid TG_LEDGER_ANCHOR_INTERVAL %q — Temporal HEAD witnessing disabled", iv)
		}
	}

	// The CONSUMING half: cross-check the DB-recorded anchor history against Temporal's independently-stored
	// witnesses and the live chain. Reuses TG_LEDGER_VERIFY_INTERVAL for the same reason as above.
	if iv := getenv("TG_LEDGER_VERIFY_INTERVAL", "24h"); iv != "" {
		if d, derr := time.ParseDuration(iv); derr == nil && d > 0 {
			verifyJob := ledgeranchor.TemporalVerifyJob{
				Anchors:  db.NewAnchorStore(dbPool).Scoped(audit.DomainGovernanceLedger),
				Ledger:   db.NewLedgerStore(dbPool),
				Temporal: witness,
				Domain:   audit.DomainGovernanceLedger,
			}
			go ledgeranchor.RunTemporalVerifyPeriodically(context.Background(), verifyJob, d,
				func(terr error) {
					log.Printf("!!! GOVERNANCE LEDGER TAMPER DETECTED (Temporal cross-check): the DB-recorded anchor or the live governance_ledger disagrees with an independently-stored Temporal witness: %v — this catches a rewrite of ALREADY-witnessed history even under a compromise that touched both governance_ledger and ledger_anchor consistently; it does NOT by itself rule out a coordinated forgery of a not-yet-witnessed row squatted before this cycle (see temporal/ledgeranchor/witness.go's THE SQUAT RESIDUAL) — investigate either way (TG-80 P1.1)", terr)
				},
				func(cerr error) {
					log.Printf("ledger verify (temporal): pass could not run: %v (retry next tick) — an unverified spine is not a clean one", cerr)
				})
			log.Printf("ledger verify (temporal): cross-checking the DB-recorded anchor history against Temporal's independently-stored witnesses every %s (TG-80 P1.1)", d)
		} else if derr != nil {
			log.Printf("ledger verify (temporal): invalid TG_LEDGER_VERIFY_INTERVAL %q — Temporal cross-check disabled", iv)
		}
	}
}
