package cisco

// TG-85 write slice 3: the COMMIT-CONFIRMED reversibility primitive over the slice-2 config transport.
//
// On IOS, `configure terminal revert timer N` enters config with an ARMED auto-revert: if `configure confirm`
// (an EXEC command) does not run within N minutes — because an out-of-band verification failed, or the session
// or box dropped — the device ROLLS BACK the whole change by itself. This is the network-device analogue of the
// deferred verify the other actuation lanes use: apply behind a dead-man's-switch, verify out of band, confirm
// ONLY on success. This slice is the TRANSPORT for that two-phase flow — RunConfigWithRevert (apply + arm the
// timer, do NOT confirm) and ConfirmConfig (a separate session that commits) — so the reversibility mechanic is
// testable in structure now. The WIRING that drives ConfirmConfig from the interceptor's deferred verdict, and
// the device-family gate (ASA has no revert timer), land with the arm-live slice.
//
// NB — validated at ARM, not here: the exact IOS syntax (`configure terminal revert timer N` to arm,
// `configure confirm` at the exec prompt to commit, auto-revert on timeout) is reconstructed from IOS docs and
// exercised only against the in-process fake. A real device is the authority; the arm-live slice validates
// against one before this lane is ever wired. Ships DARK, floored never-auto by the interceptor.

import (
	"context"
	"fmt"
	"strings"

	"github.com/territory-grounder/grounder/adapters/actuation"
)

// RunConfigWithRevert applies the vetted lines inside a config session armed with an auto-revert timer of
// revertMinutes. It is EXACTLY RunConfig (positive config-mode gate, per-line fail-closed, `end` teardown)
// except it enters via `configure terminal revert timer N` and returns WITHOUT confirming — the change is live
// but the device will auto-revert it unless ConfirmConfig runs within the window. revertMinutes must be > 0: a
// commit-confirmed apply with no armed timer is a plain persistent write, which this method refuses.
func (c *configRunner) RunConfigWithRevert(ctx context.Context, lines []string, revertMinutes int) (actuation.Result, error) {
	if revertMinutes <= 0 {
		return actuation.Result{}, fmt.Errorf("cisco config: RunConfigWithRevert needs a positive revert timer (got %d) — a commit-confirmed apply must arm the auto-revert dead-man's-switch", revertMinutes)
	}
	if len(lines) == 0 {
		return actuation.Result{}, fmt.Errorf("cisco config: no lines to apply (refusing an empty change)")
	}
	stdin, exp, cleanup, err := c.r.dialSession(ctx)
	if err != nil {
		return actuation.Result{}, err
	}
	defer cleanup()

	var transcript strings.Builder
	enter := fmt.Sprintf("configure terminal revert timer %d", revertMinutes)
	if err := c.enterConfig(ctx, stdin, exp, enter, &transcript); err != nil {
		return resultOf(&transcript), err
	}
	if err := c.applyLines(ctx, stdin, exp, lines, &transcript); err != nil {
		return resultOf(&transcript), err
	}
	if err := c.leaveConfig(ctx, stdin, exp, &transcript); err != nil {
		return resultOf(&transcript), err
	}
	// Deliberately NO confirm here — the change is live under the armed timer, pending an out-of-band verdict.
	return resultOf(&transcript), nil
}

// ConfirmConfig commits a change applied under a revert timer by sending the IOS `configure confirm` EXEC
// command, cancelling the pending auto-revert. It opens its OWN session — the apply session has already ended,
// and confirm is a distinct, separately-verified actuation. It fails closed if the device reports an error: a
// `%`-line here means no rollback was pending, or the timer already fired and reverted, which the caller MUST
// treat as "the change is NOT committed", never as success.
func (c *configRunner) ConfirmConfig(ctx context.Context) (actuation.Result, error) {
	stdin, exp, cleanup, err := c.r.dialSession(ctx)
	if err != nil {
		return actuation.Result{}, err
	}
	defer cleanup()

	var transcript strings.Builder
	if err := sendLine(stdin, "configure confirm"); err != nil {
		return actuation.Result{}, fmt.Errorf("cisco config: send confirm: %w", err)
	}
	out, err := exp.until(ctx)
	if err != nil {
		return actuation.Result{}, fmt.Errorf("cisco config: no prompt after confirm: %w", err)
	}
	transcript.WriteString(out)
	if bad := deviceError(out); bad != "" {
		return resultOf(&transcript), fmt.Errorf("cisco config: confirm rejected: %s — the change is NOT committed (no rollback pending, or the revert timer already fired)", bad)
	}
	_ = sendLine(stdin, "exit")
	return resultOf(&transcript), nil
}
