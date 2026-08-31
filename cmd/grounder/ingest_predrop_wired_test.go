package main

import (
	"strings"
	"testing"
)

// grounderSource (comment-stripping) is defined in ingest_refusals_wired_test.go and reused here.

// TG-380: the two predrop producers must be WIRED and the counter EXPOSED. The recovery producer is
// exercised end-to-end by core/httpapi's handler test; the reject-duplicate producer lives inside
// temporalTriage.StartTriage, which needs a real Temporal client to drive — so it is pinned here by a
// source scan (the ingest_refusals_wired_test.go precedent), plus the /metrics exposition.
//
// KILLING MUTATION (executed 2026-08-11): delete `ingestPredropCounter.record(predropRejectDuplicate)` from
// StartTriage, OR drop `ingestPredropCounter.samples()` from the /metrics closure — the matching assertion
// below fails. A wired producer with no exposition (or an exposed counter no producer feeds) is the
// declared-but-dead pattern; both halves are pinned.

func TestPredropRejectDuplicateSiteIsWired(t *testing.T) {
	src := grounderSource(t, "deps.go")
	if !strings.Contains(src, "ingestPredropCounter.record(predropRejectDuplicate)") {
		t.Error("StartTriage does not count a rejected-duplicate re-fire (ingestPredropCounter.record(" +
			"predropRejectDuplicate)). A re-fire of an in-flight incident then mints no new session AND leaves " +
			"no trace — the 'sent more than we triaged' drop stays unmeasurable (TG-380).")
	}
	if !strings.Contains(src, "IngestPredrop:") {
		t.Error("the httpapi Deps is not wired with IngestPredrop — the recovery predrop producer is dark.")
	}
}

func TestPredropCounterIsExposedOnMetrics(t *testing.T) {
	src := grounderSource(t, "main.go")
	if !strings.Contains(src, "ingestPredropCounter.samples()") {
		t.Error("the /metrics closure does not append ingestPredropCounter.samples() — the predrop counter " +
			"increments but never reaches Prometheus (declared-but-dead: a producer with no exposition).")
	}
}

func TestClampPredropReasonIsClosed(t *testing.T) {
	if got := clampPredropReason(predropRejectDuplicate); got != predropRejectDuplicate {
		t.Errorf("a known reason must pass through, got %q", got)
	}
	if got := clampPredropReason(predropRecoveryTransition); got != predropRecoveryTransition {
		t.Errorf("a known reason must pass through, got %q", got)
	}
	if got := clampPredropReason("something-caller-derived"); got != "other" {
		t.Errorf("an unknown reason must fold to 'other' (bounded cardinality), got %q", got)
	}
}

func TestPredropSamplesEmitNothingWhenIdle(t *testing.T) {
	c := newIngestPredrop()
	if s := c.samples(); len(s) != 0 {
		t.Fatalf("an idle predrop counter must emit nothing (absent==zero for a counter), got %v", s)
	}
	c.record(predropRecoveryTransition)
	if len(c.samples()) != 1 {
		t.Fatalf("one recorded drop must render one series")
	}
}
