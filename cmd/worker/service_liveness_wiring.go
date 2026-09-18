package main

// Service-liveness necessity reader (TG-464 close-out) + the estate sibling reader relocated from the
// main() Deps literal (the TG-501 ratchet: new wiring may not grow the god-file, so the hook that adds
// ServiceActive pays for itself by extracting an existing inline closure).

import (
	"context"
	"strings"

	"github.com/territory-grounder/grounder/core/config"
	"github.com/territory-grounder/grounder/core/estate"
	sshactuation "github.com/territory-grounder/grounder/modules/actuation/ssh"
)

// serviceActiveReader builds the rollback necessity probe's SERVICE lane (runner.Deps.ServiceActive): a
// positive `systemctl is-active <unit>` read over the ACTUATION SSH identity — the one surface the actuate
// plane can read where the LibreNMS alert lane is 403-scoped-out (TG-461), and therefore the only honest
// pre-effect answer to "is the forward service fix still holding?" for the manual rollback's eligible
// classes. It deliberately reuses the actuation leaf's OWN transport (a read-only ssh module: no gate, so
// it structurally cannot resolve a mutating command) so host naming, host-key verification, quoting, and
// the host guard's allowlist grammar are byte-identical to what the forward actuation already proves live —
// tools/guardallow ships the matching allow line from the same exported argv shape.
//
// Any of the three identity inputs missing ⇒ nil (the lane is unwired and the probe falls through, the
// pre-TG-464 posture). On the triage plane the credential plane withholds TG_ACTUATION_SSH_KEY, so this is
// structurally nil there; only the actuation plane builds a reader.
func serviceActiveReader(knownHosts, identity string, keyRef config.SecretRef) func(ctx context.Context, host, unit string) (active bool, ok bool) {
	identity = strings.TrimSpace(identity)
	knownHosts = strings.TrimSpace(knownHosts)
	if identity == "" || knownHosts == "" || strings.TrimSpace(string(keyRef)) == "" {
		return nil
	}
	run := sshactuation.NewNativeRunner(knownHosts, keyRef)
	return func(ctx context.Context, host, unit string) (bool, bool) {
		host, unit = strings.TrimSpace(host), strings.TrimSpace(unit)
		if host == "" || unit == "" {
			return false, false
		}
		mod := sshactuation.New(host, identity, run) // read-only module: no WithMutation, no gate
		res, err := mod.Exec(ctx, sshactuation.ServiceActiveProbeArgv(unit), nil)
		if err != nil {
			return false, false // dial/host-key/auth — state unestablished, fail closed (TG-182)
		}
		return serviceActiveFromExit(res.ExitCode)
	}
}

// serviceActiveFromExit maps a COMPLETED `systemctl is-active` run to the probe contract: 0 = active;
// 3 = inactive/failed and 4 = no such unit (both a finished read whose answer is "nothing running").
// Anything else — the host guard's denial (42), a transport-mangled 255 — is an UNESTABLISHED state,
// never "inactive": a guard denial mistaken for inactive would refuse the rollback with the WRONG reason
// ("nothing to undo") and mask the misconfiguration forever, where fail-closed-unreadable surfaces it.
func serviceActiveFromExit(code int) (active bool, ok bool) {
	switch code {
	case 0:
		return true, true
	case 3, 4:
		return false, true
	default:
		return false, false
	}
}

// siblingsOfReader adapts the live estate holder to the GateActivity sibling-corroboration seam
// (axis A2 blast-radius precision): a host's estate co-tenants, so the alert-class common-cause gate
// is kept ONLY when >=2 co-tenants alert too and an isolated hosted-guest down does not fan a
// speculative 26-54-host sibling cascade. Reads live state; nil-safe (fail-open in the activity).
// Relocated verbatim from the main() Deps literal (TG-501 ratchet offset for ServiceActive).
func siblingsOfReader(holder *estate.Holder) func(host string) []string {
	return func(host string) []string {
		g := holder.Graph()
		e, ok := g.Resolve(host)
		if !ok {
			return nil
		}
		var out []string
		for _, imp := range g.Siblings(e) {
			out = append(out, imp.Entity.Name)
		}
		return out
	}
}
