package main

import (
	"testing"
	"time"
)

// TG-623: the yield denominator must follow the detector's watched set when it follows the cluster, and stay
// the configured count otherwise.

type fakeWatched struct {
	follows bool
	n       int
}

func (f fakeWatched) FollowsCluster() bool { return f.follows }
func (f fakeWatched) WatchedCount() int    { return f.n }

func TestPVELivenessYieldFollowsTheClusterDenominator(t *testing.T) {
	y := newPVELivenessYield(195, fakeWatched{follows: true, n: 198})
	if got := y.watched.Load(); got != 195 {
		t.Fatalf("seeded denominator before the first poll: got %d, want the configured 195", got)
	}
	y.recordSuccess(time.Now(), 0, 0, 0)
	if got := y.watched.Load(); got != 198 {
		t.Fatalf("TG-623: under follow-cluster the denominator is the last fetch's watched set: got %d, want 198", got)
	}
	static := newPVELivenessYield(195, fakeWatched{follows: false, n: 198})
	static.recordSuccess(time.Now(), 0, 0, 0)
	if got := static.watched.Load(); got != 195 {
		t.Fatalf("static mode keeps the configured denominator: got %d, want 195", got)
	}
}

func TestPVELivenessFollowClusterKey(t *testing.T) {
	get := func(v string) func(k, def string) string {
		return func(k, def string) string {
			if k == "TG_PVE_LIVENESS_FOLLOW_CLUSTER" {
				return v
			}
			return def
		}
	}
	if pveLivenessFollowCluster(get("")) || pveLivenessFollowCluster(get("false")) {
		t.Fatal("follow-cluster must be OFF unless the key is truthy")
	}
	if !pveLivenessFollowCluster(get("true")) {
		t.Fatal("TG_PVE_LIVENESS_FOLLOW_CLUSTER=true must arm follow-cluster")
	}
	if n := len(pveLivenessOptions(get("true"), nil, 3)); n != 2 {
		t.Fatalf("armed follow-cluster must add exactly one option beyond the transport, got %d", n)
	}
	if n := len(pveLivenessOptions(get(""), nil, 3)); n != 1 {
		t.Fatalf("unarmed must be the transport alone (byte-identical), got %d", n)
	}
}
