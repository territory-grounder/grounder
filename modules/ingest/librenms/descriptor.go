package librenms

import "github.com/territory-grounder/grounder/modules/desc"

// Descriptor publishes LibreNMS's configuration schema so the console can GENERATE its dialog rather than
// hand-render one that drifts from the binary.
//
// Effect is stated per field and it is not decoration. Every env key below is read ONCE at boot —
// cmd/worker/main.go re-parses TG_LIBRENMS_DEPLOYMENTS at five independent wiring points (agent tools, the
// ingest capability registry, estate topology, the alert poller, the falsifiability observer) and
// cmd/grounder/main.go reads it again to gate the front door; every one of them captures the parsed list in
// the constructed module, so a saved change is durable but INERT until the worker restarts. The dialog must
// say that rather than implying success. The one genuinely live value is the push bearer:
// core/db.SourceResolver resolves the stored reference on EVERY /v1/ingest/librenms request, so a rotation
// lands on the next push.
//
// The per-deployment API tokens are deliberately NOT settable here. They are references embedded in the
// deployment rows — one per site — and the secret lane holds exactly one value. Offering a single "token"
// box for an N-site list would write a credential that no deployment reads: a Save that reports success and
// changes nothing, which is the defect this whole surface exists to remove.
func Descriptor() desc.Descriptor {
	return desc.Descriptor{
		Surface:    "ingest",
		SourceType: SourceType,
		Title:      "LibreNMS",
		Summary: "Device and service alerts from one or more LibreNMS servers — by push to " +
			"/v1/ingest/librenms, with an opt-in read-only pull — plus the topology TG builds the estate " +
			"graph from and the read-only device tools the agent investigates with.",
		Fields: []desc.Field{
			{
				// The whole capability hangs off this one string. With it empty LibreNMS is not a
				// capability in this deployment: the front door rejects /v1/ingest/librenms (INV-17), the
				// estate graph loses its topology source, the agent loses its device tools, and the
				// falsifiability observer honestly reports zeros. Nothing is compiled in.
				Name: "deployments", EnvKey: "TG_LIBRENMS_DEPLOYMENTS", Label: "Deployments",
				Help: "One row per LibreNMS server as site|https://base-url|token-ref[|timezone], rows " +
					"separated by ';' — e.g. \"nl|https://nms.nl.example|env:LIBRENMS_NL_TOKEN;" +
					"gr|https://nms.gr.example|env:LIBRENMS_GR_TOKEN|Europe/Athens\". The token is a " +
					"REFERENCE (env:/file:/bao:), never a literal. The timezone is the zone the server " +
					"renders its alert timestamps in (omit it only for a UTC server): LibreNMS emits " +
					"naive local time, so a wrong zone reads as FUTURE-dated. That does NOT drop the " +
					"alert — the push path clamps it to receipt time, losing the true fire time — but " +
					"with a minimum age set below, the pull then withholds EVERY alert from that site, " +
					"visible only as a persistently high \"withheld\" count in the poll log. A row with " +
					"fewer than three fields, or an empty base URL, is skipped in silence: only the " +
					"boot log's deployment COUNT shows it went missing.",
				Type: desc.TypeText, Security: desc.SecOrdinary, Effect: desc.EffectRestart,
				Required: true, MaxLen: 4096,
			},
			{
				// AUTHORITY, not an ordinary toggle. Turning verification off does not degrade the
				// connection, it removes the only check that the endpoint IS the LibreNMS it claims to be —
				// and TG sends that deployment's API token in the request header. A wrong value here hands
				// the credential to whoever can answer at the address, which is a trust-boundary move and
				// must not render as an unremarkable checkbox.
				Name: "insecure", EnvKey: "TG_LIBRENMS_INSECURE", Label: "Skip TLS verification",
				Help: "Accept the LibreNMS server's certificate without verifying it. Only for an internal " +
					"self-signed endpoint: with this on, anything that can answer at the configured URL " +
					"receives the API token TG sends. Off is correct unless you know otherwise.",
				Type: desc.TypeBool, Security: desc.SecAuthority, Effect: desc.EffectRestart,
			},
			{
				// This string is matched against alerts that actually fire, so a typo does not error — it
				// silently makes every cascade prediction unfalsifiable.
				Name: "cascade_alert", EnvKey: "TG_LIBRENMS_CASCADE_ALERT", Label: "Cascade alert name",
				Help: "The LibreNMS rule name stamped on every topology edge as the alert a dependent " +
					"device is expected to raise when its parent fails (default DeviceDown). It must match " +
					"the rule name your LibreNMS actually fires, or cascade predictions can never be " +
					"confirmed against reality.",
				Type: desc.TypeText, Security: desc.SecOrdinary, Effect: desc.EffectRestart, MaxLen: 128,
			},
			{
				Name: "alert_poll_interval", EnvKey: "TG_LIBRENMS_ALERT_POLL_INTERVAL",
				Label: "Active-alert poll interval",
				Help: "Empty (the default) means alert intake is PUSH ONLY — LibreNMS transports POST to " +
					"/v1/ingest/librenms. Set a duration (e.g. 60s) to also PULL firing alerts read-only: " +
					"the air-gapped intake when no transport can reach TG, or — with a minimum age below — " +
					"a safety net for pushes that never arrived.",
				Type: desc.TypeDuration, Security: desc.SecOrdinary, Effect: desc.EffectRestart, MaxLen: 32,
			},
			{
				// The gate that decides which of TWO jobs the poll is doing. Unparseable does not fail the
				// boot: it logs and reverts to pulling EVERY active alert, which is a very different
				// posture from the one the operator asked for.
				Name: "alert_poll_min_age", EnvKey: "TG_LIBRENMS_ALERT_POLL_MIN_AGE",
				Label: "Poll only alerts older than",
				Help: "Requires a poll interval. Empty or 0 pulls every firing alert (primary intake). A " +
					"duration (e.g. 5m) re-triages only alerts old enough that a push should already have " +
					"covered them, so a dropped transport does not leave a host down — without reacting to " +
					"transients. A value TG cannot parse reverts to pulling everything and only says so in " +
					"the log.",
				Type: desc.TypeDuration, Security: desc.SecOrdinary, Effect: desc.EffectRestart, MaxLen: 32,
			},
			{
				Name: "push_token_ref", EnvKey: "TG_LIBRENMS_INGEST_TOKEN_REF",
				Label: "Push bearer-token reference",
				Help: "Where the bearer token LibreNMS transports present to /v1/ingest/librenms is read " +
					"from. Displayed for provenance: set the token itself below, not this pointer.",
				Type: desc.TypeSecretRef, Security: desc.SecOrdinary, Effect: desc.EffectReadOnly, MaxLen: 512,
			},
			{
				// LIVE, and the proof is core/db/source.go: LookupSource resolves the stored reference on
				// every ingest request, so a rotation is authenticated on the next push with no restart.
				// The caveat is the FIRST write, and the help text says it plainly rather than letting an
				// operator conclude from a green Save that pushes are now authenticated.
				Name: "push_token", Label: "Push bearer token",
				Help: "The bearer token LibreNMS transports send to /v1/ingest/librenms. Write-only: it is " +
					"stored in the secret backend and never read back into this dialog. Rotations take " +
					"effect on the next push; the FIRST token also needs a worker restart, because the " +
					"source row that names this reference is provisioned at boot and only when the " +
					"reference already resolves.",
				Type: desc.TypeSecretValue, Security: desc.SecSecret, Effect: desc.EffectLive, MaxLen: 2048,
			},
		},
		// DERIVED, never declared: a module may not name its own secret path (desc.Validate refuses it).
		// TG_LIBRENMS_INGEST_TOKEN_REF must point HERE — it defaults to env:TG_LIBRENMS_INGEST_TOKEN, which
		// is read at boot and never rewritten at runtime, so adopting the prefix is a one-time reference
		// change. Every rotation after that is a Save, with no OpenBao or .env work.
		Secret: desc.SecretLane{KVPath: desc.ModuleSecretPath("ingest", SourceType), Field: "token"},
		Test:   desc.TestSpec{Verb: "read the LibreNMS device list from every configured deployment", Mutating: false},
	}
}
