package agent

// The base prompt's PROSE lives as embedded markdown (TG-472, epic TG-114 leaf 2 — the externalization
// half): the wire-format/protocol part (with {{TOOL_CATALOG}}/{{OPCLASS_CATALOG}} slots the renderer
// substitutes), the grounding/diagnosis-honesty guidance part, and the day-zero empty-op-class posture
// (spec/026 REQ-2601). The ASSEMBLY stays compiled (renderPreamble — a pure function under a byte-identity
// golden); go:embed keeps the bytes in the binary with no runtime file read, and a missing/blank part
// PANICS at first render (fail closed — an agent whose protocol prose vanished must not run on a thinner
// contract). The GUIDANCE half is now ALSO a store-backed graduating artifact (C-3b, TG-114 leaf 2's
// flywheel half): cmd/worker seeds BasePromptGuidance() into the skill store as a ClassPrompt row and the
// runner composes it store-first with THIS embed as the total fallback — so these bytes remain the floor
// the agent can never fall below, and the store row is what versions, pins, and graduates.

import (
	"embed"
	"fmt"
	"strings"
)

//go:embed prompts/*.md
var promptFS embed.FS

// BasePromptGuidanceVersion identifies the EMBEDDED guidance half's revision — the version the worker's
// boot seeder stamps on the store row it imports from these bytes (the compiled-skill idiom: bump it when
// prompts/base-prompt-guidance.md changes and the seeder supersedes + re-imports on the next boot). It is
// deliberately separate from promptPreambleVersion (the seed ENVELOPE's version): this names the prose.
const BasePromptGuidanceVersion = "1.0.0"

// BasePromptGuidance returns the embedded guidance half and its version — the boot seeder's source and
// the runner's total fallback. Exported so cmd/worker never reaches into promptFS.
func BasePromptGuidance() (body, version string) {
	return promptPart("base-prompt-guidance"), BasePromptGuidanceVersion
}

// promptPart returns one embedded prompt part, panicking on a missing or blank file (fail closed).
func promptPart(name string) string {
	b, err := promptFS.ReadFile("prompts/" + name + ".md")
	if err != nil {
		panic(fmt.Sprintf("agent: embedded prompt part %q missing: %v", name, err))
	}
	if strings.TrimSpace(string(b)) == "" {
		panic(fmt.Sprintf("agent: embedded prompt part %q is empty", name))
	}
	return string(b)
}
