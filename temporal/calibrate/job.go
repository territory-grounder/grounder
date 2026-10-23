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
	"fmt"

	"github.com/territory-grounder/grounder/core/metrics"
	"github.com/territory-grounder/grounder/core/observe"
	"log"
	"time"

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
// EmitTo returns an Emit that BOTH logs the curve and publishes it to /metrics. The log line is for an
// operator reading boot output; the gauges are for anyone who was not watching at the time.
//
// A 15-minute log line is not an observation surface: nobody greps a worker log to find out whether the
// agent's confidence means anything, so the answer sat where only a person already looking would see it.
// Measured live when this shipped: N=64, Brier 0.4633, ECE 0.5114, MCE 0.9000 — the stated confidence was
// worse than always guessing 0.5 (Brier 0.25), and no dashboard showed it.
func EmitTo(e observe.Emitter) func(core.Reliability) {
	return EmitFor(metrics.OutcomeBlastRadiusExact, e)
}

// EmitFor is EmitTo with the reference class named by the caller (TG-335).
//
// The outcome was hardcoded, so only one of the two classes in metrics' closed set could ever be published
// — OutcomeDiagnosisCorrect was a declared constant with no producer, and ClampCalibrationOutcome accepted a
// value nothing emitted. A `outcome` label carrying exactly one value is not a reference class, and the
// confidence alerts tell an operator to compare against the other one.
//
// The outcome is clamped at the emitter, not trusted from the caller: an unrecognised class silently
// redefines what every score beside it means.
func EmitFor(outcome string, e observe.Emitter) func(core.Reliability) {
	outcome = metrics.ClampCalibrationOutcome(outcome)
	return func(r core.Reliability) {
		LogReliability(r)
		observe.RecordCalibration(e, observe.CalibrationReading{
			N: r.N, Brier: r.Brier, ECE: r.ECE, MCE: r.MCE,
			BaseRate: r.BaseRate, Skill: r.SkillScore, SkillDefined: r.SkillDefined,
			Outcome: outcome,
		})
	}
}

// RunPeriodically performs one pass IMMEDIATELY and then one every `every` until ctx is done. It blocks;
// callers run it in a goroutine.
//
// The immediate pass is the point of this function, not a nicety. A bare `for range t.C` skips the first
// interval, so the calibration gauges did not exist for a full interval after every worker start — and
// ABSENT is not ZERO: the `tg_confidence_samples == 0` alert cannot observe a metric that is not being
// published, so the blind window was also unalertable. Measured live: a worker recreated at 21:07 published
// nothing until 21:22 while a dashboard showed the previous container's value, carried forward by
// Prometheus' 5-minute lookback. A window that opens on every deploy is exactly when someone looks.
//
// onErr receives a failed pass; a pass NEVER stops the loop and never propagates — the calibrator is
// observe-only and must not take the worker down with it.
func RunPeriodically(ctx context.Context, j Job, every time.Duration, onErr func(error)) {
	pass := func() {
		cctx, cancel := context.WithTimeout(ctx, every)
		defer cancel()
		if _, err := j.Run(cctx); err != nil && onErr != nil {
			onErr(err)
		}
	}
	pass()
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			pass()
		}
	}
}

func LogReliability(r core.Reliability) {
	if r.N == 0 {
		log.Printf("confidence calibrator: no evidence yet (0 paired confidence×verified-outcome samples) — observe-only, min_confidence gate stays OFF")
		return
	}
	skill := "undefined (degenerate base rate)"
	if r.SkillDefined {
		skill = fmt.Sprintf("%.4f", r.SkillScore)
	}
	log.Printf("confidence calibrator: N=%d Brier=%.4f ECE=%.4f MCE=%.4f base_rate=%.4f skill=%s — skill is scored against ALWAYS STATING THE BASE RATE, not against a coin; negative means the stated confidence carries LESS information than a constant. Measurement only; adjudicates and gates NOTHING (min_confidence gate stays OFF until calibrated)",
		r.N, r.Brier, r.ECE, r.MCE, r.BaseRate, skill)
}
