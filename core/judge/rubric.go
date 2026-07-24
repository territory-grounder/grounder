package judge

import (
	_ "embed"
	"encoding/json"
	"fmt"
)

// rubric.json is the ONE source of the LLM-judge rubric: the calibration text (per-dimension guidance +
// the hollow-proposal rule), the five scored dimensions, and the canonical JudgeParams (model,
// temperature, seed). It is embedded here so every Go surface (this package, eval/, eval/gate,
// tools/rejudge, temporal/skilljudge) builds its prompt/params from it, and it is READ AS A FILE by the
// Python shadowbench judge (tools/shadowbench/judge.py + tools/shadowbench/_driver.py) — so there is
// exactly one rubric, never two hand-copied copies that silently drift and make scores incomparable
// (OpenAI Evals 3.4; the one-judge principle). TestRubricIsSingleSource (Go) and
// tools/shadowbench/test_rubric.py (Python) pin that both the Go embed and the Python read resolve to
// these same bytes.
//
//go:embed rubric.json
var rubricRaw []byte

// Rubric is the parsed rubric.json. The four text fields reproduce judge.Prompt() byte-for-byte (the
// golden test proves it); the Python A/B judge reuses Guidance + HollowProposalRule verbatim as its own
// calibration text so both surfaces score by identical wording.
type Rubric struct {
	Dimensions         []string    `json:"dimensions"`
	Intro              string      `json:"intro"`                // single-session judge preamble
	ReplyInstruction   string      `json:"reply_instruction"`    // strict-JSON reply schema (single-session)
	Guidance           string      `json:"guidance"`             // per-dimension calibration (shared with shadowbench)
	HollowProposalRule string      `json:"hollow_proposal_rule"` // the hollow/timeout-proposal rule (shared with shadowbench)
	Params             JudgeParams `json:"params"`               // canonical judge sampling params
}

// rubric is the singleton parsed at package init; a malformed embed fails fast (a build-time guarantee
// the one source is well-formed). Every consumer reads through the accessors below, never a private copy.
var rubric = mustParseRubric(rubricRaw)

func mustParseRubric(raw []byte) Rubric {
	var r Rubric
	if err := json.Unmarshal(raw, &r); err != nil {
		panic(fmt.Sprintf("core/judge: embedded rubric.json is malformed: %v", err))
	}
	if len(r.Dimensions) == 0 || r.Guidance == "" || r.HollowProposalRule == "" || r.Params.Model == "" {
		panic("core/judge: embedded rubric.json is missing required fields (dimensions/guidance/hollow_proposal_rule/params.model)")
	}
	return r
}

// LoadedRubric returns the parsed rubric. Callers that need the calibration text (e.g. a cross-language
// identity check) read it here rather than re-declaring the strings.
func LoadedRubric() Rubric { return rubric }

// RubricJSON returns a copy of the raw embedded rubric.json bytes — the exact bytes the Python judge reads
// off disk. TestRubricIsSingleSource asserts these equal the on-disk file, and the Python side is pinned
// to the same path, so the Go embed and the Python read are byte-identical by construction.
func RubricJSON() []byte {
	out := make([]byte, len(rubricRaw))
	copy(out, rubricRaw)
	return out
}
