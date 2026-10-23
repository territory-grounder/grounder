package evidencepack

import (
	"strings"

	"github.com/territory-grounder/grounder/core/judge"
	"github.com/territory-grounder/grounder/core/trace"
)

// Source is one recorded session's facts as the stores hold them. Assemble turns it into a Pack without touching
// a store, so the mapping is testable without Postgres and the CLI is only plumbing. It carries NO verdict.
type Source struct {
	Row      judge.TriageRow
	Steps    []trace.AgentStep
	Evidence []trace.AgentStepEvidence
	Harness  map[string]string // skill name → body, resolved from the row's skill_loads store ids
}

// Assemble builds the pack for one session: the incident framing from the row, the rollout from steps + evidence,
// the deliverable from EXACTLY the facts the judge adjudicates (TriageRow.Facts → judge.Record, untouched), and the
// harness the session composed with. label names the rollout/deliverable files.
func Assemble(label string, src Source) Pack {
	return Pack{
		Incident:     Incident{ExternalRef: src.Row.ExternalRef, Host: src.Row.Host, AlertRule: src.Row.AlertRule},
		Rollouts:     []Rollout{{Label: label, Steps: src.Steps, Evidence: src.Evidence}},
		Deliverables: []Deliverable{{Label: label, Record: src.Row.Facts()}},
		Harness:      src.Harness,
	}
}

// HarnessLoads lists the store versions a session composed with, from its skill_loads provenance: the name and the
// store version id of every `name@version#id:store[...]` entry (the guidance row included — it is a store row like
// any other). The id/tail rule is judge.StoreVersionIDs's (one parser of this grammar decides what a store entry
// is; this adds only the name in front of the "@"). Compiled-only, pinned and marker entries carry no store id and
// are skipped; they are not bodies an improver can edit. A name that appears twice keeps the LAST id (a session
// loads one body per name).
func HarnessLoads(loads []string) map[string]int64 {
	out := map[string]int64{}
	for _, l := range loads {
		at := strings.Index(l, "@")
		if at <= 0 {
			continue
		}
		ids := judge.StoreVersionIDs([]string{l})
		if len(ids) != 1 {
			continue
		}
		out[l[:at]] = ids[0]
	}
	return out
}
