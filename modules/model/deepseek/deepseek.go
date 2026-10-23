// Package deepseek is the DeepSeek provider backend behind the bundled LiteLLM gateway (spec/008 REQ-815).
//
// It implements adapters/model.Provider. DeepSeek is a reasoning model whose response carries typed
// [thinking, text] blocks; JoinReasoning keeps only the type=="text" blocks, so the caller receives the
// answer as untrusted, typed data and never the chain-of-thought (INV-08). TG never calls the provider
// directly — the gateway fronts it. Provenance: [O] INV-08, spec/008.
package deepseek

import (
	"encoding/json"
	"fmt"
	"strings"

	model "github.com/territory-grounder/grounder/adapters/model"
)

// SourceType is the provider slug.
const SourceType = "deepseek"

// Module is the DeepSeek provider backend. Construct with New.
type Module struct{ models []string }

// New builds the provider with the given model ids (a sane default if none supplied).
func New(models ...string) *Module {
	if len(models) == 0 {
		models = []string{"deepseek/deepseek-reasoner"}
	}
	return &Module{models: models}
}

// Name implements adapters/model.Provider.
func (m *Module) Name() string { return SourceType }

// Models implements adapters/model.Provider.
func (m *Module) Models() []string { return m.models }

// compile-time proof the module satisfies the stable model-provider interface.
var _ model.Provider = (*Module)(nil)

// block is one DeepSeek response content block; a reasoning model returns [thinking, text] blocks.
type block struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// JoinReasoning parses a DeepSeek reasoning response (an array of typed blocks) and joins ONLY the
// type=="text" blocks — the thinking blocks are dropped, so the caller receives the answer as untrusted
// typed data, never the chain-of-thought (INV-08).
func JoinReasoning(raw []byte) (string, error) {
	var blocks []block
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return "", fmt.Errorf("deepseek: malformed reasoning response: %w", err)
	}
	var parts []string
	for _, b := range blocks {
		if b.Type == "text" {
			parts = append(parts, b.Text)
		}
	}
	return strings.Join(parts, ""), nil
}
