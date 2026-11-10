// Package render is the embed-export artifact generator (spec/028 REQ-2808/REQ-2818, ADR-0016 decision 2).
//
// It lives in a LIBRARY package rather than inside the command because two callers need the identical
// artifact: the `embedexport` CLI a reviewer runs by hand, and the operator console's export-embed verb.
// Two implementations would eventually disagree, and the one place they must not disagree is the document
// that carries a capability into the embedded, lockstep-hashed tamper domain.
//
// THE GENERATOR GRANTS NOTHING. It reads a ratified spec and returns text. It does not write opschema.json,
// does not touch the lockstep lock, does not append to the ledger, and has no database credentials. Every
// consequence of running it is mediated by a human opening, reading, and merging the MR it describes —
// which is exactly the property that makes the auto_notice ceiling meaningful. A generator that could
// commit its own output would have re-created the runtime-write path the ceiling exists to close.
package render

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/territory-grounder/grounder/core/actuate/opschema"
)

// Render is the whole tool as a pure function so the oracle drives exactly what main prints.
func Render(raw []byte, format string) (string, error) {
	spec, err := parseSpec(raw)
	if err != nil {
		return "", err
	}
	switch strings.TrimSpace(format) {
	case "snippet":
		return Snippet(spec)
	case "checklist":
		return Checklist(spec), nil
	case "", "mr":
		snip, err := Snippet(spec)
		if err != nil {
			return "", err
		}
		return MRBody(spec, snip), nil
	default:
		return "", fmt.Errorf("unknown -format %q (want mr, snippet, or checklist)", format)
	}
}

// parseSpec decodes the ratified spec and REFUSES anything the embedded registry would refuse.
//
// The validation is not decoration. This snippet is destined for the strongest tamper domain in the system,
// and a reviewer reading a plausible-looking diff is a weaker check than the admission gate the overlay
// already passed. Re-running it here means a spec that could never have been ratified cannot be smuggled
// into the embedded registry by hand-crafting the tool's INPUT — the one attack this tool's existence opens.
//
// hasCompiledBuilder is false: an exported class is argv-template data by construction (it was ratified as
// data), and claiming a compiled builder it does not have would produce a snippet that fails at init.
func parseSpec(raw []byte) (opschema.OpClassSpec, error) {
	var s opschema.OpClassSpec
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&s); err != nil {
		return opschema.OpClassSpec{}, fmt.Errorf("malformed ratified spec: %w", err)
	}
	validated, err := opschema.ValidateSpec(s, false)
	if err != nil {
		return opschema.OpClassSpec{}, fmt.Errorf("this spec would be REFUSED by the embedded registry, so it "+
			"must not be pasted into opschema.json: %w", err)
	}
	// An already-embedded slug means the export has nothing to do — and pasting it would create a duplicate
	// key in the reviewed registry. Refused loudly rather than emitting a snippet that breaks init.
	if opschema.IsEmbedded(validated.OpClass) {
		return opschema.OpClassSpec{}, fmt.Errorf("op-class %q is ALREADY embedded in the reviewed registry — "+
			"there is nothing to export (a duplicate key would break registry init)", validated.OpClass)
	}
	return validated, nil
}

// Snippet renders the exact JSON object to paste into core/actuate/opschema/opschema.json, indented to match
// that file's array-element style so the diff is the addition and nothing else.
func Snippet(s opschema.OpClassSpec) (string, error) {
	b, err := json.MarshalIndent(s, "  ", "  ")
	if err != nil {
		return "", fmt.Errorf("cannot render the snippet: %w", err)
	}
	return "  " + string(b) + "\n", nil
}

// Checklist is the spec/013 restamp obligation in the order a reviewer must satisfy it. Every line names the
// consequence of skipping it, because a checklist whose items read as ceremony gets skipped.
func Checklist(s opschema.OpClassSpec) string {
	var b strings.Builder
	fmt.Fprintf(&b, "## Embed-export checklist — %s\n\n", s.OpClass)
	fmt.Fprintf(&b, "Merging this MR moves `%s` from the runtime OVERLAY into the EMBEDDED registry, which is\n", s.OpClass)
	fmt.Fprint(&b, "the only way it can ever reach the silent `auto` rung (ADR-0016 decision 2). Each step below is\n")
	fmt.Fprint(&b, "load-bearing; the consequence of skipping it is stated so none of them read as ceremony.\n\n")
	fmt.Fprint(&b, "- [ ] **Paste the snippet** into `core/actuate/opschema/opschema.json`.\n")
	fmt.Fprint(&b, "      Skipped: nothing changes — the class stays capped at `auto_notice`.\n")
	fmt.Fprint(&b, "- [ ] **Restamp the lockstep binding** — `go run ./tools/specvalidate lockstep --restamp`, in THIS commit.\n")
	fmt.Fprint(&b, "      Skipped: CI reds on spec drift, because `opschema.json` is governed by spec/013.\n")
	fmt.Fprint(&b, "- [ ] **Carry `Law-Change-Approved-By: @<codeowner>`** in the commit trailer block.\n")
	fmt.Fprint(&b, "      Skipped: the protected-path gate refuses the MR. Keep the trailer CONTIGUOUS with any\n")
	fmt.Fprint(&b, "      `Co-Authored-By` line — a blank line between them makes git parse only the last paragraph,\n")
	fmt.Fprint(&b, "      and the approval silently registers on nothing.\n")
	fmt.Fprintf(&b, "- [ ] **Close opcover** for `%s` — a faultinjector pairing, or a ledgered exemption (REQ-2818).\n", s.OpClass)
	fmt.Fprint(&b, "      Skipped: `opcover --check` reds. An AUTO-capable class with no fault that exercises it is a\n")
	fmt.Fprint(&b, "      capability nothing has ever proven in anger.\n")
	fmt.Fprintf(&b, "- [ ] **Revoke the overlay row** for `%s` after this merges.\n", s.OpClass)
	fmt.Fprint(&b, "      Skipped: harmless but untidy — the composed registry drops the shadowing overlay row anyway\n")
	fmt.Fprint(&b, "      (embedded always wins), so the class keeps working; the row just lingers as dead evidence.\n\n")
	fmt.Fprint(&b, "After merge the class is embedded, and its NEXT clean-run streak at `auto_notice` promotes it to\n")
	fmt.Fprint(&b, "`auto` — the ladder does the promotion, not this MR. Nothing here grants autonomy directly.\n")
	return b.String()
}

// MRBody is the reviewable artifact: what this is, the snippet, and the checklist.
func MRBody(s opschema.OpClassSpec, snippet string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# Embed-export: %s\n\n", s.OpClass)
	fmt.Fprintf(&b, "`%s` was EARNED at runtime — proposed repeatedly by triage, ratified by an operator, and\n", s.OpClass)
	fmt.Fprint(&b, "climbed to `auto_notice` on verified-clean runs. It is now held at that ceiling: the overlay it\n")
	fmt.Fprint(&b, "lives in is a runtime-writable tamper domain, and the rung where no human watches is reserved for\n")
	fmt.Fprint(&b, "the embedded, lockstep-hashed registry. This MR is that code release.\n\n")
	fmt.Fprintf(&b, "- **family**: `%s`\n", s.Family)
	fmt.Fprintf(&b, "- **safety tier**: `%s`\n", s.SafetyTier)
	if len(s.ArgvTemplate) > 0 {
		fmt.Fprintf(&b, "- **argv template**: `%s`\n", strings.Join(s.ArgvTemplate, " "))
	}
	if len(s.RollbackTemplate) > 0 {
		fmt.Fprintf(&b, "- **rollback template**: `%s`\n", strings.Join(s.RollbackTemplate, " "))
	}
	fmt.Fprint(&b, "\n## Snippet — append to the array in `core/actuate/opschema/opschema.json`\n\n```json\n")
	b.WriteString(snippet)
	b.WriteString("```\n\n")
	b.WriteString(Checklist(s))
	return b.String()
}
