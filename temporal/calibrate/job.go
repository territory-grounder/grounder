// Package calibrate is the read-only confidence-calibration pass (spec/020 T-020-15, REQ-2021): it reads the
// persisted (stated confidence, verified-clean) pairs, bins them into a reliability curve with Brier/ECE/MCE,
// and emits the scorecard. It is OBSERVE-ONLY — it adjudicates nothing, gates nothing, and NEVER reaches an
// actuator or changes the mutation posture. The reliability it produces is EVIDENCE an operator uses to
// decide whether a stated confidence is empirically meaningful ("0.8 resolves ~80%") BEFORE the policy
// min_confidence clamp is ever trusted as a gate — which stays OFF until calibrated (INV-22). The heavy
// lifting is the pure core/calibrate math; this package is the periodic orchestration around it.
package calibrate

import (
	"context"
	"log"

	core "github.com/territory-grounder/grounder/core/calibrate"
)

// Job performs one calibration pass. Reader supplies the paired samples (a pgx store in production, an
// in-memory fake in tests); Emit is the observe sink for the resulting reliability (log/expose), nil to
// discard. Bins/Limit default to 10/5000 when unset.
type Job struct {
	Reader core.SampleReader
	Bins   int
	Limit  int
	Emit   func(core.Reliability)
}

// Run performs one calibration pass: read samples, Compute the reliability, emit it, and return it. It is a
// pure READ + pure MATH + an emit — no mutation, no actuation, no gating. An empty sample set yields the
// zero-value reliability (an honest "no evidence yet"), never a fabricated curve.
func (j Job) Run(ctx context.Context) (core.Reliability, error) {
	bins := j.Bins
	if bins <= 0 {
		bins = 10
	}
	limit := j.Limit
	if limit <= 0 {
		limit = 5000
	}
	samples, err := j.Reader.PairedSamples(ctx, limit)
	if err != nil {
		return core.Reliability{}, err
	}
	r := core.Compute(samples, bins)
	if j.Emit != nil {
		j.Emit(r)
	}
	return r, nil
}

// LogReliability is a convenient Emit that logs the scorecard honestly — including the N=0 "no evidence yet"
// state, so an operator sees the calibrator is LIVE but unfed rather than a silent gap. It never claims a
// calibration it does not have.
func LogReliability(r core.Reliability) {
	if r.N == 0 {
		log.Printf("confidence calibrator: no evidence yet (0 paired confidence×verified-outcome samples) — observe-only, min_confidence gate stays OFF")
		return
	}
	log.Printf("confidence calibrator: N=%d Brier=%.4f ECE=%.4f MCE=%.4f — measurement only; adjudicates and gates NOTHING (min_confidence gate stays OFF until calibrated)",
		r.N, r.Brier, r.ECE, r.MCE)
}
