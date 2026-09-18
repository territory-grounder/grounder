package deploy

// THE RUNTIME BRAKE MUST BE ARMABLE ON THE PLANE THAT CAN ACTUATE.
//
// The worker admin surface registers POST /halt (gate.Disable — one-way, it can only turn mutation MORE
// off) only if it can resolve TG_ADMIN_TOKEN_REF. When it cannot, the route is simply never registered:
// one log line, /metrics still served, everything else looks healthy. There is no alert for a brake that
// was never installed.
//
// Measured on dc1tg01, 2026-08-05, with the reference pointed at bao:secret/data/tg/admin#token:
//
//	POST /halt on worker (triage)  -> 401   registered, wants the bearer
//	POST /halt on worker-actuate   -> 404   NOT REGISTERED — no brake at all
//
// Exactly backwards. The triage worker cannot mutate the estate; worker-actuate is the only thing that
// can, and it was the one with no runtime stop.
//
// The cause was the TG-153 credential-plane split working AS DESIGNED: tg-actuate-ro grants read on
// secret/data/tg/actuator and secret/data/tg/proxmox and deliberately nothing else, so the actuation
// worker cannot read secret/data/tg/admin. Two correct safety properties in direct conflict — least
// privilege on the actuation plane, and the brake being present where it matters.
//
// Resolved with a dedicated path rather than by widening the plane: secret/data/tg/halt carries the same
// bearer VALUE (one operator token still works on both planes) and nothing else. Granting tg/admin to the
// actuation plane instead would have handed it the entire admin credential to restore one button.
//
// This guard keeps the reference off any path the actuation plane provably cannot read. It cannot reach
// OpenBao from a unit test, so it asserts the one thing that is checkable here and was the actual defect:
// the deployment template must not point the halt bearer at a plane-scoped path.

import (
	"os"
	"strings"
	"testing"
)

// planeScopedSecretPaths are OpenBao paths the tg-actuate-ro policy does not grant. A halt bearer behind
// any of them silently disarms POST /halt on the actuation worker.
//
// tg/admin is the one that actually happened. The others are listed because they are the obvious next
// choices for "somewhere secure to put the admin token" and each fails the same way.
var planeScopedSecretPaths = []string{
	"tg/admin",
	"tg/operator",
	"tg/session",
	"tg/actuator", // readable by actuate but NOT by triage — the same defect mirrored
}

func TestHaltBearerIsNotOnAPlaneScopedPath(t *testing.T) {
	raw, err := os.ReadFile(".env.example")
	if err != nil {
		t.Fatalf("read deploy/.env.example: %v", err)
	}

	var ref string
	for _, line := range strings.Split(string(raw), "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "#") {
			continue // the commented example line above the real one is prose, not configuration
		}
		if strings.HasPrefix(trimmed, "TG_ADMIN_TOKEN_REF=") {
			ref = strings.TrimPrefix(trimmed, "TG_ADMIN_TOKEN_REF=")
		}
	}
	if ref == "" {
		t.Fatal("deploy/.env.example sets no TG_ADMIN_TOKEN_REF. Unresolved means POST /halt is never " +
			"registered on ANY worker — the runtime kill-switch would be absent fleet-wide, and the only " +
			"symptom is one log line at boot. This guard read nothing and must not pass.")
	}

	for _, bad := range planeScopedSecretPaths {
		if strings.Contains(ref, bad) {
			t.Errorf("the halt bearer resolves from %q, which is on the plane-scoped path %q.\n"+
				"One credential plane cannot read it, so POST /halt is never registered on that worker and "+
				"the brake is silently absent there — measured as a 404 on worker-actuate while triage "+
				"returned 401. Put the bearer on a path BOTH plane policies grant (secret/data/tg/halt) "+
				"rather than widening a plane policy to reach an admin credential.", ref, bad)
		}
	}
}
