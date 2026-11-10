package db

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/territory-grounder/grounder/core/holds"
)

// Gated on TG_TEST_POSTGRES_DSN (CI provides it): the hold projection round-trips through real SQL, because the
// defects that matter here — an inverted expiry comparison, a dropped host predicate, a non-sticky row hiding a
// sticky one — are exactly the ones a pgx fake cannot see.
func TestHoldStoreRemembersOpenStickyHoldsOnly(t *testing.T) {
	dsn := os.Getenv("TG_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("set TG_TEST_POSTGRES_DSN to a migrated database to run the hold_decision round-trip")
	}
	ctx := context.Background()
	if err := Migrate(ctx, dsn); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	p, err := Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	defer p.Close()
	s := NewHoldStore(p)

	now := time.Now().UTC()
	uniq := fmt.Sprintf("%d-%d", os.Getpid(), now.UnixNano())
	host, rule := "tg667-host-"+uniq, "Devices-up/down"

	if err := s.Record(ctx, holds.HoldDecision{ExternalRef: "x", Host: host, AlertRule: rule, ReasonKey: "", ExpiresAt: now.Add(time.Hour)}); err == nil {
		t.Fatal("a hold with no reason must be refused")
	}
	if err := s.Record(ctx, holds.HoldDecision{ExternalRef: "x", Host: host, AlertRule: rule, ReasonKey: "human-deny"}); err == nil {
		t.Fatal("a hold with no expiry must be refused — it could never be released")
	}

	expired := holds.HoldDecision{ExternalRef: "tg667-expired-" + uniq, Host: host, AlertRule: rule, ReasonKey: "actor-attributed-authorized",
		DecidedBy: "tg:fallback-approver", DecidedAt: now.Add(-48 * time.Hour), ExpiresAt: now.Add(-24 * time.Hour)}
	sticky := holds.HoldDecision{ExternalRef: "tg667-sticky-" + uniq, Host: host, AlertRule: rule, OpClass: "start-guest", ActionID: "a1",
		ReasonKey: "actor-attributed-authorized", Why: "Held: a person stopped it", ActorAttribution: "attributed-authorized",
		DecidedBy: "tg:fallback-approver", DecidedAt: now.Add(-40 * time.Minute), ExpiresAt: now.Add(23 * time.Hour)}
	// A NEWER non-sticky hold on the same subject must not hide the sticky one behind it.
	nonSticky := holds.HoldDecision{ExternalRef: "tg667-prose-" + uniq, Host: host, AlertRule: rule, ReasonKey: "rationale-names-a-different-host",
		DecidedBy: "tg:fallback-approver", DecidedAt: now.Add(-10 * time.Minute), ExpiresAt: now.Add(23 * time.Hour)}
	otherRule := holds.HoldDecision{ExternalRef: "tg667-other-" + uniq, Host: host, AlertRule: "Service-up/down", ReasonKey: "human-deny",
		DecidedBy: "operator:alice", DecidedAt: now.Add(-5 * time.Minute), ExpiresAt: now.Add(23 * time.Hour)}
	for _, h := range []holds.HoldDecision{expired, sticky, nonSticky, otherRule} {
		if err := s.Record(ctx, h); err != nil {
			t.Fatalf("record %s: %v", h.ExternalRef, err)
		}
	}

	// A RUN of newer non-sticky rows (a flapping proposal re-held 30 times) must never hide the sticky one — the
	// sticky filter is applied in SQL, not behind a LIMIT (fresh-eyes review, 2026-11-07).
	for i := 0; i < 30; i++ {
		if err := s.Record(ctx, holds.HoldDecision{ExternalRef: fmt.Sprintf("tg667-flap-%d-%s", i, uniq), Host: host, AlertRule: rule,
			ReasonKey: "rationale-names-a-different-host", DecidedBy: "tg:fallback-approver",
			DecidedAt: now.Add(-time.Duration(i) * time.Second), ExpiresAt: now.Add(23 * time.Hour)}); err != nil {
			t.Fatal(err)
		}
	}
	got, found, err := s.OpenFor(ctx, host, rule, now)
	if err != nil {
		t.Fatal(err)
	}
	if !found || got.ExternalRef != sticky.ExternalRef {
		t.Fatalf("OpenFor must return the newest OPEN STICKY hold (%s); got found=%v %+v", sticky.ExternalRef, found, got)
	}
	if got.Why != sticky.Why || got.OpClass != "start-guest" || got.ActorAttribution != "attributed-authorized" || got.Seq == 0 {
		t.Fatalf("round-trip lost content: %+v", got)
	}
	if _, found, _ := s.OpenFor(ctx, host, rule, now.Add(24*time.Hour)); found {
		t.Fatal("past expires_at the hold is closed")
	}
	if _, found, _ := s.OpenFor(ctx, "tg667-nobody-"+uniq, rule, now); found {
		t.Fatal("another host's hold must not leak")
	}
	if _, found, _ := s.OpenFor(ctx, "", rule, now); found {
		t.Fatal("a blank subject matches nothing")
	}

	open, err := s.OpenHolds(ctx, now)
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, h := range open {
		seen[h.ExternalRef] = true
	}
	if seen[expired.ExternalRef] || !seen[sticky.ExternalRef] || !seen[nonSticky.ExternalRef] || !seen[otherRule.ExternalRef] {
		t.Fatalf("OpenHolds must list every open hold (sticky or not) and no expired one: %v", seen)
	}
}

func TestHoldStoreListsUnretriedFailedInvestigations(t *testing.T) {
	dsn := os.Getenv("TG_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("set TG_TEST_POSTGRES_DSN to a migrated database to run the failed-investigation read")
	}
	ctx := context.Background()
	if err := Migrate(ctx, dsn); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	p, err := Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	defer p.Close()
	s := NewHoldStore(p)

	now := time.Now().UTC()
	uniq := fmt.Sprintf("%d-%d", os.Getpid(), now.UnixNano())
	hostA, hostB := "tg667-fa-"+uniq, "tg667-fb-"+uniq
	ins := func(ref, host, outcome, stop string, at time.Time) {
		t.Helper()
		if _, err := p.Exec(ctx, `INSERT INTO session_triage (external_ref, host, alert_rule, outcome, stop_reason, created_at)
			VALUES ($1, $2, 'Service-up/down', $3, $4, $5)`, ref, host, outcome, stop, at); err != nil {
			t.Fatalf("seed %s: %v", ref, err)
		}
	}
	// host A: failed, then retried successfully ⇒ NOT listed.
	ins("tg667-fa1-"+uniq, hostA, "failed:investigate", "gateway-dial", now.Add(-3*time.Hour))
	ins("tg667-fa2-"+uniq, hostA, "proposed", "", now.Add(-2*time.Hour))
	// host B: failed and never retried ⇒ listed. An OLDER failure outside the lookback is not.
	ins("tg667-fb0-"+uniq, hostB, "failed:investigate", "old", now.Add(-10*24*time.Hour))
	ins("tg667-fb1-"+uniq, hostB, "failed:investigate", "gateway-dial", now.Add(-time.Hour))

	got, err := s.FailedInvestigationsUnretried(ctx, now.Add(-7*24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	refs := map[string]bool{}
	for _, f := range got {
		refs[f.ExternalRef] = true
		if f.ExternalRef == "tg667-fb1-"+uniq && (f.Host != hostB || f.StopReason != "gateway-dial" || f.FailedAt.IsZero()) {
			t.Fatalf("row lost content: %+v", f)
		}
	}
	if refs["tg667-fa1-"+uniq] {
		t.Fatal("a failure followed by a non-failed session on the same (host, rule) is RETRIED and must not be listed")
	}
	if !refs["tg667-fb1-"+uniq] {
		t.Fatal("an unretried failure inside the lookback must be listed")
	}
	if refs["tg667-fb0-"+uniq] {
		t.Fatal("a failure older than the lookback must not be listed")
	}
}

// A hold is ACTIONABLE when its incident fires again after the decision. The session that PRODUCED the hold
// predates decided_at and must not count; a later session on the same (host, rule) does; another host's, an
// expired hold's, and a same-host different-rule session do not.
func TestHoldStoreReportsRedetectionsAfterTheDecision(t *testing.T) {
	dsn := os.Getenv("TG_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("set TG_TEST_POSTGRES_DSN to a migrated database to run the hold redetection read")
	}
	ctx := context.Background()
	if err := Migrate(ctx, dsn); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	p, err := Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	defer p.Close()
	s := NewHoldStore(p)

	now := time.Now().UTC()
	uniq := fmt.Sprintf("%d-%d", os.Getpid(), now.UnixNano())
	hostA, hostB, hostC, rule := "tg667-ra-"+uniq, "tg667-rb-"+uniq, "tg667-rc-"+uniq, "Devices-up/down"
	ins := func(ref, host, rule string, at time.Time) {
		t.Helper()
		if _, err := p.Exec(ctx, `INSERT INTO session_triage (external_ref, host, alert_rule, outcome, stop_reason, created_at)
			VALUES ($1, $2, $3, 'held', '', $4)`, ref, host, rule, at); err != nil {
			t.Fatalf("seed %s: %v", ref, err)
		}
	}
	rec := func(ref, host string, decided, expires time.Time) holds.HoldDecision {
		t.Helper()
		h := holds.HoldDecision{ExternalRef: ref, Host: host, AlertRule: rule, ReasonKey: "actor-attributed-authorized",
			DecidedBy: "tg:fallback-approver", DecidedAt: decided, ExpiresAt: expires}
		if err := s.Record(ctx, h); err != nil {
			t.Fatalf("record %s: %v", ref, err)
		}
		return h
	}
	// A: held 1h ago; its own session 2h ago (before the decision), a re-fire 30m ago and another 10m ago.
	rec("tg667-ra-hold-"+uniq, hostA, now.Add(-time.Hour), now.Add(23*time.Hour))
	ins("tg667-ra-s0-"+uniq, hostA, rule, now.Add(-2*time.Hour))
	ins("tg667-ra-s1-"+uniq, hostA, rule, now.Add(-30*time.Minute))
	ins("tg667-ra-s2-"+uniq, hostA, rule, now.Add(-10*time.Minute))
	// B: held 1h ago; only its own earlier session and a later session on a DIFFERENT rule.
	rec("tg667-rb-hold-"+uniq, hostB, now.Add(-time.Hour), now.Add(23*time.Hour))
	ins("tg667-rb-s0-"+uniq, hostB, rule, now.Add(-2*time.Hour))
	ins("tg667-rb-s1-"+uniq, hostB, "Service-up/down", now.Add(-5*time.Minute))
	// C: an EXPIRED hold with a later session — closed holds are not watched.
	rec("tg667-rc-hold-"+uniq, hostC, now.Add(-48*time.Hour), now.Add(-24*time.Hour))
	ins("tg667-rc-s1-"+uniq, hostC, rule, now.Add(-time.Minute))
	// D: a hold whose OWN terminal session row landed AFTER the decision (a stand-down before propose) — its
	// own session is never its re-detection.
	hostD := "tg667-rd-" + uniq
	rec("tg667-rd-hold-"+uniq, hostD, now.Add(-time.Hour), now.Add(23*time.Hour))
	ins("tg667-rd-hold-"+uniq, hostD, rule, now.Add(-59*time.Minute))

	open, err := s.OpenHolds(ctx, now)
	if err != nil {
		t.Fatal(err)
	}
	seqOf := map[string]int64{}
	for _, h := range open {
		seqOf[h.ExternalRef] = h.Seq
	}
	re, err := s.HoldRedetections(ctx, now)
	if err != nil {
		t.Fatal(err)
	}
	at, ok := re[seqOf["tg667-ra-hold-"+uniq]]
	if !ok {
		t.Fatal("A re-fired after its hold and must be reported")
	}
	if d := now.Add(-10 * time.Minute).Sub(at); d > time.Second || d < -time.Second {
		t.Fatalf("the redetection time is the LATEST re-fire (10m ago), got %s", at)
	}
	if _, ok := re[seqOf["tg667-rb-hold-"+uniq]]; ok {
		t.Fatal("B's only later session is on another rule — not a re-detection of this incident")
	}
	if _, ok := re[seqOf["tg667-rd-hold-"+uniq]]; ok {
		t.Fatal("D's own session row, written after the decision, is not a re-detection of itself")
	}
	for seq := range re {
		for ref, s := range seqOf {
			if s == seq && ref == "tg667-rc-hold-"+uniq {
				t.Fatal("an expired hold is not watched")
			}
		}
	}
}
