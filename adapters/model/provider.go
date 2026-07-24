package model

// Provider is a model-provider backend fronted by the bundled LiteLLM gateway. Backends are selected by
// configuration; the gateway maps an OpenAI-compatible request to the resolved provider/model and applies
// the auto-fallback ladder. A provider's output is untrusted, typed, delimited data that NEVER becomes
// control flow, a command string, or a query fragment (INV-08). Implementations live under
// modules/model/<provider>/; the gateway itself is modules/model/litellm. [O] INV-08, spec/008 REQ-815.
type Provider interface {
	// Name is the provider slug (e.g. "zai", "deepseek", "mistral", "ollama", "anthropic", "openai").
	Name() string
	// Models lists the model ids this provider serves behind the gateway.
	Models() []string
}
