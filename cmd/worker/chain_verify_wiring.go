package main

import (
	"context"
	"log"
	"sync/atomic"
	"time"

	"github.com/territory-grounder/grounder/core/db"
	"github.com/territory-grounder/grounder/core/metrics"
	"github.com/territory-grounder/grounder/temporal/ledgeranchor"
)

// THE SCHEDULED FULL RE-WALK (TG-674): core/audit.VerifyChain over the whole governance_ledger, every
// TG_LEDGER_CHAIN_VERIFY_INTERVAL and once at boot. The anchor verifiers (wireLedgerAnchor) compare STORED
// (seq, hash) pairs against witnesses; only this recomputes every row's content hash, so an edited decision
// whose stored hash was left alone is caught here and nowhere else. OBSERVE-ONLY, like every arm beside it.

type chainVerifyState struct {
	rows    int
	lastRun time.Time
	broken  bool
	readErr bool
}

var chainVerify atomic.Pointer[chainVerifyState]

// chainVerifySamples publishes the latest pass. Emitted only once a pass has completed — the absence rule
// (GovernanceLedgerChainUnverified) owns "has not run", so a 0 here can never be read as "never checked".
func chainVerifySamples() []metrics.Sample {
	st := chainVerify.Load()
	if st == nil {
		return nil
	}
	b2f := func(b bool) float64 {
		if b {
			return 1
		}
		return 0
	}
	out := []metrics.Sample{
		{Name: "tg_ledger_chain_verify_read_error", Kind: metrics.Gauge,
			Help:  "1 when the latest attempt to re-walk governance_ledger could not READ the chain (or found it empty); 0 after a completed pass. A read error is unverified, never broken.",
			Value: b2f(st.readErr)},
	}
	if st.lastRun.IsZero() {
		return out // no COMPLETED pass yet — the broken/rows/last_run gauges would be fabricated
	}
	return append(out,
		metrics.Sample{Name: "tg_ledger_chain_verify_broken", Kind: metrics.Gauge,
			Help:  "1 when the latest COMPLETED full re-walk of governance_ledger (core/audit.VerifyChain: every row's content hash recomputed, prev-hash linkage and seq monotonicity checked) found a row that no longer matches; 0 when the chain re-hashed intact (TG-674). Only a completed pass sets it — a read error is tg_ledger_chain_verify_read_error.",
			Value: b2f(st.broken)},
		metrics.Sample{Name: "tg_ledger_chain_verify_rows", Kind: metrics.Gauge,
			Help:  "rows re-walked by the latest completed full chain verification.",
			Value: float64(st.rows)},
		metrics.Sample{Name: "tg_ledger_chain_verify_last_run_timestamp_seconds", Kind: metrics.Gauge,
			Help:  "when the latest COMPLETED full chain verification finished (unix seconds). Read against TG_LEDGER_CHAIN_VERIFY_INTERVAL: a stale value is a verifier that stopped or cannot read.",
			Value: float64(st.lastRun.Unix())},
	)
}

// bootIntegritySamples is the one reader the metrics builder chains for both TG-674 halves.
func bootIntegritySamples() []metrics.Sample {
	return append(bootPostureSamples(), chainVerifySamples()...)
}

// wireChainVerify arms the periodic full re-walk. Nothing without a pool.
func wireChainVerify(dbPool *db.Pool) {
	if dbPool == nil {
		return
	}
	iv := getenv("TG_LEDGER_CHAIN_VERIFY_INTERVAL", "6h")
	d, derr := time.ParseDuration(iv)
	if derr != nil || d <= 0 {
		log.Printf("ledger chain verify: invalid TG_LEDGER_CHAIN_VERIFY_INTERVAL %q — the scheduled full re-walk is DISABLED (the anchor comparisons still run)", iv)
		return
	}
	job := ledgeranchor.ChainVerifyJob{Ledger: db.NewLedgerStore(dbPool)}
	go ledgeranchor.RunChainVerifyPeriodically(context.Background(), job, d,
		func(res ledgeranchor.ChainVerifyResult) {
			chainVerify.Store(&chainVerifyState{broken: true, rows: res.Rows, lastRun: res.At})
			log.Printf("!!! GOVERNANCE LEDGER CHAIN BROKEN: the scheduled full re-walk recomputed %d rows and found %v — a row's content no longer matches its stored hash; the audit spine can no longer be trusted (TG-674, core/audit.VerifyChain)", res.Rows, res.Broken)
		},
		func(err error) {
			prev := chainVerify.Load()
			st := chainVerifyState{readErr: true}
			if prev != nil {
				st.rows, st.lastRun, st.broken = prev.rows, prev.lastRun, prev.broken
			}
			chainVerify.Store(&st)
			log.Printf("ledger chain verify: pass could not run: %v (retry next tick) — an unverified spine is not a clean one", err)
		},
		func(res ledgeranchor.ChainVerifyResult) {
			chainVerify.Store(&chainVerifyState{rows: res.Rows, lastRun: res.At})
			log.Printf("ledger chain verify: %d rows re-hashed clean (TG-674)", res.Rows)
		})
	log.Printf("ledger chain verify: re-walking the FULL governance_ledger with VerifyChain every %s and once now — the content-hash check the anchor comparisons cannot make (TG-674)", d)
}
