package db

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/territory-grounder/grounder/core/estate"
)

// TG-188 — the SourceChaos feed reads the injection ledger (injected_fault) and joins the alert stream to see
// what ELSE failed when a host we deliberately broke went down. The query IS the deliverable, so this runs
// against a REAL migrated Postgres (gated on TG_TEST_DSN like every axis fixture): it exercises the cascade
// window bound, the same-host exclusion, the distinct-injection count, and the lookback bound. The fixture is
// small and hand-verifiable — two injections of rootA and one of rootB, with downstream alerts placed just
// inside and just outside the windows.
func TestChaosCascades_GroundTruthCascadesFromTheLedger(t *testing.T) {
	dsn := skipWithoutDB(t)
	ctx := context.Background()
	p, err := Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer p.Close()

	uniq := fmt.Sprintf("chaos-%d", os.Getpid())
	rootA, rootB := uniq+"-rootA", uniq+"-rootB"
	down1, down2, down3, down4 := uniq+"-down1", uniq+"-down2", uniq+"-down3", uniq+"-down4"
	base := time.Date(2026, 7, 29, 0, 0, 0, 0, time.UTC)

	hosts := []string{rootA, rootB, down1, down2, down3, down4}
	defer func() {
		for _, h := range hosts {
			_, _ = p.Exec(ctx, `DELETE FROM ingest_alert WHERE host = $1`, h)
			_, _ = p.Exec(ctx, `DELETE FROM injected_fault WHERE host = $1`, h)
		}
	}()

	mustExec := func(q string, args ...any) {
		t.Helper()
		if _, err := p.Exec(ctx, q, args...); err != nil {
			t.Fatalf("seed %q: %v", q, err)
		}
	}
	fault := func(host string, at time.Time) {
		mustExec(`INSERT INTO injected_fault (host, fault_type, injected_at) VALUES ($1,'device-down',$2)`, host, at)
	}
	alertRule := func(host, rule string, at time.Time) {
		ref := fmt.Sprintf("%s-%s-%d", uniq, host, at.Unix())
		mustExec(`INSERT INTO ingest_alert (external_ref, source_type, source_id, alert_rule, severity, host, site, summary, received_at)
		          VALUES ($1,'seed',$1,$2,'critical',$3,'dc1','seeded',$4)`, ref, rule, host, at)
	}
	alert := func(host string, at time.Time) { alertRule(host, "Device-Down", at) }

	// TWO injections of rootA (base, base+1h) and ONE of rootB (base+2h).
	fault(rootA, base)
	fault(rootA, base.Add(time.Hour))
	fault(rootB, base.Add(2*time.Hour))

	// down1 follows BOTH rootA injections (inside each 10-min window) ⇒ (rootA,down1) injections=2, latest=base+1h.
	alert(down1, base.Add(3*time.Minute))
	alert(down1, base.Add(time.Hour+4*time.Minute))
	// down2 alerts 20 min after rootA's FIRST injection — OUTSIDE the 10-min window ⇒ must NOT be counted.
	alert(down2, base.Add(20*time.Minute))
	// rootA alerts on ITSELF inside its window — the same-host detection signal (axis A1), EXCLUDED here.
	alert(rootA, base.Add(2*time.Minute))
	// down3 follows rootB inside its window ⇒ (rootB,down3) injections=1.
	alert(down3, base.Add(2*time.Hour+5*time.Minute))
	// down4 follows rootB with TWO distinct rules + one BLANK rule (TG-188 chaos-measured ExpectedAlerts):
	// observed_rules must be the DISTINCT, SORTED, non-blank set — the blank row is real cascade evidence for
	// the pair (it still counts toward injections/delay) but contributes no rule.
	alertRule(down4, "Service-up/down", base.Add(2*time.Hour+3*time.Minute))
	alertRule(down4, "Device-Down", base.Add(2*time.Hour+4*time.Minute))
	alertRule(down4, "", base.Add(2*time.Hour+6*time.Minute))

	store := NewAxisReadStore(p)
	cascades, err := store.ChaosCascades(ctx, base.Add(-time.Minute), DefaultChaosCascadeWindow)
	if err != nil {
		t.Fatalf("ChaosCascades: %v", err)
	}

	// Isolate our fixture — the shared test DB may hold other injections.
	got := map[string]estate.ChaosCascade{}
	for _, c := range cascades {
		if strings.HasPrefix(c.Root, uniq) {
			got[c.Root+"|"+c.Downstream] = c
		}
	}
	if len(got) != 3 {
		t.Fatalf("got %d fixture cascades, want 3 (rootA→down1, rootB→down3, rootB→down4); got=%v", len(got), got)
	}

	// rootA → down1: two DISTINCT injections followed, latest = base+1h.
	if c, ok := got[rootA+"|"+down1]; !ok {
		t.Error("missing rootA→down1 cascade")
	} else {
		if c.Injections != 2 {
			t.Errorf("rootA→down1 injections = %d, want 2 (distinct injections the downstream followed)", c.Injections)
		}
		if c.LatestInjectedAt.Unix() != base.Add(time.Hour).Unix() {
			t.Errorf("rootA→down1 latest = %s, want %s (the most recent injection)", c.LatestInjectedAt.UTC(), base.Add(time.Hour).UTC())
		}
		// TG-188 slice 2b: ground-truth mean propagation delay = mean of the two cascade delays — down1 alerted
		// 3min (180s) after injection 1 and 4min (240s) after injection 2, so (180+240)/2 = 210s.
		if d := c.MeanDelaySeconds; d < 209.5 || d > 210.5 {
			t.Errorf("rootA→down1 MeanDelaySeconds = %v, want ~210 (mean of the 180s + 240s cascade delays)", d)
		}
	}
	// rootB → down3: single injection.
	if c, ok := got[rootB+"|"+down3]; !ok {
		t.Error("missing rootB→down3 cascade")
	} else {
		if c.Injections != 1 {
			t.Errorf("rootB→down3 injections = %d, want 1", c.Injections)
		}
		// down3 alerted 5min (300s) after rootB's single injection.
		if d := c.MeanDelaySeconds; d < 299.5 || d > 300.5 {
			t.Errorf("rootB→down3 MeanDelaySeconds = %v, want ~300 (the 5min cascade delay)", d)
		}
	}
	// TG-188 chaos-measured ExpectedAlerts: rootA→down1 fired the SAME rule twice ⇒ DISTINCT collapses to one;
	// rootB→down4 fired two rules + a blank ⇒ sorted non-blank pair, blank FILTERed out (the mutation that
	// drops the FILTER puts "" into the set and this reddens).
	if c, ok := got[rootA+"|"+down1]; ok {
		if len(c.ObservedRules) != 1 || c.ObservedRules[0] != "Device-Down" {
			t.Errorf("rootA→down1 ObservedRules = %v, want [Device-Down] (DISTINCT over two same-rule alerts)", c.ObservedRules)
		}
	}
	if c, ok := got[rootB+"|"+down4]; !ok {
		t.Error("missing rootB→down4 cascade")
	} else {
		want := []string{"Device-Down", "Service-up/down"}
		if len(c.ObservedRules) != 2 || c.ObservedRules[0] != want[0] || c.ObservedRules[1] != want[1] {
			t.Errorf("rootB→down4 ObservedRules = %v, want %v (sorted, DISTINCT, blank rule FILTERed out)", c.ObservedRules, want)
		}
	}
	// KILLING MUTATION (window bound): down2 alerted 20 min out — outside the 10-min window, must not count.
	if _, ok := got[rootA+"|"+down2]; ok {
		t.Error("rootA→down2 present — an alert 20 min after injection is outside the 10-min cascade window; " +
			"widening the window is the mutation this kills")
	}
	// KILLING MUTATION (same-host exclusion): rootA's own alert must not become a self-cascade.
	if _, ok := got[rootA+"|"+rootA]; ok {
		t.Error("rootA→rootA present — same-host detection is the axis-A1 signal, excluded from the cascade join")
	}

	// KILLING MUTATION (lookback bound): a `since` after every injection returns none of our fixture.
	future, err := store.ChaosCascades(ctx, base.Add(3*time.Hour), DefaultChaosCascadeWindow)
	if err != nil {
		t.Fatalf("ChaosCascades(future): %v", err)
	}
	for _, c := range future {
		if strings.HasPrefix(c.Root, uniq) {
			t.Errorf("cascade %s→%s returned with `since` after all injections — the lookback bound is not applied", c.Root, c.Downstream)
		}
	}
}

// TG-188 slice 2c — the chaos feed also learns the downstream's RECOVERY time / MTTR: for each cascade, the
// FIRST recovery transition (ingest_transition) for the DOWNSTREAM host at/after its cascade alert, within
// DefaultChaosRecoveryWindow, measured as (recovery − alert). The query IS the deliverable, so this runs against
// a REAL migrated Postgres. Three pairs isolate the three load-bearing predicates the LATERAL adds, each its own
// killing mutation.
func TestChaosCascades_RecoveryTimeFromTransitions(t *testing.T) {
	dsn := skipWithoutDB(t)
	ctx := context.Background()
	p, err := Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer p.Close()

	uniq := fmt.Sprintf("chaosrec-%d", os.Getpid())
	rootA, rootB, rootC := uniq+"-rootA", uniq+"-rootB", uniq+"-rootC"
	downA, downB, downC := uniq+"-downA", uniq+"-downB", uniq+"-downC"
	wrong := uniq + "-wrong"
	base := time.Date(2026, 7, 30, 0, 0, 0, 0, time.UTC)

	hosts := []string{rootA, rootB, rootC, downA, downB, downC, wrong}
	defer func() {
		for _, h := range hosts {
			_, _ = p.Exec(ctx, `DELETE FROM ingest_alert WHERE host = $1`, h)
			_, _ = p.Exec(ctx, `DELETE FROM injected_fault WHERE host = $1`, h)
			_, _ = p.Exec(ctx, `DELETE FROM ingest_transition WHERE host = $1`, h)
		}
	}()

	mustExec := func(q string, args ...any) {
		t.Helper()
		if _, err := p.Exec(ctx, q, args...); err != nil {
			t.Fatalf("seed %q: %v", q, err)
		}
	}
	fault := func(host string, at time.Time) {
		mustExec(`INSERT INTO injected_fault (host, fault_type, injected_at) VALUES ($1,'device-down',$2)`, host, at)
	}
	alert := func(host string, at time.Time) {
		ref := fmt.Sprintf("%s-a-%s-%d", uniq, host, at.Unix())
		mustExec(`INSERT INTO ingest_alert (external_ref, source_type, source_id, alert_rule, severity, host, site, summary, received_at)
		          VALUES ($1,'seed',$1,'Device-Down','critical',$2,'dc1','seeded',$3)`, ref, host, at)
	}
	recovery := func(host string, at time.Time) {
		ref := fmt.Sprintf("%s-r-%s-%d", uniq, host, at.Unix())
		mustExec(`INSERT INTO ingest_transition (external_ref, kind, host, site, alert_rule, received_at)
		          VALUES ($1,'recovery',$2,'dc1','Device-Down',$3)`, ref, host, at)
	}

	// PAIR A — the POSITIVE + the after-alert guard: rootA injected at base; downA cascades at base+3min and
	// RECOVERS at base+18min ⇒ observed MTTR = 15min = 900s. A spurious recovery at base+1min (BEFORE downA
	// alerted) is planted so the after-alert guard is load-bearing: min() must pick base+18min, not base+1min —
	// dropping `r.received_at >= a.received_at` would yield a NEGATIVE recovery (base+1min − base+3min = -120s).
	fault(rootA, base)
	alert(downA, base.Add(3*time.Minute))
	recovery(downA, base.Add(time.Minute))    // before the alert — must be ignored
	recovery(downA, base.Add(18*time.Minute)) // 900s after the alert — the counted one

	// PAIR B — the HOST guard: rootB→downB cascades, but downB never recovers; a recovery on a DIFFERENT host
	// (wrong) sits in-window. Dropping `r.host = a.host` would attribute it to downB ⇒ MTTR must stay 0.
	fault(rootB, base)
	alert(downB, base.Add(3*time.Minute))
	recovery(wrong, base.Add(10*time.Minute))

	// PAIR C — the WINDOW bound: rootC→downC cascades and downC recovers, but 7h later — outside the 6h recovery
	// window. Widening/removing DefaultChaosRecoveryWindow would count it ⇒ MTTR must stay 0.
	fault(rootC, base)
	alert(downC, base.Add(3*time.Minute))
	recovery(downC, base.Add(7*time.Hour))

	store := NewAxisReadStore(p)
	cascades, err := store.ChaosCascades(ctx, base.Add(-time.Minute), DefaultChaosCascadeWindow)
	if err != nil {
		t.Fatalf("ChaosCascades: %v", err)
	}
	got := map[string]estate.ChaosCascade{}
	for _, c := range cascades {
		if strings.HasPrefix(c.Root, uniq) {
			got[c.Root+"|"+c.Downstream] = c
		}
	}

	// All three cascade pairs must still be produced — the recovery LATERAL must not drop or multiply rows.
	for _, k := range []string{rootA + "|" + downA, rootB + "|" + downB, rootC + "|" + downC} {
		if c, ok := got[k]; !ok {
			t.Errorf("missing cascade %s (the recovery LATERAL dropped a base cascade row)", k)
		} else if c.Injections != 1 {
			t.Errorf("%s injections = %d, want 1 (the recovery LATERAL multiplied rows)", k, c.Injections)
		}
	}

	// PAIR A: MTTR = 900s (the first recovery AT/AFTER the alert, not the earlier one before it).
	if c := got[rootA+"|"+downA]; c.MeanRecoverySeconds < 899.5 || c.MeanRecoverySeconds > 900.5 {
		t.Errorf("rootA→downA MeanRecoverySeconds = %v, want ~900 (recovery 15min after the cascade alert; a "+
			"negative value means the after-alert guard was dropped)", c.MeanRecoverySeconds)
	}
	// PAIR B: a recovery on another host is NOT this downstream's recovery.
	if c := got[rootB+"|"+downB]; c.MeanRecoverySeconds != 0 {
		t.Errorf("rootB→downB MeanRecoverySeconds = %v, want 0 (a recovery on a DIFFERENT host must not be "+
			"attributed — the host guard is dropped)", c.MeanRecoverySeconds)
	}
	// PAIR C: a recovery past the window is unobserved for this cascade, not counted.
	if c := got[rootC+"|"+downC]; c.MeanRecoverySeconds != 0 {
		t.Errorf("rootC→downC MeanRecoverySeconds = %v, want 0 (a recovery 7h out is beyond the 6h window — the "+
			"window bound is widened/removed)", c.MeanRecoverySeconds)
	}
}
