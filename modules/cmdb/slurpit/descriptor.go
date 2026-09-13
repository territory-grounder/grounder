package slurpit

import "github.com/territory-grounder/grounder/modules/desc"

// Descriptor publishes Slurp'it's configuration schema so the console GENERATES its dialog rather than
// hand-rendering one that drifts from the binary.
//
// Every env-backed field here is read ONCE at boot in cmd/worker/main.go (the TG_SLURPIT_* block that
// constructs the estate source), so a save is durable but INERT until the worker restarts — EffectRestart
// says so instead of implying success. The token is the one exception: do() resolves the secret reference
// inside every request, so a rotation written to the lane below is picked up by the next read with no restart.
func Descriptor() desc.Descriptor {
	return desc.Descriptor{
		Surface:    "cmdb",
		SourceType: SourceType,
		Title:      "Slurp'it",
		Summary: "Network-device discovery/inventory: reads the Slurp'it device list and contributes estate " +
			"topology — each discovered device becomes a graph node via its site membership, plus a best-effort " +
			"dependency edge to its upstream parent — at the 0.82 discovered-inventory confidence tier.",
		Fields: []desc.Field{
			{
				Name: "base_url", EnvKey: "TG_SLURPIT_URL", Label: "Slurp'it base URL",
				Help: "Base URL of the Slurp'it server, e.g. http://slurpit.example. Slurp'it is served over " +
					"PLAIN HTTP (no TLS) — a bare host or a scheme-less URL resolves to http://, NEVER https://, " +
					"because assuming TLS would dial a cleartext port and fail with what looks like a certificate " +
					"error. Only set https:// if you have deliberately fronted Slurp'it with a TLS proxy. Left " +
					"empty, the estate source is not registered at all: no Slurp'it devices reach the graph.",
				Type: desc.TypeURL, Security: desc.SecOrdinary, Effect: desc.EffectRestart,
				Required: true, MaxLen: 512,
			},
			{
				Name: "cascade_alert", EnvKey: "TG_SLURPIT_CASCADE_ALERT", Label: "Expected cascade alert",
				Help: "Name of the alert a device→parent cascade is expected to fire (e.g. \"DeviceDown\"). It is " +
					"stamped on every dependency edge this source discovers and becomes the rule the verifier " +
					"expects on each predicted downstream device. Use the name YOUR monitoring actually sends: a " +
					"predicted device that alerts under an unpredicted rule verifies PARTIAL, not match. Leave " +
					"empty to stamp no expected alert.",
				Type: desc.TypeText, Security: desc.SecOrdinary, Effect: desc.EffectRestart, MaxLen: 128,
			},
			{
				Name: "token_ref", EnvKey: "TG_SLURPIT_TOKEN_REF", Label: "API-token reference",
				Help: "Where the Slurp'it API token is read from. Displayed for provenance: set the token itself " +
					"below, not this pointer. CHECK IT NAMES THIS MODULE'S LANE — the reader reads whatever this " +
					"points at, so while it still points somewhere else a token saved below is stored in the lane " +
					"and never read. It is a READ-TRIAGE reference — if the identical reference is also configured " +
					"for an actuation credential the worker refuses to start (plane split).",
				Type: desc.TypeSecretRef, Security: desc.SecOrdinary, Effect: desc.EffectReadOnly, MaxLen: 512,
			},
			{
				Name: "token", Label: "Slurp'it API token",
				Help: "The API token TG reads Slurp'it with, sent as a Bearer credential. Give it READ-ONLY " +
					"scope: this credential sits on the triage plane and must never be able to write the " +
					"inventory. Write-only here — it is stored in the secret backend and never read back.",
				Type: desc.TypeSecretValue, Security: desc.SecSecret, Effect: desc.EffectLive, MaxLen: 2048,
			},
		},
		// DERIVED, never declared (desc.Validate refuses a module that names its own secret path).
		Secret: desc.SecretLane{KVPath: desc.ModuleSecretPath("cmdb", SourceType), Field: "token"},
		Test:   desc.TestSpec{Verb: "read one bounded page of the Slurp'it device inventory", Mutating: false},
	}
}
