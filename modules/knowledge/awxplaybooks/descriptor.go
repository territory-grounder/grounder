package awxplaybooks

import "github.com/territory-grounder/grounder/modules/desc"

// Descriptor publishes the playbooks-as-knowledge lane's configuration schema so the console can GENERATE
// its dialog rather than hand-render one that drifts from the binary.
//
// EVERY FIELD IS RESTART, INCLUDING THE TOKEN. armAWXPlaybooksIngest (cmd/worker/main.go:5309) reads all
// five keys ONCE at boot: the base URL and CA go into the *Client, the corpus path is frozen into a
// FileCorpus, and the interval becomes a time.Ticker created once. The token deserves its own sentence
// because matrix's is LIVE and the difference cannot be seen from the dialog: ingest.go token() caches the
// resolved value in c.cached for the client's lifetime. A rotation saved to the lane below is durable but
// inert — the next tick still presents the old token. Calling that "live" would report a rotation that did
// not happen.
//
// THE LANE IS OFF UNLESS ALL FOUR REQUIRED FIELDS ARE SET. A partial configuration logs "disabled" and
// ingests nothing; there is no half-armed state. The dialog must therefore mark them Required rather than
// let an operator save three of four and believe the lane is running.
func Descriptor() desc.Descriptor {
	return desc.Descriptor{
		Surface:    "knowledge",
		SourceType: SourceType,
		Title:      "AWX playbooks as knowledge",
		Summary: "Read-only ingest of AWX job templates into the retrieval corpus, so the agent can DISCOVER " +
			"and cite a sanctioned runbook. It launches nothing: discovery grants no authority, and a " +
			"surfaced runbook still has to re-enter the actuation pipeline as a governed proposal.",
		Fields: []desc.Field{
			{
				Name: "base_url", EnvKey: "TG_AWXPLAYBOOKS_BASE_URL", Label: "AWX base URL",
				Help: "Base URL of AWX / automation-controller, e.g. https://awx.example — no trailing " +
					"slash and no /api. The lane is disabled entirely unless this, the sensor token " +
					"reference, the corpus path and a non-zero interval are ALL set.",
				Type: desc.TypeURL, Security: desc.SecOrdinary, Effect: desc.EffectRestart,
				Required: true, MaxLen: 512,
			},
			{
				Name: "corpus_path", EnvKey: "TG_AWXPLAYBOOKS_CORPUS", Label: "Runbook corpus file",
				Help: "File the ingested runbooks are written to. POINT THIS AT THE RETRIEVER'S OWN CORPUS " +
					"(TG_KNOWLEDGE_FILE): the fused retriever drops any semantic hit whose ref is absent " +
					"from that corpus, so a separate file gives you runbooks that are indexed and " +
					"findable by nothing. The worker names that state in the log, but ONLY on a tick " +
					"that changed something AND only when a semantic index is configured — silence is " +
					"not evidence this path is right.",
				Type: desc.TypeText, Security: desc.SecOrdinary, Effect: desc.EffectRestart,
				Required: true, MaxLen: 512,
			},
			{
				Name: "interval", EnvKey: "TG_AWXPLAYBOOKS_INTERVAL", Label: "Ingest interval",
				Help: "How often the runbooks are re-read from AWX, e.g. \"30m\". Zero or unset means the " +
					"lane is OFF — it is opt-in, not defaulted on. Each run re-reads every template by id " +
					"rather than trusting the list copy, so an edited runbook is ingested as it now is.",
				Type: desc.TypeDuration, Security: desc.SecOrdinary, Effect: desc.EffectRestart,
				Required: true, MaxLen: 32,
			},
			{
				Name: "ca_cert", EnvKey: "TG_AWXPLAYBOOKS_CA", Label: "Private CA certificate path",
				Help: "Path to a PEM CA bundle to trust when AWX is behind a private CA. Leave empty for a " +
					"publicly trusted certificate. An unreadable path disables the lane with a log line " +
					"rather than skipping verification or crashing the worker.",
				Type: desc.TypeText, Security: desc.SecOrdinary, Effect: desc.EffectRestart, MaxLen: 512,
			},
			{
				Name: "sensor_token_ref", EnvKey: "TG_AWXPLAYBOOKS_SENSOR_TOKEN_REF", Label: "Sensor-token reference",
				Help: "Where this lane's READ-ONLY AWX token is read from. Displayed for provenance: set " +
					"the token itself below, not this pointer. Keep it DISTINCT from the AWX launch " +
					"token — this lane has no launch path, and a launch-capable credential here would " +
					"hand write scope to a sensor that never needs it.",
				Type: desc.TypeSecretRef, Security: desc.SecOrdinary, Effect: desc.EffectReadOnly, MaxLen: 512,
			},
			{
				Name: "sensor_token", Label: "AWX read-only sensor token",
				Help: "The read-only AWX OAuth2 token this lane reads templates and inventories with. " +
					"Write-only: stored in the secret backend and never read back into this dialog. The " +
					"client caches it after first use, so a rotation saved here applies at the next " +
					"worker restart, not the next tick.",
				Type: desc.TypeSecretValue, Security: desc.SecSecret, Effect: desc.EffectRestart, MaxLen: 2048,
			},
		},
		// DERIVED, never declared: a module may not name its own secret path (desc.Validate refuses it).
		// TG_AWXPLAYBOOKS_SENSOR_TOKEN_REF must point here — that pointer is read at boot and nothing
		// rewrites it at runtime, so adopting the prefix is a one-time reference change per module.
		Secret: desc.SecretLane{KVPath: desc.ModuleSecretPath("knowledge", SourceType), Field: "token"},
		// Every method on this client is a GET; the template list is the first thing an ingest does, so the
		// Test is the real first step of the real run.
		Test: desc.TestSpec{Verb: "list the AWX job templates read-only", Mutating: false},
	}
}
