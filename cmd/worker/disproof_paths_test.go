package main

import (
	"testing"

	"github.com/territory-grounder/grounder/core/falsify"
	"github.com/territory-grounder/grounder/core/verify"
)

// ★ THE PAIRING MUST SURVIVE THE PRODUCER TOO (TG-206).
//
// core/estate guards what decay does with the paths. Nothing guarded how they are BUILT — and a producer
// that pairs every target with every capture's surprise hosts reconstructs the exact flat-set defect while
// every test in core/estate stays green. That mutation was executed and survived until this file existed.
func TestDisproofPathsDoNotCrossCaptures(t *testing.T) {
	captured := []falsify.CapturedDeviation{
		{Record: falsify.DiscoveryRecord{TargetHost: "pve01", SurpriseHosts: []string{"web7"}}},
		{Record: falsify.DiscoveryRecord{TargetHost: "db3", SurpriseHosts: []string{"cache2"}}},
	}
	got := disproofPaths(captured)

	if len(got) != 2 {
		t.Fatalf("built %d path(s) from two captures, want 2: %+v", len(got), got)
	}
	for _, p := range got {
		switch p.Target {
		case "pve01":
			if len(p.Surprised) != 1 || p.Surprised[0] != "web7" {
				t.Errorf("pve01 paired with %v, want [web7] only. Pairing it with db3's surprise hosts "+
					"rebuilds the flat set this ticket is about.", p.Surprised)
			}
		case "db3":
			if len(p.Surprised) != 1 || p.Surprised[0] != "cache2" {
				t.Errorf("db3 paired with %v, want [cache2] only", p.Surprised)
			}
		default:
			t.Errorf("unexpected target %q", p.Target)
		}
	}
}

// Rule mismatches belong to the same target as the capture that produced them.
func TestRuleMismatchesArePairedToTheirOwnTarget(t *testing.T) {
	got := disproofPaths([]falsify.CapturedDeviation{
		{Record: falsify.DiscoveryRecord{
			TargetHost: "pve01",
			Mismatches: []verify.RuleMismatch{{Host: "sw01"}},
		}},
	})
	if len(got) != 1 || got[0].Target != "pve01" {
		t.Fatalf("got %+v, want one path from pve01", got)
	}
	if len(got[0].Surprised) != 1 || got[0].Surprised[0] != "sw01" {
		t.Errorf("mismatch host paired as %v, want [sw01]", got[0].Surprised)
	}
}

// A self-referential pair is not a path: an edge from a host to itself carries no prediction.
func TestATargetIsNeverPairedWithItself(t *testing.T) {
	got := disproofPaths([]falsify.CapturedDeviation{
		{Record: falsify.DiscoveryRecord{TargetHost: "pve01", SurpriseHosts: []string{"pve01", "web7"}}},
	})
	if len(got) != 1 {
		t.Fatalf("got %+v, want one path", got)
	}
	for _, h := range got[0].Surprised {
		if h == "pve01" {
			t.Error("the target was paired with itself — a self-edge carries no prediction to disprove")
		}
	}
}

// A capture with no target yields no path rather than a path keyed on "".
func TestACaptureWithNoTargetYieldsNoPath(t *testing.T) {
	got := disproofPaths([]falsify.CapturedDeviation{
		{Record: falsify.DiscoveryRecord{SurpriseHosts: []string{"web7"}}},
	})
	if len(got) != 0 {
		t.Errorf("got %+v for a capture with no target, want none — an empty target would pair with every "+
			"surprise host under one meaningless key", got)
	}
}
