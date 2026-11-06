package regime

import (
	"context"
	"testing"
	"time"

	"github.com/territory-grounder/grounder/core/safety"
	"github.com/territory-grounder/grounder/core/verify"
)

// THE DEFERRED VERIFIER'S SITE SCOPE (spec/002 REQ-107 on the async channel). The synchronous interceptor
// and the propose-path scorer adjudicate through verify.ComputeVerdictDetailScoped with the estate site
// authority; the deferred channel kept calling the unscoped author — so an unrelated flap at the OTHER
// site, minutes after an awx-job launch, still scored as that launch's cascade (the ledger-6555 shape, on
// the lane with the WIDEST observation window). These oracles run the REAL verdict author end to end
// through Verify and pin the asymmetry: exclude only what the estate can PROVE is elsewhere; surface
// everything else — and a channel with no authority wired behaves exactly as before (fail closed).

// estateSites is a SiteAuthority over a fixed host→site table (the estate graph's closed vocabulary in
// production); any host not listed is unknown — never excluded.
func estateSites(m map[string]string) verify.SiteAuthority {
	return func(host string) (string, bool) {
		s, ok := m[host]
		return s, ok
	}
}

// establishedEmptyBaseline is a wired, established, empty host arm — so a deviation, if computed, is
// VERIFIED (never withheld), and the site filter alone decides these oracles' outcomes.
func establishedEmptyBaseline(context.Context, time.Time) (map[string]bool, bool) {
	return map[string]bool{}, true
}

// A provably-OTHER-site flap during a deferred verify is background noise, not the launch's cascade:
// with the estate authority knowing BOTH sites (target nl, flapper gr) the real verdict author excludes
// it and the launch adjudicates a clean MATCH.
func TestDeferredCrossSiteFlapIsNotTheLaunchsCascade(t *testing.T) {
	ctx := context.Background()
	av, store, poller, _ := newChannel(t,
		WithObserver(func(context.Context, verify.Prediction) ([]verify.ObservedAlert, bool) {
			return []verify.ObservedAlert{{Host: "dc2lte01", Rule: "Sensor-over-limit", Site: "gr"}}, true
		}),
		WithPreAnomalous(establishedEmptyBaseline),
		WithSiteAuthority(estateSites(map[string]string{"web01": "nl", "dc2lte01": "gr"})))
	reserveAndBind(ctx, t, av, "a-xsite")
	poller.Script("job-a-xsite", JobSuccessful)

	if _, err := av.Verify(ctx, "a-xsite"); err != nil {
		t.Fatalf("Verify: %v", err)
	}
	rec, _ := store.Get(ctx, "a-xsite")
	if !rec.Verified || rec.Verdict != safety.VerdictMatch || !rec.CleanRun() {
		t.Fatalf("an estate-proven other-site flap must not adjudicate as the launch's cascade, got verified=%t verdict=%q clean=%t",
			rec.Verified, rec.Verdict, rec.CleanRun())
	}
}

// The fail-closed half of the asymmetry: a host whose site the estate does NOT know is NEVER excluded —
// a genuine cross-site tunnel cascade (or any host outside the naming convention) still deviates, even
// with the authority wired.
func TestDeferredUnknownSiteHostStillDeviates(t *testing.T) {
	ctx := context.Background()
	av, store, poller, _ := newChannel(t,
		WithObserver(func(context.Context, verify.Prediction) ([]verify.ObservedAlert, bool) {
			return []verify.ObservedAlert{{Host: "chzrh01vps01", Rule: "Device-Down", Site: "ch"}}, true
		}),
		WithPreAnomalous(establishedEmptyBaseline),
		// The authority knows the TARGET's site but nothing about the VPS — the estate makes no claim, so
		// the verdict must make no exclusion (the alert's own self-reported "ch" label must count for nothing).
		WithSiteAuthority(estateSites(map[string]string{"web01": "nl"})))
	reserveAndBind(ctx, t, av, "a-unknown")
	poller.Script("job-a-unknown", JobSuccessful)

	if _, err := av.Verify(ctx, "a-unknown"); err != nil {
		t.Fatalf("Verify: %v", err)
	}
	rec, _ := store.Get(ctx, "a-unknown")
	if !rec.Verified || rec.Verdict != safety.VerdictDeviation {
		t.Fatalf("an unknown-site surprise must still deviate (fail closed), got verified=%t verdict=%q",
			rec.Verified, rec.Verdict)
	}
}

// No estate graph ⇒ nil authority ⇒ behavior unchanged: the same other-site flap that the scoped channel
// excludes still deviates on a channel with no authority wired — nothing is ever excluded without estate
// proof, exactly the pre-change posture.
func TestDeferredNilSiteAuthorityExcludesNothing(t *testing.T) {
	ctx := context.Background()
	av, store, poller, _ := newChannel(t,
		WithObserver(func(context.Context, verify.Prediction) ([]verify.ObservedAlert, bool) {
			return []verify.ObservedAlert{{Host: "dc2lte01", Rule: "Sensor-over-limit", Site: "gr"}}, true
		}),
		WithPreAnomalous(establishedEmptyBaseline),
		WithSiteAuthority(nil)) // nil is ignored — the option must not install an authority
	reserveAndBind(ctx, t, av, "a-noauth")
	poller.Script("job-a-noauth", JobSuccessful)

	if _, err := av.Verify(ctx, "a-noauth"); err != nil {
		t.Fatalf("Verify: %v", err)
	}
	rec, _ := store.Get(ctx, "a-noauth")
	if !rec.Verified || rec.Verdict != safety.VerdictDeviation {
		t.Fatalf("with no site authority the deferred verdict must exclude nothing (unchanged behavior), got verified=%t verdict=%q",
			rec.Verified, rec.Verdict)
	}
}
