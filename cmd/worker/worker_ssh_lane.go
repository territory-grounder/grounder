package main

// The NATIVE-SSH ACTUATION LANE builders, carved out of main()'s composition root (TG-501 LOC-debt paydown):
// nativeSSHLaneFor assembles the regime.Lane that runs an approved operation over SSH behind the safety
// chokepoint (allowlist-scoped units/containers providers); perTargetSSHConfig is its per-target construction
// input; and buildPerTargetSSHLeaf builds the per-target SSH actuator leaf. Pure relocation; the
// manifest_allowlist guard (call-based) pins the lane's allowlist behaviour. Behaviour is unchanged by the move.

import (
	_ "time/tzdata" // embed the IANA zoneinfo DB so time.LoadLocation resolves on distroless (no OS tzdata)

	"context"
	"fmt"
	"log"
	"strings"

	actuation "github.com/territory-grounder/grounder/adapters/actuation"
	"github.com/territory-grounder/grounder/core/config"
	"github.com/territory-grounder/grounder/core/regime"
	"github.com/territory-grounder/grounder/core/safety"
	"github.com/territory-grounder/grounder/core/worldmodel"
	sshactuation "github.com/territory-grounder/grounder/modules/actuation/ssh"
	"github.com/territory-grounder/grounder/modules/bootstrap"
)

// wireActuationRegime constructs the regime resolver (config-not-code rules over the SHARED estate object-
// model), the native-ssh + awx-job lanes, the append-only regime audit writer, and — when an AWX launch
// client is declared — the GLOBAL deferred-verify channel + its poll cron. It fails the boot CLOSED on a
// malformed rule/allowlist (a bad regime mapping must never route a target down an undefined channel) and
// logs the exact posture: regime wired, lanes registered, the awx-job actuator state (real vs fail-closed),
// mode Shadow, may_actuate=false. sshLeaf is the SAME effect leaf the interceptor already wires.
// nativeSSHLaneFor builds the regime native-ssh lane. By DEFAULT it is the STATIC single-host / read-only leaf
// the interceptor already wires (behaviour-preserving). When TG_ACTUATION_SSH_PER_TARGET is set (REQ-1717,
// P3-B2) it instead returns a PER-TARGET lane: each action's effect leaf binds to the action's OWN target host,
// authenticating with the operator's configured ACTUATION identity + key (TG_ACTUATION_SSH_IDENTITY /
// TG_ACTUATION_SSH_KEY — DISTINCT from any read/hostdiag identity, preserving the credential plane-split)
// verified against the fleet-wide TG_ACTUATION_SSH_KNOWN_HOSTS. It stays DORMANT until explicitly armed: the
// flag is OFF by default; the mode chokepoint (Shadow) refuses every Exec until the owner flips mutation on; an
// empty TG_ACTUATION_ALLOWED_UNITS allowlist refuses every unit; and the spec/013 host-match gate (REQ-1219)
// passes only because ActuationHost()==target by construction and stays as defense-in-depth. A per-target build
// refusal (unset identity/key, empty target) surfaces as a GOVERNED refusal at LaneEffect.Apply, never a bypass.
// unitsProvider/containersProvider are the spec/027 REQ-2704 AllowlistProvider seams: the UNION of the
// boot-frozen env allowlist and the operator-ADOPTED manifest entries. They are consulted PER LANE BUILD —
// each action's leaf is constructed at actuation time — so an adopt click takes effect without a restart.
// A nil provider (no manifest store wired) degrades to the env allowlist alone: fail-closed toward the
// grant the operator already authored, never toward a wider one.
//
// PLANE-SCOPED (TG-153): every read below goes through planeEnv. On the TRIAGE plane the per-target flag is
// withheld, so this returns the static (read-only) lane and the per-target closure — which would construct a
// native SSH runner around the actuation key on EVERY action — is never created at all.
func nativeSSHLaneFor(chokepoint *safety.Chokepoint, staticLeaf actuation.Actuator, unitsProvider, containersProvider worldmodel.AllowlistProvider) regime.Lane {
	if !truthyValue(planeEnv("TG_ACTUATION_SSH_PER_TARGET", "")) {
		return regime.NewNativeSSHLane(staticLeaf)
	}
	identity := planeEnv("TG_ACTUATION_SSH_IDENTITY", "")
	keyRef := config.SecretRef(planeEnv("TG_ACTUATION_SSH_KEY", ""))
	knownHosts := planeEnv("TG_ACTUATION_SSH_KNOWN_HOSTS", "")
	allowedUnits := bootstrap.ParseAllowedUnits(planeEnv("TG_ACTUATION_ALLOWED_UNITS", ""))
	allowedContainers := bootstrap.ParseAllowedContainers(planeEnv("TG_ACTUATION_ALLOWED_CONTAINERS", ""))
	log.Printf("actuation regime: native-ssh lane = PER-TARGET (TG_ACTUATION_SSH_PER_TARGET on, REQ-1717) — each action's leaf binds to its own target host via actuation identity %q over the fleet known_hosts; %d allowed unit(s); mutation still gated by the mode chokepoint + allowlist + host-match gate (dormant until an operator arms mutation)", identity, len(allowedUnits))
	return regime.NewNativeSSHLaneFunc(func(ctx context.Context, target string) (actuation.Actuator, error) {
		return buildPerTargetSSHLeaf(ctx, perTargetSSHConfig{
			chokepoint: chokepoint, identity: identity, keyRef: keyRef, knownHosts: knownHosts,
			envUnits: allowedUnits, envContainers: allowedContainers,
			unitsProvider: unitsProvider, containersProvider: containersProvider,
		}, target)
	})
}

// perTargetSSHConfig is the resolved per-target actuation configuration. Extracted as a named type + func
// so the union-reaches-the-leaf property is DIRECTLY testable: the lane's build closure is unexported and
// unreachable from outside core/regime, which left the seam's most important behaviour — that an ADOPTED
// entry actually arrives at the constructed leaf — provable only by signature shape. A RED control proved
// that gap real (neutering the provider inside the closure broke no test and no build), so the logic moved
// here where an oracle can drive it.
type perTargetSSHConfig struct {
	chokepoint                        *safety.Chokepoint
	identity, knownHosts              string
	keyRef                            config.SecretRef
	envUnits, envContainers           []string
	unitsProvider, containersProvider worldmodel.AllowlistProvider
}

// buildPerTargetSSHLeaf constructs THIS action's effect leaf, resolving the operator's grant at build time:
// the env allowlist UNION the adopted manifest entries (REQ-2704). The leaf's own default-deny gate is
// byte-untouched — this function decides only WHAT the operator granted, never whether the gate applies.
func buildPerTargetSSHLeaf(ctx context.Context, cfg perTargetSSHConfig, target string) (actuation.Actuator, error) {
	{
		target = strings.TrimSpace(target)
		if target == "" {
			return nil, fmt.Errorf("empty target host — refusing to build an actuation leaf")
		}
		chokepoint, identity, keyRef, knownHosts := cfg.chokepoint, cfg.identity, cfg.keyRef, cfg.knownHosts
		allowedUnits, allowedContainers := cfg.envUnits, cfg.envContainers
		unitsProvider, containersProvider := cfg.unitsProvider, cfg.containersProvider
		// Fail CLEANLY on an incomplete actuation identity: BuildSSHActuator only rejects an empty HOST or
		// IDENTITY, so a set identity + empty key ref would build a leaf that fails opaquely at connect
		// (not a clean fail-closed refusal). Require both up front so a misconfig is a governed refusal.
		if strings.TrimSpace(identity) == "" || strings.TrimSpace(string(keyRef)) == "" {
			return nil, fmt.Errorf("per-target actuation requires BOTH TG_ACTUATION_SSH_IDENTITY and TG_ACTUATION_SSH_KEY — refusing to build a leaf with an empty identity or key ref")
		}
		// REQ-2704: resolve the operator's grant HERE (env UNION adopted manifest entries). The leaf's
		// default-deny gate is byte-untouched — only WHERE the grant was authored has moved.
		units, containers := allowedUnits, allowedContainers
		if unitsProvider != nil {
			units = unitsProvider(ctx)
		}
		if containersProvider != nil {
			containers = containersProvider(ctx)
		}
		m := bootstrap.BuildSSHActuator(chokepoint, target, identity, sshactuation.NewNativeRunner(knownHosts, keyRef), units, containers)
		if m == nil {
			return nil, fmt.Errorf("actuation leaf could not be built for %q (incomplete actuation config)", target)
		}
		return m, nil
	}
}
