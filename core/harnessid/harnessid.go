// Package harnessid is the ONE place the harness identity math lives (TG-575 fingerprint, TG-600 render
// record; Harness² learnings H2-1/H2-1c). A harness fingerprint names the CONFIGURATION a session was
// composed under: the system half of the prompt exactly as the model receives it — protocol prose, the
// rendered tool catalog, the op-class catalog including any ratified runtime overlay, and the effective
// guidance half — bound to the trusted-preamble version. The agent's preamble renderer assembles those
// four parts through Assemble, the runner binds the content hash to the preamble version through Bind,
// and the render store refuses any row whose key does not derive from its parts through Verify — so the
// three can never drift apart: a stored rendering IS the bytes its fingerprint hashes.
//
// A leaf package (crypto + strings only): agent, temporal/runner, core/db and the operator tools all
// import it; it imports none of them. Nothing here reads incident data, and nothing on the decision
// path reads anything produced here (agent/fingerprint.go: OBSERVABILITY ONLY).
package harnessid

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"regexp"
	"strings"
)

// The two placeholders the protocol prose carries; Assemble substitutes them in this order. They are
// named here so the renderer and the store cannot disagree about what a "part" is.
const (
	ToolCatalogPlaceholder    = "{{TOOL_CATALOG}}"
	OpClassCatalogPlaceholder = "{{OPCLASS_CATALOG}}"
)

// Parts are the four EFFECTIVE components of the system half. "Effective" means every fallback has
// already been resolved by the renderer: Guidance is the store-composed half or the embedded one, and
// OpClassCatalog is the rendered registry or the day-zero posture text — the store records what the
// model saw, not what was configured.
type Parts struct {
	Prose          string // the protocol template WITH its two placeholders (the embedded base-prompt-protocol bytes)
	ToolCatalog    string // substituted for ToolCatalogPlaceholder
	OpClassCatalog string // substituted for OpClassCatalogPlaceholder (registry + ratified overlay, or the empty posture)
	Guidance       string // appended after the protocol, separated by one newline
}

// Hashes are the per-component SHA-256s (hex). Four hashes let an operator say WHICH component moved
// between two fingerprints without pulling the text, and let SQL compare configurations by column.
type Hashes struct {
	Prose          string
	ToolCatalog    string
	OpClassCatalog string
	Guidance       string
}

// Assemble renders the system half from its parts — byte-identical to the agent's historical
// renderPreamble (TG-472): both placeholders replaced, in this order, then one newline and the guidance.
// It is the only assembly; the agent calls it to build the live prompt and the store calls it to verify a
// record, which is what makes a stored rendering trustworthy.
func Assemble(p Parts) string {
	protocol := strings.ReplaceAll(p.Prose, ToolCatalogPlaceholder, p.ToolCatalog)
	protocol = strings.ReplaceAll(protocol, OpClassCatalogPlaceholder, p.OpClassCatalog)
	return protocol + "\n" + p.Guidance
}

// Hashes returns the per-component content hashes.
func (p Parts) Hashes() Hashes {
	return Hashes{Prose: ContentHash(p.Prose), ToolCatalog: ContentHash(p.ToolCatalog), OpClassCatalog: ContentHash(p.OpClassCatalog), Guidance: ContentHash(p.Guidance)}
}

// Bytes is the size of the assembled system half.
func (p Parts) Bytes() int { return len(Assemble(p)) }

// ContentHash is the hex SHA-256 of a text — the agent-level fingerprint of the assembled system half.
func ContentHash(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

// Bind ties a content hash to the trusted-preamble version: hex SHA-256 over the hex content hash, one
// NUL byte, and the version string — exactly the two-step hash the runner has persisted as
// session_triage.harness_fp since TG-575 (migration 0115). The NUL keeps "ab"+"c/1" and "a"+"bc/1" apart.
func Bind(contentHash, promptVersion string) string {
	h := sha256.New()
	h.Write([]byte(contentHash))
	h.Write([]byte{0})
	h.Write([]byte(promptVersion))
	return hex.EncodeToString(h.Sum(nil))
}

// Fingerprint is the session-row identity a set of parts derives to under a preamble version.
func Fingerprint(p Parts, promptVersion string) string {
	return Bind(ContentHash(Assemble(p)), promptVersion)
}

// Record is what the render store persists for one configuration: the key the session rows carry, the
// preamble version it was bound with, the build that rendered it, and the parts themselves.
type Record struct {
	HarnessFP     string
	PromptVersion string
	BuildSHA      string // the worker binary's VCS revision; "" when the build carried none
	Parts         Parts
}

var hexSHA256 = regexp.MustCompile(`^[0-9a-f]{64}$`)

// IsFingerprint reports whether s has the shape of a harness fingerprint (64 lowercase hex).
func IsFingerprint(s string) bool { return hexSHA256.MatchString(s) }

// Verify refuses a record whose key does not derive from its content: a render stored under a
// fingerprint it does not hash to would poison every later diff ("the model saw X" when it saw Y). It
// also refuses an empty preamble version and a malformed key, so a half-built record never lands.
func Verify(r Record) error {
	if !IsFingerprint(r.HarnessFP) {
		return fmt.Errorf("harnessid: key %q is not a hex SHA-256", r.HarnessFP)
	}
	if strings.TrimSpace(r.PromptVersion) == "" {
		return fmt.Errorf("harnessid: record %s carries no preamble version", r.HarnessFP[:12])
	}
	if strings.TrimSpace(r.Parts.Prose) == "" {
		return fmt.Errorf("harnessid: record %s carries no protocol prose", r.HarnessFP[:12])
	}
	if want := Fingerprint(r.Parts, r.PromptVersion); want != r.HarnessFP {
		return fmt.Errorf("harnessid: record key %s does not derive from its parts under %s (they hash to %s) — refused", r.HarnessFP[:12], r.PromptVersion, want[:12])
	}
	return nil
}

// Short is the operator-facing prefix of a fingerprint (12 hex), the width every log line and tool uses.
func Short(fp string) string {
	if len(fp) <= 12 {
		return fp
	}
	return fp[:12]
}
