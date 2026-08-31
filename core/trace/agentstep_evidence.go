package trace

import (
	"context"
	"errors"
	"unicode/utf8"
)

// ErrEvidenceNotFound signals that no stored observation exists for a (session, evidence id) pair.
//
// It lives HERE, beside the reader interface, rather than in the pgx package: core/db imports core/httpapi, so
// an HTTP handler that needed to name a db sentinel to map it to 404 would close an import cycle. A sentinel
// that a consumer cannot reference is a sentinel that becomes a 503 — the handler would report "the store is
// broken" for the completely ordinary case of a walk recorded before evidence capture existed.
var ErrEvidenceNotFound = errors.New("trace: no stored evidence for that step")

// MaxEvidenceBytes bounds one stored evidence payload.
//
// It is deliberately smaller than what a tool may return. hostdiag alone bounds its own output at 256 KiB, and
// a session can hold a dozen cycles — storing every byte would put megabytes behind a single audit click and
// make the read a denial-of-service surface on the operator's own console. 64 KiB holds every real diagnostic
// observed on this estate whole (the largest measured was ~9 KiB: a `du` two-level walk), so truncation is the
// exception, and when it happens the row RECORDS that it happened. A truncated body that does not say it is
// truncated is a quiet lie on the one surface whose job is proving what the agent actually saw.
const MaxEvidenceBytes = 64 * 1024

// AgentStepEvidence is the SCREENED, SCRUBBED tool output behind one agent_step — the ground truth the
// #reasoning citation opens (TG-272).
//
// It is a separate record from AgentStep, and separately fetched, because the two have opposite read profiles:
// the walk is read whole on every page load, the evidence is read one row at a time when an operator asks a
// specific question. Inlining it would put a megabyte on the critical path of a view that must render fast.
//
// Payload is the output of agent's screenToolOutput (injection spans neutralized, secrets redacted) run through
// screen.Scrub — never a raw tool result. A tool result is attacker-influenceable data (INV-08) and can carry a
// leaked token (INV-13); the console must not become the surface that un-redacts it.
type AgentStepEvidence struct {
	ExternalRef string
	Cycle       int
	EvidenceID  string
	Tool        string
	Payload     string
	// Truncated and FullBytes describe the payload against what the tool actually returned, so the console can
	// say "showing 64 KiB of 210 KiB" rather than presenting a clipped body as the whole answer.
	Truncated bool
	FullBytes int
}

// Truncate returns the payload bounded to MaxEvidenceBytes along with the honesty fields, backing off to a
// RUNE boundary so a clipped body is still valid UTF-8. Slicing mid-sequence leaves a trailing partial rune
// that renders as U+FFFD — evidence that looks CORRUPTED rather than merely cut, which is the worse lie of the
// two on this surface.
func Truncate(payload string) (out string, truncated bool, full int) {
	full = len(payload)
	if full <= MaxEvidenceBytes {
		return payload, false, full
	}
	b := payload[:MaxEvidenceBytes]
	// Drop trailing bytes until the last rune decodes cleanly. DecodeLastRuneInString returns (RuneError, 1)
	// for an invalid encoding and (RuneError, 3) for a legitimately-encoded U+FFFD, so size distinguishes a
	// real replacement character in the tool's own output from a sequence this slice cut in half.
	for len(b) > 0 {
		if r, size := utf8.DecodeLastRuneInString(b); r != utf8.RuneError || size > 1 {
			break
		}
		b = b[:len(b)-1]
	}
	return b, true, full
}

// AgentStepEvidenceSink appends evidence rows. OBSERVE-ONLY, exactly like AgentStepSink: the Runner emits as a
// pure side effect, an Emit error MUST NOT change the investigation, and a nil sink is a no-op.
type AgentStepEvidenceSink interface {
	EmitEvidence(ctx context.Context, e AgentStepEvidence) error
}

// AgentStepEvidenceReader serves one stored observation to the console's citation click.
type AgentStepEvidenceReader interface {
	Evidence(ctx context.Context, externalRef, evidenceID string) (AgentStepEvidence, error)
}
