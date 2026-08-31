package metrics

import "testing"

// names returns the CLOSED set of metric names a reading renders. Asserting over the whole set rather than
// probing for one name is deliberate: a withheld gauge is an ABSENCE, and only an enumeration sees an
// absence. A `contains(brier)` style check passes just as happily when something extra leaks out.
func names(c CalibrationReading) map[string]float64 {
	out := map[string]float64{}
	for _, s := range CalibrationSamples(c) {
		out[s.Name] = s.Value
	}
	return out
}

func TestSkillIsWithheldWhenUndefined(t *testing.T) {
	t.Parallel()
	// A real curve whose base rate is degenerate: the ratio is undefined, so the score must NOT appear.
	got := names(CalibrationReading{N: 12, Brier: 0.09, ECE: 0.3, MCE: 0.4, BaseRate: 1.0, SkillDefined: false})
	want := map[string]bool{
		MetricConfidenceSamples:  true,
		MetricConfidenceBrier:    true,
		MetricConfidenceECE:      true,
		MetricConfidenceMCE:      true,
		MetricConfidenceBaseRate: true,
	}
	for name := range got {
		if !want[name] {
			t.Errorf("published %q at an undefined skill — a 0 there reads as 'no skill' when the truth is "+
				"'unmeasurable'", name)
		}
	}
	for name := range want {
		if _, ok := got[name]; !ok {
			t.Errorf("withheld %q, which does not depend on the skill being defined", name)
		}
	}
	if _, ok := got[MetricConfidenceSkill]; ok {
		t.Fatal("tg_confidence_skill must be ABSENT when the base rate is degenerate")
	}
}

func TestSkillIsPublishedWhenDefined(t *testing.T) {
	t.Parallel()
	got := names(CalibrationReading{N: 64, Brier: 0.4633, ECE: 0.5114, MCE: 0.9,
		BaseRate: 0.2656, Skill: -1.3752, SkillDefined: true})
	v, ok := got[MetricConfidenceSkill]
	if !ok {
		t.Fatal("tg_confidence_skill must be published when the base rate is non-degenerate")
	}
	if v >= 0 {
		t.Fatalf("the live reading must publish a NEGATIVE skill, got %.4f", v)
	}
	if got[MetricConfidenceBaseRate] != 0.2656 {
		t.Fatalf("base rate = %.4f, want the live 0.2656 — the skill score is meaningless without the "+
			"constant it is measured against", got[MetricConfidenceBaseRate])
	}
}

// TestNothingButTheDenominatorSurvivesAnEmptySampleSet re-pins REQ-2022 over the WIDENED family: adding
// gauges must not smuggle a new score past the N=0 withholding.
func TestNothingButTheDenominatorSurvivesAnEmptySampleSet(t *testing.T) {
	t.Parallel()
	got := names(CalibrationReading{N: 0, Brier: 0.4633, ECE: 0.5, MCE: 0.9, BaseRate: 0.3,
		Skill: -1.4, SkillDefined: true}) // scores deliberately populated: they must still be withheld
	if len(got) != 1 {
		t.Fatalf("at N=0 exactly one sample (the denominator) may be published, got %d: %v", len(got), got)
	}
	if _, ok := got[MetricConfidenceSamples]; !ok {
		t.Fatalf("the one published sample must be %s, got %v", MetricConfidenceSamples, got)
	}
}

// TestEveryCalibrationGaugeCarriesItsOutcomeLabel — a calibration number without its outcome variable is
// not interpretable. Measured live: the SAME stated confidences score Brier 0.4633 against blast-radius
// exactness and 0.0555 against diagnosis correctness. The unlabelled gauge was misread once already, by
// the author of the alert that read it, so the label is asserted on EVERY sample rather than on a sampled one.
func TestEveryCalibrationGaugeCarriesItsOutcomeLabel(t *testing.T) {
	t.Parallel()
	for _, c := range []CalibrationReading{
		{N: 0, Outcome: OutcomeBlastRadiusExact},
		{N: 64, Brier: 0.46, ECE: 0.5, MCE: 0.9, BaseRate: 0.27, Skill: -1.4, SkillDefined: true,
			Outcome: OutcomeBlastRadiusExact},
		{N: 619, Brier: 0.055, ECE: 0.14, MCE: 0.59, BaseRate: 0.95, SkillDefined: false,
			Outcome: OutcomeDiagnosisCorrect},
	} {
		samples := CalibrationSamples(c)
		if len(samples) == 0 {
			t.Fatalf("N=%d produced no samples at all", c.N)
		}
		for _, s := range samples {
			got, ok := s.Labels["outcome"]
			if !ok {
				t.Errorf("%s carries no `outcome` label — the number is uninterpretable without it", s.Name)
				continue
			}
			if got != c.Outcome {
				t.Errorf("%s labelled outcome=%q, want %q", s.Name, got, c.Outcome)
			}
		}
	}
}

// TestOutcomeLabelIsClampedToTheClosedSet — an unrecognised outcome silently redefines what every score
// beside it means, so it must clamp rather than be published verbatim.
func TestOutcomeLabelIsClampedToTheClosedSet(t *testing.T) {
	t.Parallel()
	for _, in := range []string{"", "made_up", "Diagnosis_Correct", "blast_radius"} {
		for _, s := range CalibrationSamples(CalibrationReading{N: 5, Outcome: in}) {
			if got := s.Labels["outcome"]; got != OutcomeOther {
				t.Errorf("outcome %q rendered as %q, want %q — an unbounded label is not a reference class",
					in, got, OutcomeOther)
			}
		}
	}
	for _, in := range []string{OutcomeBlastRadiusExact, OutcomeDiagnosisCorrect} {
		for _, s := range CalibrationSamples(CalibrationReading{N: 5, Outcome: in}) {
			if got := s.Labels["outcome"]; got != in {
				t.Errorf("known outcome %q was clamped to %q", in, got)
			}
		}
	}
}
