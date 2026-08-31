package main

import (
	"context"
	"log"
	"time"

	"github.com/territory-grounder/grounder/core/db"
)

// wireEvidenceReap arms the agent-step evidence retention sweep (TG-295), carved out of main()'s
// composition root (TG-501 LOC-debt paydown). See the comments below for the full rationale. Behaviour is
// unchanged by the move.
func wireEvidenceReap(evidenceStore *db.AgentStepEvidenceStore) {
	// REAP EXPIRED EVIDENCE (TG-295) — the retention bound for the corpus the line above starts filling.
	//
	// 0053 gave untrusted host output a durable write primitive and, with `REVOKE UPDATE, DELETE FROM
	// tg_runtime`, no erasure path for anyone. Append-only was right; permanent was not. Verbatim tool
	// output is the PURGEABLE operational body (docs/DATA-MODEL.md §5.2, INV-14), not the audit spine
	// (§5.1) — it is raw content from a host, screened but not sealed, and it can hold whatever the host
	// held. Without this sweep the table has one behaviour: grow, forever, on every session.
	//
	// Deletion runs through reap_agent_step_evidence, the SECURITY DEFINER function migration 0055 makes
	// the ONE privileged path: it can only remove rows older than a cutoff (never a named row), it refuses
	// a cutoff inside the last 24h, and it journals every purge to agent_step_evidence_reap in the same
	// transaction — a table this role cannot insert into, so a deletion cannot happen unrecorded.
	//
	// Same shape as the abandoned-decision sweep below (bounded context per tick, non-blocking error, work
	// first then wait): a hung DB must not wedge the goroutine, and a failed sweep must never take down a
	// worker whose job is to run incidents. The batch cap means a shortened retention drains over several
	// ticks instead of holding locks on the table the agent is still writing to.
	evidenceRetention := db.ClampEvidenceRetention(envDuration("TG_EVIDENCE_RETENTION", db.DefaultEvidenceRetention))
	evidenceReapEvery := envDuration("TG_EVIDENCE_REAP_INTERVAL", 6*time.Hour)
	go func() {
		t := time.NewTicker(evidenceReapEvery)
		defer t.Stop()
		for {
			sctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
			n, err := evidenceStore.ReapEvidenceOlderThan(sctx, time.Now().UTC().Add(-evidenceRetention), db.DefaultEvidenceReapBatch)
			cancel()
			if err != nil {
				log.Printf("agent-step evidence: retention sweep failed (non-blocking) — the corpus is UNBOUNDED until this succeeds: %v", err)
			} else if n > 0 {
				log.Printf("agent-step evidence: reaped %d row(s) older than %s; the purge is journalled in agent_step_evidence_reap", n, evidenceRetention)
			}
			<-t.C
		}
	}()
	log.Printf("agent-step evidence: retention %s (TG_EVIDENCE_RETENTION, floor %s), sweep every %s, at most %d row(s) per sweep",
		evidenceRetention, db.EvidenceRetentionFloor, evidenceReapEvery, db.DefaultEvidenceReapBatch)
}
