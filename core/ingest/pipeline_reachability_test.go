package ingest

import (
	"fmt"
	"testing"
	"time"
)

// THE PREMISE BEHIND "DO NOT WIRE Pipeline" IS AN ARITHMETIC ONE, SO IT IS PINNED AS ARITHMETIC.
//
// Pipeline has no production caller. That has been read as a wiring gap several times, and each time the
// remedy proposed was "give it a caller". The measurement says otherwise: every stage computes its window
// WITHIN THE BATCH, and the largest webhook this estate has ever delivered carries two alerts
// (prometheus-alertmanager, the only BatchIngester: 36 received-seconds with 1 alert, 6 with 2, measured
// 2026-07-29 over the live ingest_alert table). flapThreshold and burstThreshold are both 3.
//
// So flap and burst are not "inert pending config" — they are unreachable by arithmetic on live traffic.
// This test states that as a property rather than a comment, so the day it stops being true — a threshold
// lowered to 2, or a source that genuinely groups alerts — the decision is re-made on evidence instead of
// inherited from prose.
//
// What this test would still prove if the model behind it were wrong: if the live batch ceiling were
// actually LARGER than 2, the arithmetic guard below (thresholds > ceiling) would be measuring the wrong
// ceiling — so the ceiling is not hard-coded as a bare number. It is named, explained, and asserted
// against the thresholds, and the behavioural half drives the REAL Process() over batches at and below
// that ceiling rather than trusting the inequality alone.

// liveBatchCeiling is the largest number of alerts observed in a single webhook on the live control
// plane. Update it only from a fresh measurement, and expect the assertions below to move with it.
const liveBatchCeiling = 2

func TestPipelineCannotFireOnLiveBatchSizes(t *testing.T) {
	// --- the arithmetic: a stage needing N events cannot fire in a batch that never holds N ---
	if flapThreshold <= liveBatchCeiling {
		t.Errorf("flapThreshold=%d is now reachable within a live batch (ceiling %d) — the "+
			"'Pipeline is superseded, do not wire it' decision rests on this being unreachable and must be re-made",
			flapThreshold, liveBatchCeiling)
	}
	if burstThreshold <= liveBatchCeiling {
		t.Errorf("burstThreshold=%d is now reachable within a live batch (ceiling %d) — re-make the decision",
			burstThreshold, liveBatchCeiling)
	}

	// --- the behaviour: drive the REAL Process() at and below the ceiling ---
	// An inequality can be right about the wrong quantity. This runs the actual chain over the worst case
	// live traffic can present — a full-ceiling batch of the SAME key, arriving inside every window, which
	// is the most favourable input flap and burst could possibly get — and asserts neither fires.
	now := time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC)
	p := NewPipeline()

	for n := 1; n <= liveBatchCeiling; n++ {
		evs := make([]IncidentEnvelope, 0, n)
		for i := 0; i < n; i++ {
			evs = append(evs, IncidentEnvelope{
				ExternalRef: fmt.Sprintf("lnms-%d", i),
				SourceID:    "librenms-dc1",
				Host:        "dc1nc02",
				Site:        "dc1",
				AlertRule:   "Service up/down",
				Severity:    SeverityCritical,
				// inside flapWindow AND burstWindow — the most favourable spacing for both stages
				ObservedAt: now.Add(time.Duration(i) * time.Minute),
				ReceivedAt: now.Add(time.Duration(i) * time.Minute),
			})
		}
		res := p.Process(evs, now)
		if len(res.Decisions) != n {
			t.Fatalf("batch of %d: Process returned %d decisions — the harness is not driving the real chain", n, len(res.Decisions))
		}
		for i, d := range res.Decisions {
			if d.Flapping {
				t.Errorf("batch of %d, decision %d: Flapping=true — flap fired within a live-sized batch, "+
					"so wiring Pipeline WOULD change decisions and the 'superseded' finding is stale", n, i)
			}
			if d.InBurst {
				t.Errorf("batch of %d, decision %d: InBurst=true — burst fired within a live-sized batch; re-make the decision", n, i)
			}
		}
	}
}

// The chain is not broken, it is unreachable — a distinction worth keeping, because "it never fires" and
// "it cannot fire" call for opposite responses. Given a batch big enough, the stages work: this drives
// flapThreshold identical fires inside flapWindow and requires the flag. If this ever fails, Pipeline is
// broken and the finding above ("superseded, not starving") would be the wrong reason to leave it unwired.
func TestPipelineDoesFireWhenTheBatchIsBigEnough(t *testing.T) {
	now := time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC)
	evs := make([]IncidentEnvelope, 0, flapThreshold)
	for i := 0; i < flapThreshold; i++ {
		evs = append(evs, IncidentEnvelope{
			ExternalRef: fmt.Sprintf("lnms-flap-%d", i),
			SourceID:    "librenms-dc1",
			Host:        "dc1nc02",
			Site:        "dc1",
			AlertRule:   "Service up/down",
			Severity:    SeverityCritical,
			ObservedAt:  now.Add(time.Duration(i) * time.Minute), // 3 fires inside the 15m flapWindow
			ReceivedAt:  now.Add(time.Duration(i) * time.Minute),
		})
	}
	res := NewPipeline().Process(evs, now)
	flagged := 0
	for _, d := range res.Decisions {
		if d.Flapping {
			flagged++
		}
	}
	if flagged == 0 {
		t.Fatalf("a batch of %d identical fires inside flapWindow set Flapping on none of %d decisions — "+
			"the chain is BROKEN, not merely unreachable, and the finding recorded on Pipeline is the wrong one",
			flapThreshold, len(res.Decisions))
	}
	if len(res.Order) == 0 {
		t.Error("Process reported no stage order — the chain did not run")
	}
}
