package db

import (
	"context"
	"fmt"
)

// EvidenceShapeCount is one reading of the agent_step_evidence corpus: how many rows it holds, and how many
// of them carry something that LOOKS like credential material.
//
// ★ WHY THIS EXISTS. TG-302 decided NOT to seal agent_step_evidence at rest, and the whole argument rests on
// one measured fact: across the live corpus there were 0 redaction markers, 0 PEM blocks, 0 provider keys
// and 0 assigned-secret shapes. What is stored is screened host command output with no credential material
// in it, so encrypting it — onto a sealing seam with 2 rows of production exercise, changing the read path
// the console depends on — was not a trade worth making.
//
// That premise is NOT a property of the design. It is a property of what the estate's hosts happen to
// print, and it stops being true the first time a tool reads a file containing a key. Re-measured
// 2026-08-06: still 0 of 354 on every shape, on a corpus that has doubled since TG-302 counted 172. The
// decision holds; nothing was watching whether it still would (TG-345).
type EvidenceShapeCount struct {
	// Rows is the denominator. It is published even at zero, so an ABSENT series means the watcher is gone
	// rather than the corpus being clean — the distinction TG-336/TG-173/TG-343 all turn on.
	Rows int64
	// RedactionMarker counts rows carrying a redaction marker. A marker means the screen FIRED, which is
	// the screen working — but it also means credential-shaped text reached the screen, so the premise
	// ("what is stored has no credential material in it") is now being maintained by a control rather than
	// by the estate's behaviour. That is worth knowing before it is worth alarming about.
	RedactionMarker int64
	// PEMBlock counts rows containing a private-key header.
	PEMBlock int64
	// ProviderKey counts rows containing a recognisable cloud/SaaS key shape.
	ProviderKey int64
	// AssignedValue counts `password=`/`token:`-style assignments with a non-trivial value.
	AssignedValue int64
}

// SecretShaped is the total the alert rule reads. It is deliberately a SUM of the specific shapes rather
// than a separate count-distinct query: a row matching two shapes is two findings for a human to look at,
// and undercounting a doubly-suspicious row is the wrong direction for a hygiene signal.
func (c EvidenceShapeCount) SecretShaped() int64 {
	return c.RedactionMarker + c.PEMBlock + c.ProviderKey + c.AssignedValue
}

// The shape patterns, as POSIX regexes evaluated in Postgres.
//
// They are DELIBERATELY the same four the TG-302 measurement used, verbatim, because the point of this
// watcher is to keep answering the question that decision was made on. Widening them here would silently
// change what "the premise still holds" means, and a premise measured one way and re-checked another is not
// re-checked at all.
const (
	evidenceRedactionRe = `(?i)\[redacted\]|<redacted>|\*\*\*REDACTED`
	evidencePEMRe       = `-----BEGIN [A-Z ]*PRIVATE KEY-----`
	evidenceProviderRe  = `(?i)(AKIA[0-9A-Z]{16}|sk-[A-Za-z0-9]{20,}|ghp_[A-Za-z0-9]{20,}|xox[baprs]-[A-Za-z0-9-]{10,})`
	evidenceAssignedRe  = `(?i)(password|passwd|secret|api[_-]?key|token)\s*[=:]\s*\S{6,}`
)

// CountEvidenceShapes measures the corpus. It reads ONLY counts — never a payload — so a watcher for
// credential material cannot itself become a way to read one.
func (s *AgentStepEvidenceStore) CountEvidenceShapes(ctx context.Context) (EvidenceShapeCount, error) {
	var c EvidenceShapeCount
	const q = `
		SELECT count(*),
		       count(*) FILTER (WHERE payload ~ $1),
		       count(*) FILTER (WHERE payload ~ $2),
		       count(*) FILTER (WHERE payload ~ $3),
		       count(*) FILTER (WHERE payload ~ $4)
		FROM agent_step_evidence`
	if err := s.p.QueryRow(ctx, q, evidenceRedactionRe, evidencePEMRe, evidenceProviderRe, evidenceAssignedRe).
		Scan(&c.Rows, &c.RedactionMarker, &c.PEMBlock, &c.ProviderKey, &c.AssignedValue); err != nil {
		return EvidenceShapeCount{}, fmt.Errorf("db: count agent_step_evidence shapes: %w", err)
	}
	return c, nil
}
