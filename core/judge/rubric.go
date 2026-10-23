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
	// Version is the operator-declared rubric identity stamped onto every session_judgment row
	// (TG-194). Bump it with EVERY change to this file's calibration content: pooled statistics only
	// ever combine rows judged under one version, so an un-bumped edit would silently mix
	// incomparable scores — the drift this field exists to make impossible.
	// TestRubricVersionMovesWithContent pins version->content-hash, so an edit without a bump is a
	// red build, not a convention.
	Version    string   `json:"version"`
	Dimensions []string `json:"dimensions"`
	// DeterministicDimensions are the axes scored IN GO from facts the orchestrator bound, not by the judge
	// model (TG-201: diagnosis_grounded). They are declared here — one rubric source for every scored axis,
	// one version stamp on every row — but deliberately kept OUT of Dimensions, which is both the LLM reply
	// schema and the eval Overall's fixed denominator: asking the model for a score we do not want it to
	// author would re-open a checkable fact to free text, and widening the denominator would move every
	// historical Overall by denominator change alone.
	DeterministicDimensions []string `json:"deterministic_dimensions"`
	// DiagnosisRule is the written calibration for diagnosis_grounded — the scale, its N/A rule, and the
	// explicit statement that honest uncertainty scores WELL. Not rendered into the judge prompt (the model
	// does not score this axis); it is here so rubric.json remains the one place an operator reads to learn
	// how a session is graded.
	DiagnosisRule string `json:"diagnosis_rule"`
	// EstateRule is the written calibration for estate_grounded (TG-202) — the scale, and the explicit
	// statement that a graph which does not KNOW scores nothing at all. Same reason as DiagnosisRule: the
	// model does not grade this axis, and an axis whose rule an operator cannot read is one nobody can audit.
	EstateRule         string      `json:"estate_rule"`
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
	// A deterministic dimension with no declared name would leave DimDiagnosisGrounded writing rows under
	// "" — an unattributable judgment, which is the same class of defect the version stamp exists to end.
	// Its calibration text is required for the same reason the LLM guidance is: an axis nobody can read the
	// rule for is an axis nobody can audit (TG-201).
	if len(r.DeterministicDimensions) == 0 || r.DiagnosisRule == "" || r.EstateRule == "" {
		panic("core/judge: embedded rubric.json declares no deterministic_dimensions/diagnosis_rule/estate_rule — " +
			"the deterministically-scored axes must be named and calibrated in the ONE rubric source (TG-201/TG-202)")
	}
	if r.Version == "" {
		panic("core/judge: embedded rubric.json declares no version — every judgment row must be " +
			"attributable to the rubric that produced it (TG-194); add a top-level \"version\" key")
	}
	return r
}

// mustDeterministicDim returns the named deterministic dimension from the ONE rubric source, panicking at
// package init if the rubric does not declare it. The dimension names are looked up BY NAME rather than by
// index (TG-202 added a second axis beside diagnosis_grounded): an index would silently re-point a Dim* var —
// and with it the durable session_judgment rows it keys — the day someone reorders the JSON array.
func mustDeterministicDim(name string) string {
	for _, d := range rubric.DeterministicDimensions {
		if d == name {
			return d
		}
	}
	panic(fmt.Sprintf("core/judge: rubric.json declares no deterministic dimension %q — the Go scorer and the "+
		"one rubric source disagree about which axes exist, so rows would be written under a name nothing "+
		"documents", name))
}

// RubricVersion returns the declared rubric identity — the value the judge stamps on every judgment
// row and the poolers partition by.
func RubricVersion() string { return rubric.Version }

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
