// Package opcover answers one question about the actuation surface: can every op-class TG is allowed to
// perform ever actually be EXERCISED?
//
// The lattice already proves a lot. `lockstep` binds code to specs, `ratify` proves every requirement has a
// route to an oracle, and the op-class registry itself fails closed on a schema/builder mismatch in BOTH
// directions. None of them notices this, and the failure is silent by construction:
//
//	AN OP-CLASS WITH NO FAULT SOURCE. It is registered, validated, has a compiled argv builder, renders in the
//	prompt catalog, and holds a row on the graduation ladder. It simply can never earn autonomy, because
//	nothing in the estate will ever produce the condition it answers. Nothing fails. The class just sits at
//	`approve` forever while the ladder reports it as merely "not yet graduated".
//
// Measured when this shipped: 3 of 6 op-classes — restart-service, start-service, reload-service — had NO
// fault source whatsoever, and reload-service had never been proposed once across the entire ledger. A5
// breadth ("op-classes demonstrated hands-off") was therefore capped at 2 permanently, regardless of effort,
// and no check anywhere said so. It took a human noticing that a graduation drive kept failing.
//
// This is the same shape as every reachability defect in this repo: a thing complete on one side of a
// boundary and unreferenced on the other, where the boundary is exactly what nobody checks. As with INV-22
// and `ratify`, an UNDECLARED gap is the failure; a DECLARED one with a reason is a legitimate engineering
// position that an operator can review.
package opcover

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Exemption is an operator declaration that an op-class has no fault source, and why. Silence is the failure;
// this is how a gap becomes auditable instead.
type Exemption struct {
	OpClass string `json:"op_class"`
	Why     string `json:"why"`
}

// Finding is one uncovered thing, named precisely enough to act on.
type Finding struct {
	Kind   string // "uncovered-op-class" | "empty-exemption-reason" | "stale-exemption" | "phantom-provokes"
	Detail string
}

// Report is the whole-surface result.
type Report struct {
	OpClasses  int
	Covered    int
	Exempted   int
	FaultClass int
	Findings   []Finding
	Pairs      []string // "fault-class -> op-class", sorted, for the human-readable render
}

func (r Report) Covered100() bool { return len(r.Findings) == 0 }

// Check proves every op-class is provoked by at least one fault class, or is explicitly exempted.
//
// opClasses maps op_class slug -> true (the registry). provokes maps fault-class name -> the op-class slugs
// it drives. Both are passed in rather than imported so this package stays a pure function of its inputs and
// can be exercised with fixtures that do not need the real registry.
func Check(root string, opClasses map[string]bool, provokes map[string][]string) (Report, error) {
	var rep Report
	rep.OpClasses = len(opClasses)
	rep.FaultClass = len(provokes)

	covered := map[string]bool{}
	for fc, ops := range provokes {
		for _, op := range ops {
			op = normalize(op)
			if op == "" {
				continue
			}
			// A fault class naming an op-class that does not exist is its own defect: the fault will be
			// injected forever and credited to nothing. Report it rather than let it read as coverage.
			if !opClasses[op] {
				rep.Findings = append(rep.Findings, Finding{
					Kind: "phantom-provokes",
					Detail: fmt.Sprintf("fault class %q declares it provokes %q, which is not a registered op-class "+
						"— that fault can never be credited to an op-class", fc, op),
				})
				continue
			}
			covered[op] = true
			rep.Pairs = append(rep.Pairs, fc+" -> "+op)
		}
	}
	sort.Strings(rep.Pairs)

	ex, err := loadExemptions(root)
	if err != nil {
		return rep, err
	}
	exempt := map[string]bool{}
	for _, e := range ex {
		op := normalize(e.OpClass)
		if strings.TrimSpace(e.Why) == "" {
			rep.Findings = append(rep.Findings, Finding{
				Kind: "empty-exemption-reason",
				Detail: fmt.Sprintf("%s is exempted with no reason — a declaration without a WHY is silence "+
					"with extra steps", e.OpClass),
			})
			continue
		}
		if !opClasses[op] {
			rep.Findings = append(rep.Findings, Finding{
				Kind: "stale-exemption",
				Detail: fmt.Sprintf("%s is exempted but is not a registered op-class — the exemption outlived "+
					"the class and now hides nothing", e.OpClass),
			})
			continue
		}
		if covered[op] {
			rep.Findings = append(rep.Findings, Finding{
				Kind: "stale-exemption",
				Detail: fmt.Sprintf("%s is exempted but IS now provoked by a fault class — delete the exemption "+
					"so the next real gap is not lost in a list of resolved ones", e.OpClass),
			})
			continue
		}
		exempt[op] = true
	}
	rep.Exempted = len(exempt)

	var uncovered []string
	for op := range opClasses {
		switch {
		case covered[op]:
			rep.Covered++
		case exempt[op]:
			// declared, reviewable — not a finding
		default:
			uncovered = append(uncovered, op)
		}
	}
	sort.Strings(uncovered)
	for _, op := range uncovered {
		rep.Findings = append(rep.Findings, Finding{
			Kind: "uncovered-op-class",
			Detail: fmt.Sprintf("%s is actuatable but NO fault class provokes it and no exemption declares why "+
				"— it can never earn autonomy, and nothing else in the lattice will say so", op),
		})
	}
	return rep, nil
}

func normalize(s string) string { return strings.ToLower(strings.TrimSpace(s)) }

// ExemptionsPath is the operator-authored declaration file.
func ExemptionsPath(root string) string {
	return filepath.Join(root, "spec", "opcover-exemptions.json")
}

func loadExemptions(root string) ([]Exemption, error) {
	b, err := os.ReadFile(ExemptionsPath(root))
	if os.IsNotExist(err) {
		return nil, nil // no file ⇒ no exemptions ⇒ every gap is a finding (fail closed, not open)
	}
	if err != nil {
		return nil, err
	}
	var doc struct {
		Exemptions []Exemption `json:"exemptions"`
	}
	if err := json.Unmarshal(b, &doc); err != nil {
		return nil, fmt.Errorf("opcover: %s: %w", ExemptionsPath(root), err)
	}
	return doc.Exemptions, nil
}

// Render writes the report. Every count carries its denominator.
func (r Report) Render() string {
	var b strings.Builder
	fmt.Fprintf(&b, "specvalidate opcover — %d op-class(es), %d fault class(es)\n", r.OpClasses, r.FaultClass)
	fmt.Fprintf(&b, "  provoked by a fault: %d/%d   (declared exemptions: %d)\n", r.Covered, r.OpClasses, r.Exempted)
	for _, p := range r.Pairs {
		fmt.Fprintf(&b, "    %s\n", p)
	}
	if len(r.Findings) == 0 {
		b.WriteString("  COVERED — every op-class is provoked by a fault class or its absence is declared.\n")
		return b.String()
	}
	byKind := map[string]int{}
	for _, f := range r.Findings {
		byKind[f.Kind]++
	}
	kinds := make([]string, 0, len(byKind))
	for k := range byKind {
		kinds = append(kinds, k)
	}
	sort.Strings(kinds)
	fmt.Fprintf(&b, "  NOT COVERED — %d finding(s):\n", len(r.Findings))
	for _, k := range kinds {
		fmt.Fprintf(&b, "    %-24s %d\n", k, byKind[k])
	}
	b.WriteString("\n")
	for _, f := range r.Findings {
		fmt.Fprintf(&b, "  [%s] %s\n", f.Kind, f.Detail)
	}
	return b.String()
}
