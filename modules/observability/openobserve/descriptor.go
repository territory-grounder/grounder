package openobserve

import "github.com/territory-grounder/grounder/modules/desc"

// Descriptor publishes OpenObserve's configuration schema so the console can GENERATE its dialog rather
// than hand-render one that drifts from the binary.
//
// Effect is stated per field and it is not decoration. openobserve.go:99 resolves the ingest token inside
// post(), on every send, so a token written to the lane below takes effect on the NEXT export with no
// restart. The endpoint is captured at construction — bootstrap.RegisterConfiguredObservability builds the
// Module once from TG_OPENOBSERVE_URL at boot — so a saved endpoint is durable but inert until the worker
// restarts, and the dialog must say that instead of implying success.
//
// The endpoint is also the ENABLE switch: empty means RegisterConfiguredObservability skips the module, so
// there is no exporter at all rather than a silent one. That is the fact an operator needs at 3am when the
// series they are looking for was never shipped.
//
// THE SECOND SWITCH, and the reason it is named in the help rather than rendered as a field. A fully
// configured endpoint still ships NOTHING unless TG_OBSERVABILITY_EXPORT_INTERVAL is set — that knob gates
// the ONE export loop (cmd/worker/main.go:1082, off by default) which is the only production caller of
// Export for any exporter on this surface. It is deliberately NOT a field here: it is platform-wide, so a
// save under "OpenObserve" would silently change Langfuse's and the dead-man switch's cadence too — exactly
// the dishonest control this surface exists to remove. Naming it in the help is the honest half.
//
// WHAT ACTUALLY SHIPS, and it is now TWO things on two different clocks.
//
//  1. The worker's own self-telemetry batch (liveness, declared-capability, suppression and seam-yield
//     gauges), on the TG_OBSERVABILITY_EXPORT_INTERVAL cadence described above — still off by default.
//  2. As of TG-44, the per-session TRACE: every completed investigation ships its ordered spans (one
//     summary span carrying latency, cycle/tool counts, terminal outcome, decision tier and the
//     PROVIDER-REPORTED token totals, then one span per ReAct cycle) keyed by external_ref.
//
// The second is NOT on the interval knob, and the difference matters to an operator reading this dialog: a
// trace is an EVENT emitted when a session ends, so it needs an endpoint and nothing else. Setting the URL
// is sufficient for traces; metrics still need the interval as well.
//
// This paragraph used to read "ExportSpans exists on this module and tracing is default-on, but NO
// composition root calls it, so no trace reaches OpenObserve today." It was accurate for the whole life of
// the module, and it is the reason the wiring was findable: the surface said what it did not do instead of
// implying success. Keep that habit — if the caller is ever removed, this text goes back.
func Descriptor() desc.Descriptor {
	return desc.Descriptor{
		Surface:    "observability",
		SourceType: SourceType,
		Title:      "OpenObserve",
		Summary: "Ships the worker's freshness-stamped self-telemetry — liveness, declared-capability and " +
			"seam gauges — to OpenObserve, so a worker that dies is visible from OUTSIDE TG instead of " +
			"reading as healthy. It ALSO ships a trace per completed investigation (ordered spans with " +
			"latency, tool counts, outcome and the provider-reported token totals). Traces need only this " +
			"endpoint; the metric batch additionally needs the platform-wide export interval below. The same endpoint and credential also back the read-only correlate-logs agent tool, which searches this instance's log index across an incident's blast-radius hosts.",
		Fields: []desc.Field{
			{
				Name: "endpoint", EnvKey: "TG_OPENOBSERVE_URL", Label: "OpenObserve base URL",
				// THE /api/<org> PREFIX IS PART OF THE VALUE, not a detail. The module posts to
				// <this URL>/{stream}/_json (the native bulk-ingest route), whose full path is
				// {host}/api/{org}/{stream}/_json — so a URL stopping at the host 404s EVERY export,
				// silently, and the store simply stays empty. The
				// example carried no prefix for months and the dialog gave an operator no way to know; the
				// TEST button now fails visibly on exactly this (selftest.go), so the example must agree
				// with it rather than send them to a red button.
				Help: "OpenObserve ingest base URL INCLUDING the org prefix, e.g. " +
					"https://openobserve.example/api/default — TG appends /{stream}/_json to it, and OpenObserve's " +
					"native ingest route is {host}/api/{org}/{stream}/_json, so a URL that stops at the host 404s every " +
					"export silently. Leave it empty and the " +
					"exporter is not registered at all: nothing ships, and nothing reports an error. " +
					"This endpoint alone IS enough for session TRACES (they are emitted per completed " +
					"investigation, not on a schedule). The METRIC batch additionally needs the worker's " +
					"platform-wide TG_OBSERVABILITY_EXPORT_INTERVAL, which is off by default; until it is " +
					"set you will see traces here and no gauges.",
				Type: desc.TypeURL, Security: desc.SecOrdinary, Effect: desc.EffectRestart,
				Required: true, MaxLen: 512,
			},
			{
				// THE READ SIDE (TG-39). The exporter WRITES to this endpoint; the correlate-logs agent tool
				// READS from it, and it needs to know which stream the syslog-ng device logs land in. A wrong
				// name is not an error — the tool searches an empty stream and honestly reports "no matching
				// lines", the empty-vs-broken trap — so the TEST button runs a bounded read of THIS stream to
				// fail visibly on a name the shipping pipeline never created.
				Name: "log_stream", EnvKey: "TG_OPENOBSERVE_LOG_STREAM", Label: "Log stream (correlate-logs)",
				Help: "The OpenObserve stream the syslog-ng device logs are shipped into — the stream the " +
					"read-only correlate-logs agent tool searches (default \"syslog\"). Empty ⇒ the default. Read " +
					"at boot: a change is durable but inert until the worker restarts.",
				Type: desc.TypeText, Security: desc.SecOrdinary, Effect: desc.EffectRestart,
				Pattern: `^[A-Za-z0-9._-]*$`, MaxLen: 128,
			},
			{
				Name: "host_field", EnvKey: "TG_OPENOBSERVE_HOST_FIELD", Label: "Host field (correlate-logs)",
				Help: "The stream field carrying the device host each log line came from — the column " +
					"correlate-logs filters the blast-radius host set on and attributes results by (default " +
					"\"host\"). A property of the shipping pipeline, not of TG. Empty ⇒ the default. Read at boot.",
				Type: desc.TypeText, Security: desc.SecOrdinary, Effect: desc.EffectRestart,
				Pattern: `^[A-Za-z0-9._-]*$`, MaxLen: 128,
			},
			{
				Name: "token_ref", EnvKey: "TG_OPENOBSERVE_TOKEN_REF", Label: "Ingest-token reference",
				Help: "Where the ingest token is read from (default env:OPENOBSERVE_TOKEN). Displayed for " +
					"provenance: set the token itself below, not this pointer.",
				Type: desc.TypeSecretRef, Security: desc.SecOrdinary, Effect: desc.EffectReadOnly, MaxLen: 512,
			},
			{
				// The credential OpenObserve issues, presented as HTTP Basic — NOT a Bearer token. Saying so
				// here is the difference between a five-minute fix and an evening: a bare API key pasted in
				// this box 401s every export, and the only symptom is a metrics store that stays empty.
				Name: "token", Label: "Ingest token",
				Help: "OpenObserve's base64 ingest credential — base64(user:password) — sent as HTTP Basic. " +
					"A raw API key here 401s every export. Write-only: it is stored in the secret backend " +
					"and never read back into this dialog.",
				Type: desc.TypeSecretValue, Security: desc.SecSecret, Effect: desc.EffectLive, MaxLen: 2048,
			},
		},
		// DERIVED, never declared: a module may not name its own secret path (desc.Validate refuses it).
		// TG_OPENOBSERVE_TOKEN_REF must point HERE — that pointer is read at boot and nothing rewrites it at
		// runtime, so adopting the prefix is a one-time change to the reference. Every rotation after that is
		// a Save, with no OpenBao or .env work.
		Secret: desc.SecretLane{KVPath: desc.ModuleSecretPath("observability", SourceType), Field: "token"},
		Test: desc.TestSpec{
			// A read, not a write. Ingest is a POST to /v1/logs; probing THAT from a settings dialog would
			// put junk records in the operator's metrics store — an unreviewed change to the estate.
			//
			// THE WIRED PROBE IS THE CORRELATE READ PATH (search_selftest.go). This connector serves two
			// capabilities on one credential and one endpoint: the exporter WRITES, and the read-only
			// correlate-logs agent tool READS via /_search over the configured log stream. The button runs one
			// bounded, read-only search of that stream — the exact path correlate-logs uses — which proves more
			// than a stream list could: a credential granted list-but-not-search (OpenObserve scopes them
			// separately) and a TG_OPENOBSERVE_LOG_STREAM naming a stream the pipeline never created both fail
			// HERE, where the alternative is correlate-logs silently returning nothing during an incident. It
			// shares the /api/<org> prefix with the ingest path too, so a base URL missing that prefix fails
			// HERE instead of silently 404ing every export.
			//
			// The verb states what a PASS does not prove: OpenObserve grants read and write separately, so an
			// accepted search credential is not a permitted ingest. (The exporter's own stream-list probe,
			// selftest.go, remains as the write-half Tester; the read path is the one wired to this button.)
			Verb: "run one bounded, read-only search over the configured log stream (the exact path the " +
				"correlate-logs agent tool queries) — a read; nothing is ingested, and a pass proves the " +
				"endpoint answers, the credential is accepted for search, and the stream is queryable, not " +
				"that ingest itself is permitted",
			Mutating: false,
		},
	}
}
