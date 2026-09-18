package agent

import (
	"fmt"
	"strings"

	"github.com/territory-grounder/grounder/adapters/model"
)

// TG-47 — recall-optimized observation compaction. The ReAct loop appends every tool result to the transcript
// verbatim (loop.go), so a long DeepInvestigation grows the model context unbounded up to the cycle limit. The
// bounded-triage sessions today rarely reach it, but Phase-2 and deep investigations do, and an ever-larger
// prompt both costs tokens and buries the signal. Compaction elides the OLDEST observation PAYLOADS while
// keeping their OBSERVATION[<id>] envelope byte-identical — the id is the anchor the citation gate, INV-11
// evidence-binding and the console ground-truth citation all resolve against (see observationEnvelope), so an
// elided observation stays fully CITABLE: the model just no longer re-reads bytes it already reasoned over, and
// the full result is retained on the durable record (Result.Steps/ToolResults, a separate capture).

// compactKeepRecent observations always stay VERBATIM even over budget — the model needs full detail on what it
// just read; only OLDER observations are candidates for elision.
const compactKeepRecent = 3

// compactPayloadKeep is how many leading bytes (a one-line recall hint) of an elided observation's payload are
// kept alongside its preserved id.
const compactPayloadKeep = 200

// obsEnvelopePrefix marks a user message carrying a tool OBSERVATION (observationEnvelope). Only these are ever
// touched; the preamble, seed, decide/stop nudges and rejection messages are left verbatim.
const obsEnvelopePrefix = "TOOL_OUTCOME["

// compactElisionMarker is the sentinel written into an elided payload; its presence makes elision idempotent.
const compactElisionMarker = "bytes elided to fit the context budget"

// compactObservationBudget caps the total bytes of msgs to approximately budget by ELIDING the payloads of the
// OLDEST tool observations. It keeps VERBATIM: (a) the preamble+seed prefix msgs[:seedLen]; (b) the most recent
// compactKeepRecent observations; (c) EVERY observation's OBSERVATION[<id>] envelope (the id is never touched).
// It mutates the elided messages' Content in place (idempotent — an already-elided payload carries the marker)
// and returns msgs. A budget <= 0, or a msgs already under budget, is a no-op. The durable transcript
// (Result.Steps/ToolResults) is a separate capture and is unaffected, so evidence-binding still sees the full
// result even after the model's copy is elided. Note the recent-verbatim floor: if the kept observations alone
// exceed the budget the result can still be over it — compaction never drops a recent reading to hit a number.
func compactObservationBudget(msgs []model.Message, seedLen, budget int) []model.Message {
	if budget <= 0 || seedLen < 0 || seedLen > len(msgs) {
		return msgs
	}
	total := 0
	for i := range msgs {
		total += len(msgs[i].Content)
	}
	if total <= budget {
		return msgs
	}
	// Observation-message indices in the per-cycle region, oldest first.
	var obs []int
	for i := seedLen; i < len(msgs); i++ {
		if strings.HasPrefix(msgs[i].Content, obsEnvelopePrefix) {
			obs = append(obs, i)
		}
	}
	elidableUpto := len(obs) - compactKeepRecent // never elide the most recent compactKeepRecent
	for n := 0; n < elidableUpto && total > budget; n++ {
		i := obs[n]
		if compacted, saved := elideObservationPayload(msgs[i].Content, compactPayloadKeep); saved > 0 {
			msgs[i].Content = compacted
			total -= saved
		}
	}
	return msgs
}

// elideObservationPayload keeps the TOOL_OUTCOME[...] + OBSERVATION[<id>]: header of an observationEnvelope
// VERBATIM and replaces the payload after it with its first `keep` bytes (one line) plus an elision marker.
// Returns the new content and the bytes saved; saved is 0 when the message is not an envelope, has no
// OBSERVATION marker, was already elided, is already at/under `keep`, or when eliding would not actually shrink
// it (a payload barely over `keep`, where the marker overhead cancels the saving).
func elideObservationPayload(content string, keep int) (string, int) {
	const obsMarker = "\nOBSERVATION["
	i := strings.Index(content, obsMarker)
	if i < 0 {
		return content, 0
	}
	rest := content[i+len(obsMarker):] // "<id>]: <payload>"
	j := strings.Index(rest, "]: ")
	if j < 0 {
		return content, 0
	}
	headerEnd := i + len(obsMarker) + j + len("]: ")
	header := content[:headerEnd] // "...OBSERVATION[<id>]: " (the id anchor, kept byte-for-byte)
	payload := content[headerEnd:]
	if strings.Contains(payload, compactElisionMarker) || len(payload) <= keep {
		return content, 0 // already elided, or already small — idempotent
	}
	hint := payload
	if len(hint) > keep {
		hint = hint[:keep]
	}
	if k := strings.IndexByte(hint, '\n'); k >= 0 {
		hint = hint[:k] // one line of recall hint only
	}
	hint = strings.ToValidUTF8(hint, "") // the byte-slice at `keep` can split a multi-byte rune (log output can be non-ASCII) — keep the hint valid
	stub := fmt.Sprintf("%s […%d %s; the full result is on the durable record — cite this id if your diagnosis relied on it, TG-47]",
		hint, len(payload)-len(hint), compactElisionMarker)
	newContent := header + stub
	saved := len(content) - len(newContent)
	if saved <= 0 {
		return content, 0 // eliding would not shrink it (marker overhead ≥ the payload we removed)
	}
	return newContent, saved
}
