package main

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

// THE BOOT RACE (live, 2026-11-08 00:35 UTC): on the deploy that introduced self_identity the actuation worker booted
// before migration 0129 was applied; its ONE publication failed and TG's own heal logins read indeterminate until a
// manual restart. A failed publication must be retried until it lands.

// tg684FlakyPublisher fails its first `failures` calls, then behaves like the real store.
type tg684FlakyPublisher struct {
	mu       sync.Mutex
	failures int
	calls    int
	rows     []string
}

func (p *tg684FlakyPublisher) Publish(_ context.Context, _, actor, _ string) (bool, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls++
	if p.calls <= p.failures {
		return false, errors.New(`relation "self_identity" does not exist`)
	}
	if n := len(p.rows); n > 0 && p.rows[n-1] == actor {
		return false, nil
	}
	p.rows = append(p.rows, actor)
	return true, nil
}

func (p *tg684FlakyPublisher) snapshot() (int, []string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.calls, append([]string(nil), p.rows...)
}

// KILLING MUTATION: make startSelfIdentityPublisher return after the first attempt (no retry loop) — RED here.
func TestAFailedSelfIdentityPublicationIsRetriedUntilItLands(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	pub := &tg684FlakyPublisher{failures: 2}
	if settled := startSelfIdentityPublisher(ctx, pub, "journal", "root!SHA256:tg684retry", "actuation", 5*time.Millisecond); settled {
		t.Fatal("control: the first attempt fails, so it must not report settled")
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		calls, rows := pub.snapshot()
		if len(rows) == 1 {
			if rows[0] != "root!SHA256:tg684retry" || calls != 3 {
				t.Fatalf("want exactly one publication after two failures (3 calls), got calls=%d rows=%v", calls, rows)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("a failed publication must be retried until it lands; after 2 s calls=%d rows=%v", calls, rows)
		}
		time.Sleep(5 * time.Millisecond)
	}
	// Once it landed, the retry loop stops: no further calls.
	time.Sleep(30 * time.Millisecond)
	if calls, _ := pub.snapshot(); calls != 3 {
		t.Fatalf("the retry loop must stop after the publication lands, got %d calls", calls)
	}
}

func TestASettledSelfIdentityPublicationStartsNoRetries(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ok := &tg684FlakyPublisher{}
	if !startSelfIdentityPublisher(ctx, ok, "journal", "root!SHA256:tg684ok", "actuation", 5*time.Millisecond) {
		t.Fatal("a first-try success must report settled")
	}
	time.Sleep(30 * time.Millisecond)
	if calls, rows := ok.snapshot(); calls != 1 || len(rows) != 1 {
		t.Fatalf("a settled publication must not be retried, got calls=%d rows=%v", calls, rows)
	}
	// Nothing to publish (the triage plane holds no key) and no database are settled too: a retry could change nothing.
	if !startSelfIdentityPublisher(ctx, ok, "journal", "", "triage", 5*time.Millisecond) {
		t.Fatal("a plane with no key has nothing to publish and must report settled")
	}
	if !startSelfIdentityPublisher(ctx, nil, "journal", "root!SHA256:tg684nodb", "actuation", 5*time.Millisecond) {
		t.Fatal("no database: a retry cannot help, so it must report settled")
	}
}

func TestTheSelfIdentityRetryStopsWithItsContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	never := &tg684FlakyPublisher{failures: 1 << 30}
	startSelfIdentityPublisher(ctx, never, "journal", "root!SHA256:tg684ctx", "actuation", 5*time.Millisecond)
	time.Sleep(25 * time.Millisecond)
	cancel()
	time.Sleep(15 * time.Millisecond)
	before, _ := never.snapshot()
	time.Sleep(30 * time.Millisecond)
	if after, _ := never.snapshot(); after != before {
		t.Fatalf("the retry loop must stop when its context ends, calls went %d -> %d", before, after)
	}
}
