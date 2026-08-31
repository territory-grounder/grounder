package litellm

import "github.com/territory-grounder/grounder/modules/desc"

// Descriptor publishes the model gateway's configuration schema so the console GENERATES its dialog rather
// than hand-rendering one that drifts from the binary.
//
// SCOPE — why these six keys and not others. The composition root builds exactly ONE model client
// (cmd/worker/main.go:768, model.NewGateway) and every completion, embedding, judge call and skill
// generation in TG goes through it. That single object is configured by six env keys and no more: BaseURL
// (TG_LITELLM_URL), APIKeyRef (TG_LITELLM_KEY_REF), its slow-call observer (TG_MODEL_SLOW_CALL_SECONDS,
// main.go:1572) and its per-tier circuit breaker (TG_MODEL_BREAKER_*, main.go:2195-2197). The provider
// packages behind it — anthropic, openai, deepseek, mistral, ollama, zai — are registered with no
// configuration at all (modules/bootstrap/bootstrap.go:89, each constructed as New()), so this descriptor
// is the whole operator-facing surface for the model plane. Nothing here is inferred from a field name;
// each key was read out of the composition root.
//
// The fallback LADDER is deliberately ABSENT. It is a constructor argument to New() in this package, and no
// composition root calls it — declaring a TG_LITELLM_LADDER key would render an input that accepts a value
// and reaches no code, which is the exact defect this surface exists to remove. (There is no longer a
// component→model resolver argument to omit either: TG-298 deleted it, because the one live mapping from a
// tier name to a provider + fallback chain is deploy/litellm-config.yaml's model_list, which is edited on
// the box and not through this dialog.)
//
// EFFECT IS PER FIELD AND IT IS NOT DECORATION. Only the gateway key is live: config.SecretRef.Resolve
// performs a fresh backend read with no cache, and it is called INSIDE the request path — adapters/model
// model.go:192 (do, per completion), embed.go:52 (per embedding), and gateway.go callModel in this package
// — so a rotation takes effect on the NEXT call. Everything else is EffectRestart, and that is a checked
// claim rather than the safe default: the live-override holder that makes a saved value take effect without
// a restart (liveValue/liveList/liveKV, cmd/worker/main.go:861) is consulted by the matrix notifier's fields
// and by NOTHING on the model plane. No model key reaches it, so a save here is durable but inert until the
// worker restarts, and the dialog must say that instead of implying success.
//
// ONE THING THIS DIALOG CANNOT SAY, so read it here. The console's per-module "enabled" flag comes from the
// module REGISTRY, and bootstrap registers only the six provider backends under the model surface
// (RegisterModelProviders) — never the gateway itself, which the worker constructs directly. So model/litellm
// renders as not-enabled while every model call in TG is flowing through the object this dialog configures.
// That is a registry gap, not a descriptor one; do not "fix" it by inflating the Summary, and do not read the
// flag as evidence the gateway is down.
func Descriptor() desc.Descriptor {
	return desc.Descriptor{
		Surface:    "model",
		SourceType: SourceType,
		Title:      "LiteLLM model gateway",
		Summary: "The one OpenAI-compatible endpoint every TG model call traverses — agent loop, judge, " +
			"skill generator and RAG embedder — fronting the configured provider backends with a " +
			"per-model-tier circuit breaker.",
		Fields: []desc.Field{
			{
				Name: "url", EnvKey: "TG_LITELLM_URL", Label: "Gateway base URL",
				Help: "Base URL of the LiteLLM endpoint, e.g. http://litellm:4000. Wrong or unreachable " +
					"and TG has no model at all: the agent loop proposes nothing, the judge cron leaves " +
					"sessions unjudged, and semantic retrieval degrades to lexical.",
				Type: desc.TypeURL, Security: desc.SecOrdinary, Effect: desc.EffectRestart,
				Required: true, MaxLen: 512,
			},
			{
				Name: "key_ref", EnvKey: "TG_LITELLM_KEY_REF", Label: "Gateway key reference",
				Help: "Where the gateway master key is read from (env:/file:/bao:). Displayed for " +
					"provenance: set the key itself below, not this pointer. Rotation is only a Save once " +
					"this points at the module secret lane rather than an env: name.",
				Type: desc.TypeSecretRef, Security: desc.SecOrdinary, Effect: desc.EffectReadOnly, MaxLen: 512,
			},
			{
				Name: "key", Label: "Gateway master key",
				Help: "The bearer key TG presents to the gateway. Write-only: it is stored in the secret " +
					"backend and never read back into this dialog. Without it every model call fails at " +
					"key resolution before a request is sent.",
				Type: desc.TypeSecretValue, Security: desc.SecSecret, Effect: desc.EffectLive, MaxLen: 2048,
			},
			{
				// A COUNT OF SECONDS, not a duration. main.go:1572 reads it through envInt and multiplies by
				// time.Second, so "60s" parses as zero and silently falls back to the default. Typing this as
				// TypeDuration would render a widget whose natural value the binary cannot read — a control
				// that accepts input and changes nothing, which is what the Pattern below refuses.
				Name: "slow_call_seconds", EnvKey: "TG_MODEL_SLOW_CALL_SECONDS", Label: "Slow-call log threshold (seconds)",
				Help: "A gateway call slower than this logs a structured line. Observe-only — it never " +
					"cancels or gates a call. Blank or non-positive means 60.",
				Type: desc.TypeText, Security: desc.SecOrdinary, Effect: desc.EffectRestart,
				Pattern: `^[0-9]+$`, MaxLen: 8,
			},
			{
				// Same envInt reading as above: an integer count, never a duration string.
				Name: "breaker_threshold", EnvKey: "TG_MODEL_BREAKER_THRESHOLD", Label: "Breaker failure threshold",
				Help: "Consecutive upstream failures on ONE model tier before its circuit opens. Too high " +
					"and a dead provider is retried on every call; too low and a single flap stops the " +
					"agent loop, which fails closed and proposes nothing. Blank or non-positive means 3.",
				Type: desc.TypeText, Security: desc.SecOrdinary, Effect: desc.EffectRestart,
				Pattern: `^[0-9]+$`, MaxLen: 8,
			},
			{
				// The ONLY duration here, because envDuration reads it through time.ParseDuration. The
				// pattern must therefore admit everything ParseDuration admits and the binary would honour:
				// composite ("1m30s") and fractional ("1.5h") forms, and "1m0s" — which is how the boot log
				// PRINTS the default, so an operator copying the running value back into the dialog must not
				// be refused. It deliberately refuses a bare number and a negative: envDuration silently
				// falls back to the default on both, and a box that accepts a value the binary discards is
				// the same lie as a key nothing reads.
				Name: "breaker_cooldown", EnvKey: "TG_MODEL_BREAKER_COOLDOWN", Label: "Breaker cooldown",
				Help: "How long an open circuit waits before admitting ONE half-open probe, as a Go " +
					"duration (e.g. 60s, 5m, 1m30s). A unit is required. Blank or non-positive means 60s.",
				Type: desc.TypeDuration, Security: desc.SecOrdinary, Effect: desc.EffectRestart,
				Pattern: `^([0-9]+(\.[0-9]+)?(ns|us|µs|ms|s|m|h))+$`, MaxLen: 16,
			},
			{
				// Same envInt reading: an integer count, never a duration string.
				Name: "breaker_half_open_successes", EnvKey: "TG_MODEL_BREAKER_HALF_OPEN_SUCCESSES",
				Label: "Half-open successes to close",
				Help: "Consecutive probe successes that close a half-open circuit and readmit the tier. " +
					"Blank or non-positive means 1.",
				Type: desc.TypeText, Security: desc.SecOrdinary, Effect: desc.EffectRestart,
				Pattern: `^[0-9]+$`, MaxLen: 8,
			},
		},
		// DERIVED, never declared: a module may not name its own secret path (desc.Validate refuses it).
		// TG_LITELLM_KEY_REF must point here — that pointer is read at boot and nothing rewrites it at
		// runtime, so adopting the prefix is a one-time reference change. Every rotation after that is a
		// Save, with no OpenBao or .env work.
		Secret: desc.SecretLane{KVPath: desc.ModuleSecretPath("model", SourceType), Field: "key"},
		// Read-only by construction: GET /v1/models lists what the gateway will serve and sends no prompt,
		// so it proves the URL and the key together without spending a token or touching the estate.
		// NO TEST VERB, DELIBERATELY — the console renders this module's TEST button disabled and says "no
		// test is implemented", which is the truth.
		//
		// This descriptor promised "list the models the gateway will serve (GET /v1/models)" and nothing
		// could ever have performed it, because THE WORKER BUILDS NO *litellm.Module. Verified rather than
		// assumed: cmd/worker/main.go does not import this package at all; the only call to litellm.New
		// anywhere in the tree is a spec acceptance test; the worker's one model client is
		// adapters/model.NewGateway; and modules/bootstrap registers only the six provider backends under
		// the model surface — this type could not be registered there even deliberately, because it has
		// SourceType/Usage/Complete and no Name/Models, so it does not satisfy adapters/model.Provider.
		//
		// A SelfTest method here would therefore be dead code reachable from nothing: the exact defect this
		// whole surface exists to remove, reproduced inside the fix for it. The fields above are NOT dead —
		// TG_LITELLM_URL, TG_LITELLM_KEY_REF and the TG_MODEL_BREAKER_* keys are read by the composition
		// root and drive the gateway that actually serves every model call (the env-key guard proves it).
		// What is missing is the module instance, so the honest state is a described, configurable module
		// with no probe. See TG-252 and adapters/model/breaker.go, which records the same gap from the
		// other side: the ported per-rung breaker control has no production constructor either.
		Test: desc.TestSpec{Mutating: false},
	}
}
