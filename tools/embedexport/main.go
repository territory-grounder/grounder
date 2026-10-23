// Command embedexport renders the EMBED-EXPORT artifact for an earned op-class: the opschema.json snippet
// plus the spec/013 restamp checklist, as the body of a reviewable merge request.
//
// WHY THIS TOOL EXISTS (spec/028 REQ-2808/REQ-2818, ADR-0016 decision 2). An overlay class climbs the ladder
// on evidence and STOPS at auto_notice. The silent rung — auto, where no human sees the action — is reserved
// for classes present in the EMBEDDED, lockstep-hashed registry, because that is the tamper domain whose
// contents cannot be changed by a runtime write. "The last rung requires a code release" is the whole of the
// safety trade, and this tool is the road: it turns an earned overlay row into the exact diff a reviewer
// approves, so the promotion is a normal code review rather than a privileged runtime act.
//
// THE TOOL GRANTS NOTHING. It reads a ratified spec and PRINTS. It does not write opschema.json, does not
// touch the lockstep lock, does not append to the ledger, and has no database credentials. Every consequence
// of running it is mediated by a human opening, reading, and merging the MR it describes — which is exactly
// the property that makes the ceiling meaningful. A generator that could commit its own output would have
// re-created the runtime-write path the ceiling exists to close.
//
// WHY THE CHECKLIST IS PART OF THE OUTPUT. Pasting a snippet into opschema.json is necessary but not
// sufficient: the file is lockstep-governed (spec/013), so the same MR must restamp the binding and carry the
// Law-Change trailer, and the class must acquire a faultinjector pairing or a ledgered opcover exemption
// (REQ-2818). Emitting the checklist beside the snippet means the reviewer sees the whole obligation in one
// place instead of discovering half of it when CI goes red.
//
// Usage:
//
//	embedexport -spec ratified-spec.json                 # snippet + checklist as markdown (an MR body)
//	embedexport -spec ratified-spec.json -format snippet # just the opschema.json snippet
//	cat spec.json | embedexport                          # stdin
//
// The input is the ratified OpClassSpec as stored in opclass_ratified.spec (canonical JSON).
package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/territory-grounder/grounder/tools/embedexport/render"
)

func main() {
	specPath := flag.String("spec", "", "path to the ratified OpClassSpec JSON (default: stdin)")
	format := flag.String("format", "mr", "output: mr (snippet + checklist) | snippet | checklist")
	flag.Parse()

	raw, err := readInput(*specPath)
	if err != nil {
		fatal(err)
	}
	out, err := render.Render(raw, *format)
	if err != nil {
		fatal(err)
	}
	fmt.Print(out)
}

func fatal(err error) {
	fmt.Fprintf(os.Stderr, "embedexport: %v\n", err)
	os.Exit(1)
}

func readInput(path string) ([]byte, error) {
	if strings.TrimSpace(path) == "" {
		return io.ReadAll(os.Stdin)
	}
	return os.ReadFile(path)
}
