package langfuse

import "github.com/territory-grounder/grounder/modules/desc"

// Descriptor publishes Langfuse's configuration schema so the console can GENERATE its dialog rather than
// hand-render one that drifts from the binary.
//
// Effect is stated per field and it is not decoration. langfuse.go:118 resolves BOTH key references inside
// ingest(), on every call, so a key written to the lane below takes effect on the NEXT export or trace with
// no restart. The endpoint is captured at construction — bootstrap.RegisterConfiguredObservability builds
// the Module once from TG_LANGFUSE_URL at boot — so a saved endpoint is durable but inert until the worker
// restarts, and the dialog must say that instead of implying success.
//
// WHY ONLY ONE SECRET VALUE when Langfuse authenticates with a PAIR. Ingestion is HTTP Basic, public key as
// username and secret key as password, and the secret lane holds exactly one value. It holds the one that is
// actually a credential: the boot secret-policy gate classifies TG_LANGFUSE_SECRET_REF as a business secret
// and TG_LANGFUSE_PUBLIC_REF as public material (cmd/worker/main.go workerSecretEntries, exempt). So the
// secret key rotates from this dialog and the public key does not — and the public reference is shown below
// so an operator can see where it comes from rather than guessing why auth still fails after a rotation.
//
// THE SECOND SWITCH, and the reason it is named in the help rather than rendered as a field. A fully
// configured endpoint still ships NO SELF-TELEMETRY BATCH unless TG_OBSERVABILITY_EXPORT_INTERVAL is set —
// that knob gates the ONE export loop (cmd/worker/main.go, off by default) which is the only production
// caller of Export on this surface. It is deliberately NOT a field here: it is platform-wide, so a save
// under "Langfuse" would silently change OpenObserve's and the dead-man switch's cadence too.
//
// It does NOT gate traces, and the distinction is now load-bearing rather than pedantic. As of TG-44 the
// per-session recorder has a composition-root caller: every completed investigation is recorded as a
// Langfuse trace the moment it ends, needing only this endpoint and its keys. A cadence knob has nothing to
// say about an event.
//
// This paragraph used to read: "Record — the per-session trajectory recorder this module exists for — has
// NO composition-root caller, so no session appears as a Langfuse trace today." That was true for the
// module's whole life, and saying it here is what made the hole findable. Record was reachable only under
// that name; the stable interface calls the idea ExportSpans, so langfuse.go now exposes it under both and
// a composition root can ENUMERATE trace-capable exporters instead of knowing each one by hand.
func Descriptor() desc.Descriptor {
	return desc.Descriptor{
		Surface:    "observability",
		SourceType: SourceType,
		Title:      "Langfuse",
		Summary: "Ships the worker's freshness-stamped self-telemetry to Langfuse as ingestion events, and " +
			"records every completed agent session as a Langfuse trace (one observation per ReAct cycle, " +
			"plus a summary carrying latency, tool counts, outcome and the provider-reported token totals). " +
			"Traces need only this endpoint and its keys; the self-telemetry batch additionally needs the " +
			"platform-wide export interval below.",
		Fields: []desc.Field{
			{
				Name: "endpoint", EnvKey: "TG_LANGFUSE_URL", Label: "Langfuse base URL",
				Help: "Base URL of the Langfuse instance, e.g. https://langfuse.example. Leave it empty and " +
					"the exporter is not registered at all: nothing is ingested, and nothing reports an " +
					"error. Setting it is not sufficient either — export also needs the worker's " +
					"platform-wide TG_OBSERVABILITY_EXPORT_INTERVAL, which is off by default; until it is " +
					"set this connector is configured, enabled, and silent.",
				Type: desc.TypeURL, Security: desc.SecOrdinary, Effect: desc.EffectRestart,
				Required: true, MaxLen: 512,
			},
			{
				// PUBLIC material, but still a pointer: it is display-only for the same reason every other
				// reference is. Rewriting a reference from a settings form would move what the boot
				// secret-policy gate inspects out from under it.
				Name: "public_key_ref", EnvKey: "TG_LANGFUSE_PUBLIC_REF", Label: "Public-key reference",
				Help: "Where the Langfuse public key — the Basic-auth username, pk-lf-… — is read from " +
					"(default env:LANGFUSE_PUBLIC_KEY). Public material, so it is not rotated from this " +
					"dialog: change it where this reference points.",
				Type: desc.TypeSecretRef, Security: desc.SecOrdinary, Effect: desc.EffectReadOnly, MaxLen: 512,
			},
			{
				Name: "secret_key_ref", EnvKey: "TG_LANGFUSE_SECRET_REF", Label: "Secret-key reference",
				Help: "Where the Langfuse secret key — the Basic-auth password, sk-lf-… — is read from " +
					"(default env:LANGFUSE_SECRET_KEY). Displayed for provenance: set the key itself below, " +
					"not this pointer.",
				Type: desc.TypeSecretRef, Security: desc.SecOrdinary, Effect: desc.EffectReadOnly, MaxLen: 512,
			},
			{
				Name: "secret_key", Label: "Langfuse secret key",
				Help: "The sk-lf-… secret key TG authenticates ingestion with. It is half of a PAIR: if you " +
					"rotated the whole Langfuse project, the public key must be changed too, or every export " +
					"401s. Write-only: stored in the secret backend and never read back into this dialog.",
				Type: desc.TypeSecretValue, Security: desc.SecSecret, Effect: desc.EffectLive, MaxLen: 2048,
			},
		},
		// DERIVED, never declared: a module may not name its own secret path (desc.Validate refuses it).
		// TG_LANGFUSE_SECRET_REF must point HERE — that pointer is read at boot and nothing rewrites it at
		// runtime, so adopting the prefix is a one-time change to the reference. Every rotation after that is
		// a Save, with no OpenBao or .env work.
		Secret: desc.SecretLane{KVPath: desc.ModuleSecretPath("observability", SourceType), Field: "secret_key"},
		Test: desc.TestSpec{
			// A read, not a write. Ingestion is a POST and the only ingestion route is the batch write, so
			// probing it would leave a synthetic trace in the operator's Langfuse project.
			//
			// THE ROUTE IS THE AUTHENTICATED ONE, and the verb changed to say so (selftest.go). The obvious
			// read — /api/public/health — is UNAUTHENTICATED: it answers 200 on an instance whose keys were
			// revoked an hour ago, so a probe built on it would certify a half-rotated pair (new secret key,
			// stale public key) that 401s every single export. /api/public/projects is what the Langfuse
			// SDKs' own auth check calls: it takes the same Basic pair ingestion takes and answers with the
			// PROJECT that pair belongs to, which is also the only thing that can reveal a worker
			// authenticated against the staging Langfuse instead of the production one.
			//
			// The verb still states what a PASS does not prove: Langfuse settles per-event validity when a
			// batch arrives, so an accepted credential is not an accepted ingestion.
			Verb: "authenticate to Langfuse with the public/secret key pair and read back the project it " +
				"belongs to — no trace or sample is ingested, and a pass proves the pair is ACCEPTED, not " +
				"that ingestion of a given event would succeed",
			Mutating: false,
		},
	}
}
