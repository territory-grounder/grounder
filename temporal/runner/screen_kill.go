package runner

// The KILLED terminal (TG-80 P2-6; clean-room from h-apache-stack's kill-and-flag-hostile pattern,
// attribution: SOURCE-BENCHMARK-CATALOG). Today a jailbreak hit in the MODEL'S OWN OUTPUT (the
// proposal rationale/approval text, screened at classify) forces POLL_PAUSE: the poisoned proposal
// still flows to the gate, seals a manifest, and waits for a human vote. The armed kill terminal ends
// the session INSTEAD — no gate, no vote, no manifest for a proposal an injected instruction may be
// steering — with a first-class `killed:hostile-output` outcome on the session record and a
// `screen:killed` entry on the governance ledger (the "one channel allowed to say no" vocabulary).
//
// SHIPS OFF: TG_SCREEN_KILL_TERMINAL unset/blank keeps today's poll path byte-identical — the flip is
// an eval-gated arming decision, because killing sessions changes the judged population. The hostile
// DISPOSITION half (audit-row signal + repeat-offender count) is always on; it is observe-only.

import (
	"context"
	"os"
	"strings"
	"time"

	"github.com/territory-grounder/grounder/core/audit"
)

// hostileDispositionWindow bounds the repeat-offender lookback: prior jailbreak-polled
// classifications for the incident host inside this window count toward the disposition.
const hostileDispositionWindow = 7 * 24 * time.Hour

// ScreenKillInput carries what the ledger entry needs; the disposition was computed at classify (it
// rides Decision.Signals) so the kill activity never re-reads the database.
type ScreenKillInput struct {
	ExternalRef string
	ActionID    string
	Disposition string // "jailbreak-output" or "repeat-offender:<n>", from Decision.Signals
}

// ScreenKillResult reports whether the kill terminal is armed. The ledger append happens HERE (an
// activity — the workflow must not do I/O) and only when armed: an unarmed deployment leaves no trace
// but the always-on audit-row disposition.
type ScreenKillResult struct {
	Armed bool
}

// ScreenKillActivity reads the flip and, when armed, appends the screen:killed governance entry. The
// activity itself never decides the outcome — the workflow owns the terminal — and an unarmed answer
// is the pre-feature path.
func (a *Activities) ScreenKillActivity(ctx context.Context, in ScreenKillInput) (ScreenKillResult, error) {
	v := strings.ToLower(strings.TrimSpace(os.Getenv("TG_SCREEN_KILL_TERMINAL")))
	armed := v == "1" || v == "true" || v == "yes"
	if armed && a.D.Ledger != nil {
		_, _ = a.D.Ledger.AppendContext(ctx, audit.GovDecision{
			Decision: "screen:killed",
			Reason:   "hostile model output (" + in.Disposition + ") — session " + in.ExternalRef + " terminated before gate/vote/manifest",
			ActionID: in.ActionID,
			Withheld: true,
		})
	}
	return ScreenKillResult{Armed: armed}, nil
}
