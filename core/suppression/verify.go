package suppression

import "context"

// CleanBootReason is the only boot reason that confirms a suppressed scheduled reboot (REQ-406). A
// suppression is provisional until the two-phase verify confirms the reboot was clean.
const CleanBootReason = "systemd-reboot"

// Reopener reopens an incident whose suppression was not confirmed.
type Reopener interface {
	Reopen(ctx context.Context, externalRef string) error
}

// Pager pages an approver tier.
type Pager interface {
	Page(ctx context.Context, externalRef, tier string) error
}

// TwoPhaseVerifier is the durable second phase of a suppressed scheduled reboot: after the host
// returns, it reads the recorded boot reason and confirms or reverses the suppression.
type TwoPhaseVerifier struct {
	Reopen Reopener
	Pager  Pager
}

// VerifyResult is the outcome of the two-phase verify.
type VerifyResult struct {
	Reopened   bool
	Confirmed  bool
	BootReason string
}

// Confirm is the verifier's CLASSIFICATION half with no side effects: it answers whether a recorded boot
// was a genuinely clean, orderly reboot. It is the same judgement Verify makes, split out so the LEARNING
// path can use the identical verifier on an occurrence that was never suppressed — where there is no
// incident to reopen and paging would be noise. An occurrence counts toward promotion ONLY when Confirm
// returns Confirmed, which is what makes "learned across at least two VERIFIED occurrences" true rather
// than "seen twice" (REQ-409/410).
func (v *TwoPhaseVerifier) Confirm(bootReason string) VerifyResult {
	if !IsCleanBoot(bootReason) {
		return VerifyResult{BootReason: bootReason}
	}
	return VerifyResult{Confirmed: true, BootReason: bootReason}
}

// Verify confirms a suppressed scheduled reboot. A boot reason that is NOT a genuinely clean reboot
// (IsCleanBoot) — a reactive OOM/panic/watchdog boot, or an unknown reason — reopens the incident and pages
// the approver graph (REQ-406); a clean one confirms the suppression and the incident stays closed.
func (v *TwoPhaseVerifier) Verify(ctx context.Context, externalRef, bootReason string) (VerifyResult, error) {
	if res := v.Confirm(bootReason); !res.Confirmed {
		if err := v.Reopen.Reopen(ctx, externalRef); err != nil {
			return VerifyResult{}, err
		}
		if err := v.Pager.Page(ctx, externalRef, "approver-graph"); err != nil {
			return VerifyResult{}, err
		}
		return VerifyResult{Reopened: true, BootReason: bootReason}, nil
	}
	return VerifyResult{Confirmed: true, BootReason: bootReason}, nil
}
