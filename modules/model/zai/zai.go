// Package zai is the zai provider backend behind the bundled LiteLLM gateway (spec/008 REQ-815).
//
// It implements adapters/model.Provider: it declares its identity and the models it serves behind the
// gateway. TG never calls the provider directly — the gateway fronts it over one OpenAI-compatible
// endpoint and applies the fallback ladder; a provider's output is untrusted, typed data that never
// becomes control flow, a command string, or a query fragment (INV-08). Provenance: [O] INV-08, spec/008.
package zai

import model "github.com/territory-grounder/grounder/adapters/model"

// SourceType is the provider slug.
const SourceType = "zai"

// Module is the zai provider backend. Construct with New.
type Module struct{ models []string }

// New builds the provider with the given model ids (a sane default if none supplied).
func New(models ...string) *Module {
	if len(models) == 0 {
		models = []string{"zai/glm-4.6"}
	}
	return &Module{models: models}
}

// Name implements adapters/model.Provider.
func (m *Module) Name() string { return SourceType }

// Models implements adapters/model.Provider.
func (m *Module) Models() []string { return m.models }

// compile-time proof the module satisfies the stable model-provider interface.
var _ model.Provider = (*Module)(nil)
