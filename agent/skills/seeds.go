package skills

// The compiled seed bodies live as embedded markdown (TG-471, epic TG-114 leaf 3): prose an operator can
// read, diff, and (later) export as SKILL.md — while the SELECTOR stays a compiled pure function (INV-08;
// nothing loadable decides WHICH skills compose). go:embed keeps the byte-for-byte compile-time guarantee
// the previous Go string literals had: the bytes are in the binary, there is no runtime file read, and a
// missing or empty seed is a BUILD/BOOT defect, never a silently thinner behavioral library — seedBody
// panics at package init (Default() is wired at boot), which is the fail-closed direction for a library
// whose absence would erode the agent's protocols without a trace. Byte-identity with the pre-embed
// literals is pinned by TestDefaultRegistryGolden (the eval-gate waiver's evidence, TG-394 precedent).

import (
	"embed"
	"fmt"
	"strings"
)

//go:embed seeds/*.md
var seedFS embed.FS

// seedBody returns the embedded body for a skill, panicking on a missing or blank seed (fail closed — see
// the package note above; the golden test reddens first in CI, the panic is the last line on a broken build).
func seedBody(name string) string {
	b, err := seedFS.ReadFile("seeds/" + name + ".md")
	if err != nil {
		panic(fmt.Sprintf("skills: embedded seed %q missing: %v (a thinner library must never load silently)", name, err))
	}
	if strings.TrimSpace(string(b)) == "" {
		panic(fmt.Sprintf("skills: embedded seed %q is empty (a blank protocol is not a protocol)", name))
	}
	return string(b)
}
