package healthchecks

import "github.com/territory-grounder/grounder/modules/desc"

// Descriptor publishes the Healthchecks.io dead-man switch's configuration schema so the console can
// GENERATE its dialog rather than hand-render one that drifts from the binary.
//
// Effect is stated per field and it is not decoration. healthchecks.go:68 resolves the check reference
// inside ping(), on every heartbeat, so a check id written to the lane below takes effect on the NEXT ping
// with no restart. The ping host is captured at construction — bootstrap.RegisterConfiguredObservability
// builds the Module once from TG_HEALTHCHECKS_URL at boot — so a saved host is durable but inert until the
// worker restarts, and the dialog must say that instead of implying success.
//
// This module is the OUT-OF-BAND watchdog: a wedged control plane cannot page anyone from the inside, so
// misconfiguring it does not degrade a feature, it removes the only observer that would notice TG went
// quiet. Both fields are worded for that stake.
//
// WHAT DRIVES THE HEARTBEAT, and why it is named in the help rather than rendered as a field. The ping is
// not on its own timer: Export calls Ping, and the only production caller of Export is the worker's shared
// telemetry loop (cmd/worker/main.go:1082), gated by the platform-wide TG_OBSERVABILITY_EXPORT_INTERVAL and
// OFF BY DEFAULT. So a fully configured dead-man switch pings NOTHING until that interval is set, and once
// set it is the ping cadence — longer than the check's grace period and the check flaps on a healthy TG.
// The interval is not a field here because it is shared: a save under "Healthchecks.io" would change every
// other exporter's cadence too.
func Descriptor() desc.Descriptor {
	return desc.Descriptor{
		Surface:    "observability",
		SourceType: SourceType,
		Title:      "Healthchecks.io dead-man switch",
		Summary: "Pings an external dead-man check each time the worker's shared telemetry export runs. If " +
			"TG goes quiet the alert is raised by Healthchecks.io, on infrastructure TG cannot wedge.",
		Fields: []desc.Field{
			{
				Name: "ping_host", EnvKey: "TG_HEALTHCHECKS_URL", Label: "Ping host",
				Help: "Base URL heartbeats are sent to, e.g. https://hc-ping.com (or your self-hosted " +
					"instance). Leave it empty and the dead-man switch is not registered at all: TG can go " +
					"silent with nothing outside it watching, and nothing reports an error. Setting it is " +
					"not sufficient either — the heartbeat rides the worker's platform-wide " +
					"TG_OBSERVABILITY_EXPORT_INTERVAL, which is off by default and, once set, IS the ping " +
					"cadence: make it comfortably shorter than the check's period or a healthy TG flaps.",
				Type: desc.TypeURL, Security: desc.SecOrdinary, Effect: desc.EffectRestart,
				Required: true, MaxLen: 512,
			},
			{
				Name: "check_ref", EnvKey: "TG_HEALTHCHECKS_CHECK_REF", Label: "Check-id reference",
				Help: "Where the dead-man check's uuid is read from (default env:HEALTHCHECKS_UUID). " +
					"Displayed for provenance: set the check id itself below, not this pointer.",
				Type: desc.TypeSecretRef, Security: desc.SecOrdinary, Effect: desc.EffectReadOnly, MaxLen: 512,
			},
			{
				// The check id is a SECRET, not an identifier that merely happens to be a uuid: whoever holds
				// it can ping the check, and a check being pinged by someone else is a dead control plane
				// that still reads as alive. Classifying it as ordinary text would put it in the config store
				// and into ledger rows.
				Name: "check_id", Label: "Check id",
				Help: "The uuid of the Healthchecks.io check to heartbeat. Treat it as a credential: anyone " +
					"who has it can ping the check, and a check pinged by anything other than TG makes a dead " +
					"control plane read as alive. Write-only: stored in the secret backend, never read back.",
				Type: desc.TypeSecretValue, Security: desc.SecSecret, Effect: desc.EffectLive, MaxLen: 512,
			},
		},
		// DERIVED, never declared: a module may not name its own secret path (desc.Validate refuses it).
		// TG_HEALTHCHECKS_CHECK_REF must point HERE — that pointer is read at boot and nothing rewrites it at
		// runtime, so adopting the prefix is a one-time change to the reference. Every rotation after that is
		// a Save, with no OpenBao or .env work.
		Secret: desc.SecretLane{KVPath: desc.ModuleSecretPath("observability", SourceType), Field: "check_id"},
		Test: desc.TestSpec{
			// DELIBERATELY NOT A PING. The module's only outbound call resets the dead-man timer, so a Test
			// that "just pings" would silence a genuinely missed heartbeat from a settings dialog — the alert
			// would clear itself and nobody would learn the control plane had been quiet.
			Verb: "resolve the check id and confirm the ping host is reachable — the check itself is NOT " +
				"pinged, because that would reset the dead-man timer and silence a real alert",
			Mutating: false,
		},
	}
}
