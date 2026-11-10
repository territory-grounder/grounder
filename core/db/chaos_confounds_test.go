package db

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"
)

// TG-655 (2026-11-07): during the G3 soak (up to six concurrent injections, then a post-outage estate) the chaos
// learner minted 0.90 "ground truth" edges from coincidence: pool guests "depending on" each other because each
// alerted for its OWN injected fault inside another's cascade window, and a hypervisor "depending on" a guest of
// another node because it was already flapping. Both confounds are excluded; a genuine cascade still counts.
func TestChaosCascades_ExcludesConcurrentInjectionAndAmbientNoise(t *testing.T) {
	dsn := skipWithoutDB(t)
	ctx := context.Background()
	p, err := Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer p.Close()

	uniq := fmt.Sprintf("chaosconf-%d", os.Getpid())
	root := uniq + "-root"
	busy, flappy, real, oldfault := uniq+"-busy", uniq+"-flappy", uniq+"-real", uniq+"-oldfault"
	base := time.Date(2026, 7, 30, 0, 0, 0, 0, time.UTC)
	hosts := []string{root, busy, flappy, real, oldfault}
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
	faultUntil := func(host string, at, restored time.Time) {
		mustExec(`INSERT INTO injected_fault (host, fault_type, injected_at, restored_at) VALUES ($1,'container-down',$2,$3)`, host, at, restored)
	}
	alert := func(host, rule string, at time.Time) {
		ref := fmt.Sprintf("%s-%s-%d", uniq, host, at.UnixNano())
		mustExec(`INSERT INTO ingest_alert (external_ref, source_type, source_id, alert_rule, severity, host, site, summary, received_at)
		          VALUES ($1,'seed',$1,$2,'critical',$3,'dc1','seeded',$4)`, ref, rule, host, at)
	}

	// The root is injected at base.
	faultUntil(root, base, base.Add(45*time.Minute))
	// busy has ITS OWN fault injected 2 min earlier and still active: its alert inside root's window is its own.
	faultUntil(busy, base.Add(-2*time.Minute), base.Add(43*time.Minute))
	alert(busy, "Service-up/down", base.Add(3*time.Minute))
	// flappy already raised the same rule 20 min BEFORE the injection: ambient noise.
	alert(flappy, "Device-Down", base.Add(-20*time.Minute))
	alert(flappy, "Device-Down", base.Add(4*time.Minute))
	// oldfault's own fault was restored hours earlier (outside the tail): it is NOT explained by it.
	faultUntil(oldfault, base.Add(-5*time.Hour), base.Add(-4*time.Hour))
	alert(oldfault, "Service-up/down", base.Add(5*time.Minute))
	// real: no fault of its own, quiet before: a genuine cascade (the positive control).
	alert(real, "Service-up/down", base.Add(3*time.Minute))
	// An alert naming NO host inside the window is never a dependency.
	blankRef := fmt.Sprintf("%s-blank-%d", uniq, base.UnixNano())
	mustExec(`INSERT INTO ingest_alert (external_ref, source_type, source_id, alert_rule, severity, host, site, summary, received_at)
	          VALUES ($1,'seed',$1,'Service-up/down','critical','','dc1',$2,$3)`, blankRef, uniq+"-blank", base.Add(2*time.Minute))
	defer func() { _, _ = p.Exec(ctx, `DELETE FROM ingest_alert WHERE external_ref = $1`, blankRef) }()

	cascades, err := NewAxisReadStore(p).ChaosCascades(ctx, base.Add(-time.Minute), DefaultChaosCascadeWindow)
	if err != nil {
		t.Fatalf("ChaosCascades: %v", err)
	}
	got := map[string]bool{}
	for _, c := range cascades {
		if strings.HasPrefix(c.Root, uniq) {
			got[c.Downstream] = true
		}
	}
	if !got[real] || !got[oldfault] {
		t.Fatalf("genuine cascades must still count (real, and a host whose own fault ended hours ago), got %v", got)
	}
	if got[busy] {
		t.Errorf("busy alerted for its OWN active injected fault: a concurrent-injection confound, not a cascade (got %v)", got)
	}
	if got[""] {
		t.Errorf("a blank downstream host must never become a cascade (got %v)", got)
	}
	if got[flappy] {
		t.Errorf("flappy raised the same rule before the injection: ambient noise, not a cascade (got %v)", got)
	}
}
