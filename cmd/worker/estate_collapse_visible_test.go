package main

import (
	"strings"
	"testing"
)

// TG-395. Two independent silences on one event.
//
// (a) The refresh's edge-drop WARNING was gated `if len(srcErrs) > 0 && !kept` — it reported the drop
// only when a SOURCE FAILED, which is the case where the graph is LEAST likely to be damaged: an empty
// rebuild sets `kept` and the prior graph is retained. A refresh in which every source SUCCEEDS can
// still collapse the topology, and that is the reading that matters.
//
// Measured 2026-08-06 02:57:32: the pve source correctly reported that a dead node has no guests, 52
// `runs_on` edges were dropped, `srcErrs` was empty — the guard was false and NOTHING PRINTED. The only
// two warnings the operator got that morning were 17-edge wobbles at the other site.
//
// The comment directly above the guard already stated the intent it failed to deliver: "an operator
// seeing 412 edges become 37 knows what they are looking at, and today nothing tells them at all."

func estateRefreshSource(t *testing.T) string {
	t.Helper()
	src := stripGoComments(readWorkerMain(t))
	if len(src) < 10_000 {
		t.Fatalf("VACUITY FLOOR: main.go stripped to %d bytes", len(src))
	}
	return src
}

// TestTheEdgeDropWarningIsNotGatedOnSourceFailure is the finding.
func TestTheEdgeDropWarningIsNotGatedOnSourceFailure(t *testing.T) {
	src := estateRefreshSource(t)

	if strings.Contains(src, "if len(srcErrs) > 0 && !kept {") {
		t.Fatal("the edge-drop warning is still gated on a SOURCE FAILURE. A refresh in which every " +
			"source succeeds can collapse the graph — a dead hypervisor correctly reporting no guests " +
			"dropped 52 edges on 2026-08-06 with srcErrs empty, and nothing printed. That gate fires in " +
			"the case where the graph is LEAST likely to be wrong.")
	}
	// The drop test must be the OUTER condition — the thing being reported is the drop itself.
	k := strings.Index(src, "if after := estateHolder.Graph().Len(); after < before {")
	if k < 0 {
		t.Fatal("the refresh no longer compares the post-refresh edge count against the pre-refresh one, " +
			"so a collapse cannot be detected at all")
	}
}

// TestBothCollapseCausesAreDistinguished. "Sources failed" and "every source succeeded" call for
// different actions — suspect missing data, versus the estate really changed. Reporting one line for
// both would tell an operator that something dropped and nothing about what to do.
func TestBothCollapseCausesAreDistinguished(t *testing.T) {
	src := estateRefreshSource(t)
	k := strings.Index(src, "if after := estateHolder.Graph().Len(); after < before {")
	if k < 0 {
		t.Skip("the drop comparison is absent; the test above reports that")
	}
	block := src[k:min(k+1400, len(src))]

	if !strings.Contains(block, "source(s) failed") {
		t.Error("the source-failure cause is no longer named — an operator cannot tell a drop that may " +
			"be MISSING DATA from one that is a real topology change")
	}
	if !strings.Contains(block, "EVERY source succeeding") {
		t.Error("the all-sources-succeeded cause is no longer named. That is the branch this ticket " +
			"exists for: it is the silent one, and it means the estate genuinely changed.")
	}
	if !strings.Contains(block, "else") {
		t.Error("the two causes are not on separate branches, so one message must be covering both")
	}
}
