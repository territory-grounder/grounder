package wiring

import (
	"fmt"
	"sort"
	"strings"
	"time"

	observability "github.com/territory-grounder/grounder/adapters/observability"
)

// Finding is one seam's verdict, carrying the SeamSpec's Consequence verbatim so an operator reading the
// boot report or the ledger row learns what the darkness costs, not merely that it exists.
type Finding struct {
	Seam     Seam
	State    State
	Critical bool
	// Cause is the seam's dark-state reason ("deps.Notify is nil"). Carried here and printed by Reason
	// because THIS report is the dark one — the yield register renders starved and unobserved seams and
	// deliberately does not receive it (TG-354).
	Cause       string
	Consequence string
	Detail      string
}

// Reason renders a finding as one ledger/report line.
func (f Finding) Reason() string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s: %s", f.Seam, f.State)
	if f.Critical {
		b.WriteString(" [CRITICAL]")
	}
	// Cause then cost. A dark seam is the ONE state where naming the missing thing is the most useful
	// sentence available, and it is the only state this report renders — so the cause is printed here and
	// nowhere else. Five of six Consequence strings used to carry their cause inline, which made them
	// false in the starved report; the split gives the cause back to the place it is true.
	switch {
	case f.Cause != "" && f.Consequence != "":
		fmt.Fprintf(&b, " — %s: %s", f.Cause, f.Consequence)
	case f.Cause != "":
		fmt.Fprintf(&b, " — %s", f.Cause)
	case f.Consequence != "":
		fmt.Fprintf(&b, " — %s", f.Consequence)
	}
	if f.Detail != "" {
		fmt.Fprintf(&b, " (%s)", f.Detail)
	}
	return b.String()
}

// Report returns one finding per DARK seam, plus a sample for EVERY seam in the closed set.
//
// THE LOAD-BEARING DETAIL IS THAT IT RANGES OVER All(), NEVER OVER m.recorded. Ranging over what was
// recorded is the live bug in modules/telemetry (`counts[key{surface, enabled}]++`), where a series
// exists only for pairs that occurred — so a wholly dark surface emits no series at all and is
// INVISIBLE rather than zero. Invisible is indistinguishable from healthy on every dashboard ever built.
//
// Errors collected during declaration (an invalid Because, an unaccepted Critical waiver) are appended
// as findings on their own seam, so a malformed waiver cannot quietly become a valid one.
func (m *Manifest) Report(now time.Time) ([]Finding, []observability.Sample) {
	specs := All()
	findings := make([]Finding, 0, len(specs))
	samples := make([]observability.Sample, 0, len(specs))

	for _, sp := range specs {
		rec, seen := record{}, false
		if m != nil {
			rec, seen = m.recorded[sp.ID]
		}
		st := DarkUnrecorded
		detail := "no Bind and no Absent ran for this seam — an unrecorded branch is a dark branch"
		if seen {
			st = rec.state
			if rec.detail != "" {
				detail = rec.detail
			} else {
				detail = ""
			}
		}
		if st.dark() {
			findings = append(findings, Finding{
				Seam: sp.ID, State: st, Critical: sp.Criticality == Critical,
				Cause: sp.Cause, Consequence: sp.Consequence, Detail: detail,
			})
		}
		// A sample for EVERY seam, live ones included at 0, so "dark" and "never reported" are
		// distinguishable on a dashboard.
		samples = append(samples, observability.Sample{
			Name: "tg_wiring_seam_dark", Value: boolValue(st.dark()), Stamped: now,
			Labels: map[string]string{
				"seam": string(sp.ID), "state": st.String(),
				"critical": fmt.Sprintf("%t", sp.Criticality == Critical),
			},
		})
	}

	if m != nil {
		for _, e := range m.errs {
			findings = append(findings, Finding{
				Seam: Seam(strings.SplitN(e, ":", 2)[0]), State: DarkUnrecorded,
				Critical: true, Detail: e,
				Consequence: "a seam declaration was rejected; treat the seam as dark",
			})
		}
	}
	sort.SliceStable(findings, func(i, j int) bool { return findings[i].Seam < findings[j].Seam })
	return findings, samples
}

func boolValue(b bool) float64 {
	if b {
		return 1
	}
	return 0
}

// DarkReport renders every finding as one block of prose for the boot-time governance ledger row, or ""
// when every seam is live. Returning "" on a clean tree is what keeps the ledger free of no-op rows.
func DarkReport(findings []Finding) string {
	if len(findings) == 0 {
		return ""
	}
	lines := make([]string, 0, len(findings)+1)
	lines = append(lines, fmt.Sprintf("%d dark wiring seam(s) at boot:", len(findings)))
	for _, f := range findings {
		lines = append(lines, "  - "+f.Reason())
	}
	return strings.Join(lines, "\n")
}
