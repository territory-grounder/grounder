package cisco

// configRunner is the production ConfigRunner (TG-85 write slice 2): the concrete CONFIG-mode transport behind
// WriteModule's ConfigRunner seam. It drives a config session over the SAME verified PTY transport the read
// runner uses (interactiveRunner.dialSession — one dial/host-key/watchdog/PTY/pager-off path), enters config
// mode, applies the already-vetted lines in order, and FAILS CLOSED the instant the device rejects config mode
// or any line. WriteModule has already enforced the mode chokepoint and the config-line allowlist/guard, so
// this type sends exactly what it is given and reports what the device said — it adds no policy, only transport.
//
// SLICE 2 IS THE TRANSPORT + FAIL-CLOSED DEVICE-ERROR DETECTION. The commit-confirmed REVERSIBILITY mechanic
// (IOS `configure terminal revert timer N` … `configure confirm`, so an unconfirmed change auto-rolls-back if
// verification fails or the session drops) is SLICE 3, layered on top before any arm-live. Like the read runner
// and WriteModule, nothing wires this — the whole write lane ships DARK, floored never-auto by the interceptor.

import (
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/territory-grounder/grounder/adapters/actuation"
)

type configRunner struct {
	r *interactiveRunner
}

// NewConfigRunner builds the production config-session runner for a device, over the same dial transport as the
// read runner (NewInteractiveRunner).
func NewConfigRunner(dev Device) *configRunner {
	return &configRunner{r: NewInteractiveRunner(dev)}
}

// RunConfig opens the session, enters config mode, applies each line in order, leaves config mode, and returns
// the full transcript. It fails CLOSED if the device rejects config mode or any line: Cisco IOS/ASA reports a
// rejected command with a line beginning `%` ("% Invalid input", "% Incomplete command", "% Ambiguous
// command", "% Authorization failed"). On such a rejection it STOPS immediately — it does not send the
// remaining lines — and returns the transcript so far with an error, so a caller never reads a partial,
// half-rejected change as success. The already-applied lines persist until slice 3's revert timer or an
// explicit inverse; that is precisely why the write lane refuses a change carrying neither.
func (c *configRunner) RunConfig(ctx context.Context, lines []string) (actuation.Result, error) {
	if len(lines) == 0 {
		return actuation.Result{}, fmt.Errorf("cisco config: no lines to apply (refusing an empty change)")
	}
	stdin, exp, cleanup, err := c.r.dialSession(ctx)
	if err != nil {
		return actuation.Result{}, err
	}
	defer cleanup()

	var transcript strings.Builder
	// Enter config mode (the transport's job — WriteModule forbids the model from naming `configure`/`terminal`),
	// apply the vetted lines, leave. Each step is a shared helper so the commit-confirmed variant (slice 3) drives
	// the SAME apply/fail-closed path, differing only in the enter command and the confirm phase.
	if err := c.enterConfig(ctx, stdin, exp, "configure terminal", &transcript); err != nil {
		return resultOf(&transcript), err
	}
	if err := c.applyLines(ctx, stdin, exp, lines, &transcript); err != nil {
		return resultOf(&transcript), err
	}
	if err := c.leaveConfig(ctx, stdin, exp, &transcript); err != nil {
		return resultOf(&transcript), err
	}
	return resultOf(&transcript), nil
}

// enterConfig sends enterCmd (`configure terminal`, or the revert-timer variant), then gates config-mode entry
// BOTH ways: no `%` device error AND a positive `(config)` prompt must appear, else it fails closed — a device
// that silently stayed in exec mode must never receive config lines as exec commands.
func (c *configRunner) enterConfig(ctx context.Context, stdin io.WriteCloser, exp *expecter, enterCmd string, transcript *strings.Builder) error {
	if err := sendLine(stdin, enterCmd); err != nil {
		return fmt.Errorf("cisco config: enter config mode (%q): %w", enterCmd, err)
	}
	out, err := exp.until(ctx)
	if err != nil {
		return fmt.Errorf("cisco config: no prompt after %q: %w", enterCmd, err)
	}
	transcript.WriteString(out)
	if bad := deviceError(out); bad != "" {
		return fmt.Errorf("cisco config: device refused config mode (%q): %s", enterCmd, bad)
	}
	if !enteredConfigMode(out) {
		return fmt.Errorf("cisco config: device did not enter config mode (no (config) prompt after %q) — refusing to send config lines as exec commands (fail closed)", enterCmd)
	}
	return nil
}

// applyLines sends each vetted line in order, failing CLOSED the instant the device rejects one (a `%`-prefixed
// line): it stops, so the remaining lines are NOT sent and a partial/half-rejected change is never success.
func (c *configRunner) applyLines(ctx context.Context, stdin io.WriteCloser, exp *expecter, lines []string, transcript *strings.Builder) error {
	for i, line := range lines {
		if err := sendLine(stdin, line); err != nil {
			return fmt.Errorf("cisco config: send line %d (%q): %w", i+1, line, err)
		}
		out, err := exp.until(ctx)
		if err != nil {
			return fmt.Errorf("cisco config: no prompt after line %d (%q): %w", i+1, line, err)
		}
		transcript.WriteString(out)
		if bad := deviceError(out); bad != "" {
			return fmt.Errorf("cisco config: device rejected line %d (%q): %s", i+1, line, bad)
		}
	}
	return nil
}

// leaveConfig sends `end` then a best-effort `exit`. Only the `end` SEND failure is surfaced; the prompt-wait and
// `exit` are best-effort (the deferred cleanup handles a device that ignores exit). Every config line was already
// confirmed line-by-line, so no unconfirmed/unsent line can leak through the teardown.
func (c *configRunner) leaveConfig(ctx context.Context, stdin io.WriteCloser, exp *expecter, transcript *strings.Builder) error {
	if err := sendLine(stdin, "end"); err != nil {
		return fmt.Errorf("cisco config: leave config mode: %w", err)
	}
	if out, err := exp.until(ctx); err == nil {
		transcript.WriteString(out)
	}
	_ = sendLine(stdin, "exit")
	return nil
}

func resultOf(b *strings.Builder) actuation.Result {
	return actuation.Result{Stdout: []byte(b.String())}
}

// enteredConfigMode reports whether the device's response shows a config-mode prompt. Cisco IOS/ASA config and
// every sub-config prompt contains "(config" ("hostname(config)#", "(config-if)#", "(config-line)#" …). This
// is the POSITIVE half of the config-mode gate: without it, a device that silently stayed in exec mode would
// receive config lines as exec commands.
func enteredConfigMode(out string) bool {
	return strings.Contains(out, "(config")
}

// deviceError returns the first line of out that Cisco IOS/ASA uses to report a rejected command — a line whose
// first non-space rune is '%'. Empty string ⇒ the device accepted the command. This is the fail-closed sensor:
// a config line the device could not parse or apply is NEVER treated as success.
func deviceError(out string) string {
	for ln := range strings.SplitSeq(out, "\n") {
		s := strings.TrimSpace(ln)
		if strings.HasPrefix(s, "%") {
			return s
		}
	}
	return ""
}

var _ ConfigRunner = (*configRunner)(nil)
