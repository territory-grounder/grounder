package main

import (
	"context"
	"testing"
	"time"

	"github.com/territory-grounder/grounder/core/db"
	"github.com/territory-grounder/grounder/core/estate"
	"github.com/territory-grounder/grounder/core/learn"
)

// TG-451 — the estate-refresh goroutine reads these late-bound loaders while main() binds them on the post-connect
// path. Exercise the atomic handoff: a reader goroutine hammers load() while the main goroutine calls bind(). Under
// `go test -race` this is race-free BECAUSE the loader is an atomic.Pointer. KILLING MUTATION: in estate_loaders.go
// change either holder's `fn atomic.Pointer[...]` to a plain field (bind = l.fn = fn; load reads l.fn) and the race
// detector flags the concurrent read/write here — reddening these tests. Also asserts the unbound-errors contract
// (per-source isolation degrades a pre-bind read to "no edges yet", never a panic) survives the refactor.

func TestEstateRelayLoaderAtomicHandoff(t *testing.T) {
	l := &estateRelayLoader{}
	// Unbound: errors, never panics — exactly what the plain-closure nil-check did.
	if _, _, err := l.load(context.Background()); err == nil {
		t.Fatal("unbound estateRelayLoader.load must error until the post-connect prime binds it")
	}

	done := make(chan struct{})
	go func() {
		for i := 0; i < 3000; i++ {
			_, _, _ = l.load(context.Background()) // the refresh-goroutine read, racing the bind below
		}
		close(done)
	}()
	l.bind(func(ctx context.Context) (estate.Snapshot, time.Time, error) {
		return estate.Snapshot{}, time.Time{}, nil
	})
	<-done

	if _, _, err := l.load(context.Background()); err != nil {
		t.Fatalf("bound estateRelayLoader.load errored: %v", err)
	}
}

func TestChaosLoaderAtomicHandoff(t *testing.T) {
	l := &chaosLoader{}
	if _, err := l.load(context.Background()); err == nil {
		t.Fatal("unbound chaosLoader.load must error until the post-connect prime binds it")
	}

	done := make(chan struct{})
	go func() {
		for i := 0; i < 3000; i++ {
			_, _ = l.load(context.Background()) // the refresh-goroutine read, racing the bind below
		}
		close(done)
	}()
	l.bind(func(ctx context.Context) ([]estate.ChaosCascade, error) {
		return []estate.ChaosCascade{{Root: "root", Downstream: "down"}}, nil
	})
	<-done

	got, err := l.load(context.Background())
	if err != nil {
		t.Fatalf("bound chaosLoader.load errored: %v", err)
	}
	if len(got) != 1 || got[0].Root != "root" {
		t.Fatalf("bound chaosLoader.load returned %+v, want the single seeded cascade", got)
	}
}

func TestRecoveryFeedLoaderAtomicHandoff(t *testing.T) {
	l := &recoveryFeedLoader{}
	if _, _, err := l.load(context.Background(), db.RecoveryCursor{}); err == nil {
		t.Fatal("unbound recoveryFeedLoader.load must error until the post-connect prime binds it")
	}

	done := make(chan struct{})
	go func() {
		for i := 0; i < 3000; i++ {
			_, _, _ = l.load(context.Background(), db.RecoveryCursor{}) // the refresh-goroutine read, racing the bind below
		}
		close(done)
	}()
	l.bind(func(ctx context.Context, cur db.RecoveryCursor) ([]learn.ClearObservation, db.RecoveryCursor, error) {
		return nil, cur, nil
	})
	<-done

	if _, _, err := l.load(context.Background(), db.RecoveryCursor{}); err != nil {
		t.Fatalf("bound recoveryFeedLoader.load errored: %v", err)
	}
}
