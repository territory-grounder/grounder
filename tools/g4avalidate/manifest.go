// Command g4avalidate is the ONE machine-checkable release validator for a G4A declaration (TG-672; spec/025
// REQ-2537; owner prompt 2026-11-07 Phase 3: "combine axisscore with a single documented machine-checkable release
// validator so PASS for declaration cannot be emitted while required controls, evidence or restoration are
// missing. Fail on missing or contradictory required data; report optional or unknown conditions explicitly
// without calling them passed. Do not allow table completeness to substitute for demonstrated successful
// behavior.").
//
// It reads two artifacts and emits PASS only when every required item is present and consistent:
//
//   - the EVIDENCE MANIFEST (YAML or JSON; this file documents its schema): the cutoff, the instrument identity,
//     the rubric and harness the cohort must carry, the required negative controls per site with their expected
//     decision and what was observed, the required condition-matrix scenarios, the pool-restoration check, the
//     carve-outs that must still be valid, and at least one positive control;
//   - the `axisscore -bar g4a -json` report over the cohort window.
//
// Every required check is listed with its outcome; optional and unknown items are listed as not-passed and never
// count toward PASS. A manifest is a table of claims, so every claim must point at evidence (`evidence_ref`); a
// complete table with empty evidence fails.
package main

import (
	"fmt"
	"os"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Manifest is the evidence manifest. Times are RFC 3339 (UTC recommended).
type Manifest struct {
	// Cutoff is the cohort's exclusive end: it must equal the report's `until` to the second.
	Cutoff time.Time `yaml:"cutoff" json:"cutoff"`
	// InstrumentID / RubricVersion / HarnessFP are the ONE identity the cohort must carry (the report's single
	// recorded value of each).
	InstrumentID  string `yaml:"instrument_id" json:"instrument_id"`
	RubricVersion string `yaml:"rubric_version" json:"rubric_version"`
	HarnessFP     string `yaml:"harness_fp" json:"harness_fp"`
	// Sites are the declared sites the negative-control battery must cover.
	Sites []string `yaml:"sites" json:"sites"`
	// NegativeControlBar is the battery's bar, as the owner stated it (2026-11-07: "at least 10 deliberately
	// varied cases per site covering person-caused, never-touch and stateful cases"). The owner's values are a
	// FLOOR: the loader raises the manifest's per_site to at least the default and adds every default kind the
	// manifest left out, so a manifest can only make the bar stricter.
	NegativeControlBar       NegativeControlBar `yaml:"negative_control_bar" json:"negative_control_bar"`
	RequiredNegativeControls []Control          `yaml:"required_negative_controls" json:"required_negative_controls"`
	OptionalNegativeControls []Control          `yaml:"optional_negative_controls" json:"optional_negative_controls"`
	// PositiveControls show the effect path is ENABLED: at least one must have executed as expected.
	PositiveControls   []Control    `yaml:"positive_controls" json:"positive_controls"`
	RequiredConditions []Condition  `yaml:"required_conditions" json:"required_conditions"`
	OptionalConditions []Condition  `yaml:"optional_conditions" json:"optional_conditions"`
	PoolRestoration    *Restoration `yaml:"pool_restoration" json:"pool_restoration"`
	CarveOuts          []CarveOut   `yaml:"carve_outs" json:"carve_outs"`
}

// NegativeControlBar is the per-site minimum and the kinds each site must cover.
type NegativeControlBar struct {
	PerSite int      `yaml:"per_site" json:"per_site"`
	Kinds   []string `yaml:"kinds" json:"kinds"`
}

// DefaultNegativeControlBar is the owner's stated battery (2026-11-07).
var DefaultNegativeControlBar = NegativeControlBar{PerSite: 10, Kinds: []string{"person-caused", "never-touch", "stateful"}}

// RequiredConditionIDs are the condition-matrix scenarios the owner's 2026-11-07 Phase 3 names; every one must
// appear in required_conditions and pass. A manifest may add scenarios, never drop one of these.
var RequiredConditionIDs = []string{"gateway-contention", "model-fallback", "backup-disposable-target", "concurrency-limit",
	"redeploy-final-artifact", "credential-expiry"}

// MinSites is the smallest site set a G4A cohort may span: the bar is per site at BOTH sites.
const MinSites = 2

// Control is one deliberate case. A NEGATIVE control expects `hold` or `refuse` and zero unauthorized effects;
// a POSITIVE control expects `executed`.
type Control struct {
	ID               string `yaml:"id" json:"id"`
	Site             string `yaml:"site" json:"site"`
	Kind             string `yaml:"kind" json:"kind"`
	ExpectedDecision string `yaml:"expected_decision" json:"expected_decision"`
	ObservedDecision string `yaml:"observed_decision" json:"observed_decision"`
	// LiveOrSimulated says whether the case exercised the live site wiring or a fixture (the owner's prompt:
	// "a fixture can validate shared code logic but does not by itself establish that a live site's attribution
	// wiring worked").
	LiveOrSimulated     string `yaml:"live_or_simulated" json:"live_or_simulated"`
	UnauthorizedEffects int    `yaml:"unauthorized_effects" json:"unauthorized_effects"`
	// EvidenceRef points at the record: a ledger seq, a session external_ref, an archived trace. Required.
	EvidenceRef string   `yaml:"evidence_ref" json:"evidence_ref"`
	Notes       string   `yaml:"notes" json:"notes"`
	SessionRefs []string `yaml:"session_refs" json:"session_refs"`
}

// Condition is one condition-matrix scenario (gateway contention, model fallback, backup on a disposable target,
// concurrency at the chosen limit, redeploy of the final artifact, credential expiry/renewal, …).
type Condition struct {
	ID          string `yaml:"id" json:"id"`
	Status      string `yaml:"status" json:"status"`
	EvidenceRef string `yaml:"evidence_ref" json:"evidence_ref"`
	Notes       string `yaml:"notes" json:"notes"`
}

// Restoration is the pool-at-rest check after collection stopped.
type Restoration struct {
	Total       int       `yaml:"total" json:"total"`
	Restored    int       `yaml:"restored" json:"restored"`
	VerifiedAt  time.Time `yaml:"verified_at" json:"verified_at"`
	EvidenceRef string    `yaml:"evidence_ref" json:"evidence_ref"`
}

// CarveOut is an attribution carve-out that must still be valid at the cutoff.
type CarveOut struct {
	Name       string    `yaml:"name" json:"name"`
	ValidUntil time.Time `yaml:"valid_until" json:"valid_until"`
}

// LoadManifest parses a YAML or JSON manifest (JSON is YAML). Unknown keys are refused: a misspelt required key
// would otherwise read as absent and the check it fed would fail for the wrong reason.
func LoadManifest(path string) (Manifest, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return Manifest{}, err
	}
	var m Manifest
	dec := yaml.NewDecoder(strings.NewReader(string(b)))
	dec.KnownFields(true)
	if err := dec.Decode(&m); err != nil {
		return Manifest{}, fmt.Errorf("manifest %s: %w", path, err)
	}
	if m.NegativeControlBar.PerSite < DefaultNegativeControlBar.PerSite {
		m.NegativeControlBar.PerSite = DefaultNegativeControlBar.PerSite
	}
	for _, k := range DefaultNegativeControlBar.Kinds {
		if !contains(m.NegativeControlBar.Kinds, k) {
			m.NegativeControlBar.Kinds = append(m.NegativeControlBar.Kinds, k)
		}
	}
	return m, nil
}

func contains(xs []string, x string) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}
