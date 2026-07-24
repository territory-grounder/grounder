// Package calibrate turns the agent's self-reported confidence into a MEASURED quantity by scoring it against
// the LLM-free mechanical verified outcome (spec/020 T-020-15, REQ-2021). It is OBSERVE-ONLY: it adjudicates
// nothing and gates nothing — the reliability curve it produces is the EVIDENCE an operator uses to decide
// whether a stated "0.8" empirically resolves ~80% of the time, before the policy min_confidence clamp is ever
// trusted as a gate. The literature is unambiguous that RLHF makes verbalized confidence overconfident (the
// GPT-4 technical report's calibration finding; the wisdom-source survey), so calibration MUST be re-fit
// against real outcomes rather than taken at face value — this package is that re-fit, and this file is its
// pure, deterministic math core (no I/O, no DB, CI-testable).
package calibrate

import (
	"context"
	"math"
)

// SampleReader is the read seam the calibrator job pulls (stated confidence, verified-clean) pairs from
// (spec/020 T-020-15, REQ-2021). The pgx implementation (core/db) joins session_triage.confidence to the
// LLM-free falsify outcome on infragraph_prediction by external_ref; the in-memory fake is the oracle twin.
// It is a pure READ — resolving samples never adjudicates or gates anything (observe-only, INV-22).
type SampleReader interface {
	// PairedSamples returns up to limit (confidence, clean) pairs; an empty slice is the honest "no evidence
	// yet" that Compute renders as a zero-value reliability, never a fabricated curve.
	PairedSamples(ctx context.Context, limit int) ([]Sample, error)
}

// Sample is one (stated confidence, verified outcome) pair — the join of session_triage.confidence (the
// agent's stated confidence, migration 0024) to the LLM-free mechanical verified outcome (the falsifiability
// match on infragraph_prediction, INV-10) by external_ref (migration 0026). Clean is the ground truth: did the
// action's prediction verify as clean (the incident resolved as predicted) — the only honest label for "was
// this confidence right".
type Sample struct {
	Confidence float64 // the agent's stated 0..1 confidence (clamped to [0,1] on Compute)
	Clean      bool    // the verified-clean outcome (treated as 1.0) vs not-clean (0.0)
}

// Bin is one reliability-curve bucket: a confidence interval, its sample count, the mean STATED confidence in
// it, and the empirical CLEAN RATE (the fraction that actually verified clean). A well-calibrated model has
// MeanConf ≈ CleanRate in every populated bin; MeanConf − CleanRate > 0 means OVERCONFIDENT (the expected RLHF
// direction).
type Bin struct {
	Lo        float64 // inclusive lower confidence bound
	Hi        float64 // exclusive upper bound (the top bin includes 1.0)
	Count     int
	MeanConf  float64 // mean stated confidence of the bin's samples (0 when empty)
	CleanRate float64 // fraction verified clean = the empirical accuracy (0 when empty)
}

// Reliability is the calibration result over a sample set: the per-bin reliability curve plus the scalar
// scores. Brier is the mean squared error of confidence vs outcome (0 = perfect, lower is better). ECE
// (Expected Calibration Error) is the sample-weighted mean gap |MeanConf − CleanRate| across populated bins
// (0 = perfectly calibrated). MCE (Max Calibration Error) is the worst single populated bin's gap.
type Reliability struct {
	N     int
	Bins  []Bin
	Brier float64
	ECE   float64
	MCE   float64
}

// Compute bins the samples into `bins` equal-width buckets over [0,1] and computes the reliability curve plus
// Brier/ECE/MCE. It is PURE and deterministic (no I/O). A confidence outside [0,1] is clamped (defensive — the
// proposal grammar already bounds it to [0,1]). bins < 1 is treated as 1. An EMPTY sample set yields the
// zero-value (N=0, empty-count bins, all scores 0) — an honest "no evidence yet", never a fabricated curve.
func Compute(samples []Sample, bins int) Reliability {
	if bins < 1 {
		bins = 1
	}
	r := Reliability{Bins: make([]Bin, bins)}
	width := 1.0 / float64(bins)
	confSum := make([]float64, bins)
	cleanCount := make([]int, bins)
	var brierSum float64
	for _, s := range samples {
		c := s.Confidence
		switch {
		case c < 0:
			c = 0
		case c > 1:
			c = 1
		}
		bi := int(c / width) // [lo, hi); the top bin includes 1.0
		if bi >= bins {
			bi = bins - 1
		}
		outcome := 0.0
		if s.Clean {
			outcome = 1.0
			cleanCount[bi]++
		}
		confSum[bi] += c
		r.Bins[bi].Count++
		brierSum += (c - outcome) * (c - outcome)
		r.N++
	}
	for i := range r.Bins {
		b := &r.Bins[i]
		b.Lo = float64(i) * width
		b.Hi = float64(i+1) * width
		if i == bins-1 {
			b.Hi = 1.0
		}
		if b.Count > 0 {
			b.MeanConf = confSum[i] / float64(b.Count)
			b.CleanRate = float64(cleanCount[i]) / float64(b.Count)
		}
	}
	if r.N == 0 {
		return r
	}
	r.Brier = brierSum / float64(r.N)
	for i := range r.Bins {
		b := r.Bins[i]
		if b.Count == 0 {
			continue
		}
		gap := math.Abs(b.MeanConf - b.CleanRate)
		r.ECE += (float64(b.Count) / float64(r.N)) * gap
		if gap > r.MCE {
			r.MCE = gap
		}
	}
	return r
}
