package systemd

import "github.com/territory-grounder/grounder/modules/desc"

// Descriptor publishes the systemd discovery probe's configuration schema so the console GENERATES its
// dialog rather than hand-rendering one that drifts from the binary.
//
// EVERY FIELD HERE IS EffectRestart, and that is not laziness. cmd/worker/main.go:1234-1236 reads all three
// keys once and hands them to newDiscoveryRunner; the host set is then frozen in the runner's allowlist and
// in the Source built over it. Nothing re-reads the environment, so a save is durable and INERT until the
// worker restarts — the dialog must say that rather than report a success it did not achieve.
//
// THIS MODULE HAS NO SECRET LANE. The SSH credential for each host is resolved per read by the audited
// credential resolver, not held by this module; giving the probe a secret of its own would create a second,
// unaudited path to a host credential for a component whose entire point is that it only reads.
func Descriptor() desc.Descriptor {
	return desc.Descriptor{
		Surface:    "discovery",
		SourceType: SourceType,
		Title:      "systemd unit discovery",
		Summary: "Enumerates the services each declared host actually runs, so an operator can ADOPT a unit " +
			"as an actuation target instead of hand-typing it into an allowlist. Discovery proposes, adoption " +
			"grants, and the leaf default-deny gate still refuses everything it was not handed — nothing " +
			"configured here can widen actuation.",
		Fields: []desc.Field{
			{
				// AUTHORITY. This is not a hint about where to look: the runner REFUSES any host not on this
				// list, so the list is the boundary itself. Adding a host grants TG a credentialed session on
				// that machine, which is a trust-boundary move and must not render as an ordinary text box.
				//
				// The pattern is the runner's own host-label rule (cmd/worker/discovery_runner.go:43). A host
				// that fails it is dropped at construction WITHOUT comment, so validating it in the dialog is
				// the difference between "refused, with a reason" and "quietly never probed".
				Name: "hosts", EnvKey: "TG_DISCOVERY_SYSTEMD_HOSTS", Label: "Hosts to enumerate",
				Help: "Comma-separated hosts TG opens a READ-ONLY ssh session to and runs " +
					"`systemctl list-units --type=service` on (names are matched lowercased). Empty means the " +
					"probe is not built at all: no unit is ever drafted and nothing is offered for adoption. " +
					"A host absent from this list is refused by the runner even if something else names it. " +
					"THIS DIALOG SETS NO CREDENTIAL: the per-host ssh identity is resolved at each read by the " +
					"audited credential engine, so a host it cannot answer for is refused (fail closed) and " +
					"its units go missing rather than reading as none.",
				Type: desc.TypeIDList, Security: desc.SecAuthority, Effect: desc.EffectRestart,
				Pattern: `^[a-zA-Z0-9][a-zA-Z0-9._-]{0,99}$`, MaxItems: 64, MaxLen: 100,
			},
			{
				// AUTHORITY: this file decides which host keys are trusted. Point it at the wrong file and TG
				// reads its estate from whatever answered — a wrong fact is worse than a missing one, and the
				// operator can never see the difference downstream.
				Name: "known_hosts", EnvKey: "TG_DISCOVERY_KNOWN_HOSTS", Label: "known_hosts path",
				Help: "Path to the known_hosts file that verifies each host's SSH key before a read. Empty " +
					"means every discovery read is REFUSED rather than performed unverified — the failure " +
					"direction is a missing observation, never an unverified host. ONE SETTING, TWO DIALOGS: " +
					"a single transport serves both probes, so this and the timeout below are the same values " +
					"the docker probe shows — changing them here changes them for docker too.",
				Type: desc.TypeText, Security: desc.SecAuthority, Effect: desc.EffectRestart, MaxLen: 512,
			},
			{
				Name: "timeout", EnvKey: "TG_DISCOVERY_TIMEOUT", Label: "Per-host read timeout",
				Help: "How long one host's enumeration may take before it is abandoned (default 15s). A host " +
					"that times out is reported loudly and the others still contribute, so one unreachable " +
					"machine degrades to \"its units are missing\" and never to a silent \"this host runs " +
					"nothing\". Shared with the docker probe, as above — one value, both dialogs.",
				Type: desc.TypeDuration, Security: desc.SecOrdinary, Effect: desc.EffectRestart, MaxLen: 32,
			},
		},
		Test: desc.TestSpec{
			Verb:     "run `systemctl list-units --type=service` on the declared hosts and count the units returned",
			Mutating: false,
		},
	}
}
