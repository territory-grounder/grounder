package skillstore

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"strings"
	"time"

	"golang.org/x/crypto/blake2b"
)

// The trial engine (spec/014 REQ-1306/1308/1309): deterministic arm assignment, the traffic-aware
// start refusal, and the timeout-sweep-then-finalize decision — the predecessor's prompt-patch-trial
// machinery transliterated, with its verified failure modes designed out (assignment-key
// normalization + malformed counter against the 2.5-month dark pipeline; start refusal against the
// starvation that completed zero trials in three months).

// Trial is one skill_trial row.
type Trial struct {
	ID               int64
	SkillName        string
	CandidateIDs     []int64
	ControlVersionID int64 // 0 = the compiled/production body is control
	Dimension        string
	MinSamplesPerArm int
	MinLift          float64
	PThreshold       float64
	EndsAt           time.Time
	Status           string
	Note             string
}

// TrialStore is the persistence surface the engine drives (pgx in core/db; MemTrialStore in tests).
type TrialStore interface {
	ActiveTrialFor(ctx context.Context, skillName string) (Trial, bool, error)
	ActiveTrials(ctx context.Context) ([]Trial, error)
	// Assign persists (external_ref, trial, variant) idempotently and returns the STORED variant —
	// read-before-hash: the row wins over any recomputation (REQ-1306).
	Assign(ctx context.Context, externalRef string, trialID int64, variant int) (int, error)
	// ArmScores returns the judged scores per variant (-1 = control) for the trial's dimension.
	ArmScores(ctx context.Context, trialID int64) (map[int][]float64, error)
	// FinalizeTrial writes the terminal status + winner columns + appends to the note log.
	FinalizeTrial(ctx context.Context, trialID int64, status string, winnerVersionID int64, winnerMean, winnerP float64, note string) error
	// JudgedSessionRate returns judged sessions/day over the trailing window (the start-refusal input).
	JudgedSessionRate(ctx context.Context, window time.Duration) (float64, error)
	// CreateTrial inserts an active trial row (refusal happens in StartTrial before this).
	CreateTrial(ctx context.Context, t Trial) (Trial, error)
}

// ErrMalformedRef rejects an empty/whitespace assignment key BEFORE hashing — the predecessor's
// unnormalized-key dark pipeline (2.5 months of unjoinable rows) made this a hard gate (REQ-1306).
var ErrMalformedRef = errors.New("skillstore: malformed external_ref for trial assignment")

// ErrTrialStarvation refuses a trial that cannot reach its sample minimum before its end date at the
// observed judged-session rate (REQ-1309) — the predecessor ran 50/arm online-only trials at a traffic
// level where ZERO trials completed in three months.
var ErrTrialStarvation = errors.New("skillstore: trial refused — projected completion exceeds the end date at the observed session rate")

// AssignArm computes the deterministic arm for a session: blake2b-8(ref|trial) mod (candidates+1),
// last bucket = control (returned as -1) — byte-compatible with the predecessor's Python (verified
// against its digests). The stored row wins over the computation (read-before-hash idempotency), so a
// later hash change can never flip a recorded arm.
func AssignArm(ctx context.Context, st TrialStore, externalRef string, tr Trial) (int, error) {
	ref := strings.TrimSpace(externalRef)
	if ref == "" {
		if c, ok := st.(interface{ CountMalformed() }); ok {
			c.CountMalformed() // the dead-man metric input (tg_skill_trial_malformed_refs_total)
		}
		return 0, ErrMalformedRef
	}
	h, err := blake2b.New(8, nil)
	if err != nil {
		return 0, err
	}
	fmt.Fprintf(h, "%s|%d", ref, tr.ID)
	buckets := uint64(len(tr.CandidateIDs) + 1)
	v := int(binary.BigEndian.Uint64(h.Sum(nil)) % buckets)
	if v == len(tr.CandidateIDs) {
		v = -1 // the control bucket is elided to -1 (the predecessor's control-arm elision)
	}
	return st.Assign(ctx, ref, tr.ID, v)
}

// StartTrial applies the traffic-aware refusal then inserts the active row (REQ-1309). The projection
// is deliberately simple and honest: every judged session carries every active trial's assignment, so
// time-to-completion = min_samples_per_arm × arms ÷ rate.
func StartTrial(ctx context.Context, st TrialStore, t Trial, now time.Time) (Trial, error) {
	rate, err := st.JudgedSessionRate(ctx, 14*24*time.Hour)
	if err != nil {
		return Trial{}, err
	}
	arms := float64(len(t.CandidateIDs) + 1)
	need := float64(t.MinSamplesPerArm) * arms
	if rate <= 0 {
		return Trial{}, fmt.Errorf("%w: no judged sessions in the trailing window", ErrTrialStarvation)
	}
	days := need / rate
	projected := now.Add(time.Duration(days * 24 * float64(time.Hour)))
	if projected.After(t.EndsAt) {
		return Trial{}, fmt.Errorf("%w: need %.0f judged sessions at %.1f/day ⇒ ~%s, ends %s",
			ErrTrialStarvation, need, rate, projected.UTC().Format("2006-01-02"), t.EndsAt.UTC().Format("2006-01-02"))
	}
	t.Status = "active"
	return st.CreateTrial(ctx, t)
}

// FinalizeOutcome is one trial's finalize decision.
type FinalizeOutcome struct {
	TrialID   int64
	Status    string // active (unchanged) | completed | aborted_no_winner | aborted_timeout
	WinnerIdx int    // -2 = none
	WinnerID  int64
	Reason    string
}

// SafetyScorer optionally supplies the safety-analog dimension's per-arm scores (appropriate_band).
// When the store provides it, a candidate that wins its target dimension but REGRESSES the safety
// analog against the concurrent control does not graduate (REQ-1308's asymmetric guard).
type SafetyScorer interface {
	SafetyArmScores(ctx context.Context, trialID int64) (map[int][]float64, error)
}

// FinalizeTrials runs the daily decision over every active trial: the TIMEOUT SWEEP FIRST (the
// predecessor's proven ordering), then per-trial: all arms at min samples AND best candidate mean >=
// control mean + lift AND one-sided Welch p < threshold AND the safety dimension not regressed →
// completed + winner; all-full-no-winner → aborted_no_winner; else unchanged. Scores arrive per
// dimension from ArmScores; the safety guard reads SafetyScores when the store provides them.
func FinalizeTrials(ctx context.Context, st TrialStore, now time.Time) ([]FinalizeOutcome, error) {
	trials, err := st.ActiveTrials(ctx)
	if err != nil {
		return nil, err
	}
	var out []FinalizeOutcome

	// Pass 1 — the timeout sweep (expired trials abort BEFORE any graduation is considered).
	remaining := trials[:0]
	for _, tr := range trials {
		if now.After(tr.EndsAt) {
			reason := "timeout sweep: ended " + tr.EndsAt.UTC().Format(time.RFC3339) + " before completing"
			if err := st.FinalizeTrial(ctx, tr.ID, "aborted_timeout", 0, 0, 0, reason); err != nil {
				return out, err
			}
			out = append(out, FinalizeOutcome{TrialID: tr.ID, Status: "aborted_timeout", WinnerIdx: -2, Reason: reason})
			continue
		}
		remaining = append(remaining, tr)
	}

	// Pass 2 — finalize the completable.
	for _, tr := range remaining {
		scores, err := st.ArmScores(ctx, tr.ID)
		if err != nil {
			return out, err
		}
		full := true
		for arm := -1; arm < len(tr.CandidateIDs); arm++ {
			if len(scores[arm]) < tr.MinSamplesPerArm {
				full = false
				break
			}
		}
		if !full {
			out = append(out, FinalizeOutcome{TrialID: tr.ID, Status: "active", WinnerIdx: -2, Reason: "arms below sample minimum"})
			continue
		}
		control := scores[-1]
		var safety map[int][]float64
		if ss, ok := st.(SafetyScorer); ok {
			if safety, err = ss.SafetyArmScores(ctx, tr.ID); err != nil {
				return out, err
			}
		}
		bestIdx, bestMean, bestP := -2, 0.0, 1.0
		for i := range tr.CandidateIDs {
			cand := scores[i]
			m := mean(cand)
			if m < mean(control)+tr.MinLift {
				continue
			}
			_, p := WelchOneSided(cand, control)
			if p >= tr.PThreshold {
				continue
			}
			// The asymmetric safety guard: a target-dimension win with a regressed safety analog does
			// not graduate (competence gains never buy down safety, REQ-1308).
			if safety != nil && len(safety[i]) > 0 && len(safety[-1]) > 0 && mean(safety[i]) < mean(safety[-1]) {
				continue
			}
			// Tie-break: strictly-greater keeps the FIRST candidate (lowest CandidateIDs index) on an
			// exact tie — deterministic by the trial's stored candidate order.
			if bestIdx == -2 || m > bestMean {
				bestIdx, bestMean, bestP = i, m, p
			}
		}
		if bestIdx == -2 {
			reason := fmt.Sprintf("all arms full; no candidate beat control %.3f by lift %.2f at p<%.2f", mean(control), tr.MinLift, tr.PThreshold)
			if err := st.FinalizeTrial(ctx, tr.ID, "aborted_no_winner", 0, 0, 0, reason); err != nil {
				return out, err
			}
			out = append(out, FinalizeOutcome{TrialID: tr.ID, Status: "aborted_no_winner", WinnerIdx: -2, Reason: reason})
			continue
		}
		reason := fmt.Sprintf("winner idx %d: mean %.3f vs control %.3f, p=%.4f (dimension %s)", bestIdx, bestMean, mean(scores[-1]), bestP, tr.Dimension)
		if err := st.FinalizeTrial(ctx, tr.ID, "completed", tr.CandidateIDs[bestIdx], bestMean, bestP, reason); err != nil {
			return out, err
		}
		out = append(out, FinalizeOutcome{TrialID: tr.ID, Status: "completed", WinnerIdx: bestIdx, WinnerID: tr.CandidateIDs[bestIdx], Reason: reason})
	}
	return out, nil
}
