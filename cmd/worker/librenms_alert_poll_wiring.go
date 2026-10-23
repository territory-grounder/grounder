package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"time"

	enumspb "go.temporal.io/api/enums/v1"
	serviceerror "go.temporal.io/api/serviceerror"
	"go.temporal.io/sdk/client"

	"github.com/territory-grounder/grounder/modules/ingest/librenms"
	tg "github.com/territory-grounder/grounder/temporal"
	"github.com/territory-grounder/grounder/temporal/runner"
)

// wireLibrenmsAlertPoll arms the opt-in LibreNMS active-alert pull (TG-344), carved out of main()'s
// composition root (TG-501 LOC-debt paydown): OFF by default (TG_LIBRENMS_ALERT_POLL_INTERVAL unset,
// push-only) — see the comment below for the primary-intake / missed-push-safety-net distinction. Returns
// the constructed alert source (nil unless armed) so the caller can hand it to the upstream-probe wiring:
// the SAME source doubles as a read-only counter of what each LibreNMS currently has (TG-344). Behaviour is
// unchanged by the move.
func wireLibrenmsAlertPoll(c client.Client) (upstreamProbeSource *librenms.AlertSource) {
	// LibreNMS alert intake is PUSH by default: LibreNMS transports POST each alert to /v1/ingest/librenms
	// authenticated by a per-source static bearer (AuthIngestPush), exactly like Alertmanager — the earlier
	// belief that "LibreNMS's transport cannot sign TG's HMAC ingest, so native pull is the only path" is
	// obsolete now that the bearer path exists. The two servers (NL, GR) share the one endpoint and are
	// discriminated by the payload's `site` field. This active-alert PULL is an OPT-IN complement to push: it
	// stays OFF unless TG_LIBRENMS_ALERT_POLL_INTERVAL is set to a duration. It serves TWO roles by the
	// TG_LIBRENMS_ALERT_POLL_MIN_AGE gate: with MIN_AGE unset/0 it is the air-gapped / transport-less PRIMARY
	// intake (every firing alert); with MIN_AGE>0 it is a missed-push RECONCILIATION SAFETY-NET that re-triages
	// only alerts stale enough that a prompt push should already have covered them — so a delayed or dropped
	// LibreNMS transport no longer leaves a down host unhealed (the failure the 2026-07-24 drill exposed),
	// WITHOUT reacting to a transient that push's anti-flap delay suppresses. When enabled, each tick fetches
	// every configured deployment's firing alerts (state=1, read-only) and mints ONE triage RunnerWorkflow per
	// alert, deduped by workflow id (REJECT_DUPLICATE) so a still-firing alert is triaged exactly once whether
	// it arrives by push or pull — and only when a deployment is also configured. Best-effort throughout: a
	// fetch or start failure logs and continues — the poller NEVER crashes the worker, and it never mutates the
	// estate (mutation stays OFF).
	// PLANE-SCOPED (TG-153): this is INGEST — it pulls attacker-authored alert bodies and mints one triage
	// session per alert. The actuation plane must not ingest, so the interval is withheld there and the poller
	// is never constructed (falling into the existing "unset ⇒ push-only" branch, unchanged for everyone else).
	if iv := planeEnv("TG_LIBRENMS_ALERT_POLL_INTERVAL", ""); iv != "" {
		alertDeps := librenmsDeployments(planeEnv("TG_LIBRENMS_DEPLOYMENTS_AGENT_TOOLS", ""))
		if d, err := time.ParseDuration(iv); err == nil && d > 0 && len(alertDeps) > 0 {
			// Parse MIN_AGE EXPLICITLY (not envDuration, which silently defaults on error): a typo'd duration
			// must LOUDLY revert to primary-intake, never quietly, given the safety-net is a resilience path.
			minAge := time.Duration(0)
			if ma := getenv("TG_LIBRENMS_ALERT_POLL_MIN_AGE", ""); ma != "" {
				if pd, perr := time.ParseDuration(ma); perr == nil {
					minAge = pd
				} else {
					log.Printf("librenms: TG_LIBRENMS_ALERT_POLL_MIN_AGE=%q unparseable (%v) — safety-net DISABLED, pulling EVERY active alert (set e.g. 5m)", ma, perr)
				}
			}
			alertSrc := librenms.NewAlertSource(alertDeps, librenms.WithAlertHTTPClient(estateHTTPClient(truthyEnv("TG_LIBRENMS_INSECURE"))), librenms.WithAlertMinAge(minAge))
			// Hoisted for the UPSTREAM PROBE (TG-344): the same source doubles as a read-only counter of
			// what each LibreNMS currently HAS, which is the denominator every arrived-count was missing.
			upstreamProbeSource = alertSrc
			go func() {
				t := time.NewTicker(d)
				defer t.Stop()
				for range t.C {
					ctx, cancel := context.WithTimeout(context.Background(), d)
					envs, withheld, ferr := alertSrc.FetchActive(ctx)
					if ferr != nil {
						log.Printf("librenms alert poll: fetch failed: %v (retry next tick)", ferr)
						cancel()
						continue
					}
					minted, already := 0, 0
					for _, env := range envs {
						_, serr := c.ExecuteWorkflow(ctx, client.StartWorkflowOptions{
							ID:                    tg.WorkflowID(env.ExternalRef),
							TaskQueue:             tg.TaskQueueRunner,
							WorkflowIDReusePolicy: enumspb.WORKFLOW_ID_REUSE_POLICY_REJECT_DUPLICATE,
						}, runner.RunnerWorkflow, env)
						if serr != nil {
							var startedErr *serviceerror.WorkflowExecutionAlreadyStarted
							if errors.As(serr, &startedErr) {
								already++ // this firing alert is already being (or was) triaged — dedup
								continue
							}
							log.Printf("librenms alert poll: mint triage %s failed: %v", env.ExternalRef, serr)
							continue
						}
						minted++
					}
					if minted > 0 || already > 0 || withheld > 0 {
						// withheld = held back by the min-age safety-net gate. A PERSISTENTLY high withheld with
						// zero minted is the signal to check MIN_AGE (too high) or a deployment's Timezone (a wrong
						// TZ clamps every timestamp to now → age≈0 → all withheld). Never a silent drop.
						log.Printf("librenms alert poll: %d new triage(s), %d already-firing, %d withheld (younger than min-age)", minted, already, withheld)
					}
					cancel()
				}
			}()
			mode := "PRIMARY intake (air-gapped/transport-less; all firing alerts)"
			if minAge > 0 {
				mode = fmt.Sprintf("missed-push SAFETY-NET (only alerts firing >= %s; push stays primary)", minAge)
			}
			log.Printf("librenms: active-alert pull every %s across %d deployment(s) (read-only; never actuates) — %s", d, len(alertDeps), mode)
		} else if len(alertDeps) == 0 {
			log.Printf("librenms: alert pull idle — no TG_LIBRENMS_DEPLOYMENTS configured")
		}
	} else {
		log.Printf("librenms: alert intake is PUSH — LibreNMS transports POST /v1/ingest/librenms (per-source bearer, AuthIngestPush); active-alert pull OFF (set TG_LIBRENMS_ALERT_POLL_INTERVAL to enable the opt-in pull: air-gapped primary intake, or a missed-push safety-net with TG_LIBRENMS_ALERT_POLL_MIN_AGE)")
	}
	return
}
