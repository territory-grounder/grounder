package db

// TG-681: the per-site front-door arm. The pure oracles below need no database; the one DSN-gated oracle at the
// end pins the SQL. Each has a seed whose inversion flips it.
//
// KILLING MUTATIONS (executed 2026-11-07, each RED, then reverted):
//  1. drop the `p.Bound > 0` guard in the stale branch (always refuse an old site) →
//     TestSiteLivenessRecordOnlyNeverChangesTheAnswer fails: a record-only policy withheld a verdict.
//  2. invert the comparison (`age < p.Bound`) → TestSiteLivenessBoundWithholdsAnOlderSite fails on the fresh
//     row AND the stale row.
//  3. skip the IsDeclaredSite check → TestSiteLivenessIgnoresUndeclaredSiteLabels fails: a label outside the
//     vocabulary ("no") was asked of the ledger and refused.
//  4. treat !known as fresh → TestSiteLivenessBoundTreatsANeverDeliveringSiteAsUnobservable fails.

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/territory-grounder/grounder/core/verify"
)

type fakeSiteDoor struct {
	newest map[string]time.Time
	err    error
	asked  []string
}

func (f *fakeSiteDoor) last(_ context.Context, site string, _ time.Time) (time.Time, bool, error) {
	f.asked = append(f.asked, site)
	if f.err != nil {
		return time.Time{}, false, f.err
	}
	t, ok := f.newest[site]
	return t, ok, nil
}

func innerReader(calls *int) PostStateReader {
	return func(context.Context, string, string) ([]verify.ObservedAlert, bool) {
		*calls++
		return []verify.ObservedAlert{{Host: "h", Rule: "r"}}, true
	}
}

func TestSiteLivenessRecordOnlyNeverChangesTheAnswer(t *testing.T) {
	now := time.Date(2026, 11, 7, 21, 0, 0, 0, time.UTC)
	door := &fakeSiteDoor{newest: map[string]time.Time{"dc2": now.Add(-90 * time.Hour)}}
	var logged []string
	calls := 0
	r := SiteLivenessPolicy{LastDelivery: door.last, Now: func() time.Time { return now },
		Log: func(f string, a ...any) { logged = append(logged, fmt.Sprintf(f, a...)) }}.Wrap(innerReader(&calls))

	got, ok := r(context.Background(), "grhost", "dc2")
	if !ok || len(got) != 1 || calls != 1 {
		t.Fatalf("record-only must pass the inner answer through unchanged, got %v ok=%v calls=%d", got, ok, calls)
	}
	if len(logged) != 1 || !strings.Contains(logged[0], "dc2") || !strings.Contains(logged[0], "90h0m0s") || !strings.Contains(logged[0], "record-only") {
		t.Fatalf("the site age must be RECORDED on every observation even when it decides nothing, got %q", logged)
	}
	// An unreadable door in record-only mode is logged and still passes through.
	door.err = errors.New("boom")
	if _, ok := r(context.Background(), "grhost", "dc2"); !ok {
		t.Fatal("record-only must not withhold on a read error — it has no bound to enforce")
	}
}

func TestSiteLivenessBoundWithholdsAnOlderSite(t *testing.T) {
	now := time.Date(2026, 11, 7, 21, 0, 0, 0, time.UTC)
	door := &fakeSiteDoor{newest: map[string]time.Time{
		"dc2": now.Add(-90 * time.Hour),
		"dc1": now.Add(-5 * time.Minute),
	}}
	calls := 0
	var logged []string
	r := SiteLivenessPolicy{LastDelivery: door.last, Bound: 24 * time.Hour, Now: func() time.Time { return now },
		Log: func(f string, a ...any) { logged = append(logged, fmt.Sprintf(f, a...)) }}.Wrap(innerReader(&calls))

	if got, ok := r(context.Background(), "grhost", "dc2"); ok || got != nil || calls != 0 {
		t.Fatalf("a site whose newest delivery is older than the bound must read UNOBSERVABLE without consulting the inner reader, got %v ok=%v calls=%d", got, ok, calls)
	}
	if len(logged) != 1 || !strings.Contains(logged[0], "UNOBSERVABLE") {
		t.Fatalf("the withheld read must say why, got %q", logged)
	}
	if got, ok := r(context.Background(), "nlhost", "dc1"); !ok || len(got) != 1 || calls != 1 {
		t.Fatalf("a site inside the bound must pass through, got %v ok=%v calls=%d", got, ok, calls)
	}
	// Exactly at the bound is still within it.
	door.newest["dc2"] = now.Add(-24 * time.Hour)
	if _, ok := r(context.Background(), "grhost", "dc2"); !ok {
		t.Fatal("an age equal to the bound is within it")
	}
}

func TestSiteLivenessBoundTreatsANeverDeliveringSiteAsUnobservable(t *testing.T) {
	now := time.Date(2026, 11, 7, 21, 0, 0, 0, time.UTC)
	door := &fakeSiteDoor{newest: map[string]time.Time{}}
	calls := 0
	r := SiteLivenessPolicy{LastDelivery: door.last, Bound: time.Hour, Now: func() time.Time { return now }}.Wrap(innerReader(&calls))
	if _, ok := r(context.Background(), "grhost", "dc2"); ok || calls != 0 {
		t.Fatal("a site with no delivery on record is not a fresh site: with a bound it must read unobservable")
	}
	door.err = errors.New("read failed")
	if _, ok := r(context.Background(), "grhost", "dc2"); ok {
		t.Fatal("an unreadable site door with a bound set must read unobservable, never fresh")
	}
}

// The ledger stores sites in the canonical deployment-key vocabulary (dc1 / dc2, TG-456). A read that
// names a legacy spelling is folded before the lookup; one that names a label outside the vocabulary (the
// 2026-11-07 ledger carried 1,071 occurrences stamped "no") or no site at all makes NO site claim and is passed
// through — the estate-wide arm in LedgerObserver remains the only liveness check, exactly as before.
func TestSiteLivenessIgnoresUndeclaredSiteLabels(t *testing.T) {
	now := time.Date(2026, 11, 7, 21, 0, 0, 0, time.UTC)
	door := &fakeSiteDoor{newest: map[string]time.Time{"dc1": now.Add(-time.Minute)}}
	calls := 0
	r := SiteLivenessPolicy{LastDelivery: door.last, Bound: time.Hour, Now: func() time.Time { return now }}.Wrap(innerReader(&calls))
	for _, site := range []string{"", "no", "ch", "tx", "mars"} {
		if _, ok := r(context.Background(), "h", site); !ok {
			t.Fatalf("site %q carries no declared-site claim and must pass through", site)
		}
	}
	if len(door.asked) != 0 {
		t.Fatalf("an undeclared label must never be asked of the ledger, asked %v", door.asked)
	}
	if _, ok := r(context.Background(), "h", "nl"); !ok {
		t.Fatal("a legacy spelling folds to its canonical site and is then checked")
	}
	if len(door.asked) != 1 || door.asked[0] != "dc1" {
		t.Fatalf("the lookup must use the CANONICAL site, asked %v", door.asked)
	}
}

func TestSiteLivenessWrapIsInertWithoutADoorOrAnInner(t *testing.T) {
	calls := 0
	inner := innerReader(&calls)
	if w := (SiteLivenessPolicy{}).Wrap(inner); w == nil {
		t.Fatal("no door ⇒ the inner reader is returned unchanged, never nil")
	} else if _, ok := w(context.Background(), "h", "dc2"); !ok || calls != 1 {
		t.Fatal("no door ⇒ pass-through")
	}
	if w := (SiteLivenessPolicy{LastDelivery: (&fakeSiteDoor{}).last}).Wrap(nil); w != nil {
		t.Fatal("a nil inner reader stays nil — the verifiability gate must still see an UNWIRED observer")
	}
}

// DSN-gated: the SQL reads the newest delivery PER SITE, ignores other sites and the future, and reports a site
// with no rows as unknown rather than as a zero time.
func TestSiteFrontDoorLastDeliveryReadsPerSite(t *testing.T) {
	ctx, p, done := openFixture(t)
	defer done()
	now := time.Now().UTC()
	cleanup := func() {
		_, _ = p.Exec(ctx, `DELETE FROM ingest_alert_occurrence WHERE external_ref LIKE 'psl-%'`)
	}
	cleanup()
	defer cleanup()
	seed := func(ref, site string, at time.Time) {
		if _, err := p.Exec(ctx, `
			INSERT INTO ingest_alert_occurrence (external_ref, alert_rule, severity, host, site, received_at, observed_at)
			VALUES ($1,'Device-Down','critical','psl-host',$2,$3,$3)`, ref, site, at); err != nil {
			t.Fatalf("seed %s: %v", ref, err)
		}
	}
	seed("psl-gr-old", "dc2", now.Add(-90*time.Hour))
	seed("psl-gr-new", "dc2", now.Add(-40*time.Hour))
	seed("psl-gr-future", "dc2", now.Add(2*time.Hour))
	seed("psl-nl", "dc1", now.Add(-2*time.Minute))

	door := NewSiteFrontDoor(p)
	got, known, err := door.LastDelivery(ctx, "dc2", now)
	if err != nil || !known {
		t.Fatalf("dc2 has deliveries: err=%v known=%v", err, known)
	}
	if d := got.Sub(now.Add(-40 * time.Hour)); d < -2*time.Second || d > 2*time.Second {
		t.Fatalf("the newest PAST delivery for dc2 is 40h old (not the 90h one, not the future one), got %v", got)
	}
	if nl, known, err := door.LastDelivery(ctx, "dc1", now); err != nil || !known || now.Sub(nl) > 5*time.Minute {
		t.Fatalf("dc1's newest delivery is minutes old and must not be mixed with dc2's: %v known=%v err=%v", nl, known, err)
	}
	if _, known, err := door.LastDelivery(ctx, "psl-never-a-site", now); err != nil || known {
		t.Fatalf("a site with no rows must read known=false (err=%v known=%v)", err, known)
	}
	var nilDoor *SiteFrontDoor
	if _, _, err := nilDoor.LastDelivery(ctx, "dc2", now); err == nil {
		t.Fatal("a nil door must error, never answer")
	}
}
