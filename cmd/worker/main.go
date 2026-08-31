// Command worker runs the Territory Grounder Temporal worker: it registers the read-only session
// Runner workflow and its activities on the runner task queue. In Phase 0/1 mutation is OFF, so the
// worker drives incidents to a sealed, classified, gated proposal and stops at propose — it never
// executes an estate mutation.
//
// It requires a running Temporal server (TG_TEMPORAL_HOSTPORT) and the bundled LiteLLM gateway
// (TG_LITELLM_URL) at runtime. [O] INV-09/INV-21 · [R] paradigm-rule 7, EXECUTION-PLAN P1-7/P1-9.
package main

import (
	_ "time/tzdata" // embed the IANA zoneinfo DB so time.LoadLocation resolves on distroless (no OS tzdata)

	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	enumspb "go.temporal.io/api/enums/v1"
	serviceerror "go.temporal.io/api/serviceerror"
	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/worker"

	"github.com/territory-grounder/grounder/adapters/actorevidence"
	actuation "github.com/territory-grounder/grounder/adapters/actuation"
	cmdb "github.com/territory-grounder/grounder/adapters/cmdb"
	"github.com/territory-grounder/grounder/adapters/model"
	notifier "github.com/territory-grounder/grounder/adapters/notifier"
	observability "github.com/territory-grounder/grounder/adapters/observability"
	"github.com/territory-grounder/grounder/adapters/rerank"
	tracker "github.com/territory-grounder/grounder/adapters/tracker"
	"github.com/territory-grounder/grounder/agent"
	"github.com/territory-grounder/grounder/core/actuate"
	"github.com/territory-grounder/grounder/core/attribution"
	"github.com/territory-grounder/grounder/core/attribution/readertally"
	"github.com/territory-grounder/grounder/core/audit"
	"github.com/territory-grounder/grounder/core/breaker"
	"github.com/territory-grounder/grounder/core/config"
	"github.com/territory-grounder/grounder/core/correlate"
	"github.com/territory-grounder/grounder/core/cost"
	"github.com/territory-grounder/grounder/core/cpconfig"
	"github.com/territory-grounder/grounder/core/credential"
	"github.com/territory-grounder/grounder/core/credential/dyndb"
	"github.com/territory-grounder/grounder/core/credential/sshca"
	"github.com/territory-grounder/grounder/core/db"
	coreesc "github.com/territory-grounder/grounder/core/escalation"
	"github.com/territory-grounder/grounder/core/estate"
	"github.com/territory-grounder/grounder/core/falsify"
	coregov "github.com/territory-grounder/grounder/core/governance"
	"github.com/territory-grounder/grounder/core/httpapi"
	coreingest "github.com/territory-grounder/grounder/core/ingest"
	"github.com/territory-grounder/grounder/core/judge"
	"github.com/territory-grounder/grounder/core/knowledge"
	"github.com/territory-grounder/grounder/core/learn"
	"github.com/territory-grounder/grounder/core/lessons"
	"github.com/territory-grounder/grounder/core/metrics"
	"github.com/territory-grounder/grounder/core/observe"
	"github.com/territory-grounder/grounder/core/opclasscat"
	"github.com/territory-grounder/grounder/core/persist"
	"github.com/territory-grounder/grounder/core/policy"
	"github.com/territory-grounder/grounder/core/predict"
	"github.com/territory-grounder/grounder/core/preflight"
	"github.com/territory-grounder/grounder/core/regime"
	"github.com/territory-grounder/grounder/core/risk"
	"github.com/territory-grounder/grounder/core/safety"
	"github.com/territory-grounder/grounder/core/seal"
	"github.com/territory-grounder/grounder/core/sessionspan"
	"github.com/territory-grounder/grounder/core/skillstore"
	"github.com/territory-grounder/grounder/core/suppression"
	"github.com/territory-grounder/grounder/core/suppressionshadow"
	"github.com/territory-grounder/grounder/core/territory"
	tracepkg "github.com/territory-grounder/grounder/core/trace"
	"github.com/territory-grounder/grounder/core/trackerimport"
	"github.com/territory-grounder/grounder/core/verify"
	"github.com/territory-grounder/grounder/core/wikicompile"
	"github.com/territory-grounder/grounder/core/wiring"
	"github.com/territory-grounder/grounder/eval"
	"github.com/territory-grounder/grounder/modules"
	awxattr "github.com/territory-grounder/grounder/modules/actorevidence/awx"
	"github.com/territory-grounder/grounder/modules/actorevidence/gitopsmr"
	"github.com/territory-grounder/grounder/modules/actorevidence/journal"
	"github.com/territory-grounder/grounder/modules/actorevidence/ldapident"
	netboxattr "github.com/territory-grounder/grounder/modules/actorevidence/netbox"
	pveattr "github.com/territory-grounder/grounder/modules/actorevidence/pve"
	actorevidencetool "github.com/territory-grounder/grounder/modules/actorevidence/tool"
	sshactuation "github.com/territory-grounder/grounder/modules/actuation/ssh"
	"github.com/territory-grounder/grounder/modules/bootstrap"
	"github.com/territory-grounder/grounder/modules/catalog"
	"github.com/territory-grounder/grounder/modules/cmdb/netbox"
	"github.com/territory-grounder/grounder/modules/cmdb/pve"
	"github.com/territory-grounder/grounder/modules/cmdb/pve/confighash"
	"github.com/territory-grounder/grounder/modules/cmdb/slurpit"
	"github.com/territory-grounder/grounder/modules/cmdb/vsphere"
	"github.com/territory-grounder/grounder/modules/credsource/nativedb"
	dockerdisc "github.com/territory-grounder/grounder/modules/discovery/docker"
	systemddisc "github.com/territory-grounder/grounder/modules/discovery/systemd"
	estatetools "github.com/territory-grounder/grounder/modules/estate"
	"github.com/territory-grounder/grounder/modules/ingest/authlog"
	"github.com/territory-grounder/grounder/modules/ingest/librenms"
	"github.com/territory-grounder/grounder/modules/ingest/pveliveness"
	"github.com/territory-grounder/grounder/modules/observability/hostdiag"
	"github.com/territory-grounder/grounder/modules/observability/incidenthistory"
	"github.com/territory-grounder/grounder/modules/observability/openobserve"
	"github.com/territory-grounder/grounder/modules/observability/syslogng"
	"github.com/territory-grounder/grounder/modules/resolve"
	"github.com/territory-grounder/grounder/modules/telemetry"
	"github.com/territory-grounder/grounder/modules/tracker/trackerhistory"
	tg "github.com/territory-grounder/grounder/temporal"
	calibratejob "github.com/territory-grounder/grounder/temporal/calibrate"
	"github.com/territory-grounder/grounder/temporal/configwrite"
	"github.com/territory-grounder/grounder/temporal/credentialsync"
	"github.com/territory-grounder/grounder/temporal/enginetoggle"
	tggov "github.com/territory-grounder/grounder/temporal/governance"
	"github.com/territory-grounder/grounder/temporal/manifestwrite"
	"github.com/territory-grounder/grounder/temporal/modetransition"
	"github.com/territory-grounder/grounder/temporal/moduletest"
	"github.com/territory-grounder/grounder/temporal/nativerule"
	"github.com/territory-grounder/grounder/temporal/objectgroup"
	"github.com/territory-grounder/grounder/temporal/opclassratify"
	"github.com/territory-grounder/grounder/temporal/policytrace"
	"github.com/territory-grounder/grounder/temporal/rulesetwrite"
	"github.com/territory-grounder/grounder/temporal/runner"
	"github.com/territory-grounder/grounder/temporal/skillgen"
	"github.com/territory-grounder/grounder/temporal/skilljudge"
	"github.com/territory-grounder/grounder/temporal/skilltrial"
	"github.com/territory-grounder/grounder/temporal/skillwrite"
	"github.com/territory-grounder/grounder/temporal/worlddiscovery"
)

// getenv resolves one configuration knob: console override → environment → compiled default.
//
// The console layer is FIRST because the operator's saved setting is the more recent, more deliberate
// statement of intent — it was written through an authenticated dialog with a rationale and recorded on
// the governance ledger, where the environment is whatever the deploy last happened to bake in. Until
// TG-260 this function was os.LookupEnv alone, so 112 of the console's 115 writable settings were saved
// durably and read by nothing. See boot_config.go for what is structurally excluded from the console
// layer (the DSN, bootstrap knobs, secret values, law).
func getenv(k, def string) string {
	if v, ok := bootConfigValue(k); ok {
		return v
	}
	if v, ok := os.LookupEnv(k); ok {
		return v
	}
	return def
}

// blastWidthThreshold reads the operator-declared blast-radius width threshold (config-not-code) — the
// number of predicted-cascade hosts at or above which an action's blast radius is "wide" and ceilings at
// AUTO_NOTICE. Defaults to the predecessor's 8. A non-positive/invalid value falls back to the default.
func blastWidthThreshold() int {
	n, err := strconv.Atoi(strings.TrimSpace(getenv("TG_BLAST_RADIUS_WIDE_THRESHOLD", "8")))
	if err != nil || n <= 0 {
		return 8
	}
	return n
}

// splitTokens splits an operator-declared, comma/whitespace-separated list into its tokens.
func splitTokens(csv string) []string {
	return strings.FieldsFunc(csv, func(r rune) bool { return r == ',' || r == ' ' || r == '\t' || r == '\n' })
}

// keyValueMap parses an operator-declared "key=value,key2=value2" (comma/whitespace-separated) list into a
// map — the config-not-code source for a notifier's routed-name -> real-destination table. A token with no
// '=' is skipped; an empty input yields nil (no mapping).
func keyValueMap(csv string) map[string]string {
	out := map[string]string{}
	for _, tok := range splitTokens(csv) {
		if i := strings.IndexByte(tok, '='); i > 0 {
			out[tok[:i]] = tok[i+1:]
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// hostSet parses an operator-declared, comma/whitespace-separated host list into a lookup set. It is the
// config-not-code source for the criticality tier: NO hostname is compiled into the binary — the P0 set is
// declared per-deployment via TG_CRITICALITY_TIER_HOSTS. An empty value yields an empty set (no P0 hosts).
func hostSet(csv string) map[string]struct{} {
	set := map[string]struct{}{}
	for _, h := range splitTokens(csv) {
		if h != "" {
			set[h] = struct{}{}
		}
	}
	return set
}

// librenmsDeployments parses operator-declared LibreNMS deployments from a `site|baseurl|tokenref[|timezone]`
// list separated by ';' — config-not-code, no URLs or token references compiled in. The optional 4th field
// is the IANA timezone the server renders its alert `$timestamp` in (e.g. "Europe/Athens"). A malformed or
// URL-less entry is skipped. Empty yields no deployments (no LibreNMS topology source).
func librenmsDeployments(spec string) []librenms.Deployment {
	var out []librenms.Deployment
	for _, row := range strings.Split(spec, ";") {
		f := strings.Split(strings.TrimSpace(row), "|")
		if len(f) < 3 || strings.TrimSpace(f[1]) == "" {
			continue
		}
		d := librenms.Deployment{Site: strings.TrimSpace(f[0]), BaseURL: strings.TrimSpace(f[1]), TokenRef: strings.TrimSpace(f[2])}
		if len(f) >= 4 {
			d.Timezone = strings.TrimSpace(f[3])
		}
		out = append(out, d)
	}
	return out
}

// selfProtectedMatcher builds a word-boundary matcher for the platform's OWN control-plane service names.
// It is the config-not-code source for the self-protected-restart veto: NO service name is compiled in —
// the set is declared per-deployment via TG_SELF_PROTECTED_SERVICES. An empty value matches nothing.
func selfProtectedMatcher(csv string) func(string) bool {
	var alts []string
	for _, t := range splitTokens(csv) {
		if t != "" {
			alts = append(alts, regexp.QuoteMeta(t))
		}
	}
	if len(alts) == 0 {
		return func(string) bool { return false }
	}
	re := regexp.MustCompile(`(?i)\b(?:` + strings.Join(alts, "|") + `)\b`)
	return func(blob string) bool { return re.MatchString(blob) }
}

// estateHTTPClient returns the HTTP client the estate topology pollers use. Default is strict TLS
// verification. When insecure is set (opt-in, per source, via TG_<SOURCE>_INSECURE) it disables
// certificate verification for that poller — the pragmatic, EXPLICIT accommodation for internal
// infrastructure served over self-signed certs (LibreNMS, Proxmox on :8006). It is default-off and
// scoped to the estate READ pollers only; it never touches ingress, actuation, or the model gateway.
func estateHTTPClient(insecure bool) *http.Client {
	// The estate TOPOLOGY refresh pulls LibreNMS /api/v0/devices for the WHOLE fleet (~500 devices) in one
	// request, which the API can take well over the old 15s to answer — the refresh then times out
	// "awaiting headers" and keeps stale edges (seen in prod every 5m). Default the timeout to 45s
	// (env-tunable, config-not-code) and ALWAYS set one, so the non-insecure path can no longer hang forever
	// on http.DefaultClient (which has no timeout). The fast per-device agent-tool pulls are unaffected.
	timeout := 45 * time.Second
	if s := strings.TrimSpace(getenv("TG_ESTATE_HTTP_TIMEOUT", "")); s != "" {
		if d, err := time.ParseDuration(s); err == nil && d > 0 {
			timeout = d
		}
	}
	c := &http.Client{Timeout: timeout}
	if insecure {
		// This is the ONE client in the tree that installs its own Transport, so it is the one client the
		// TG-160 outbound meter would otherwise not see — and it is precisely the client most worth
		// watching, because it is the opt-in path that skips certificate verification. Route it through
		// the same meter (shared tallies) so an insecure poller pointed at an undeclared host still shows
		// up in tg_egress_offallowlist_requests_total instead of being the blind spot.
		var rt http.RoundTripper = &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, // internal self-signed estate endpoint (opt-in)
		}
		if egressMeter != nil {
			rt = egressMeter.Wrap(rt)
		}
		c.Transport = rt
	}
	return c
}

// truthyEnv reports whether an env flag is set to an affirmative value.
// workerSecretEntries enumerates the worker's process secret references for the boot secret-policy gate
// (spec/024 REQ-2402 — a reference the gate cannot see is a plaintext hole). It reads each ref as the
// operator SET it (raw getenv, empty when unset ⇒ skipped by the gate — an unconfigured optional secret is
// not a plaintext violation). The DEFAULTED-scheme case (an unset ref whose code default is env:, REQ-2409)
// is covered by the grounder gate, which polices the same shared refs (LiteLLM, admin token) with their full
// loadEnv defaults — both binaries boot, so the stack is governed. Exempt marks non-business references:
// the OpenBao/substrate bootstrap credentials (token / AppRole role-id + secret-id / k8s JWT — none can come
// from the backend they authenticate, REQ-2401) and public material (the LDAP CA cert, the OIDC client id,
// the Langfuse public key). Guarded against drift by TestWorkerSecretEntriesCompleteness (source scan).
func workerSecretEntries(getenv func(string) string) []preflight.SecretEntry {
	biz := func(name string) preflight.SecretEntry {
		return preflight.SecretEntry{Name: name, Ref: config.SecretRef(getenv(name))}
	}
	exempt := func(name string) preflight.SecretEntry {
		return preflight.SecretEntry{Name: name, Ref: config.SecretRef(getenv(name)), Exempt: true}
	}
	entries := []preflight.SecretEntry{
		// Business secrets — must resolve through a backend under enforce.
		biz("TG_ACTUATION_SSH_KEY"), biz("TG_ADMIN_TOKEN_REF"), biz("TG_AWX_TOKEN_REF"),
		biz("TG_AWXJOB_LAUNCH_TOKEN_REF"), biz("TG_AWXPLAYBOOKS_SENSOR_TOKEN_REF"),
		biz("TG_ANSIBLE_VAULT_PASS_REF"), biz("TG_EMAIL_SMTP_TOKEN_REF"), biz("TG_GITHUB_TOKEN_REF"),
		biz("TG_GITLAB_RO_TOKEN_REF"),
		biz("TG_HEALTHCHECKS_CHECK_REF"), biz("TG_JIRA_TOKEN_REF"), biz("TG_LANGFUSE_SECRET_REF"),
		biz("TG_LDAP_BIND_DN"), biz("TG_LDAP_BIND_PW"), biz("TG_LIBRENMS_INGEST_TOKEN_REF"),
		biz("TG_LITELLM_KEY_REF"), biz("TG_MATRIX_TOKEN_REF"), biz("TG_MATTERMOST_TOKEN_REF"),
		biz("TG_NETBOX_TOKEN_REF"), biz("TG_OIDC_CLIENT_SECRET_REF"), biz("TG_OPENOBSERVE_TOKEN_REF"),
		biz("TG_PROXMOX_TOKEN_REF"), biz("TG_PVE_RO_TOKEN_REF"), biz("TG_PVE_TOKEN_REF"),
		biz("TG_SEMAPHORE_TOKEN_REF"), biz("TG_SERVICENOW_TOKEN_REF"), biz("TG_SLACK_TOKEN_REF"),
		biz("TG_SLURPIT_TOKEN_REF"),
		biz("TG_TEAMS_TOKEN_REF"), biz("TG_TWILIO_TOKEN_REF"), biz("TG_YOUTRACK_TOKEN_REF"),
		// Exempt — substrate/bootstrap credentials + public material (REQ-2401).
		exempt("TG_OPENBAO_TOKEN_REF"), exempt("TG_OPENBAO_ROLE_ID_REF"), exempt("TG_OPENBAO_SECRET_ID_REF"),
		exempt("TG_OPENBAO_WRAP_TOKEN_REF"), exempt("TG_OPENBAO_JWT_REF"), exempt("TG_LDAP_CA"),
		exempt("TG_OIDC_CLIENT_ID_REF"), exempt("TG_LANGFUSE_PUBLIC_REF"),
		// TG_SEAL_KEY_REF is the SEAL MASTER KEY, so it is bootstrap by definition: it is the key that
		// opens the sealed store, and therefore cannot itself be fetched from the sealed store. Same
		// classification the grounder already gives it (cmd/grounder/main.go, Exempt: true).
		exempt("TG_SEAL_KEY_REF"),
		// TG-81 b3: the verdict-ledger Ed25519 signing seed (verdictsig_wire.go). A business secret —
		// under enforce it must resolve from a backend, never plaintext env.
		biz("TG_VERDICT_SIGNING_SEED_REF"),
	}
	// The deployment-wide credentials no binary ever declared (TG-278, closed by TG-284): the Alertmanager
	// and claude-proxy bearers, plus every per-site LibreNMS token declared inside TG_LIBRENMS_DEPLOYMENTS.
	// All business secrets — see core/preflight/deploymentsecrets.go for why each is here and why the
	// LibreNMS pair is read out of the compound var rather than given a knob nothing would read.
	return append(entries, preflight.DeploymentSecretEntries(getenv)...)
}

func truthyEnv(k string) bool { return truthyValue(getenv(k, "")) }

// truthyValue is truthyEnv's parsing half, split out (TG-153) so a PLANE-SCOPED read can share the exact
// same affirmative vocabulary: truthyValue(planeEnv(k, "")) is truthyEnv(k) with the off-plane refusal in
// front of it. Two independent notions of "true" between the two readers would be a slow, silent divergence.
func truthyValue(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

// splitList parses a comma-separated operator list into trimmed, non-empty entries. Empty ⇒ nil, which every
// consumer reads as "use the compiled default".
func splitList(spec string) []string {
	var out []string
	for _, p := range strings.Split(spec, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// learnedRebootLane builds the LEARNED scheduled-reboot lane: the observe→verify→promote learner, the
// governance-demotion lookup the suppression chain consults before honoring a learned pattern, the
// suppression-miss evidence store, and the demoter that turns that evidence into analysis-only rows.
//
// It returns all-nil when the lane is not armed, which is the DEFAULT — the whole lane is then absent and
// TG behaves exactly as it did: operator-declared schedules only.
//
// The two-phase verifier is wired to REAL channels: the tracker reopens the incident and the notifier pages
// the approver graph (REQ-406). Without a tracker the reopen degrades to a logged no-op — the suppression is
// still REVERSED to escalation in-path, so the incident is investigated either way; only the ticket
// transition is lost.
//
// The registry and the governance stores are per-worker in-memory today. That is honest, not hidden: the
// learned lane's state and its demotions have the SAME lifetime, so a restart forgets a lesson and its
// correction together — never a lesson without its correction. The durable twins exist behind the same
// contracts (core/persist.ScheduledRebootStore, core/db.ScheduledReboots) for when the lane graduates.
func learnedRebootLane(armed bool, pre, post time.Duration, trk tracker.Tracker,
	notify func(context.Context, notifier.Notice) error, ledger *audit.Ledger, store persist.ScheduledRebootStore,
) (*suppression.Learner, suppression.DemotionLookup, coregov.EvidenceStore, *coregov.Demoter) {
	if !armed {
		return nil, nil, nil, nil
	}
	registry := suppression.NewScheduleRegistry()
	// TG-225: back the in-memory registry with the durable store when a DB is wired, so a learned lesson (and
	// its timezone-correct window) survives a restart instead of being forgotten with the worker's memory. A
	// mirror-write failure is logged, never fatal — the in-memory registry stays authoritative for the process.
	if store != nil {
		registry.WithDurableStore(store, func(err error) {
			log.Printf("suppression(learn): durable mirror write failed: %v (lesson stays in-memory this cycle)", err)
		})
		if err := registry.LoadFromStore(context.Background()); err != nil {
			log.Printf("suppression(learn): could not rehydrate the learned registry from the durable store: %v (starting empty)", err)
		} else {
			log.Printf("suppression(learn): learned reboot schedules are DURABLE — lessons survive a restart (TG-225)")
		}
	}
	learner := &suppression.Learner{
		Registry: registry,
		Window:   suppression.WindowEvaluator{PreBuffer: pre, PostWindow: post},
		Verifier: &suppression.TwoPhaseVerifier{Reopen: trackerReopener{trk: trk}, Pager: notifierPager{notify: notify}},
		Timezone: getenv("TG_SUPPRESSION_LEARN_TZ", "UTC"),
	}
	demotions := coregov.NewMemDemotionStore()
	return learner, coregov.DemotionLookupOf(demotions), coregov.NewMemEvidenceStore(),
		&coregov.Demoter{Store: demotions, Ledger: ledger}
}

// trackerReopener reopens an incident whose suppression the two-phase boot verify could not confirm
// (REQ-406) by transitioning its tracker issue back to OPEN. A nil tracker logs instead of failing: the
// caller has already reversed the suppression to escalation, so a missing tracker costs the ticket
// transition, never the investigation.
type trackerReopener struct{ trk tracker.Tracker }

func (r trackerReopener) Reopen(ctx context.Context, externalRef string) error {
	if r.trk == nil {
		log.Printf("suppression: reopen of %s recorded to log only (no tracker wired)", externalRef)
		return nil
	}
	return r.trk.TransitionState(ctx, externalRef, tracker.StateOpen)
}

// selfPrincipalFromToken derives the platform's own actuation identity — the "PRINCIPAL" half of a PVE API
// token value formatted "PRINCIPAL=SECRET" (e.g. "root@pam!tg-actuate"). Empty when the value has no
// separator. This is the identity TG's own heals appear as in the PVE task log, so actor-attribution can
// recognize a self-remediation.
func selfPrincipalFromToken(tok string) string {
	if i := strings.Index(tok, "="); i > 0 {
		return tok[:i]
	}
	return ""
}

// envFloat reads an operator-declared float (config-not-code); a blank/invalid/non-positive value falls
// back to def, so a config slip never fires the flywheel on a looser threshold than declared.
func envFloat(k string, def float64) float64 {
	v, err := strconv.ParseFloat(strings.TrimSpace(getenv(k, "")), 64)
	if err != nil || v <= 0 {
		return def
	}
	return v
}

// envInt reads an operator-declared positive int (config-not-code); blank/invalid/non-positive ⇒ def.
func envInt(k string, def int) int {
	v, err := strconv.Atoi(strings.TrimSpace(getenv(k, "")))
	if err != nil || v <= 0 {
		return def
	}
	return v
}

// envIntAllowZero is envInt for a field where 0 is a MEANINGFUL value rather than "use the default". It falls
// back to def ONLY when the key is unset or unparseable; a validly-parsed 0 (or negative) is returned as-is so
// the receiver decides what it means. The gating recon bounds use envInt, where 0 correctly restores the
// default (a guard must never be disabled by a blank key); FanoutObserve is OBSERVE-ONLY and legitimately
// disable-able, so its explicit 0 must reach the budget instead of being silently coerced back to 12 — without
// this, the documented and tested "0 disables the fan-out flag" off-switch is unreachable through the one env
// wire an operator actually has (TG-325).
func envIntAllowZero(k string, def int) int {
	raw := strings.TrimSpace(getenv(k, ""))
	if raw == "" {
		return def
	}
	v, err := strconv.Atoi(raw)
	if err != nil {
		return def
	}
	return v
}

// dualDiscoveryWriter captures each verify-time scored deviation into BOTH the in-process rolling buffer (the
// flush cron drains it to eval/discovery-corpus.json) AND the durable pgx corpus (TG-206), so the
// "reproduces >= N" promotion signal survives a worker restart instead of resetting to zero. The DURABLE
// store is authoritative for the newly-captured-vs-reproduction result and the returned error; the in-memory
// capture is best-effort (its error is ignored — it is a bounded local buffer, not the source of truth).
type dualDiscoveryWriter struct{ mem, durable falsify.DiscoveryWriter }

func (d dualDiscoveryWriter) Capture(ctx context.Context, rec falsify.DiscoveryRecord) (bool, error) {
	_, _ = d.mem.Capture(ctx, rec)
	return d.durable.Capture(ctx, rec)
}

// runnerWorkerOptions bounds how many activities the RUNNER worker executes at once (TG-384). A burst of
// alerts must not become a burst of simultaneous model-consuming investigations that trips the model breaker
// — the pve03 cascade turned 157 alerts into 157 concurrent investigations and OPENed the model-primary
// circuit in six seconds. This is the Temporal-native belt that complements the gateway concurrency semaphore
// (adapters/model, same ticket): the gateway bounds model CALLS, this bounds the investigate ACTIVITIES that
// make them. Only the runner queue is bounded here — the actuate worker's activities are already capped far
// tighter by the actuation limiter (core/actuate/limiter.go, SessionInFlight 1 / TargetInFlight 1).
//
// Inert without the env: TG_MAX_CONCURRENT_INVESTIGATIONS unset ⇒ 0 ⇒ the option is left off worker.Options
// and the worker keeps Temporal's default (1000) — what a CI/in-memory boot gets. The DEPLOYED compose ships
// it at 16 by default (deploy/docker-compose.yml, TG-384), sized from the sidecar's service time (~8-16 at
// 8 in-flight / 11.6s mean); an operator tunes it for a bigger sidecar, or 0 restores Temporal's default.
func runnerWorkerOptions(get func(string, int) int) worker.Options {
	opts := worker.Options{}
	if n := get("TG_MAX_CONCURRENT_INVESTIGATIONS", 0); n > 0 {
		opts.MaxConcurrentActivityExecutionSize = n
	}
	return opts
}

// envDuration reads an operator-declared positive duration (config-not-code); blank/invalid/non-positive ⇒ def.
func envDuration(k string, def time.Duration) time.Duration {
	v, err := time.ParseDuration(strings.TrimSpace(getenv(k, "")))
	if err != nil || v <= 0 {
		return def
	}
	return v
}

// ladderRungFor is THE per-rung truth table (spec/028 REQ-2807/2809) — the one translation between the
// graduation ladder's policy.Level and the runner's decoupled runner.LadderRung.
//
//	policy.LevelApprove    -> RungApprove     poll, so the approval the policy engine wants is askable
//	policy.LevelAutoNotice -> RungAutoNotice  no poll; AUTO_NOTICE band floor (acts and pages)
//	policy.LevelAuto       -> RungAuto        no poll, no floor (acts silently)
//
// It exists as one function so "has this class graduated?" and "does it need a notice?" are both DERIVED
// from a single rung value and cannot be wired to disagree. That disagreement is the one worth engineering
// against because its failure mode is silent: graduated-true with notice-false makes an auto_notice class act
// with nobody paged, which is the rung's only guarantee.
//
// A nil ladder is INERT (RungAuto): an unconfigured deployment has no policy engine composing an `approve`
// verdict either, so polling everything would be a behaviour change rather than a safety gain. An
// unrecognised level maps to RungApprove — fail closed, matching the ladder's own treatment of a corrupt
// persisted level.
func ladderRungFor(l *policy.Ladder, opClass string) runner.LadderRung {
	if l == nil {
		return runner.RungAuto
	}
	switch l.LevelOf(context.Background(), opClass) {
	case policy.LevelAuto:
		return runner.RungAuto
	case policy.LevelAutoNotice:
		return runner.RungAutoNotice
	default:
		return runner.RungApprove
	}
}

func main() {
	log.SetPrefix("tg-worker: ")
	log.SetFlags(log.LstdFlags | log.LUTC)

	// TG-170: the compose HEALTHCHECK entry point, handled FIRST — before the module-config load, before
	// any credential resolves, before the plane is even determined. A liveness probe that needs the
	// database or OpenBao to answer would report the worker unhealthy every time a dependency blinked, and
	// compose would restart a process that was fine.
	if len(os.Args) > 1 && os.Args[1] == "-healthcheck" {
		runHealthcheck(healthcheckAddrFromEnv(), "/metrics")
	}

	// The operator's saved module settings become the values this process runs on (TG-260). FIRST, before
	// anything reads a knob — the OpenBao delivery keys immediately below are descriptor fields too, so a
	// later install would leave them resolving from the environment while every other field resolved from
	// the console. Non-fatal by construction: a config-plane outage logs and falls back to the environment.
	installBootConfig(context.Background())

	// ★ THE OUTBOUND METER (TG-160). Installed SECOND — after the config fold (so the allowlist can see a
	// connector configured entirely through the console) and BEFORE the first outbound call of the process
	// (the OpenBao delivery immediately below), so nothing this worker dials is unaccounted for. It replaces
	// http.DefaultTransport, which is what every module in this tree that does not set its own Transport
	// resolves to; see cmd/worker/egress.go for the measurement behind that claim. Default posture is
	// observe-only: it counts destinations and bytes and names an undeclared destination in the log, and it
	// blocks nothing. Before this, `grep -rn -w -i egress --include=*.go .` returned ZERO over the whole
	// tree while docs/THREAT-MODEL.md advertised an egress step in the interceptor chain.
	installEgressMeter()

	// Credential delivery (spec/022 REQ-2200/REQ-2204, TG-156/TG-157): make the process's own SecretRefs
	// resolvable as bao: references from OpenBao. Done FIRST, before any secret resolves (the credential
	// preflight below, the model gateway key, per-target bundles). Substrate OFF by default (TG_OPENBAO_ADDR
	// unset) ⇒ behaviour-preserving no-op. Fail-closed: a misconfigured/unreachable enabled substrate refuses
	// to start rather than let a declared bao: secret degrade to a plaintext fallback.
	// mTLS machine-identity bootstrap (spec/024 Amendment 2026-07-25): where a FreeIPA-CA client cert+key are
	// configured, the worker authenticates to OpenBao by PRESENTING that identity — no bootstrap token on disk.
	// This is the preferred, higher-assurance path; it takes precedence over the token, which stays configured
	// as a transition fallback (a deploy sets the cert env to switch; unset to fall back) until it is retired.
	wireCredentialDelivery(getenv) // OpenBao delivery (mTLS > approle > token) + the homelab vw: scheme

	// TG-422: dynamic Postgres credentials (the self-contained first slice of TG-320). When TG_DYNDB_ADDR is
	// set, the `dyn:` SecretRef scheme leases short-lived Postgres creds from OpenBao's database engine —
	// minted per lease, renewed before the TTL, revoked at shutdown — so a DSN can read `dyn:<role>` instead
	// of embedding one of TG's longest-lived static passwords. OFF by default (addr unset) ⇒ the scheme stays
	// UNREGISTERED and every dyn: ref fails closed; no DSN uses it until an operator arms it, so merging this
	// changes nothing live. Read via os.Getenv DIRECTLY, never the console-override getenv: this is the
	// credential PATH TO the database, and a database cannot supply the address of the store that mints its
	// own login (the TG_DB_DSN rule, boot_config.go). Fail-closed: a misconfigured enabled engine refuses to
	// boot rather than fall back to a static password. Wired AFTER delivery so a bao: token ref can resolve.
	dynAddr, dynMount, dynTokenRef, dynCA, dynDSNTmpl := dyndbConfigFromEnv()
	var dynProvider *dyndb.Provider
	if strings.TrimSpace(dynAddr) != "" {
		dynEngine, dynErr := dyndb.New(dyndb.Config{
			BaseURL:  strings.TrimSpace(dynAddr),
			Mount:    dynMount,
			TokenRef: config.SecretRef(dynTokenRef),
			CACert:   dynCA,
		})
		if dynErr != nil {
			log.Fatalf("dyndb: dynamic Postgres credentials are enabled (TG_DYNDB_ADDR) but the engine will not "+
				"construct — refusing to boot rather than fall back to static passwords (TG-422): %v", dynErr)
		}
		p, rErr := dyndb.Register(true, dyndb.ProviderConfig{
			Engine:      dynEngine,
			DSNTemplate: dynDSNTmpl,
			RootCtx:     context.Background(),
		}, log.Printf)
		if rErr != nil {
			log.Fatalf("dyndb: %v (TG-422)", rErr)
		}
		dynProvider = p
	} else {
		_, _ = dyndb.Register(false, dyndb.ProviderConfig{}, log.Printf)
	}
	if dynProvider != nil {
		// Revoke every leased credential at shutdown — the whole point is that a dynamic credential dies with
		// the process instead of outliving it as a static password would.
		defer func() { _ = dynProvider.Close(context.Background()) }()
	}

	// THE PROCESS SPLIT (TG-153, spec/022 T-022-4). Declare which credential plane THIS process runs, before
	// a single credential is read. `both` (the default, and every deployment that has not opted in) is the
	// pre-TG-153 posture, byte-for-byte. `triage` and `actuation` are the split: see cmd/worker/
	// credential_plane.go for why the omission is at ACQUISITION rather than an `if` around a constructed
	// runner, and for the live OpenBao evidence this binds to.
	// The alert source, hoisted so the admin surface can probe the upstream (TG-344). nil when no
	// LibreNMS alert poll is configured — the probe then emits nothing and says so at boot.
	var upstreamProbeSource *librenms.AlertSource
	credentialPlane = resolveCredentialPlane(getenv)

	// Credential plane split (spec/022 REQ-2203, TG-157): the read-only triage plane must never co-hold an
	// actuation credential. Assert at boot that the configured read-triage references (estate reads + the
	// read-scoped substrate token) are DISJOINT from the actuation references (the SSH mutate key, proxmox/AWX
	// write-tokens) — a config mistake that recombined them fails closed here, beneath the OpenBao role split.
	//
	// EVERY reference below is read through the RAW getenv, NEVER through planeEnv — deliberately, and this is
	// the load-bearing line of the whole assertion. planeEnv withholds off-plane keys, so a PlaneSet built
	// through it would find no actuation reference on the triage plane BECAUSE THE FILTER REMOVED THEM, and
	// would then report a split the operator's .env does not actually have. The check has to see what the
	// process was HANDED, not what it chose to look at. (TG-153; the same vacuity trap as a grep oracle that
	// passes because it matched nothing.)
	planes := credential.PlaneSet{
		ReadTriage: []config.SecretRef{
			config.SecretRef(getenv("TG_NETBOX_TOKEN_REF", "")), // estate read
			config.SecretRef(getenv("TG_PVE_TOKEN_REF", "")),    // estate read (audit-only PVE token)
		},
		Actuation: []config.SecretRef{
			config.SecretRef(getenv("TG_ACTUATION_SSH_KEY", "")),       // SSH mutate key
			config.SecretRef(getenv("TG_PROXMOX_TOKEN_REF", "")),       // proxmox guest lifecycle write token
			config.SecretRef(getenv("TG_AWXJOB_LAUNCH_TOKEN_REF", "")), // AWX job-launch write token
		},
	}
	// ValidateFor adds the TG-153 half to the TG-157 disjointness check: on a SPLIT plane the process must
	// declare none of the other plane's references at all. A misconfigured split fails CLOSED here rather than
	// booting a process that carries the label of a split and the credentials of the old co-holding worker.
	// On `both` this is exactly the historic Validate() and nothing else.
	if err := planes.ValidateFor(credentialPlane); err != nil {
		log.Fatalf("%v", err)
	}
	log.Printf("credential plane split: %s", planes.SummaryFor(credentialPlane))
	// Say WHAT WAS WITHHELD, by name, and only for keys the operator actually set — a count over an .env that
	// declared none of them would read like protection and measure nothing. An empty list on a split plane is
	// itself worth printing: it means this .env never carried the other plane's configuration to begin with.
	if credentialPlane != credential.ProcessPlaneBoth {
		if withheld := planeWithheldKeys(credentialPlane); len(withheld) > 0 {
			log.Printf("credential plane split: WITHHELD %d configured key(s) from this plane — %s. They are not read, not resolved, and no subsystem depending on them is constructed in this process.",
				len(withheld), strings.Join(withheld, ", "))
		} else {
			log.Printf("credential plane split: no off-plane key was configured in this process's environment (nothing to withhold) — %s=%s", CredentialPlaneEnv, credentialPlane)
		}
	}

	// Actuation is OFF by construction — this worker is read-only (Phase 0/1). The mode-driven actuation
	// chokepoint (the absorbed MutationGate, REQ-1520) starts with NO mode authority ⇒ MayActuate is false
	// (fail closed); the real ModeController is BOUND later (after the durable stores exist), and it defaults to
	// Shadow, so the worker stays read-only unless an operator later escalates the mode. The retired
	// TG_MUTATION_ENABLED knob is gone — enabling actuation is a mode transition, never an env flag.
	chokepoint := safety.NewChokepoint(nil)
	if chokepoint.MayActuate() {
		log.Fatal("actuation posture is ON at boot — refusing to start the read-only worker")
	}

	// THE READ LANE'S VOLUME BOUND (TG-165). The mutation lane above is throttled by mode, breaker, ledger
	// and policy; the READ lane had per-call bounds and $-spend and nothing else — no read counter fed any
	// kill path, `/halt` never reached recon, and the anti-thrash veto keys on identical (tool, args), so
	// distinct probes were free and no cross-session bound existed at all. A hijacked-but-read-only worker
	// could enumerate the estate at full rate entirely within policy (docs/THREAT-MODEL.md §5.2).
	//
	// Constructed HERE, beside the chokepoint it kills through and before anything that reads the estate, so
	// there is exactly ONE governor per process: a per-session or per-activity meter would be reset by every
	// Temporal retry, which is precisely the cross-session hole this closes. It is handed to the runner (the
	// agent loop consults it before each read) and to the admin surface (POST /halt stops recon too, and the
	// read-lane counters reach /metrics).
	//
	// Every bound is operator-overridable UPWARD or downward through the store-resolving envInt, and a blank
	// or malformed value falls back to the shipped default rather than to "unlimited" (safety.ReconBudget).
	reconGovernor := safety.NewReconGovernor(safety.ReconBudget{
		PerSession:    envInt("TG_RECON_PER_SESSION", safety.DefaultReconPerSession),
		PerHour:       envInt("TG_RECON_PER_HOUR", safety.DefaultReconPerHour),
		Burst:         envInt("TG_RECON_BURST", safety.DefaultReconBurst),
		BurstWindow:   envDuration("TG_RECON_BURST_WINDOW", safety.DefaultReconBurstWindow),
		FanoutObserve: envIntAllowZero("TG_RECON_FANOUT_OBSERVE", safety.DefaultReconFanoutObserve),
	}, chokepoint, safety.WithReconLogf(log.Printf))
	log.Printf("recon budget armed (TG-165): %d reads/session, %d reads/hour across all sessions, burst %d in %s "+
		"→ forces the mode to Shadow; POST /halt now stops the READ lane too. A refused read is reported to the "+
		"agent in words, never silently dropped. Fan-out flag (TG-325, observe-only): %d distinct targets/session "+
		"(0 = off).",
		reconGovernor.Budget().PerSession, reconGovernor.Budget().PerHour,
		reconGovernor.Budget().Burst, reconGovernor.Budget().BurstWindow,
		reconGovernor.Budget().FanoutObserve)

	// Credential preflight (TG-113, live-safety): PROVE this worker's REAL runtime user can resolve, read,
	// and parse every SSH private key it will use for native investigation + actuation — BEFORE it advertises
	// healthy. The distroless worker runs as nonroot uid:gid 65532; the known silent-kill was /secrets/one_key
	// ABSENT (a re-provision dropped it) or root-owned 0600 (65532 got permission-denied), which killed ALL
	// native SSH yet booted preflight-GREEN and looked healthy (masked as "hostkey"/"no logs"). CheckSSHKeys
	// runs os.ReadFile + ssh.ParsePrivateKey IN THIS PROCESS (as 65532), so a root-run check cannot falsely
	// pass. Design choice (TG-113): the worker BOOTS DEGRADED + LOUD rather than hard-failing — it keeps
	// triaging so telemetry still flows, but the degraded state is (a) logged as an ERROR here and (b)
	// surfaced on /metrics as tg_ssh_credential_ready=0 (below), so nobody is fooled into thinking SSH works.
	// The deploy-time HARD gate is `grounder --check` (fails the pipeline before the worker goes live).
	//
	// PLANE-SCOPED (TG-153). The reader is planeEnv, not getenv: CheckSSHKeys does os.ReadFile +
	// ssh.ParsePrivateKey, i.e. it pulls the PRIVATE KEY MATERIAL into this process's memory. A triage worker
	// running this over TG_ACTUATION_SSH_KEY would hold the estate-mutating key in its address space before it
	// had triaged a single alert — which is the entire defect this ticket closes, reintroduced by a health
	// check. On the triage plane the actuation ref is withheld, so it is neither listed nor read; the syslog /
	// hostdiag / credential-rule refs are checked exactly as before. On the actuation plane the mirror holds.
	sshCredReport := preflight.CheckSSHKeys(preflight.SSHKeyRefsFromEnv(func(k string) string { return planeEnv(k, "") }))
	switch {
	case sshCredReport.Configured() == 0:
		log.Printf("credential preflight: no SSH key references configured — native SSH investigation/actuation not in use")
	case sshCredReport.Failed():
		log.Printf("ERROR credential preflight DEGRADED (tg_ssh_credential_ready=0) — %s. Native SSH investigation + actuation is DISABLED for the failed ref(s) even though the worker is booting read-only. Provision the key readable by uid:gid %d:%d mode 0640 (see deploy/secrets/README.md).", sshCredReport.Summary(), os.Getuid(), os.Getgid())
	default:
		log.Printf("credential preflight OK — %d SSH key ref(s) resolve+parse as uid:gid %d:%d: %s", sshCredReport.Configured(), os.Getuid(), os.Getgid(), strings.Join(sshCredReport.OK, ", "))
	}

	// Secret-scheme policy (spec/024 REQ-2400): the worker half of the boot gate. Under enforce, refuse to
	// start on any non-exempt business secret resolving through a plaintext-bearing scheme (env:/file:/literal)
	// instead of a backend. Default off = behaviour-preserving. Classification never resolves or logs a value.
	// workerSecretEntries enumerates the COMPLETE worker ref set (REQ-2402) — guarded against drift by
	// TestWorkerSecretEntriesCompleteness, which scans this source for every getenv("*_REF") read.
	//
	// TG-284: the gate also SHAPE-scans the real process environment (os.Environ), because the enumeration
	// above can only see what someone remembered to declare — and on the live worker two raw LibreNMS API
	// tokens sat in this very process while the gate reported green under enforce. os.Environ() is passed
	// deliberately: a check run against a curated list would pass exactly where the real environment fails.
	{
		policy := preflight.ParseSecretPolicy(getenv("TG_SECRET_POLICY", "off"))
		rep := preflight.CheckSecretPolicyWithEnv(workerSecretEntries(func(k string) string { return getenv(k, "") }), os.Environ())
		if policy != preflight.PolicyOff {
			// Log the scan's REACH, not just its verdict: "no violations" out of nothing scanned and "no
			// violations" out of a fully scanned environment must never read alike in this log.
			log.Printf("secret policy=%s: %s", policy, rep.EnvScanSummary())
		}
		if policy == preflight.PolicyWarn {
			for _, v := range rep.Violations {
				if v.RawPlaintext {
					log.Printf("secret policy=warn: %s holds a RAW credential VALUE in the process env (not a reference) — move it to a secret backend (bao:/vault:/store:) and REMOVE the plaintext variable", v.Name)
					continue
				}
				log.Printf("secret policy=warn: %s resolves through the %s: scheme (plaintext) — move it to a secret backend (bao:/vault:/store:)", v.Name, v.Scheme)
			}
		}
		if err := preflight.EnforceSecretPolicy(policy, rep); err != nil {
			log.Fatalf("boot preflight: %v", err)
		}
	}

	hostPort := getenv("TG_TEMPORAL_HOSTPORT", client.DefaultHostPort)
	c, err := client.Dial(client.Options{HostPort: hostPort})
	if err != nil {
		log.Fatalf("temporal dial %s: %v", hostPort, err)
	}
	defer c.Close()

	// Model gateway + its three operator-set output bounds (TG-384/TG-48), carved into buildModelGateway
	// (worker_model_budget.go); pinned by worker_model_budget_test.go.
	gw := buildModelGateway()

	// probeReg collects the modules that can prove themselves for the console's TEST button. Declared
	// HERE, at the top of main(), because collection happens at each module's POINT OF CONSTRUCTION —
	// see cmd/worker/probe_registry.go for why the previous end-of-main() assembly silently reduced
	// twenty-nine promised dialogs to one working prober.
	probeReg := newProbeRegistry()
	tools := agent.NewReadOnlyToolSet()
	// Ground the agent in OBSERVED estate state: register the read-only LibreNMS investigation tools
	// (device status, event log, active alerts) from the same declared deployments the ingest fleet uses.
	// Without these the agent triages by inference alone (evidence_grounded floored); with them it reads the
	// live device before proposing. Every tool is GET-only — Register refuses a non-read-only tool (INV-17).
	// PLANE-SCOPED (TG-153): these are AGENT tools — they carry LibreNMS alert and event-log TEXT into the
	// model loop, which is the untrusted-content surface the split exists to keep away from the actuation
	// credential. Read through the _AGENT_TOOLS alias so the ACTUATION plane gets "" here and constructs no
	// tool, while the estate TOPOLOGY refresh further down keeps reading the same variable directly (a device
	// inventory is not attacker-authored prose, and the mutation gate needs it in order to refuse anything).
	if lnmsDeps := librenmsDeployments(planeEnv("TG_LIBRENMS_DEPLOYMENTS_AGENT_TOOLS", "")); len(lnmsDeps) > 0 {
		lnmsTools := librenms.NewTools(lnmsDeps, estateHTTPClient(truthyEnv("TG_LIBRENMS_INSECURE")))
		// RegisterFrom's first argument is the tool's SOURCE NAMESPACE (TG-215): the coarse plane this
		// composition root — the one place that knows which module built a tool — declares so the
		// FAST_AGENT preamble can group its class-keyed catalog ("librenms:", "host:", "estate:",
		// "history:"). Rendering metadata only; the tool names and dispatch are unchanged.
		for _, tl := range lnmsTools {
			if err := tools.RegisterFrom("librenms", tl); err != nil {
				log.Fatalf("register agent tool %s (fail-closed): %v", tl.Name(), err)
			}
		}
		log.Printf("agent: registered %d read-only LibreNMS investigation tools across %d deployment(s)", len(lnmsTools), len(lnmsDeps))
		for _, tl := range lnmsTools {
			probeReg.offer("ingest", librenms.SourceType, tl)
		}
	}

	// NetBox read-only inventory investigation tool (TG-56) — the "consume a read-only vendor server to
	// broaden investigation reach" pattern re-authored onto TG's agent.Tool surface. The MCP actuation
	// chokepoint (modules/actuation/mcp) is a separate, mutation-only lane the investigation loop never
	// reaches, so a read-only vendor consumer belongs here, exactly like the LibreNMS tools above. It surfaces
	// NetBox record TEXT into the model loop, so it reads the endpoint through the _AGENT_TOOLS alias and the
	// ACTUATION plane constructs nothing (the read-triage secret/data/tg/netbox must never sit beside the
	// mutation keys — TG-346). Explicit opt-in (TG_NETBOX_INVESTIGATION), default dormant: arming a reader
	// changes triage semantics (transparency-gated), a deliberate operator act like TG_NETBOX_ACTOREVIDENCE.
	if truthyEnv("TG_NETBOX_INVESTIGATION") {
		if nbURL := planeEnv("TG_NETBOX_URL_AGENT_TOOLS", ""); nbURL != "" {
			nbTool := netbox.New(nbURL, config.SecretRef(getenv("TG_NETBOX_TOKEN_REF", "env:NETBOX_TOKEN")))
			regd := 0
			for _, tl := range netbox.NewTools(nbTool) {
				if err := tools.RegisterFrom("netbox", tl); err != nil {
					log.Fatalf("register agent tool %s (fail-closed): %v", tl.Name(), err)
				}
				probeReg.offer("cmdb", netbox.SourceType, tl)
				regd++
			}
			log.Printf("agent: registered %d read-only NetBox inventory investigation tool(s) (TG-56)", regd)
		} else {
			log.Printf("agent: TG_NETBOX_INVESTIGATION set but no NetBox endpoint on this plane — investigation tool NOT registered (fail closed)")
		}
	}

	// THE PAIRED-YIELD REGISTER (TG-250) — constructed here, before its first observer. It has no
	// dependencies; what decides its position is that the syslog-ng tools immediately below and the
	// hostdiag tools further down report into it, and the observability export loop reads it. It moved up
	// from below hostdiag when syslog-ng became the first observer (TG-297): the instrumentation guard
	// reads this file for a literal `wiringYield.Observe(...)`, so an indirection through an atomic
	// pointer to keep the old position would defeat the guard for both lanes.
	wiringYield := wiring.NewYieldRegister()

	// Ground the agent in OBSERVED device logs: register the read-only syslog-ng investigation tools
	// (get-host-logs, search-host-logs) from the declared per-site syslog servers (TG_SYSLOGNG_DEPLOYMENTS).
	// This is the firewall/switch/router syslog window the predecessor's cisco-asa-specialist and
	// triage-researcher had and TG lacked. Config-not-code: absent config ⇒ no tools, no error. Every tool
	// is read-only (Register refuses a non-read-only tool, INV-17) and reads a FIXED argv over host-key-
	// verified SSH — no shell, mutation stays OFF. The nil runner selects the production SSH runner.
	// The syslog tools are built here but the wiring MANIFEST is ~1,800 lines below, so the Bind/Absent is
	// deferred through this variable (the same hand-off hostDiagTools uses).
	var syslogTools []agent.Tool
	var authlogReg *authlogYield
	// Hoisted for the SAME reason syslogTools is: the authlog collector (TG-315) reads the same
	// syslog-ng trees these tools do, and must reuse this runner rather than open a second
	// transport with its own host-key posture.
	var authlogServers []syslogng.Server
	var authlogRunner syslogng.Runner
	// PLANE-SCOPED (TG-153): device syslog is attacker-influenced text, read over SSH with a key. Withheld
	// from the actuation plane at ACQUISITION — no servers parsed, no runner, no key, no tool.
	if sgServers := syslogng.ParseServers(planeEnv("TG_SYSLOGNG_DEPLOYMENTS", "")); len(sgServers) > 0 {
		// The runner is built HERE, from the store-resolving getenv, and passed down explicitly. The nil
		// fallback inside modules/ reads os.Getenv, which never sees a value an operator saved in the UX —
		// that bypass is how a key can be "set" in the config store while every read still fails (TG-265).
		sgRunner := syslogng.NewNativeRunner(getenv("TG_SYSLOGNG_KNOWN_HOSTS", ""))
		// The PER-SESSION search cap (TG-297) and the yield observer, both wired from the root for the same
		// TG-265 reason as the runner: the cap is read through the store-resolving envInt so a value an
		// operator sets in the console actually binds, and envInt's own rule (blank/invalid/non-positive ⇒
		// the default) means a config slip restores the sane bound rather than removing it.
		sgSearchCap := envInt("TG_SYSLOGNG_SEARCH_SESSION_CAP", syslogng.DefaultSearchSessionCap)
		sgTools := syslogng.NewTools(sgServers, sgRunner,
			syslogng.WithSearchSessionCap(sgSearchCap),
			// EVERY read reports its outcome to the seam-yield register. A spent search budget returns a
			// well-formed refusal string, and so does an unverified host key — both are perfectly good
			// return values that no invocation-counting check can tell from a successful read. Only the
			// produced/attempted PAIR distinguishes a lane that is answering from one that is only
			// replying, which is the lesson TG-271 cost weeks to learn on the hostdiag lane.
			syslogng.WithYield(func(produced bool) {
				wiringYield.Observe(wiring.SeamSyslogRead, 1, boolCount(produced), time.Now().UTC())
			}))
		for _, tl := range sgTools {
			if err := tools.RegisterFrom("host", tl); err != nil {
				log.Fatalf("register agent tool %s (fail-closed): %v", tl.Name(), err)
			}
		}
		syslogTools = sgTools
		authlogServers, authlogRunner = sgServers, sgRunner
		log.Printf("agent: registered %d read-only syslog-ng investigation tools across %d server(s) — search-host-logs is capped at %d call(s) per investigation (%s)",
			len(sgTools), len(sgServers), sgSearchCap, syslogng.SearchSessionCapEnv)
		// The MODULE, not the tools. The agent-facing tools read logs; the probe must open a session and
		// close it without running anything, which is a distinct capability on syslogng.Module — and it
		// is what lets the descriptor's verb say "and close it" truthfully. Offering the tools registered
		// nothing at all, which the boot cross-check caught on the first deployment: syslog-ng appeared
		// as BOTH "publishes a TEST verb but has no prober" and "constructed but publishes no probe".
		probeReg.offer("observability", syslogng.SourceType, syslogng.NewModule(sgServers, sgRunner))
	}

	// Read-only HOST-DIAGNOSTICS tools are registered LATER (below), once the credential engine exists: the
	// SSH investigation path now resolves per-host identity THROUGH the engine (spec/016, fail-closed) instead
	// of reading it off the allowlist, so it needs the resolver built first.

	// Populate the runtime module registry from the built connector fleet. The registry shipped unpopulated,
	// so INV-17/18 were only ever enforced in acceptance tests; this is the composition root that declares the
	// live capability set at boot. It fails closed: a duplicate (surface, source) registration (INV-18) aborts
	// startup rather than running with an ambiguous fleet. The model-provider family declares here; other
	// families join as their config surfaces land, and each surface migrates to registry-backed resolution.
	moduleReg, err := bootstrap.NewRegistry()
	if err != nil {
		log.Fatalf("module registry bootstrap failed (fail-closed): %v", err)
	}
	// Declare the configured issue trackers (config-not-code): a tracker is a capability only where its
	// endpoint is declared; credentials are secret references (env:/file:), never literals. An unconfigured
	// tracker is simply absent from the live set.
	if err := bootstrap.RegisterTrackers(moduleReg, bootstrap.TrackerConfig{
		YouTrackURL:             getenv("TG_YOUTRACK_URL", ""),
		YouTrackTokenRef:        getenv("TG_YOUTRACK_TOKEN_REF", "env:YOUTRACK_TOKEN"),
		YouTrackStateInProgress: getenv("TG_YOUTRACK_STATE_INPROGRESS", ""),
		YouTrackStateResolved:   getenv("TG_YOUTRACK_STATE_RESOLVED", ""),
		YouTrackStateOpen:       getenv("TG_YOUTRACK_STATE_OPEN", ""),
		YouTrackStateField:      getenv("TG_YOUTRACK_STATE_FIELD", ""),
		// READ-ONLY BY DEFAULT (TG-238). Writes require an explicit TG_YOUTRACK_WRITES=1. The corpus TG reads
		// for incident memory is the same one the predecessor is driven by, so an accidental comment or state
		// transition contaminates a running comparison at the input, where no analysis can undo it.
		YouTrackWritesEnabled: getenv("TG_YOUTRACK_WRITES", "") == "1",
		JiraURL:               getenv("TG_JIRA_URL", ""),
		JiraEmail:             getenv("TG_JIRA_EMAIL", ""),
		JiraTokenRef:          getenv("TG_JIRA_TOKEN_REF", "env:JIRA_TOKEN"),
		// The deployment's own Jira workflow transition ids (config-not-code); empty ⇒ reference default.
		JiraTransitionInProgress:  getenv("TG_JIRA_TRANSITION_INPROGRESS", ""),
		JiraTransitionResolved:    getenv("TG_JIRA_TRANSITION_RESOLVED", ""),
		JiraTransitionOpen:        getenv("TG_JIRA_TRANSITION_OPEN", ""),
		GitHubURL:                 getenv("TG_GITHUB_URL", ""),
		GitHubOwner:               getenv("TG_GITHUB_OWNER", ""),
		GitHubRepo:                getenv("TG_GITHUB_REPO", ""),
		GitHubTokenRef:            getenv("TG_GITHUB_TOKEN_REF", "env:GITHUB_TOKEN"),
		ServiceNowURL:             getenv("TG_SERVICENOW_URL", ""),
		ServiceNowUser:            getenv("TG_SERVICENOW_USER", ""),
		ServiceNowTokenRef:        getenv("TG_SERVICENOW_TOKEN_REF", "env:SERVICENOW_TOKEN"),
		ServiceNowStateInProgress: getenv("TG_SERVICENOW_STATE_INPROGRESS", ""),
		ServiceNowStateResolved:   getenv("TG_SERVICENOW_STATE_RESOLVED", ""),
		ServiceNowStateOpen:       getenv("TG_SERVICENOW_STATE_OPEN", ""),
	}); err != nil {
		log.Fatalf("tracker registration failed (fail-closed): %v", err)
	}
	// Declare the configured notifiers (config-not-code). Each channel's approver set is the human
	// authorization roster (INV-12: a vote binds a decision only from a listed sender); credentials are
	// secret references, never literals. An unconfigured channel is absent from the live set.
	// The live module-config holder. The notifier family is assembled HERE, hundreds of lines before the
	// database pool exists, so the accessors below cannot capture a store — they read this holder at USE
	// time instead, and it is populated once the pool is up. Nil until then ⇒ the boot values stand,
	// which is the correct behaviour for the window before any override could have been read anyway.
	liveCfg := &atomic.Pointer[liveModuleConfig]{}
	liveList := func(key string, fallback []string) []string {
		if l := liveCfg.Load(); l != nil {
			return l.list(key, fallback)
		}
		return fallback
	}
	liveValue := func(key, fallback string) string {
		if l := liveCfg.Load(); l != nil {
			return l.value(key, fallback)
		}
		return fallback
	}
	liveKV := func(key string, fallback map[string]string) map[string]string {
		if l := liveCfg.Load(); l != nil {
			return l.kvMap(key, fallback)
		}
		return fallback
	}
	if err := bootstrap.RegisterNotifiers(moduleReg, bootstrap.NotifierConfig{
		MatrixHomeserver:  getenv("TG_MATRIX_HOMESERVER", ""),
		MatrixTokenRef:    getenv("TG_MATRIX_TOKEN_REF", "env:MATRIX_TOKEN"),
		MatrixApprovers:   splitTokens(getenv("TG_MATRIX_APPROVERS", "")),
		MatrixRooms:       keyValueMap(getenv("TG_MATRIX_ROOMS", "")),
		MatrixDefaultRoom: getenv("TG_MATRIX_DEFAULT_ROOM", ""),
		// Read per USE, so revoking an approver or re-routing a room takes effect without a restart.
		// The approver set is the security-relevant one: the reason to revoke is usually urgent.
		MatrixLiveApprovers: func() []string {
			return liveList(catalog.ConfigKeyName("notifier", "matrix", "approvers"),
				splitTokens(getenv("TG_MATRIX_APPROVERS", "")))
		},
		MatrixLiveRooms: func() (map[string]string, string) {
			return liveKV(catalog.ConfigKeyName("notifier", "matrix", "rooms"), keyValueMap(getenv("TG_MATRIX_ROOMS", ""))),
				liveValue(catalog.ConfigKeyName("notifier", "matrix", "default_room"), getenv("TG_MATRIX_DEFAULT_ROOM", ""))
		},
		SlackURL:            getenv("TG_SLACK_URL", ""),
		SlackTokenRef:       getenv("TG_SLACK_TOKEN_REF", "env:SLACK_TOKEN"),
		SlackApprovers:      splitTokens(getenv("TG_SLACK_APPROVERS", "")),
		SlackChannels:       keyValueMap(getenv("TG_SLACK_CHANNELS", "")),
		SlackDefaultChannel: getenv("TG_SLACK_DEFAULT_CHANNEL", ""),
		TeamsURL:            getenv("TG_TEAMS_URL", ""),
		TeamsConversation:   getenv("TG_TEAMS_CONVERSATION", ""),
		TeamsTokenRef:       getenv("TG_TEAMS_TOKEN_REF", "env:TEAMS_TOKEN"),
		TeamsApprovers:      splitTokens(getenv("TG_TEAMS_APPROVERS", "")),
		EmailSMTP:           getenv("TG_EMAIL_SMTP", ""),
		EmailFrom:           getenv("TG_EMAIL_FROM", ""),
		EmailTo:             splitTokens(getenv("TG_EMAIL_TO", "")),
		EmailApprovers:      splitTokens(getenv("TG_EMAIL_APPROVERS", "")),
		EmailUser:           getenv("TG_EMAIL_SMTP_USER", ""),
		EmailPasswordRef:    getenv("TG_EMAIL_SMTP_TOKEN_REF", "env:EMAIL_SMTP_PASSWORD"),
		TwilioURL:           getenv("TG_TWILIO_URL", ""),
		TwilioSID:           getenv("TG_TWILIO_SID", ""),
		TwilioFrom:          getenv("TG_TWILIO_FROM", ""),
		TwilioTo:            getenv("TG_TWILIO_TO", ""),
		TwilioTokenRef:      getenv("TG_TWILIO_TOKEN_REF", "env:TWILIO_TOKEN"),
		MattermostURL:       getenv("TG_MATTERMOST_URL", ""),
		MattermostTokenRef:  getenv("TG_MATTERMOST_TOKEN_REF", "env:MATTERMOST_TOKEN"),
		MattermostApprovers: splitTokens(getenv("TG_MATTERMOST_APPROVERS", "")),
		MattermostChannels:  keyValueMap(getenv("TG_MATTERMOST_CHANNELS", "")),
	}); err != nil {
		log.Fatalf("notifier registration failed (fail-closed): %v", err)
	}
	// Declare the remaining config-driven capabilities (config-not-code, reusing the estate's NetBox/LibreNMS
	// endpoints): the NetBox CMDB reader, the endpoint-driven observability exporters, and the LibreNMS ingest
	// source. Each is a capability only where configured.
	if err := bootstrap.RegisterCMDB(moduleReg, getenv("TG_NETBOX_URL", ""), getenv("TG_NETBOX_TOKEN_REF", "env:NETBOX_TOKEN")); err != nil {
		log.Fatalf("cmdb registration failed (fail-closed): %v", err)
	}
	if err := bootstrap.RegisterConfiguredObservability(moduleReg, bootstrap.ObservabilityConfig{
		OpenObserveEndpoint:  getenv("TG_OPENOBSERVE_URL", ""),
		OpenObserveTokenRef:  getenv("TG_OPENOBSERVE_TOKEN_REF", "env:OPENOBSERVE_TOKEN"),
		LangfuseEndpoint:     getenv("TG_LANGFUSE_URL", ""),
		LangfusePublicRef:    getenv("TG_LANGFUSE_PUBLIC_REF", "env:LANGFUSE_PUBLIC_KEY"),
		LangfuseSecretRef:    getenv("TG_LANGFUSE_SECRET_REF", "env:LANGFUSE_SECRET_KEY"),
		HealthchecksURL:      getenv("TG_HEALTHCHECKS_URL", ""),
		HealthchecksCheckRef: getenv("TG_HEALTHCHECKS_CHECK_REF", "env:HEALTHCHECKS_UUID"),
	}); err != nil {
		log.Fatalf("observability registration failed (fail-closed): %v", err)
	}
	if err := bootstrap.RegisterConfiguredIngest(moduleReg, librenmsDeployments(getenv("TG_LIBRENMS_DEPLOYMENTS", ""))); err != nil {
		log.Fatalf("ingest registration failed (fail-closed): %v", err)
	}
	if len(moduleReg.Manifest()) == 0 {
		log.Fatalf("module registry declares no capabilities — refusing to start (fail-closed)")
	}
	// Reconcile the live capability set against the operator-declared expected set (config-not-code). A
	// deployment that pins its fleet via TG_EXPECTED_CAPABILITIES refuses to start if the live set diverges —
	// an unexpected capability (a config slip or supply-chain surprise) or a missing one (a connector that
	// failed to register). Unset = opt-out: the fleet is logged but not pinned.
	if err := bootstrap.Reconcile(moduleReg.Manifest(), splitTokens(getenv("TG_EXPECTED_CAPABILITIES", ""))); err != nil {
		log.Fatalf("module registry reconciliation FAILED (fail-closed): %v", err)
	}
	log.Printf("module registry: %d capabilities declared — %v", len(moduleReg.Manifest()), moduleReg.Manifest())

	// TG-267: register every construction the probe registry was offered — including the ones offered
	// BEFORE moduleReg existed (syslogng and the librenms tool set are built at the top of main). The
	// registry REPLAYS probeReg's identity set rather than hooking future offers only, so the earliest
	// constructions are not silently missed by an ordering accident.
	//
	// STRICTLY AFTER Reconcile — the pin (TG_EXPECTED_CAPABILITIES) governs the bootstrap families it has
	// always governed, and declaring the wider set before it would refuse boot on any pinned deployment.
	// The first draft of this wiring sat one line ABOVE the pin and its own AST oracle caught it.
	probeReg.declare = declareConstructed(moduleReg)
	if n := declareOffered(moduleReg, probeReg); n > 0 {
		log.Printf("module registry: %d construction(s) declared from the probe registry (TG-267)", n)
	}

	// The credential/identity engine (spec/016), instantiated LIVE from operator config: a SyncEngine over the
	// native fallback + every configured READ-ONLY source (OpenBao/Vault, AWX, Semaphore on the machine plane;
	// LDAP/FreeIPA on the human plane). Each source's creds are SecretRef references, never literals (INV-13). A
	// source whose config is absent is skipped; a source whose config is PARTIAL/invalid FAILS THE BOOT closed
	// (a misconfigured credential source must never silently drop and let actuation resolve a wrong/blank
	// identity). The engine is HELD for future actuation resolution + the grounder read surface; mutation stays
	// OFF — this is read-only credential resolution, Phase-1-safe.
	credEngine, credSources, err := bootstrap.BuildSyncEngine(bootstrap.CredentialConfig{
		NativeRules: getenv("TG_CREDENTIAL_NATIVE_RULES", ""),
		// PLANE-SCOPED (TG-153): these rows carry the hostdiag SSH KEY REFS. The actuation plane must not hold
		// read-plane host keys, so it registers no native hostdiag credential source.
		HostDiagDeployments:  planeEnv("TG_HOSTDIAG_DEPLOYMENTS", ""),
		OpenBaoAddr:          getenv("TG_OPENBAO_ADDR", ""),
		OpenBaoSourceID:      getenv("TG_OPENBAO_SOURCE_ID", ""),
		OpenBaoAuthMethod:    getenv("TG_OPENBAO_AUTH_METHOD", ""),
		OpenBaoTokenRef:      getenv("TG_OPENBAO_TOKEN_REF", ""),
		OpenBaoRoleIDRef:     getenv("TG_OPENBAO_ROLE_ID_REF", ""),
		OpenBaoSecretIDRef:   getenv("TG_OPENBAO_SECRET_ID_REF", ""),
		OpenBaoWrapTokenRef:  getenv("TG_OPENBAO_WRAP_TOKEN_REF", ""),
		OpenBaoJWTRef:        getenv("TG_OPENBAO_JWT_REF", ""),
		OpenBaoJWTRole:       getenv("TG_OPENBAO_JWT_ROLE", ""),
		OpenBaoCertPath:      getenv("TG_OPENBAO_CERT", ""),
		OpenBaoKeyPath:       getenv("TG_OPENBAO_KEY", ""),
		OpenBaoCertRole:      getenv("TG_OPENBAO_CERT_ROLE", ""),
		OpenBaoCACertPath:    getenv("TG_OPENBAO_CA", ""),
		OpenBaoKVMount:       getenv("TG_OPENBAO_KV_MOUNT", ""),
		OpenBaoKVPrefix:      getenv("TG_OPENBAO_KV_PREFIX", ""),
		AWXAddr:              getenv("TG_AWX_ADDR", ""),
		AWXSourceID:          getenv("TG_AWX_SOURCE_ID", ""),
		AWXTokenRef:          getenv("TG_AWX_TOKEN_REF", "env:AWX_TOKEN"),
		AWXCACertPath:        getenv("TG_AWX_CA", ""),
		AWXInventoryID:       getenv("TG_AWX_INVENTORY_ID", ""),
		AWXRefScheme:         getenv("TG_AWX_REF_SCHEME", ""),
		AWXRefPrefix:         getenv("TG_AWX_REF_PREFIX", ""),
		AWXRefField:          getenv("TG_AWX_REF_FIELD", ""),
		AWXCredRefMap:        getenv("TG_AWX_CRED_REF_MAP", ""),
		AWXDefaultUser:       getenv("TG_AWX_DEFAULT_USER", ""),
		SemaphoreAddr:        getenv("TG_SEMAPHORE_ADDR", ""),
		SemaphoreSourceID:    getenv("TG_SEMAPHORE_SOURCE_ID", ""),
		SemaphoreTokenRef:    getenv("TG_SEMAPHORE_TOKEN_REF", "env:SEMAPHORE_TOKEN"),
		SemaphoreCACertPath:  getenv("TG_SEMAPHORE_CA", ""),
		SemaphoreProjectID:   getenv("TG_SEMAPHORE_PROJECT_ID", ""),
		SemaphoreRefScheme:   getenv("TG_SEMAPHORE_REF_SCHEME", ""),
		SemaphoreRefPrefix:   getenv("TG_SEMAPHORE_REF_PREFIX", ""),
		SemaphoreRefField:    getenv("TG_SEMAPHORE_REF_FIELD", ""),
		LDAPURLs:             getenv("TG_LDAP_URLS", ""),
		LDAPUserBase:         getenv("TG_LDAP_USER_BASE", ""),
		LDAPGroupBase:        getenv("TG_LDAP_GROUP_BASE", ""),
		LDAPSourceID:         getenv("TG_LDAP_SOURCE_ID", ""),
		LDAPBindDNRef:        getenv("TG_LDAP_BIND_DN", "env:LDAP_BIND_DN"),
		LDAPBindPWRef:        getenv("TG_LDAP_BIND_PW", "env:LDAP_BIND_PW"),
		LDAPCACertRef:        getenv("TG_LDAP_CA", ""),
		LDAPStartTLS:         getenv("TG_LDAP_STARTTLS", ""),
		OIDCTokenURL:         getenv("TG_OIDC_TOKEN_URL", ""),
		OIDCClientIDRef:      getenv("TG_OIDC_CLIENT_ID_REF", ""),
		OIDCClientSecretRef:  getenv("TG_OIDC_CLIENT_SECRET_REF", ""),
		OIDCScope:            getenv("TG_OIDC_SCOPE", ""),
		OIDCAudience:         getenv("TG_OIDC_AUDIENCE", ""),
		OIDCCACertPath:       getenv("TG_OIDC_CA", ""),
		OIDCAuthStyle:        getenv("TG_OIDC_AUTH_STYLE", ""),
		AnsibleRoot:          getenv("TG_ANSIBLE_ROOT", ""),
		AnsibleSourceID:      getenv("TG_ANSIBLE_SOURCE_ID", ""),
		AnsibleInventoryPath: getenv("TG_ANSIBLE_INVENTORY", ""),
		AnsibleVaultPassRef:  getenv("TG_ANSIBLE_VAULT_PASS_REF", ""),
		AnsibleDefaultUser:   getenv("TG_ANSIBLE_DEFAULT_USER", ""),
	})
	if err != nil {
		log.Fatalf("credential engine bootstrap failed (fail-closed): %v", err)
	}
	// The DB-backed operator-authored NATIVE mapping (TG-109, spec/016 REQ-1610): credential_native_rule
	// rows as a first-class machine-plane source on the SAME engine. REGISTERED AT STARTUP (INV-17 — the
	// adapter is compiled in; only its ROW LOADER is late-bound post-connect, the TG-451 atomic handoff,
	// so a console write lands with NO restart and NO boot risk). Registered ONLY when this process will
	// connect a pool — the same planeDBDSNFromEnv condition the durable-stores block below gates on — an
	// in-memory worker has no rule table and must not carry a source that can never sync. Precedence 90:
	// operator-authored DB rules outrank only the native hostdiag fallback (100); every real synced
	// system-of-record (OpenBao 10 / AWX 20 / Semaphore 30 / Ansible 35) still shadows them.
	nativeDB := nativedb.New()
	if dsn, _ := planeDBDSNFromEnv(credentialPlane); dsn != "" {
		const nativeDBPrecedence = 90
		if rerr := credEngine.RegisterSource(nativeDB, nativeDBPrecedence); rerr != nil {
			log.Fatalf("credential engine: register native-db source (fail-closed): %v", rerr)
		}
		// Joined to credSources so the boot log, the precedence publication, and the sync loop below all
		// include it. No SourceType: like the native hostdiag source it is operator-authored data, not a
		// vendor connector — no descriptor, no configuration dialog to probe.
		credSources = append(credSources, bootstrap.RegisteredCredentialSource{
			ID: nativeDB.ID(), Plane: nativeDB.Plane(), Precedence: nativeDBPrecedence, Instance: nativeDB, SourceType: "",
		})
	}
	for _, rs := range credSources {
		log.Printf("credential engine: registered source %q (plane=%s, precedence=%d) — read-only sync", rs.ID, rs.Plane, rs.Precedence)
		// Offer the source to the probe registry under its VENDOR SLUG, not its operator-configurable ID:
		// the slug is what keys a descriptor, and therefore which dialog's TEST button this instance
		// answers. A source with no slug (the inline native host-diag allowlist) is not a connector and
		// has no dialog.
		if rs.SourceType != "" {
			probeReg.offer("credsource", rs.SourceType, rs.Instance)
		}
	}
	if len(credSources) == 0 {
		log.Printf("credential engine: no external sources configured — native-fallback-only resolution (fail-closed for any uncovered target)")
	}
	// publishCredentialState projects the engine's NON-SECRET coverage + sync state to the console's DB
	// (migration 0017). It is a no-op until the DB pool exists (below); an in-memory worker simply never
	// publishes. It NEVER writes a secret — the SyncRun/coverage types are secret-free by construction (INV-13).
	publishCredentialState := func([]credential.SyncRun, []db.CredentialCoverage) {}
	// credCoverage reconstructs each source's live target count from the per-sync drift (added−removed): the
	// SyncEngine holds the converged set internally, and the deltas recover the absolute coverage without
	// reaching into it. A failed sync contributes (0,0,0) and leaves the count intact (prior state retained).
	credCoverage := map[string]int{}

	// The SHARED audited resolution seam (spec/016 REQ-1604/1617): both the read-only investigation path
	// (hostdiag, below) and — in a later flip slice — the actuation effect leaf resolve per-host identity
	// through this ONE resolver. It resolves via the SyncEngine (native hostdiag fallback + any synced source),
	// fails closed on ErrUnresolved/ErrAmbiguous (NO hardcoded one_key+root fallback), and appends one
	// non-secret credential_resolution audit row per Resolve. Its durable sink is installed once the DB pool
	// exists (below); until then resolutions still fail closed and return bundles, they just append no row.
	credResolver := credential.NewAuditedResolver(credEngine)

	// The hostdiag tools are built here but the wiring MANIFEST is ~1,500 lines below, so the Bind/Absent
	// is deferred through this variable. The yield REGISTER is not deferred: it is constructed just above
	// (it has no dependencies) precisely so the Observe call below is a literal wiringYield.Observe —
	// TestEveryDeclaredSeamIsYieldInstrumented reads the composition root looking for exactly that, and an
	// indirection through an atomic pointer defeats it. A seam whose instrumentation the guard cannot see
	// is a seam that reports UNOBSERVED forever.
	var hostDiagTools []agent.Tool

	// Read-only HOST-DIAGNOSTICS tools (the predecessor's SSH df/free/systemctl investigation): SSH the
	// alerting host and run a FIXED read-only diagnostic so the agent can GROUND a resource alert instead of
	// escalating blind. The allowlist (TG_HOSTDIAG_DEPLOYMENTS) gates WHETHER the tools exist; the per-host SSH
	// identity is resolved through credResolver (fail-closed) — the SAME allowlist also feeds the engine's
	// native hostdiag source, so a host resolves to exactly the (user, keyref) it reached before, now audited.
	// PLANE-SCOPED (TG-153): host diagnostics puts the STDOUT of commands run on estate hosts straight into
	// the agent's context — the most directly attacker-shapeable text TG reads. Withheld from the actuation
	// plane at acquisition, so that process builds no hostdiag tool and resolves no host SSH identity.
	if hdAccess := hostdiag.ParseAccess(planeEnv("TG_HOSTDIAG_DEPLOYMENTS", "")); len(hdAccess) > 0 {
		// Runner from the ROOT via the store-resolving getenv, for the same reason as syslog-ng above: the
		// module-side os.Getenv fallback is blind to UX-saved config (TG-265). The probe module shares the
		// SAME inputs the tools run on, so its green certifies the configuration the agent actually uses.
		hdKnownHosts := getenv("TG_HOSTDIAG_KNOWN_HOSTS", "")
		// EVERY read reports its outcome to the seam-yield register (TG-271). This lane failed on 100% of
		// calls for weeks and nothing said so: the tools were registered, the boot log was cheerful, and
		// each read returned the "(host was unreachable or the read errored)" sentinel — a perfectly valid
		// return value that no invocation-counting check can distinguish from success. Only the
		// produced/attempted PAIR tells a quiet estate from a blind agent, and all-failing now reports
		// STARVED instead of nothing at all.
		// The register is built ~700 lines below this point, so the observer reads it through an atomic
		// pointer at CALL time — the same hand-off suppGate uses. Tools are only invoked during triage,
		// long after boot has populated it; a nil pointer is a no-op, never a panic.
		hdTools := hostdiag.NewTools(hdAccess, syslogng.NewNativeRunner(hdKnownHosts), credResolver,
			hostdiag.WithYield(func(produced bool) {
				wiringYield.Observe(wiring.SeamHostDiag, 1, boolCount(produced), time.Now().UTC())
			}))
		for _, tl := range hdTools {
			if err := tools.RegisterFrom("host", tl); err != nil {
				log.Fatalf("register agent tool %s (fail-closed): %v", tl.Name(), err)
			}
		}
		log.Printf("agent: registered %d read-only host-diagnostics tools across %d access rule(s) — identity via the credential engine", len(hdTools), len(hdAccess))
		// The probe now performs a REAL READ and reports it to the same seam register the agent tools feed
		// (TG-300/TG-301). Before this, the register was fed ONLY during triage, so a lane that could read
		// nothing reported `hostdiag.read: unobserved` indefinitely — indistinguishable from a lane nobody
		// had needed yet. Measured 2026-08-04: the key authenticated to 0 of 20 hosts, every read failed,
		// the probe sweep said "10 ran, 10 ok", and the register said UNOBSERVED.
		probeReg.offer("observability", hostdiag.SourceType, hostdiag.NewModule(hdAccess, hdKnownHosts,
			hostdiag.WithProbeRead(syslogng.NewNativeRunner(hdKnownHosts), credResolver,
				func(produced bool) {
					wiringYield.Observe(wiring.SeamHostDiag, 1, boolCount(produced), time.Now().UTC())
				})))
		hostDiagTools = hdTools
		// BOOT COVERAGE, stated as a ratio the operator can compare against their estate. Host-key
		// verification fails CLOSED, so a host absent from this file is a host the agent can never
		// diagnose — and that is invisible at configure time. It surfaces mid-incident as a session that
		// stands down without naming the failing unit. On 2026-08-03 the file covered 16 of the 38 hosts
		// TG had alerted on in 30 days; nothing anywhere said so.
		if n, err := hostdiag.KnownHostEntryCount(hdKnownHosts); err != nil {
			log.Printf("hostdiag: known_hosts %q is UNREADABLE (%v) — every host-diagnostic read will be "+
				"refused, fail-closed, and the agent will triage every alert blind", hdKnownHosts, err)
		} else if n == 0 {
			log.Printf("hostdiag: known_hosts %q holds ZERO entries — every host-diagnostic read will be "+
				"refused, fail-closed, and the agent will triage every alert blind", hdKnownHosts)
		} else {
			log.Printf("hostdiag: known_hosts %s holds %d host-key entr%s — compare that against your "+
				"estate: a host missing from it cannot be diagnosed, and fails closed at read time",
				hdKnownHosts, n, map[bool]string{true: "y", false: "ies"}[n == 1])
		}
	}

	// TG-85 read-tool slice: the cisco-show tool over the operator-declared device set. DARK by default
	// (TG_CISCO_READ_DEVICES unset ⇒ nothing registers ⇒ the model's preamble is unchanged); a declared
	// device set that cannot be parsed or built is FATAL — a half-usable declaration must stop the boot,
	// never silently ship a narrower tool. Both states are boot-log-readable.
	if ciscoTool, nDev, cerr := wireCiscoReadTool(getenv); cerr != nil {
		log.Fatalf("cisco-show (fail-closed): %v", cerr)
	} else if ciscoTool == nil {
		log.Printf("cisco-show: DARK — no TG_CISCO_READ_DEVICES declared; the read catalog has no model-visible surface")
	} else {
		if err := tools.RegisterFrom("cisco", ciscoTool); err != nil {
			log.Fatalf("register agent tool %s (fail-closed): %v", ciscoTool.Name(), err)
		}
		log.Printf("agent: registered cisco-show over %d declared device(s) — the CLOSED show catalog, credential-bearing entries refused by name", nDev)
	}

	// The tier-1 suppression gate is constructed later (it needs the tracker + config); the telemetry loop
	// reads its decision counts through this atomic pointer, set when the gate is built. nil ⇒ no suppression
	// samples (the gate is not wired).
	var suppGate atomic.Pointer[runner.LiveSuppressGate]
	// TG-380 decision-stage instrument, declared here so both the gate construction and the admin exposition
	// closure below capture the SAME tally. The suppress stage records its offered/eligible/acted triple into
	// it on every Decide; the admin surface renders it on /metrics.
	stageTally := observe.NewStageTally()
	// OBSERVING THE SUPPRESSION SEAM MUST NOT DEPEND ON AN EXPORTER BEING CONFIGURED.
	//
	// This observation used to exist ONLY inside the observability export loop below, nested three deep:
	//
	//	if TG_OBSERVABILITY_EXPORT_INTERVAL != ""   (empty in production)
	//	  if len(exporters) > 0                     (needs an enabled exporter module)
	//	    for range t.C
	//
	// so on dc1tg01 nothing ever called it and suppression.tier1 reported UNOBSERVED forever — the
	// register's honest reading of "this seam could be producing nothing and I would not know". The
	// suppression gate is a SAFETY control (TG-219, the Tier-1 learning chain); whether it is admitting
	// everything or suppressing everything is exactly the question the yield pair answers.
	//
	// ObserveTotals SETS cumulative totals rather than adding, so calling this from more than one cadence
	// is idempotent — which is why the export loop can keep calling it too.
	observeSuppressionYield := func() {
		g := suppGate.Load()
		if g == nil {
			return // the gate is not armed yet; UNOBSERVED is the honest reading until it is
		}
		var admitted, suppressed int64
		for outcome, n := range g.Counts() {
			admitted += int64(n)
			if strings.Contains(strings.ToLower(outcome), "suppress") {
				suppressed += int64(n)
			}
		}
		wiringYield.ObserveTotals(wiring.SeamSuppression, admitted, suppressed, time.Now().UTC())
	}
	// wiringSamples holds the per-seam dark gauge for the export loop below. It needs an atomic for the
	// same reason suppGate does: the export goroutine is started HERE, ~2,600 lines before the wiring
	// report is taken (the report must run after every Bind/Absent site, which is the last thing boot
	// does). Without a hand-off the samples are computed and stranded — which is exactly what happened:
	// they were assigned to `_` under a comment promising an export "below" that was never written.
	// THE PAIRED-YIELD REGISTER (TG-250). The manifest answers "was this seam BOUND?", once, at boot. Six
	// of the nine defects fixed on 2026-08-01 were invisible to it because every one was bound AND
	// RUNNING: a guard whose predicate could never fire, probes that drafted zero entities, a corpus that
	// "loaded" with zero rows, an MTTR denominator silently missing its commonest class. This answers the
	// other half — did the wired seam PRODUCE anything — and publishes BOTH numbers, so a filter that
	// quietly stops matching is visible from outside the code.
	//
	// Constructed HERE, above the observability export loop that reads it, rather than beside the
	// manifest several hundred lines below: a register declared after its first reader is a compile error
	// today and would be a silently empty gauge under any lazier wiring.
	// (constructed above, before the hostdiag tools that observe into it)
	var wiringSampleSet atomic.Pointer[[]observability.Sample]
	// The YIELD gauges ride the same hand-off, for the same reason: a seam's offered/produced pair is a
	// RUNTIME fact, and a pair computed once at boot would report every seam as unobserved forever.
	var wiringYieldSampleSet atomic.Pointer[[]observability.Sample]
	// The mode reader, handed off the same way and for the same reason: the admin surface is built at the
	// END of main() while the mode controller is constructed mid-way, inside the DB-backed branch.
	var policyModeForMetrics atomic.Pointer[func() string]
	var policyPostureWarningsForMetrics atomic.Pointer[func() []policy.PolicyWarning]
	// The POLICY rate governor's counters, handed to the admin surface the same way the mode reader is
	// (TG-339). It is constructed ~1,200 lines below inside the DB-backed branch and may legitimately be
	// absent (no pool ⇒ no engine ⇒ no governor), so the metrics block reads it through the pointer and
	// publishes nothing when it is nil — an ABSENT series is the vacuity signal, not a fabricated zero.
	var policyRateGovForMetrics atomic.Pointer[*policy.RateGovernor]

	// Worker self-telemetry: periodically export liveness + declared-capability gauges to the ENABLED
	// observability exporters resolved from the registry (the 4th surface made load-bearing). Config-gated
	// (TG_OBSERVABILITY_EXPORT_INTERVAL, off by default) and fail-open — an export error is logged, never
	// fatal, and no exporter configured means no loop.
	if iv := getenv("TG_OBSERVABILITY_EXPORT_INTERVAL", ""); iv != "" {
		if d, derr := time.ParseDuration(iv); derr == nil && d > 0 {
			var exporters []observability.Exporter
			for _, cp := range moduleReg.Capabilities() {
				if cp.Surface == modules.SurfaceObservability && cp.Enabled {
					if exp, eerr := resolve.Exporter(moduleReg, cp.SourceType); eerr == nil {
						exporters = append(exporters, exp)
					}
				}
			}
			if len(exporters) > 0 {
				go func() {
					t := time.NewTicker(d)
					defer t.Stop()
					for range t.C {
						samples := telemetry.CapabilitySamples(moduleReg, time.Now())
						if g := suppGate.Load(); g != nil {
							counts := g.Counts()
							samples = append(samples, telemetry.SuppressionSamples(counts, time.Now())...)
							// The seam's yield pair, from the SAME counters. Shared with the unconditional
							// observer defined next to suppGate, so this loop is no longer the only path —
							// it was, and the seam read UNOBSERVED in every deployment without an exporter.
							observeSuppressionYield()
						}
						// The dark-seam gauge. Re-emitted every tick rather than once at boot: a gauge that
						// stops being reported is indistinguishable from a gauge reading zero, and this
						// series exists to alert on a seam going dark.
						if ws := wiringSampleSet.Load(); ws != nil {
							samples = append(samples, *ws...)
						}
						// The seam-YIELD pair (offered/produced/starved/unobserved). Both numbers, every
						// tick: a seam that is bound and producing nothing is invisible on the dark gauge
						// above, because it is not dark — it is running and emitting zero.
						if ys := wiringYieldSampleSet.Load(); ys != nil {
							samples = append(samples, *ys...)
						}
						for _, exp := range exporters {
							if eerr := exp.Export(context.Background(), samples); eerr != nil {
								log.Printf("observability: export to %s failed: %v (continuing)", exp.SourceType(), eerr)
							}
						}
					}
				}()
				log.Printf("observability: self-telemetry export every %s to %d exporter(s)", d, len(exporters))
			}
		} else {
			log.Printf("observability: invalid TG_OBSERVABILITY_EXPORT_INTERVAL %q — export disabled", iv)
		}
	}

	// THE TRACE EXPORT PATH (TG-44) — the composition root openobserve.ExportSpans never had.
	//
	// modules/observability/openobserve has carried ExportSpans, with tracing default-ON, since spec/008,
	// and nothing in the tree called it: the module's own descriptor said "Span export exists in the module
	// but no worker path calls it today, so no traces ship." A capability nobody invokes is not a
	// capability, and INV-14's "the session trajectory is reconstructable" was true only of TG's own
	// database — the one place you cannot look when the question is whether TG itself is behaving.
	//
	// DELIBERATELY NOT GATED ON TG_OBSERVABILITY_EXPORT_INTERVAL, unlike the periodic self-telemetry loop
	// above. That knob is a CADENCE for a repeating batch; a session trace is an EVENT, emitted once when an
	// investigation ends, and there is no interval to set. Riding the same switch would also mean an
	// operator who wanted traces had to accept a platform-wide metrics cadence — the coupled control the
	// descriptor already calls out as dishonest.
	//
	// The gate is simply "is a trace-capable exporter configured": an endpoint-less deployment resolves no
	// exporters, the fanout is empty, deps.SessionSpans stays nil, and the investigate activity's export
	// block is a no-op. That is the safe default — this ships nothing that was not explicitly configured.
	var sessionSpanSink sessionspan.Sink
	{
		var traceSinks sessionspan.Fanout
		var traceNames []string
		for _, cp := range moduleReg.Capabilities() {
			if cp.Surface != modules.SurfaceObservability || !cp.Enabled {
				continue
			}
			exp, eerr := resolve.Exporter(moduleReg, cp.SourceType)
			if eerr != nil {
				continue
			}
			// Type assertion, not a hard-coded module list: a future exporter that grows ExportSpans is
			// picked up here without touching this block, and one that never will (healthchecks.io is a
			// dead-man ping) is skipped instead of being handed a batch it would drop.
			te, ok := exp.(observability.TraceExporter)
			if !ok {
				continue
			}
			traceSinks = append(traceSinks, te)
			traceNames = append(traceNames, cp.SourceType)
		}
		if len(traceSinks) > 0 {
			sessionSpanSink = traceSinks
			log.Printf("observability: session TRACE export wired to %d exporter(s) %v — every completed "+
				"investigation now ships its ordered spans (summary + one per ReAct cycle) keyed by "+
				"external_ref (TG-44; before this, ExportSpans had no caller and no trace had ever shipped)",
				len(traceSinks), traceNames)
		} else {
			log.Print("observability: no trace-capable exporter configured — session spans are NOT exported " +
				"(honest no-op; configure TG_OPENOBSERVE_URL to enable)")
		}
	}

	// Build the causal estate graph the prediction gate reasons over, seeded from the configured CMDB
	// topology sources (config-not-code — a source is added only when its endpoint is declared). Each source
	// is per-source-isolated: an unconfigured or failing source contributes nothing rather than aborting the
	// others, and a fetch error is surfaced (logged), never silently presented as an empty truth. A target
	// that does not resolve still fails closed on eligibility — the correct behavior, not a vacuous prediction.
	var estateSources []estate.EdgeSource
	// TG-378: the pve topology source ALSO carries guest power states (same /cluster/resources fetch);
	// each estate sweep projects them into guest_liveness once the shared pool exists. Hoisted like
	// dbPool: the refresh closures below capture these and nil-check at call time — no pool, no pve
	// source, or no completed sweep each write NOTHING, so the projection's absence stays honest
	// (unknown, never an invented empty cluster).
	var pveGuestSource *pve.EstateSource
	var guestLivenessStore atomic.Pointer[db.GuestLivenessStore]
	// TG-466 slice 2: the PVE guest CONFIG-hash reader + collector (modules/cmdb/pve/confighash) — the
	// grounded positive observed-mutation signal AttributeActivity threads into Observation.MutationObserved
	// (temporal/runner/activities.go, Deps.GuestConfigChangedWithin). Hoisted for the SAME reason
	// guestLivenessStore above is: the estate refresh tick's goroutine is defined and started below, BEFORE
	// the durable pool connects, so the tick must load a pointer filled in LATER rather than close over a
	// value that does not exist yet. Both stay nil unless TG_PVE_CONFIGHASH_ENABLED is truthy AND the
	// read-only PVE credential resolves (TG_PVE_URL + TG_PVE_RO_TOKEN_REF) — the ship-dark default (TG-466):
	// unset, no Collector is ever built, the tick sweeps nothing, guest_config_baseline stays empty, and
	// AttributeActivity's Observation stays the zero value — byte-identical to pre-TG-466 behavior.
	// confighashReader != nil is ALSO the gate the attribution read seam (Deps.GuestConfigChangedWithin,
	// ~confighashReadArmed below) keys on — not the flag alone — so flag-ON-but-unresolved-credential can
	// never leave the read wired against a baseline nothing swept (a half-armed shape a review caught).
	var confighashReader *confighash.Reader
	var confighashCollector atomic.Pointer[confighash.Collector]
	// The seal-time precondition's freshness bound, derived from the CONFIGURED sweep cadence below
	// (reviewer finding on !1316: a bound smaller than the cadence is fail-closed but structurally dead).
	guestLivenessBound := guestLivenessStaleAfter
	estateRefreshArmed := false
	feedLiveness := func(ctx context.Context, when string, verbose bool) {
		var src guestStateSource
		if pveGuestSource != nil {
			src = pveGuestSource
		}
		var sink guestLivenessSink
		if st := guestLivenessStore.Load(); st != nil {
			sink = st
		}
		n, swept, err := feedGuestLiveness(ctx, sink, src)
		if err != nil {
			log.Printf("guest liveness: %s upsert failed: %v (projection goes stale — readers treat stale as unknown)", when, err)
			return
		}
		if verbose && swept {
			log.Printf("guest liveness: %d state(s) projected from the pve sweep (%s)", n, when)
		}
	}
	if nbURL := getenv("TG_NETBOX_URL", ""); nbURL != "" {
		nb := netbox.New(nbURL, config.SecretRef(getenv("TG_NETBOX_TOKEN_REF", "env:NETBOX_TOKEN")))
		probeReg.offer("cmdb", netbox.SourceType, nb)
		estateSources = append(estateSources, netbox.NewEstateSource(nb, getenv("TG_NETBOX_CASCADE_ALERT", "HostDown")))
	}
	// Slurp'it network-device discovery (TG-91): a read-only estate source contributing site-membership and
	// discovered-parent edges at the 0.82 discovered-inventory tier. Dark unless TG_SLURPIT_URL is set — exactly
	// like netbox above. Slurp'it is served over PLAIN HTTP, so a scheme-less URL resolves to http:// (never
	// https) inside slurpit.New — assuming TLS against a cleartext port would fail misleadingly.
	if slURL := getenv("TG_SLURPIT_URL", ""); slURL != "" {
		var slopts []slurpit.Option
		if ca := getenv("TG_SLURPIT_CASCADE_ALERT", "DeviceDown"); ca != "" {
			slopts = append(slopts, slurpit.WithExpectedAlerts(ca))
		}
		sl := slurpit.New(slURL, config.SecretRef(getenv("TG_SLURPIT_TOKEN_REF", "env:SLURPIT_TOKEN")), slopts...)
		probeReg.offer("cmdb", slurpit.SourceType, sl)
		estateSources = append(estateSources, sl)
	}
	if deps := librenmsDeployments(getenv("TG_LIBRENMS_DEPLOYMENTS", "")); len(deps) > 0 {
		topts := []librenms.TopoOption{librenms.WithExpectedAlerts(getenv("TG_LIBRENMS_CASCADE_ALERT", "DeviceDown"))}
		if truthyEnv("TG_LIBRENMS_INSECURE") {
			topts = append(topts, librenms.WithTopologyHTTPClient(estateHTTPClient(true)))
			log.Printf("estate: LibreNMS TLS verification DISABLED (TG_LIBRENMS_INSECURE=true)")
			reportTLSSkip(deps[0].BaseURL)
		}
		lnmsEstate := librenms.NewEstateSource(deps, topts...)
		probeReg.offer("ingest", librenms.SourceType, lnmsEstate)
		estateSources = append(estateSources, lnmsEstate)
	}
	// TWO FLAGS, ONE DECISION — report a disagreement rather than resolving it silently. See
	// pveTLSFlagDisagreement for why this does not change either flag's effect.
	if disagree, detail := pveTLSFlagDisagreement(truthyEnv("TG_PVE_INSECURE"), truthyEnv("TG_PROXMOX_INSECURE"), pveLivenessTLSFlagKey(planeEnv)); disagree {
		log.Printf("config: Proxmox TLS flags DISAGREE — %s", detail)
	}

	if pveURL := getenv("TG_PVE_URL", ""); pveURL != "" {
		popts := []pve.Option{pve.WithExpectedAlerts(getenv("TG_PVE_CASCADE_ALERT", "HostDown"))}
		if truthyEnv("TG_PVE_INSECURE") {
			popts = append(popts, pve.WithHTTPClient(estateHTTPClient(true)))
			log.Printf("estate: PVE TLS verification DISABLED (TG_PVE_INSECURE=true)")
			reportTLSSkip(pveURL)
		}
		pveSrc := pve.New(pveURL, config.SecretRef(getenv("TG_PVE_TOKEN_REF", "env:PVE_API_TOKEN")), popts...)
		probeReg.offer("cmdb", pve.SourceType, pveSrc)
		estateSources = append(estateSources, pveSrc)
		pveGuestSource = pveSrc // TG-378: the same source feeds the guest-liveness projection

		// TG-466 slice 2: arm the confighash reader over the SAME PVE cluster with a DEDICATED least-privilege
		// read-only token (INV-13 — never the actuation write token), gated on TG_PVE_CONFIGHASH_ENABLED so
		// merging this wiring changes nothing until an operator deliberately arms it (ship dark). Mirrors the
		// actor-evidence PVE reader's own credential resolution below (TG_PVE_RO_TOKEN_REF, no fallback to the
		// topology token or the actuation write pair) — the two signals are meant to corroborate each other.
		if truthyEnv("TG_PVE_CONFIGHASH_ENABLED") {
			roRef := getenv("TG_PVE_RO_TOKEN_REF", "")
			if roTok, rerr := config.SecretRef(roRef).Resolve(); roRef != "" && rerr == nil && strings.TrimSpace(roTok) != "" {
				chopts := []confighash.ReaderOption{confighash.WithTimeout(8 * time.Second)}
				if truthyEnv("TG_PVE_INSECURE") {
					chopts = append(chopts, confighash.WithHTTPClient(estateHTTPClient(true)))
					log.Printf("confighash: PVE TLS verification DISABLED (TG_PVE_INSECURE=true)")
					reportTLSSkip(pveURL) // TG-367: deduped by endpoint — this is the SAME pveURL the topology source above already reported on
				}
				confighashReader = confighash.NewReader(pveURL, config.SecretRef(roRef), chopts...)
				log.Printf("confighash: PVE guest config-hash reader armed (read-only token) — TG-466 slice 2 grounded mutation signal populates once the estate refresh tick AND the durable pool are both up")
			} else {
				log.Printf("confighash: TG_PVE_CONFIGHASH_ENABLED is set but TG_PVE_RO_TOKEN_REF is unset or resolves empty — the confighash reader is NOT armed (config-gated; the mutation signal stays absent)")
			}
		}
	}
	// TG-466 slice 2: the loud half-armed WARNING (review finding) — fires whether TG_PVE_URL was empty (so
	// the block above never even ran) or the token inside it failed to resolve; confighashReader reflects the
	// true end state either way. This also gates the READ arm below (~confighashReadArmed): the read seam can
	// now only wire when confighashReader != nil, so it can never end up armed against a baseline nothing swept.
	if w := confighashSweepWarning(truthyEnv("TG_PVE_CONFIGHASH_ENABLED"), confighashReader != nil); w != "" {
		log.Printf("%s", w)
	}
	// vSphere / vCenter VM placement (TG-91): a live-hypervisor source ALONGSIDE pve, read-only, DARK unless
	// TG_VSPHERE_URL is set (see vsphereEstateSource). Emits VM→physical_host runs_on edges for a VMware estate.
	if vsSrc, ok := vsphereEstateSource(getenv); ok {
		estateSources = append(estateSources, vsSrc)
		probeReg.offer("cmdb", vsphere.SourceType, vsSrc)
		log.Printf("estate: vSphere source ENABLED — VM→physical_host runs_on edges, read-only (TG-91)")
	}
	// The operator-declared estate: edges an administrator maintains to fill gaps the live sources miss. They
	// carry SourceDeclared (0.85), so a live source always out-ranks them — "live devices state is the source
	// of truth", declared fills the gaps. A malformed file is logged loudly and skipped, never a silent gap.
	if declFile := getenv("TG_ESTATE_DECLARED_FILE", ""); declFile != "" {
		if f, err := os.Open(declFile); err != nil {
			log.Printf("estate: declared-estate file %s unreadable: %v (skipped)", declFile, err)
		} else {
			edges, perr := estate.ParseDeclared(f)
			f.Close()
			if perr != nil {
				log.Printf("estate: declared-estate file %s rejected: %v (skipped — no phantom edges seeded)", declFile, perr)
			} else {
				estateSources = append(estateSources, estate.NewDeclaredSource(edges))
			}
		}
	}
	// The TOP tier: declared network tunnels (routes_via at 1.0 — ground truth). A cross-site VPS whose only
	// path is a firewall tunnel is placed in that firewall's blast radius, so a genuine tunnel cascade is not
	// lost as background noise.
	if tunnelFile := getenv("TG_ESTATE_TUNNEL_FILE", ""); tunnelFile != "" {
		if f, err := os.Open(tunnelFile); err != nil {
			log.Printf("estate: tunnel file %s unreadable: %v (skipped)", tunnelFile, err)
		} else {
			tunnels, perr := estate.ParseTunnels(f)
			f.Close()
			if perr != nil {
				log.Printf("estate: tunnel file %s rejected: %v (skipped)", tunnelFile, perr)
			} else {
				estateSources = append(estateSources, estate.NewTunnelSource(tunnels))
			}
		}
	}
	// The self-learning tier: incident co-occurrence observations (an operator-exported history, until the
	// outcome-labelled memory loop feeds it automatically). Learned edges are capped at 0.75 — below every
	// live source and the suppression cutoff — so they only enrich prediction, never outrank truth or suppress.
	if learnFile := getenv("TG_ESTATE_LEARNED_FILE", ""); learnFile != "" {
		if f, err := os.Open(learnFile); err != nil {
			log.Printf("estate: learned-estate file %s unreadable: %v (skipped)", learnFile, err)
		} else {
			obs, perr := estate.ParseCoOccurrences(f)
			f.Close()
			if perr != nil {
				log.Printf("estate: learned-estate file %s rejected: %v (skipped)", learnFile, perr)
			} else {
				estateSources = append(estateSources, estate.NewLearnedSource(obs))
			}
		}
	}
	// ── THE SERVICE-OBSERVING DISCOVERY PROBES (spec/027 plane 2, seam discovery.service) ───────────
	//
	// modules/discovery/{systemd,docker} were linked into NO BINARY. They are the ONLY producers of
	// estate.TypeService, and core/worldmodel/manifest.go routes exclusively TypeService to KindUnit and
	// KindContainer — so two of the three adoption kinds could never receive a drafted entry, while the
	// world.discovery seam reported LIVE and the boot log announced "armed every N over M source(s)".
	// spec/027 marked the registration task completed against a registration that did not exist.
	//
	// Config-not-code and DARK BY DEFAULT: no host list ⇒ no probe is constructed and the seam declares
	// why. The probes are read-only by construction (each holds its enumeration as a package constant),
	// they never traverse the mode chokepoint, and adoption still requires an operator — discovery
	// proposes, adoption grants, and the leaf default-deny actuation gate refuses everything it was not
	// handed. This cannot widen actuation; it only stops an operator hand-typing what TG can observe.
	var discoverySources []estate.EdgeSource
	var discoveryHostCounts []int // parallel to discoverySources; the probe's OFFERED denominator
	discoveryKnownHosts := getenv("TG_DISCOVERY_KNOWN_HOSTS", "")
	discoveryTimeout := envDuration("TG_DISCOVERY_TIMEOUT", 15*time.Second)
	// PLANE-SCOPED (TG-153): the service-observing probes open an SSH session per host and read what runs
	// there. Untrusted-content acquisition — triage plane only.
	if r := newDiscoveryRunner(splitList(planeEnv("TG_DISCOVERY_SYSTEMD_HOSTS", "")), credResolver, discoveryKnownHosts, discoveryTimeout); r != nil {
		src := systemddisc.New(r.hostList(), systemddisc.WithRunner(r))
		probeReg.offer(modules.SurfaceDiscovery, systemddisc.SourceType, src)
		discoverySources = append(discoverySources, src)
		discoveryHostCounts = append(discoveryHostCounts, len(r.hostList()))
		if err := moduleReg.Register(modules.Registration{
			Surface: modules.SurfaceDiscovery, SourceType: systemddisc.SourceType,
			Capability: modules.SurfaceDiscovery + "." + systemddisc.SourceType, Enabled: true, Adapter: src,
		}); err != nil {
			log.Fatalf("register discovery source %s (fail-closed): %v", systemddisc.SourceType, err)
		}
		log.Printf("discovery: systemd-unit probe armed over %d host(s) — observed units become adoptable KindUnit drafts", len(r.hostList()))
	}
	if r := newDiscoveryRunner(splitList(planeEnv("TG_DISCOVERY_DOCKER_HOSTS", "")), credResolver, discoveryKnownHosts, discoveryTimeout); r != nil {
		src := dockerdisc.New(r.hostList(), dockerdisc.WithRunner(r))
		probeReg.offer(modules.SurfaceDiscovery, dockerdisc.SourceType, src)
		discoverySources = append(discoverySources, src)
		discoveryHostCounts = append(discoveryHostCounts, len(r.hostList()))
		if err := moduleReg.Register(modules.Registration{
			Surface: modules.SurfaceDiscovery, SourceType: dockerdisc.SourceType,
			Capability: modules.SurfaceDiscovery + "." + dockerdisc.SourceType, Enabled: true, Adapter: src,
		}); err != nil {
			log.Fatalf("register discovery source %s (fail-closed): %v", dockerdisc.SourceType, err)
		}
		log.Printf("discovery: docker-container probe armed over %d host(s) — observed containers become adoptable KindContainer drafts", len(r.hostList()))
	}
	// EVERY REGISTERED MODULE IS OFFERED A PROBE, ONCE, HERE — the registry is now fully populated.
	//
	// This sweep exists because a per-feature offer inherits that feature's gate. The observability
	// exporters are resolved only inside `if TG_OBSERVABILITY_EXPORT_INTERVAL != ""`, so a deployment with
	// a configured Healthchecks watchdog and no export loop had a working probe that nothing ever handed
	// over: the module is registered, enabled and reachable, and its dialog would still have reported "no
	// test is implemented". The trackers had the same shape, resolved into trackersByName a thousand lines
	// further down for an unrelated purpose.
	//
	// Asking the REGISTRY rather than each downstream consumer removes that whole class: a module is
	// probe-offered because it is registered, not because some later feature happened to need it.
	// Modules outside the registry — pve, pve-liveness, the awx-job client, the awxplaybooks client, the
	// credential sources, the syslog-ng tools — are offered at their own constructors above, which is the
	// only place they exist.
	for _, cp := range moduleReg.Capabilities() {
		if !cp.Enabled {
			continue
		}
		reg, rerr := moduleReg.Resolve(cp.Surface, cp.SourceType)
		if rerr != nil {
			continue
		}
		probeReg.offer(cp.Surface, cp.SourceType, reg.Adapter)
	}

	// The seam is recorded at the world-discovery arming block below, where wiringManifest exists — the
	// two discovery seams belong together in the boot report anyway.
	estateSources = append(estateSources, discoverySources...)

	// TG-346 — THE ACTUATION PLANE RELAYS THE TRIAGE PLANE'S GRAPH, THROUGH THE DATABASE, NOT THROUGH
	// CREDENTIALS. This plane's blast-radius gate was reasoning over 17 edges against the triage plane's
	// 392+: the estate readers need read-triage secret references the credential-plane split (TG-153,
	// REQ-2203) refuses to hand this process — handing them over was tried and the boot guard failed it
	// closed, twice, correctly. The GRAPH is not a credential: the triage plane persists it to
	// estate_snapshot on every refresh, and this process already holds a database identity. relayLoad is
	// LATE-BOUND at pool connect (the pool does not exist yet here); until then the source errors, the
	// per-source isolation keeps the prior graph, and the post-connect prime below installs the relayed
	// estate before the first gate decision needs it.
	estateRelayLoad := &estateRelayLoader{} // TG-451: atomic handoff — bound post-connect, read from the refresh goroutine
	estateRelayArmed := credentialPlane == credential.ProcessPlaneActuation
	if estateRelayArmed {
		relayMaxAge := envDuration("TG_ESTATE_SNAPSHOT_RELAY_MAX_AGE", 30*time.Minute)
		estateSources = append(estateSources, estate.SnapshotRelaySource{
			Load:   estateRelayLoad.load,
			MaxAge: relayMaxAge,
		})
		log.Printf("estate: snapshot relay ARMED on the actuation plane — this plane composes the triage plane's persisted graph (max age %s) so the mutation gate refuses over the whole estate, not the 4%% this plane's own credentials can see (TG-346)", relayMaxAge)
	}

	// TG-188 — CHAOS-CALIBRATE THE INFRAGRAPH. Every deliberately-injected fault is recorded in injected_fault
	// (ground truth: WE broke that host). SourceChaos turns each injection + the hosts that alarmed downstream
	// inside the cascade window into depends_on edges at 0.90 — above the learned cap because the root is
	// observed, not guessed — so chaos drills TEACH the estate graph, not merely score it. TRIAGE PLANE ONLY:
	// the actuation plane RELAYS the triage graph (above) and inherits these edges through the snapshot rather
	// than re-reading the ledger. LATE-BOUND exactly like the relay — the pool does not exist yet here, so until
	// the post-connect prime binds chaosLoad the source errors and per-source isolation keeps the prior graph.
	chaosLoad := &chaosLoader{} // TG-451: atomic handoff — bound post-connect, read from the refresh goroutine
	if !estateRelayArmed {
		estateSources = append(estateSources, estate.NewChaosSource(chaosLoad.load))
	}
	// TG-188 organic recovery: the refresh goroutine also pulls new recovery transitions and feeds the
	// learner's onset→clear pairing, so learned edges accrue an observed MTTR alongside their delay. Same
	// late-bound handoff; until the pool binds it the pull errors and the tick simply carries no clears.
	recoveryFeed := &recoveryFeedLoader{}

	// TG-343 follow-through: tg_estate_sources_failed said "on the last refresh" in its Help and was fed
	// `len(estateErrs)` — the BOOT build's errors, captured once and never reassigned. So the gauge froze
	// at its boot value: a source that failed at boot and self-healed read as failing forever, and — the
	// dangerous direction — a source that DIED AFTER BOOT was invisible to it. Found live 2026-08-07 when
	// the TG-346 relay (which fails once at boot by design, before the pool connects) left the actuation
	// plane reporting 1 while every refresh since had succeeded. One counter, written by every path that
	// rebuilds the graph.
	var estateSourcesFailed atomic.Int64
	initialGraph, estateErrs := estate.Build(context.Background(), estateSources, estate.WithDefaultEdgeSchema())
	estateSourcesFailed.Store(int64(len(estateErrs)))
	for _, e := range estateErrs {
		log.Printf("estate: source %s failed to seed: %v (its edges are absent, not silently assumed true)", e.Source, e.Err)
	}
	// Hold the graph behind an atomic Holder so it can be re-read from the live topology sources at runtime
	// without a restart. A periodic refresh (TG_ESTATE_REFRESH_INTERVAL, off by default) re-runs the build; a
	// total-source-outage refresh keeps the last good graph (never blanks the estate into vacuous predictions).
	estateHolder := estate.NewHolder(initialGraph)
	// The self-learning tier's LIVE feed: a thread-safe co-occurrence learner accrues from the incident stream
	// (each investigated incident's alert) and its learned edges are folded into every refresh, so the estate
	// improves itself from observed outcomes. Learned edges are capped 0.75 — they only ever enrich prediction.
	learner := learn.NewCoOccurrenceLearner(0)
	// publishEstate publishes the live causal graph to the read API's snapshot table (REQ-516). It starts
	// as a no-op and is replaced with the durable writer once the DB pool exists (below); so an in-memory
	// worker simply never publishes, and the grounder's estate surface honestly reports "no snapshot".
	publishEstate := func(*estate.Graph) {}
	// skillRows is the composer's production-snapshot reader (spec/014); nil until the DB pool exists,
	// which the composer treats as "compiled registry only" (the total fallback is the default).
	var skillRows func(context.Context) ([]skillstore.ProductionRow, error)
	// skillWriteActs executes console-ordered skill transitions in THIS process — the ledger's single
	// writer (spec/014 REQ-1311). nil without a DB: the write workflow is then not registered at all.
	var skillWriteActs *skillwrite.Activities
	// manifestWriteActs executes console-ordered world-model adoptions in THIS process — the ledger's
	// single writer (spec/027 REQ-2703). An adopted entry materializes into the actuation allowlist, so a
	// second status writer would mean a grant whose ledger entry could be missing. nil without a DB: the
	// write workflow is then not registered at all and the surface 503s rather than half-working.
	var manifestWriteActs *manifestwrite.Activities
	// opClassVerbActs executes the operator's earned-op-class verbs in THIS process (spec/028 REQ-2813).
	// The most consequential of the three write lanes: a ratified class is an argv template that runs as
	// root, so the grant and the ledger row that explains it must be produced by one writer in one order.
	// nil without a DB: the workflow is then not registered at all and the surface 503s rather than
	// half-working.
	var opClassVerbActs *opclassratify.Activities
	// policyTraceActs answers the policy packet-tracer (TG-105) in THIS process, over the SAME engine the
	// interceptor consults — never a grounder-side copy. nil unless the policy engine built (it needs a
	// composed engine to trace): POST /v1/policy/trace is then 503, not a fabricated verdict.
	var policyTraceActs *policytrace.Activities
	// configWriteActs executes console-ordered config overrides + sealed-secret commits in THIS
	// process (task #27 Phases C+D, REQ-523/524) — same single-ledger-writer discipline. nil without
	// a DB: the workflows are then not registered at all.
	var configWriteActs *configwrite.Activities
	// modeTransitionActs executes an operator-invoked autonomy-mode transition in THIS process on the
	// single chokepoint-bound ModeController (spec/015 REQ-1502) — the LAST gate before the mutation flip.
	// nil without a DB: the transition workflow is then not registered at all (POST /v1/mode fails closed).
	var modeTransitionActs *modetransition.Activities
	// engineToggleActs executes an operator-invoked policy-engine enable/disable on the worker's live
	// EngineToggle (spec/015 REQ-1519) — the single ledger writer. A nil bound toggle (TG_POLICY_ENGINE_TOGGLE
	// unset) makes the activity fail closed with ErrNoToggle, and the grounder surface reports "not armed".
	var engineToggleActs *enginetoggle.Activities
	// rulesetWriteActs executes an operator-invoked active-ruleset replacement in THIS process (spec/015
	// REQ-1503, TG-104) — validated (ParseRuleSet, fail-closed), ledgered, then persisted (active singleton
	// + immutable version archive). nil without a DB: the ruleset-write workflow is then not registered at
	// all (POST /v1/policy/ruleset fails closed).
	var rulesetWriteActs *rulesetwrite.Activities
	// nativeRuleActs executes an operator-invoked write to the DB-backed native credential mapping in
	// THIS process (TG-109, spec/016 REQ-1610) — validated (ParseRules, exactly one rule, fail-closed),
	// ledgered, then persisted to credential_native_rule. nil without a DB: the write workflow is then
	// not registered at all (the native-rule write routes fail closed).
	var nativeRuleActs *nativerule.Activities
	// objectGroupActs executes an operator-invoked object-group write (TG-481): validated + ledgered +
	// persisted to estate_object_group in the single-writer worker. nil without a DB ⇒ the write workflow is
	// not registered (the object-group write routes fail closed).
	var objectGroupActs *objectgroup.Activities
	// The trial engine's collaborators (spec/014 REQ-1306/1308); nil without a DB.
	var skillTrials skillstore.TrialStore
	var skillVersionByID func(context.Context, int64) (skillstore.Version, error)
	var skillTrialActs *skilltrial.Activities
	// The durable judge spine (task #26, spec/012 REQ-1106): the Runner's terminal triage-record
	// writer + the 2-hourly judge cron. Both nil without a DB — the record activity is then a
	// fail-open no-op and the cron is not registered (sessions stay honestly unjudged).
	var triageRecord func(context.Context, judge.TriageRow) error
	var triageMarkCleared func(context.Context, string, bool) error
	var triageMarkMutated func(context.Context, string) error
	// The earned-catalog evidence seam (spec/028 Stage 2, REQ-2802): every shadow proposal the runner
	// diverts feeds the candidacy journal here. nil without a DB — the seam is DOCUMENTED INERT
	// (spec/026 ships it stubbed), and the same facts stay durable on the session_triage row, so a
	// clustering backfill can recover them. It grants nothing: the journal is evidence, never capability.
	var recordProposalOccurrence func(context.Context, runner.ProposalOccurrence) error
	// The classifier's PRIOR-VERDICT input (spec/001 REQ-015, TG-223): the durable actuation verdicts recorded
	// for the target inside TG_PRIOR_VERDICT_WINDOW. nil without a DB — there is then no durable verdict ledger
	// to read, so the classifier's verdict branch stays inert and every classification is byte-identical to the
	// pre-feature ladder (fail toward caution: absent evidence is never a laxer band, and never an invented poll).
	var priorVerdicts func(context.Context, string) ([]runner.PriorVerdict, error)
	// The incident CORRELATION stage's two seams (TG-169): the evidence read over the ingest_alert front-door
	// ledger, and the durable routing-decision record. BOTH nil without a DB — the Runner then falls back to
	// the pre-TG-169 `severity == critical` rule and marks the session degraded on its own record, so a
	// deployment with no durable pool routes exactly as it did before this shipped rather than silently
	// taking a cheaper class for everything.
	var correlationWindow func(context.Context, time.Time) (correlate.Window, error)
	var execClassRecord func(context.Context, correlate.Decision) error
	// TG-385: the durable cluster-identity join a correlated cascade collapses on. DB-gated like the window
	// above — nil without a durable pool, which leaves every member investigating (the pre-collapse posture).
	var clusterJoin func(context.Context, int64, string, time.Time, time.Duration) (int64, error)
	var skillJudgeActs *skilljudge.Activities
	// The flywheel CREATION half (spec/014 REQ-1314): the daily generator cron that fires
	// GenerateCandidates -> AdmitToTrial -> StartTrial from the durable judge signal. nil without a DB —
	// the cron is then not registered (no durable means/drafts to act on). Generate-only; this lane never actuates.
	var skillGenActs *skillgen.Activities
	if iv := getenv("TG_ESTATE_REFRESH_INTERVAL", ""); iv != "" && len(estateSources) > 0 {
		if d, err := time.ParseDuration(iv); err == nil && d > 0 {
			estateRefreshArmed = true
			// TG-378: the precondition's freshness bound tracks the CONFIGURED cadence (max(15m, 3×d)) so
			// a slow sweep never makes every reading stale-by-construction.
			if guestLivenessBound = livenessBoundFor(d); guestLivenessBound > guestLivenessStaleAfter {
				log.Printf("guest liveness: freshness bound raised to %s (3× the configured %s estate refresh)", guestLivenessBound, d)
			}
			go func() {
				t := time.NewTicker(d)
				defer t.Stop()
				// The recovery-feed cursor starts at boot: earlier recoveries cannot be attributed anyway,
				// because the learner's onset map is in-memory and post-boot only (an episode straddling a
				// restart goes unattributed rather than mispaired — the same restore boundary Snapshot keeps).
				clearCursor := db.RecoveryCursor{At: time.Now().UTC()}
				for range t.C {
					// Feed new recovery transitions to the learner BEFORE folding its co-occurrences, so a
					// clear observed this tick reaches this tick's learned edges (TG-188). An unbound or
					// failing feed is "no clears yet" — the cursor holds and the next tick retries — but it
					// is SAID, like the sibling refresh errors below: a feed that fails every tick freezes
					// every learned MTTR, and silence there is the degradation this loop's own history
					// punishes (review finding on this MR).
					if clears, cur, err := recoveryFeed.load(context.Background(), clearCursor); err == nil {
						for _, c := range clears {
							learner.ObserveClear(c)
						}
						clearCursor = cur
					} else {
						log.Printf("estate refresh: recovery feed failed: %v (learned MTTR frozen at cursor %s until it recovers)", err, clearCursor.At.Format(time.RFC3339))
					}
					// re-read the live sources AND fold in the learner's current co-occurrences.
					sources := append(append([]estate.EdgeSource(nil), estateSources...), learner.LearnedSource())
					before := estateHolder.Graph().Len()
					kept, srcErrs := estateHolder.Refresh(context.Background(), sources, estate.WithDefaultEdgeSchema())
					estateSourcesFailed.Store(int64(len(srcErrs)))
					for _, e := range srcErrs {
						// Say which of the two things ACTUALLY happened. This line used to assert
						// "(kept prior edges)" unconditionally, while the guard that would have kept them
						// was unsatisfiable — so it printed reassurance during exactly the outage it was
						// describing.
						if kept {
							log.Printf("estate refresh: source %s failed: %v (rebuild was empty — prior graph KEPT)", e.Source, e.Err)
							continue
						}
						log.Printf("estate refresh: source %s failed: %v (the partial rebuild was INSTALLED)", e.Source, e.Err)
					}
					// A partial rebuild that collapses the graph is the case no threshold here can safely
					// judge — a real decommission and a source outage look identical from one sample. So
					// report the FACT rather than invent a policy: an operator seeing 412 edges become 37
					// knows what they are looking at, and today nothing tells them at all.
					//
					// THE GATE USED TO BE `len(srcErrs) > 0 && !kept`, AND THAT IS BACKWARDS (TG-395). It
					// reported the drop only when a source FAILED — the case where the graph is LEAST
					// likely to be damaged, because an empty rebuild sets `kept` and the prior graph is
					// retained. A refresh in which EVERY SOURCE SUCCEEDS can still collapse the topology,
					// and that is the reading that matters: measured 2026-08-06 02:57:32, the pve source
					// correctly reported that a dead node has no guests, 52 `runs_on` edges were dropped,
					// srcErrs was empty — so the guard was false and NOTHING PRINTED. The only two warnings
					// an operator got that morning were 17-edge wobbles at the other site.
					//
					// Report on the DROP, and say which of the two situations produced it, because they
					// call for different actions: sources failed (suspect data loss, the graph may be
					// wrong) versus every source succeeded (the estate really changed — a decommission, or
					// a node that has died and correctly reports no guests).
					if after := estateHolder.Graph().Len(); after < before {
						if len(srcErrs) > 0 {
							log.Printf("estate refresh: WARNING — the graph went from %d to %d edge(s) (-%d) AND %d source(s) failed; the drop may be missing data rather than a real change, and predictions now reason over the reduced topology",
								before, after, before-after, len(srcErrs))
						} else {
							log.Printf("estate refresh: WARNING — the graph went from %d to %d edge(s) (-%d) with EVERY source succeeding; this is a real topology change (a decommission, or a node that has died and correctly reports no guests) and predictions now reason over the reduced topology",
								before, after, before-after)
						}
					}
					publishEstate(estateHolder.Graph()) // republish the refreshed graph for the read API
					// TG-378: project the sweep's guest power states (quiet on the tick; errors always
					// log — the table's observed_at is the periodic evidence).
					feedLiveness(context.Background(), "refresh tick", false)
					// TG-466 slice 2: sweep the PVE guest CONFIG-hash baseline (nil until TG_PVE_CONFIGHASH_ENABLED
					// arms it above — the tick is then a no-op, byte-identical to pre-TG-466 behavior). Errored is
					// reported LOUDLY (Collector.Report's own contract, TG-365): a starving sweep must never look
					// like a quiet estate, because that silence would read as "no mutations anywhere" downstream.
					if cc := confighashCollector.Load(); cc != nil {
						if rep, cherr := cc.Sweep(context.Background()); cherr != nil {
							log.Printf("confighash: sweep failed: %v (baseline stale until the next tick — the mutation signal degrades to absent, never fabricated)", cherr)
						} else if rep.Errored > 0 || rep.Changed > 0 {
							log.Printf("confighash: swept %d guest(s), %d changed %v, %d errored (config-hash baseline, TG-466)",
								rep.Swept, rep.Changed, rep.ChangedGuests, rep.Errored)
						}
					}
				}
			}()
			log.Printf("estate: periodic topology refresh every %s (with the learned tier)", d)
		} else {
			log.Printf("estate: invalid TG_ESTATE_REFRESH_INTERVAL %q — refresh disabled", iv)
		}
	}

	// The opt-in LibreNMS active-alert pull (TG-344), carved into wireLibrenmsAlertPoll
	// (librenms_alert_poll_wiring.go); pure relocation.
	upstreamProbeSource = wireLibrenmsAlertPoll(c)

	// TG-NATIVE liveness detection (A1 detection latency). Opt-in via TG_PVE_LIVENESS_POLL_INTERVAL: a read-only
	// goroutine polls Proxmox guest status every interval and mints ONE triage per observed running→stopped
	// transition of an operator-allowlisted guest — beating LibreNMS's ~6–11 min device-down push pipeline (the
	// dominant A1-miss cause) by an order of magnitude, through the SAME ingest→StartTriage path so a
	// liveness-sourced incident is indistinguishable downstream from a pushed one (INV-04). It reads with the
	// estate READ pair (TG_PVE_URL / TG_PVE_TOKEN_REF — see pve_liveness_config.go: reading with the actuation
	// lane's WRITE token is what killed this detector when the plane split landed, TG-350), falling back to
	// the TG_PROXMOX_* pair for `both`-plane installs; it NEVER actuates (GET only — mutation stays behind the
	// mode chokepoint) and needs no self-actuation guard (TG only ever STARTS a guest, so a down transition is
	// always a real fault).
	// Deduped by workflow id (REJECT_DUPLICATE); best-effort — a poll error logs and retries, never crashes the
	// worker. OFF by default (interval unset).
	// dbPool is the shared runtime pool, hoisted so later planes (the semantic retrieval index) and the
	// pveLivenessReg is the detector's yield register (TG-350 follow-through); nil until the poller is
	// wired, and its samples() is nil-safe so the scrape emits nothing when the detector is not running.
	var pveLivenessReg *pveLivenessYield
	// pve-liveness poller's ingest-ledger write can both reuse it; nil without a DSN — every consumer
	// nil-checks and degrades honestly. Declared HERE rather than beside the durable-store block below,
	// because the liveness goroutine closes over it and Go scoping is textual: a declaration further down
	// the function is not in scope up here.
	var dbPool *db.Pool

	// PLANE-SCOPED (TG-153): INGEST — it mints triage sessions, so it is withheld from the actuation plane.
	if iv := planeEnv("TG_PVE_LIVENESS_POLL_INTERVAL", ""); iv != "" {
		pvePair, pveHavePair := resolvePVELivenessPair(planeEnv)
		pveGuests, pveGuestKey := resolvePVELivenessGuests(planeEnv)
		if d, err := time.ParseDuration(iv); err == nil && d > 0 && pveHavePair && len(pveGuests) > 0 {
			livenessSrc := pveliveness.New(pvePair.baseURL, pvePair.tokenRef, pveGuests, getenv("TG_PVE_LIVENESS_SITE", ""),
				pveliveness.WithHTTPClient(estateHTTPClient(pvePair.insecure)))
			probeReg.offer("ingest", pveliveness.SourceType, livenessSrc)
			// TG-350 follow-through: the detector publishes its own yield. Seeded with the watched count
			// BEFORE the first tick so the denominator exists even if the loop never runs.
			pveLivenessReg = &pveLivenessYield{}
			pveLivenessReg.watched.Store(int64(len(pveGuests)))
			go func() {
				t := time.NewTicker(d)
				defer t.Stop()
				// Log the detector's projection denominator ONCE (a silent second feeder is indistinguishable
				// from an unwired one — the estate sweep logs its denominator, this fast feed should too, TG-365).
				detectorProjLogged := false
				for range t.C {
					ctx, cancel := context.WithTimeout(context.Background(), d)
					minted, already := 0, 0
					// ONE cycle: fetch → PROJECT the fresh watched-guest states into guest_liveness → THEN
					// dispatch. The projection commits BEFORE dispatch so the triage each envelope starts reads
					// a fresh STOPPED state at correlate + seal time (TG-496); see pve_liveness_poll.go.
					// Nil-safe interface conversion (mirrors feedLiveness at ~1805): guestLivenessStore.Load()
					// is a CONCRETE *db.GuestLivenessStore, nil when no durable pool is armed (a supported
					// no-DSN degrade-honestly config). Boxed straight into the sink INTERFACE it would be a
					// typed-nil that slips past feedGuestLivenessDetector's nil guard and panics on Upsert
					// (TG-496); convert to a genuine nil interface so the projection simply no-ops.
					var livenessSink guestLivenessSink
					if st := guestLivenessStore.Load(); st != nil {
						livenessSink = st
					}
					res, ferr := runLivenessPoll(ctx, livenessSrc, livenessSink,
						func(ctx context.Context, env coreingest.IncidentEnvelope) {
							_, serr := c.ExecuteWorkflow(ctx, client.StartWorkflowOptions{
								ID:                    tg.WorkflowID(env.ExternalRef),
								TaskQueue:             tg.TaskQueueRunner,
								WorkflowIDReusePolicy: enumspb.WORKFLOW_ID_REUSE_POLICY_REJECT_DUPLICATE,
							}, runner.RunnerWorkflow, env)
							if serr != nil {
								var startedErr *serviceerror.WorkflowExecutionAlreadyStarted
								if errors.As(serr, &startedErr) {
									already++ // this guest-down is already being triaged (push or a prior tick) — dedup
									return
								}
								log.Printf("pve-liveness poll: mint triage %s failed: %v", env.ExternalRef, serr)
								return
							}
							minted++
							// Record the ACCEPTED envelope exactly as the HTTP front door does
							// (httpapi.RecordFromEnvelope — ONE constructor for both callers, so the two records
							// cannot drift). Written only on a SUCCESSFUL mint: a detection that opened no
							// investigation is not an accepted alert, and recording it would credit a detection
							// that led nowhere. Append never blocks or fails detection — the alert log is a
							// record, not a gate.
							// dbPool is assigned during startup, well before the first tick; the closure captures
							// the VARIABLE, so it is resolved here rather than at wiring time. A worker with no
							// durable pool simply records nothing — detection still works, it just is not ledgered.
							if dbPool != nil {
								db.NewAlertLogStore(dbPool).Append(ctx, httpapi.RecordFromEnvelope(
									pveliveness.SourceType, env, tg.WorkflowID(env.ExternalRef)))
								// AND the same suppression observation the HTTP front door makes. Without it this
								// detector's alerts were invisible to the shadow measurement — and it is the
								// intake that GROWS, being TG's own fastest detector (~85s vs ~610s mean). A
								// number that silently excludes the source it will increasingly consist of drifts
								// in the one direction nobody is watching for.
								suppressionshadow.New(db.NewAlertHistoryStore(dbPool), log.Printf).
									ObserveAccepted(env.Host, env.AlertRule, env.ReceivedAt)
							}
						})
					if ferr != nil {
						pveLivenessReg.recordFailure(time.Now())
						log.Printf("pve-liveness poll: fetch failed: %v (retry next tick)", ferr)
						cancel()
						continue
					}
					// The projection was refreshed BEFORE the envelopes above were dispatched. Feeding is
					// MEASUREMENT, never a gate (TG-378): a write error is logged with its denominator and the
					// poll continues — the fast-path reader treats stale as unknown and the incident falls back
					// to the normal loop, never a blind heal.
					if res.ProjErr != nil {
						log.Printf("guest liveness: pve-liveness detector-poll upsert failed: %v (projection goes stale — readers treat stale as unknown)", res.ProjErr)
					} else if !detectorProjLogged {
						log.Printf("guest liveness: %d watched-guest state(s) projected from the pve-liveness DETECTOR every %s — the fast feed that keeps the observed-stopped read fresh for the TG-496 deterministic heal and the TG-378 seal gate; the 5-min estate sweep stays the all-guests backstop", res.Projected, d)
						detectorProjLogged = true
					}
					// Recorded on EVERY successful poll, including the all-quiet one the log deliberately
					// stays silent about — that silence is what made six days of nothing unreadable.
					pveLivenessReg.recordSuccess(time.Now(), res.Fetched, minted, already)
					if minted > 0 || already > 0 {
						log.Printf("pve-liveness poll: %d new guest-down triage(s), %d already-firing", minted, already)
					}
					cancel()
				}
			}()
			log.Printf("pve-liveness: guest-liveness pull every %s over %d guest(s) from %s, via the %s credential pair (%s + %s, TLS verification %s per %s) — read-only, mints a triage on running→stopped, beats the ~6–11min LibreNMS device-down push",
				d, len(pveGuests), pveGuestKey, pvePair.name, pvePair.urlKey, pvePair.tokenKey,
				map[bool]string{true: "SKIPPED", false: "enforced"}[pvePair.insecure], pvePair.insecureKey)
		} else if !pveHavePair {
			// Naming the READ pair first is load-bearing on a split deployment: TG_PROXMOX_TOKEN_REF is
			// withheld from the triage plane by design, so an operator who follows the old message sets a key
			// that planeEnv will refuse to hand back, and concludes the detector is broken (TG-350).
			log.Printf("pve-liveness: poll idle — no Proxmox read pair configured. Set TG_PVE_URL + TG_PVE_TOKEN_REF (the estate READ token; this is the pair the triage plane may hold). TG_PROXMOX_BASE_URL + TG_PROXMOX_TOKEN_REF also work on a `both`-plane worker, but that token is withheld from the triage plane")
		} else if len(pveGuests) == 0 {
			log.Printf("pve-liveness: poll idle — no guests to watch. Set TG_PVE_LIVENESS_GUESTS (or TG_PROXMOX_ALLOWED_GUESTS on a `both`-plane worker); an empty list watches NOTHING by design, so that an unrelated guest going down for maintenance never mints a triage")
		}
	}

	// ---- the authlog collector (TG-315): the correlator's first NON-AVAILABILITY witness ----
	//
	// ingest_alert holds 3,167 rows across three source types — librenms, pve-liveness,
	// prometheus-alertmanager — and every one answers "is it up?". core/correlate's cross-source rule keys
	// on DISTINCT source_type, so it has never had a second KIND of witness. Zero rows carry
	// category=security-incident.
	//
	// Admission is the SAME sequence pve-liveness performs, and deliberately so: mint the workflow, then
	// record the accepted alert through httpapi.RecordFromEnvelope (ONE constructor for the HTTP front door
	// and every poller, so the two records cannot drift), then make the suppression-shadow observation
	// without which this source would be invisible to that measurement.
	authlogReg = startAuthlogCollector(planeEnv, authlogServers, authlogRunner,
		func(ctx context.Context, env coreingest.IncidentEnvelope) error {
			_, err := c.ExecuteWorkflow(ctx, client.StartWorkflowOptions{
				ID:                    tg.WorkflowID(env.ExternalRef),
				TaskQueue:             tg.TaskQueueRunner,
				WorkflowIDReusePolicy: enumspb.WORKFLOW_ID_REUSE_POLICY_REJECT_DUPLICATE,
			}, runner.RunnerWorkflow, env)
			if err != nil {
				var startedErr *serviceerror.WorkflowExecutionAlreadyStarted
				if errors.As(err, &startedErr) {
					return nil // already being triaged this window — dedup, not a failure
				}
				return err
			}
			// Written only on a SUCCESSFUL mint: an observation that opened no investigation is not an
			// accepted alert, and recording it would credit a detection that led nowhere.
			if dbPool != nil {
				db.NewAlertLogStore(dbPool).Append(ctx, httpapi.RecordFromEnvelope(
					authlog.SourceType, env, tg.WorkflowID(env.ExternalRef)))
				suppressionshadow.New(db.NewAlertHistoryStore(dbPool), log.Printf).
					ObserveAccepted(env.Host, env.AlertRule, env.ReceivedAt)
			}
			return nil
		})

	// The organization's criticality tier (P0 hosts) is operator-declared config — never hostnames in code.
	// A host on this set is ceilinged at AUTO_NOTICE, never silently AUTO (classifier step 4). An empty set
	// (the default) declares no P0 hosts; the estate graph will supply a criticality attribute once topology
	// readers land, at which point this env set becomes the override/seed rather than the sole source.
	critHosts := hostSet(getenv("TG_CRITICALITY_TIER_HOSTS", ""))
	// The platform's own control-plane services — a restart of these is vetoed to a poll. Declared config.
	selfProtected := selfProtectedMatcher(getenv("TG_SELF_PROTECTED_SERVICES", ""))
	// A predicted estate blast radius at/above this width ceilings the action at AUTO_NOTICE. Declared config.
	blastWide := blastWidthThreshold()
	// The staged-canary allowlist: a (host, op) here is FORCED to POLL_PAUSE so the first mutations require a
	// human vote (spec/001 REQ-009). Declared config (a file: ref, config-not-code); empty ⇒ nothing pinned.
	// A malformed policy is a hard boot error — a silently-dropped pin would let a staged mutation reach AUTO.
	canaryPins, err := risk.LoadCanaryPins(getenv("TG_CANARY_POLL_POLICY_FILE", ""))
	if err != nil {
		log.Fatalf("canary poll policy: %v", err)
	}
	if n := canaryPins.Len(); n > 0 {
		log.Printf("canary poll policy: %d pinned (host,op) rule(s) — forced POLL_PAUSE (human vote); never actuates", n)
	}

	// The durable stores: when a runtime DSN is configured (config-not-code — the DSN carries its own secret
	// env/file refs) the prediction gate and the governance ledger are pgx-backed and survive a restart, else
	// the in-memory oracle twins. Both satisfy their interfaces, so the wiring is identical either way.
	var predStore predict.PredictionStore = predict.NewMemPredictionStore()
	ledger := audit.NewLedger()
	// The OBSERVE-ONLY observability emitter (spec/012, SK observable-by-default): one registry injected into
	// the Runner's activities (runner.Deps.Metrics) so the agent loop, verify, and classify steps record the
	// five-metric agent family + governance-decision counts, and installed as the process-global default the
	// read-only /metrics handler collects. It only counts — it never gates or touches a chokepoint.
	obsRegistry := observe.NewRegistry()
	observe.SetDefault(obsRegistry)
	// Observe EVERY model-gateway call at the boundary (not per-caller), so the offline gate, judge cron,
	// skill generator, calibrator, and agent loop are all covered: tg_model_calls_total{model,outcome} +
	// tg_model_call_seconds_total{model} on /metrics, plus a structured log line for any non-ok/slow call.
	// Set before the gateway is used by any runner below; observe-only, never gates.
	gw.Obs = observe.NewGatewayObserver(obsRegistry, time.Duration(envInt("TG_MODEL_SLOW_CALL_SECONDS", 60))*time.Second, nil)
	var manifestSink runner.ManifestSink           // durable sealed-manifest writer when a DSN is configured, else nil
	var manifestBackfill runner.ManifestBackfiller // lifecycle-label backfiller (approval_choice/verdict), same store
	var agentStepSink tracepkg.AgentStepSink       // scrubbed per-ReAct-cycle transcript writer (spec/020 T-020-8), else nil
	// The GROUND TRUTH behind each step (TG-272). Wired from the SAME pool and in the same breath as the
	// transcript, deliberately: the console citation is useless without it, and the recurring failure in this
	// repo is machinery that gets built while one consumer nobody re-pointed keeps serving the old view.
	var agentStepEvidenceSink tracepkg.AgentStepEvidenceSink
	// NO SEPARATE SINK FOR THE TYPED CLAIM (TG-201) — it needs no wiring here because it has no writer of its
	// own. The claim rides the terminal triage record into session_triage.diagnosis, on the TriageRecord seam
	// already wired below, which is also the column the judge scores and the console reads.
	// The wired-by-construction actuation chain (spec/013) + the durable readers the execute activity uses to
	// reconstruct the governed Request. Constructed only with a DB; nil ⇒ the Runner's execute activity is a
	// no-op (the in-memory oracle path). Even wired, the chain cannot mutate: mutation ships OFF, so Do
	// refuses at GuardMutation — the boot SelfTest only proves the chain is not dark.
	var (
		interceptor *actuate.Interceptor
		// The actuation REGIME ENGINE (spec/017) + its LaneEffect composition seam, hoisted so they wire into
		// the runner Deps below. Constructed after the interceptor's collaborators exist; nil ⇒ the execute
		// activity falls back to the single native-ssh interceptor (behavior-preserving).
		regimeEngine *regime.Engine
		laneEffect   *regime.LaneEffect
		// The deferred-verify PRODUCER seam (TG-122 slice 0): non-nil only when wireActuationRegime armed the
		// async channel (awx launch client present). nil ⇒ handle-returning lanes keep the structural refusal.
		asyncLauncher *regime.AsyncVerify
		// Collaborators captured for LaneEffect's interceptor BUILDER — it must build a per-lane spec/013
		// interceptor with the IDENTICAL wiring the native-ssh interceptor gets, so a routed lane preserves
		// every gate. Assigned where each is constructed in the DB-present block below.
		bEffectLeaf      actuation.Actuator
		bVerdictSink     actuate.VerdictSink
		bExecutionSink   actuate.ExecutionSink
		bGateVerdict     tracepkg.GateVerdictSink
		bGraduation      actuate.GraduationRecorder
		bPreStateSink    actuate.PreStateSink    // TG-58: shared durable pre-mutation state recorder (0102)
		bTargetAdmission actuate.TargetAdmission // TG-81 b2: the ONE shared durable per-target admission store
		// bActuationLimiter is the ONE per-session/per-target actuation-frequency + concurrency governor this
		// worker enforces (TG-166a). Unlike every other collaborator here it is NOT optional — the interceptor
		// builds its own by default — but it MUST be SHARED: the direct native-ssh interceptor and every
		// per-lane interceptor the builder below produces have to count against the SAME window, or the cap
		// silently becomes (lanes × cap) and a loop just alternates lanes. Constructed once, unconditionally,
		// so it exists even on the no-DB boot path.
		bActuationLimiter = actuate.NewActuationLimiter(nil)
		// bLadder is the SAME ladder as bGraduation, kept with its concrete type so the risk classifier can
		// READ a class's level. bGraduation is narrowed to the recorder interface (write path) and cannot answer
		// "has this class earned auto?".
		bLadder        *policy.Ladder
		bPolicyDecider actuate.PolicyDecider
		bPolicyModeNow func() policy.Mode
		// bApproveByFor answers the runner gate's "who may approve this poll?" over the UNAUDITED engine
		// (spec/015 REQ-1516, TG-254). Nil until the engine builds.
		bApproveByFor func(context.Context, runner.ApproveByQuery) []string
		// bApproveByConfigured is the BUNDLE fact that makes the answer above binding: does ANY rule in the
		// active bundle declare an approver? False here (the zero value, and the value that survives a failed
		// engine build) means admission is INERT rather than refusing everyone — see the ruleset load below.
		bApproveByConfigured bool
		manifestReader       runner.ManifestReader
		predReader           runner.PredictionReader
		verdictReader        runner.VerdictReader
		// pendingWriter projects open POLL_PAUSE decisions for the console (REQ-519). Interface-typed so it
		// stays truly nil without a DSN (a nil *db.PendingStore in the interface would defeat the activity's
		// nil check and panic) — nil ⇒ the projection activities are fail-open no-ops.
		pendingWriter persist.PendingWriter
		// The verify-time falsifiability writeback seams (#23/#26): the committed-but-unscored prediction
		// reader + the score writer + the cascade-stats window writer + the verdict writer. Interface-typed so
		// they stay truly nil without a DSN — the scoring loop below is then not started (honest zeros in the
		// grounding scorecard, never a fabricated signal). Measurement only; never mutation-gated.
		falsifyUnscored falsify.UnscoredReader
		falsifyScores   falsify.ScoreWriter
		falsifyCascade  falsify.CascadeStatsWriter
		falsifyVerdicts falsify.VerdictWriter
		// gradCredits is the exactly-once ladder-credit claimer (TG-266/REQ-2804), hoisted so the terminus
		// promote path can consult it. Nil without a DSN: an in-memory ladder dies with the process, so
		// there is no durable streak for a double-credit to corrupt.
		gradCredits *db.GraduationCreditStore
		// escalationStore is the durable dropped-escalation requeue lane, hoisted so the FireDue cron + the
		// reconcile→escalation re-check hand-off share ONE lane; nil without a DSN ⇒ the escalation lane is
		// inert (there is nowhere durable to enqueue a re-check).
		escalationStore *db.EscalationStore
		// workerSealer is the SAME sealer that makes store: refs resolvable below, hoisted so the
		// operator-driven DEK rewrap lane (TG-163) re-keys through the identical key configuration. If the
		// rewrap ever built its OWN sealer it could re-wrap under a key this worker does not read with —
		// the exact "two halves, one key" failure seal.FromEnv was extracted to prevent (TG-275).
		workerSealer *seal.Sealer
	)
	// ★ THE PLANE-SCOPED DATABASE IDENTITY (TG-164). planeDBDSN picks THIS process's DSN: tg_triage's on the
	// triage plane, tg_actuate's on the actuation plane, TG_DB_DSN (tg_runtime) under `both` and as the
	// fallback whenever the plane key is unset. os.Getenv, never the console-override getenv — a database
	// cannot supply the address of the database it lives in (boot_config.go).
	planeDSN, planeDSNWhy := planeDBDSNFromEnv(credentialPlane)
	if dsn := planeDSN; dsn != "" {
		// TG-422 slice 2: a plane DSN of `dyn:<role>` leases its Postgres login PER CONNECTION from the
		// OpenBao database engine (db.ConnectDynamic + Provider.Credentials), instead of resolving one
		// static string. Requires the dyndb wiring above to be armed — a dyn: DSN with the engine OFF
		// refuses the boot rather than guess a static credential (fail closed, no fallback path).
		var pool *db.Pool
		var err error
		if role, isDyn := strings.CutPrefix(dsn, dyndb.Scheme+":"); isDyn {
			if dynProvider == nil {
				log.Fatalf("durable stores: the plane DSN is a dyn: reference but dynamic credentials are " +
					"OFF (TG_DYNDB_ADDR unset) — refusing to boot rather than fall back to a static " +
					"credential (TG-422)")
			}
			pool, err = db.ConnectDynamic(context.Background(), dynDSNTmpl, dynProvider.Credentials(role))
			if err == nil {
				log.Printf("durable stores: plane DSN leases role %q per connection from OpenBao's database "+
					"engine — rotated at max_ttl, revoked at shutdown (TG-422 slice 2)", role)
				dyndb.ArmRotationEviction(dynProvider, role, pool.Reset, log.Printf) // TG-553: evict pooled conns on lease rotation
			}
		} else {
			pool, err = db.Connect(context.Background(), dsn)
		}
		if err != nil {
			log.Fatalf("durable stores: connect %v", err)
		}
		defer pool.Close()
		dbPool = pool
		// TG-378: arm the guest-liveness projection now that a pool exists — every estate sweep from here
		// upserts the pve source's guest power states; absent/stale rows read as UNKNOWN, never stopped.
		guestLivenessStore.Store(db.NewGuestLivenessStore(pool))
		log.Printf("guest liveness: projection ARMED (guest_liveness; upserted from each estate sweep; freshness bound %s)", guestLivenessBound)
		// TG-466 slice 2: arm the confighash Collector now that a pool exists — the estate refresh tick loads
		// this pointer fresh each tick (confighashCollector.Load()) and sweeps the CONFIG-hash baseline into
		// guest_config_baseline. confighashReader stays nil unless TG_PVE_CONFIGHASH_ENABLED armed it above,
		// so this stays a no-op (no Collector ever stored, the tick sweeps nothing) on the ship-dark default.
		if confighashReader != nil {
			confighashCollector.Store(confighash.New(confighashReader, confighashBaselineAdapter{db.NewGuestConfigBaselineStore(pool)}))
			log.Printf("confighash: guest config-hash baseline projection ARMED (guest_config_baseline; swept from the estate refresh tick) — TG-466 slice 2 grounded mutation signal is now populating")
		}
		if pveGuestSource != nil && !estateRefreshArmed {
			log.Printf("guest liveness: WARNING — the estate refresh is NOT armed (TG_ESTATE_REFRESH_INTERVAL unset/invalid), so the projection ages out after the boot prime and every state-preconditioned op-class (start-guest) will REFUSE once stale: fail-closed, and now said out loud rather than structural silence (TG-378)")
		}
		// TG-346: bind the estate snapshot relay's loader now that a pool exists. The relayed plane is
		// 'triage' EXPLICITLY — Latest() by recency alone answers with whichever worker wrote last, which
		// is the defect the plane column ended.
		if estateRelayArmed {
			relayStore := db.NewEstateWriteStore(pool)
			estateRelayLoad.bind(func(ctx context.Context) (estate.Snapshot, time.Time, error) {
				return relayStore.LatestSnapshotForPlane(ctx, string(credential.ProcessPlaneTriage))
			})
		}
		// TG-188: bind the chaos-cascade loader now that a pool exists. The injection ledger (injected_fault) is
		// readable with THIS process's own database identity — no triage secret — so the late-bound SourceChaos
		// reader added to estateSources above starts contributing ground-truth cascade edges on the first
		// post-connect prime. Bound unconditionally: only the triage plane appended the source, so on the
		// actuation plane this closure is simply never called.
		chaosLoad.bind(func(ctx context.Context) ([]estate.ChaosCascade, error) {
			return db.NewAxisReadStore(pool).ChaosCascades(ctx, time.Now().Add(-estate.ChaosEdgeTTL), db.DefaultChaosCascadeWindow)
		})
		// TG-188 organic recovery: bind the refresh goroutine's clear feed to the durable transition log.
		recoveryFeed.bind(db.NewTransitionLogStore(pool).RecoveryEventsSince)
		// TG-109: bind the native-db credential source's row loader now that a pool exists (the source was
		// REGISTERED at startup, INV-17; this is the TG-451 atomic handoff). Every Sync from here re-reads
		// the CURRENT credential_native_rule rows (INV-05); until this line a sync failed closed with
		// "pool not yet connected", retaining the prior converged state.
		nativeRuleStore := db.NewCredentialNativeRuleStore(pool)
		nativeDB.Bind(func(ctx context.Context) ([]nativedb.RuleRow, error) {
			rows, lerr := nativeRuleStore.List(ctx)
			if lerr != nil {
				return nil, lerr
			}
			out := make([]nativedb.RuleRow, 0, len(rows))
			for _, r := range rows {
				out = append(out, nativedb.RuleRow{ID: r.ID, Entry: r.Entry})
			}
			return out, nil
		})
		// TG-481: load the operator-authored OBJECT GROUPS now that a pool exists, and hand the converged
		// set to the credential SyncEngine so Resolve unions them into Target.Groups (spec/016). Additive +
		// fail-closed: a load failure logs and leaves the seam DISARMED (no groups -> resolution unchanged),
		// and an EMPTY store (no groups authored yet) is a no-op. A modest background poll (started below) then
		// re-reads the store on a cadence, so a group authored through the write lane takes effect WITHOUT a
		// worker restart (TG-481 slice 4).
		ogStore := db.NewEstateObjectGroupStore(pool)
		if n, oerr := loadObjectGroupsInto(context.Background(), ogStore, credEngine); oerr != nil {
			log.Printf("object groups: initial load failed: %v -- resolution runs with none (fail-closed, TG-481)", oerr)
		} else {
			log.Printf("object groups: loaded %d group(s) into the credential resolver at boot (TG-481)", n)
		}
		startObjectGroupRefresh(ogStore, credEngine, objectGroupRefreshInterval, worker.InterruptCh())
		// The co-occurrence learner's snapshot restore + periodic persistence (TG-388 face c), carved into
		// wireCoOccurrencePersist (cooccurrence_persist_wiring.go); pure relocation.
		wireCoOccurrencePersist(pool, learner)
		// THE BOOT SELF-CHECK, asked of Postgres rather than of the configuration. Everything about this split
		// is enforced by the database except WHICH DSN this process was handed, and that is enforced by an
		// operator editing a .env. Getting it wrong — a "split" triage worker still pointed at tg_runtime, or
		// worker-actuate pointed at tg_triage — is invisible until an activity fails hours later. So: name the
		// role we actually authenticated as, and say whether it can still write its off-plane tables.
		//
		// It never fails the boot. The fallback posture (tg_runtime on a split plane) is DOCUMENTED and
		// supported — it is what every TG-153 deployment runs until the roles are created — so refusing to
		// start would break the upgrade path this whole design preserves. It is logged as an exposure, not a
		// footnote, which is the same call credential_plane.go makes about a co-holding worker.
		if withheld := planeWithheldTables(credentialPlane); len(withheld) > 0 {
			actx, acancel := context.WithTimeout(context.Background(), 10*time.Second)
			audit, aerr := pool.AuditPlaneWrites(actx, withheld)
			acancel()
			switch {
			case aerr != nil:
				log.Printf("credential plane DB: could not audit this connection's off-plane write privileges (%v) — posture UNKNOWN, not proven (dsn from %s)", aerr, planeDSNWhy)
			case audit.Split():
				log.Printf("credential plane DB: plane=%s connected as %q — DENIED write on all %d off-plane table(s) %v (dsn from %s). A compromised process here cannot forge those rows.", credentialPlane, audit.Role, len(audit.Checked), audit.Checked, planeDSNWhy)
			default:
				log.Printf("credential plane DB: LIVE EXPOSURE — plane=%s connected as %q, which CAN STILL WRITE %d of %d off-plane table(s): %v (dsn from %s). The PROCESS split (TG-153) is in force; the DATABASE split (TG-164) is NOT. Create the plane roles and set the plane DSN — see deploy/postgres-init/01-plane-roles.sh.", credentialPlane, audit.Role, len(audit.Writable), len(audit.Checked), audit.Writable, planeDSNWhy)
			}
		} else {
			log.Printf("credential plane DB: plane=%s holds both queues — no off-plane half to withhold; connected via %s", credentialPlane, planeDSNWhy)
		}
		// ★ THE WORKER CAN NOW READ SEALED SECRETS (TG-275).
		//
		// It could not before, and that was not a small gap. `store:` SecretRefs resolve through
		// config.RegisterStoreResolver, and that was called ONLY in cmd/grounder — so the process that
		// actually USES credentials (host diagnostics, syslog-ng, actuation, the AWX sync) could not
		// resolve a single one of them. A secret written through the console landed in a store the
		// consumer could not open, which is why `sealed_secret` held zero rows on a live deployment:
		// an encrypted secret store, fully built and tested, that nothing could use.
		//
		// seal.FromEnv is the SAME constructor the grounder calls. Two roots, one function — so the two
		// processes cannot end up sealing and unsealing under different keys.
		if sealer, how := seal.FromEnv(config.SecretRef(getenv("TG_SEAL_KEY_REF", "env:TG_SEAL_KEY"))); sealer != nil {
			config.RegisterStoreResolver(seal.StoreResolver(sealer, db.NewSealedSecretStore(pool)))
			workerSealer = sealer
			log.Printf("sealed secrets: store: refs resolvable — %s", how)
		} else {
			// FAIL LOUD, not silent. Unresolved is the correct behaviour without a key, but an operator
			// who configured `store:` refs must learn it HERE rather than from a connector failing far
			// downstream with an empty credential.
			log.Printf("sealed secrets: NO seal backend (set TG_SEAL_TRANSIT_KEY or a usable " +
				"TG_SEAL_KEY_REF) — every store: ref in this worker will fail closed")
		}
		// Now a config store exists, so the live accessors declared above start honouring console
		// overrides. The TTL keeps a Save visible within seconds without putting Postgres on the hot path
		// of the notification lane — the lane whose whole job is to work during an incident.
		liveCfg.Store(newLiveModuleConfig(db.NewCPConfigStore(pool), envDuration("TG_MODULE_CONFIG_TTL", 10*time.Second)))
		pstore := db.NewPredictionStore(pool)
		predStore = pstore
		predReader = pstore
		// Continue the governance chain from its persisted tail, and mirror every new decision to the DB
		// write-through — so the tamper-evident audit trail is unbroken across restarts (INV-19).
		// TG-80 (audit tamper-isolation): the ledger + risk-audit sinks may write through a SEPARATE credential
		// — an INSERT+SELECT-only role on the audit tables, no UPDATE/DELETE — so the runtime pool, which can
		// read/write everything else, cannot tamper the governance spine. The cousin platform airgaps its log
		// into a separate credential DOMAIN; TG's append-only is grant-revocation within one DB, reversible by
		// 0015.down — this closes that gap without a second service. OFF by default (the runtime pool,
		// append-only by grant); ARMED by pointing TG_LEDGER_WRITE_DSN at the airgapped role. A set-but-unusable
		// DSN fails the boot CLOSED rather than silently falling back to the tamperable pool: a governance spine
		// that cannot be written is a reason to refuse to start, never to quietly downgrade its integrity.
		ledgerPool := pool
		if wdsn := strings.TrimSpace(getenv("TG_LEDGER_WRITE_DSN", "")); wdsn != "" {
			ap, aerr := db.Connect(context.Background(), wdsn)
			if aerr != nil {
				log.Fatalf("governance ledger: TG_LEDGER_WRITE_DSN is set but its connection failed — refusing to boot with an unusable airgapped ledger sink rather than fall back to the tamperable runtime pool (TG-80): %v", aerr)
			}
			ledgerPool = ap
			log.Print("governance ledger: airgapped write domain ARMED — ledger + risk-audit writes go through a SEPARATE credential; the runtime pool can no longer UPDATE/DELETE the spine (TG-80)")
		} else {
			log.Print("governance ledger: writes via the runtime pool (append-only by grant); the write-domain airgap is available by pointing TG_LEDGER_WRITE_DSN at an INSERT+SELECT-only role (TG-80)")
		}
		lstore := db.NewLedgerStore(ledgerPool)
		seq, hash, terr := lstore.Tail(context.Background())
		if terr != nil {
			log.Fatalf("governance ledger: read tail %v", terr)
		}
		ledger = audit.NewLedgerFromTail(seq, hash).WithSink(lstore).WithRiskSink(db.NewRiskAuditStore(ledgerPool))
		mstore := db.NewManifestStore(pool)
		manifestSink = mstore
		manifestBackfill = mstore // same pgx store also backfills approval_choice/verdict (spec/020 T-020-4)
		manifestReader = mstore
		agentStepSink = db.NewAgentStepStore(pool) // scrubbed per-cycle transcript (spec/020 T-020-8), observe-only
		evidenceStore := db.NewAgentStepEvidenceStore(pool)
		agentStepEvidenceSink = evidenceStore // screened tool payload behind each step (TG-272), observe-only

		// SEED THE RECON WINDOW FROM THE DURABLE READ RECORD (TG-165). agent_step_evidence is one row per
		// recorded read; without this seed a restart would hand whatever was mid-burst a brand-new hour, and
		// "restart the worker" is not a step an intruder finds difficult. The seed is a FLOOR, not the truth
		// — a probe that finds nothing writes no row — which is the safe direction: it can only ever admit
		// reads a perfect meter would also have admitted. A failed seed is logged and the worker boots with a
		// cold window: refusing to start over an unseedable meter would trade a read bound for an outage.
		if seeded, serr := reconGovernor.SeedFromLedger(context.Background(), evidenceStore); serr != nil {
			log.Printf("recon budget: could not seed the rolling hour from agent_step_evidence (%v) — the worker "+
				"boots with a COLD window, so the first hour after this restart is bounded from zero", serr)
		} else if seeded > 0 {
			log.Printf("recon budget: seeded the rolling hour with %d read(s) recorded in agent_step_evidence — a "+
				"restart does not hand a fresh hour to whatever was already reading", seeded)
		}

		// The agent-step evidence retention sweep (TG-295), carved into wireEvidenceReap
		// (evidence_reap_wiring.go); pure relocation.
		wireEvidenceReap(evidenceStore)

		// ESTATE-SNAPSHOT RETENTION (TG-355), sharing the shape immediately above for the same reasons.
		//
		// estate_snapshot was 84 MB of a 140 MB database on 2026-08-06 — bigger than the next seven tables
		// combined, 6692 rows at 334.6/day, no reaper. Both workers write a full serialized graph every
		// refresh: ~12 KB a row, ~3.9 MB a day, ~1.4 GB a year on a 21.5 GB disk.
		//
		// BOTH PLANES RUN THIS, deliberately. The reaper's predicate ranks per plane and keeps the newest N
		// of EACH, so two sweepers converge on the same retained set rather than fighting; and gating it to
		// one plane would mean a triage-only deployment never reaps, which is the shape of defect this tree
		// keeps finding. The sweep is idempotent by construction — a second call after the first simply
		// deletes 0.
		//
		// Every floor is in the DATABASE (migration 0065): the keep-per-plane clamp, the 24-hour window, the
		// first-of-day sample, and the journal INSERT in the same transaction as the DELETE. Nothing here can
		// widen them, and there is no parameter with which to name a row.
		snapshotKeep := envInt("TG_ESTATE_SNAPSHOT_KEEP_PER_PLANE", db.DefaultKeepPerPlane)
		snapshotReapEvery := envDuration("TG_ESTATE_SNAPSHOT_REAP_INTERVAL", 6*time.Hour)
		snapshotReaper := db.NewEstateSnapshotReapStore(pool)
		go func() {
			t := time.NewTicker(snapshotReapEvery)
			defer t.Stop()
			for {
				sctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
				n, err := snapshotReaper.Reap(sctx, snapshotKeep, db.DefaultSnapshotReapBatch)
				cancel()
				if err != nil {
					log.Printf("estate snapshot: retention sweep failed (non-blocking) — the projection is UNBOUNDED until this succeeds: %v", err)
				} else if n > 0 {
					log.Printf("estate snapshot: reaped %d row(s); kept the newest %d per plane, the first snapshot of each UTC day, and everything from the last 24h; the purge is journalled in estate_snapshot_reap", n, snapshotKeep)
				}
				<-t.C
			}
		}()
		log.Printf("estate snapshot: retention %d row(s) per plane (TG_ESTATE_SNAPSHOT_KEEP_PER_PLANE, database floor %d) plus one per UTC day, sweep every %s, at most %d row(s) per sweep",
			snapshotKeep, db.MinKeepPerPlane, snapshotReapEvery, db.DefaultSnapshotReapBatch)
		vstore := wireVerdictSigning(db.NewVerdictStore(pool))
		verdictReader = vstore
		// The verify-time falsifiability writeback stores (#23/#26): the pgx reader/writer over
		// infragraph_prediction's score columns + the append-only cascade-stats window writer. The verdict
		// writer is the SAME vstore the interceptor uses (ComputeVerdict is the sole author, INV-10). Wiring
		// these is what finally gives the grounding scorecard REAL scored predictions instead of the degenerate
		// zero — the score loop is armed below once a live post-incident observer is also wired.
		fstore := db.NewFalsifiabilityStore(pool)
		falsifyUnscored = fstore
		falsifyScores = fstore
		falsifyCascade = db.NewCascadeStatsStore(pool)
		// SINGLE WRITER PER MEANING (roadmap P2-2): the propose-path scorer writes to prediction_verdict, NOT
		// to action_verdict. Both stores share a Commit signature, so this wiring line is the whole decision —
		// which is the point: the scorer cannot get it wrong, only the composition root can.
		//
		// They shared vstore until migration 0042, and pooling them produced a verified-match rate describing
		// neither: measured live at the split, executed actions ran 85.7% match (24/28) while never-executed
		// predictions ran 44.9% (22/49), reported together as 59.7%. That single number understated actuation
		// accuracy by 26 points, overstated the world model by 15, and — because 23 of the 24 deviations were
		// propose-path — made "TG deviated 23 times" read as TG doing the wrong thing to a machine when nothing
		// had been actuated at all. action_verdict now means exactly one thing and has exactly one writer:
		// core/actuate's interceptor, for actions that really executed.
		falsifyVerdicts = db.NewPredictionVerdictStore(pool)
		// the durable pending-decisions projection (REQ-519): the console reads what this worker writes
		// across the process boundary, so it MUST be the shared pgx store, never in-memory.
		pendingWriter = db.NewPendingStore(pool)

		// The abandoned pending-decision sweep, carved into wireAbandonedDecisionReap
		// (abandoned_decision_reap_wiring.go); pure relocation.
		wireAbandonedDecisionReap(pendingWriter)
		// Wire the actuation interceptor: gate + effect-leaf actuator + ledger + verdict sink. The effect leaf
		// is selected by BuildEffectActuator: the read-only reference adapter by DEFAULT (no SSH host declared
		// — exactly today's posture), or the GATED SSH mutating actuator when an SSH host+identity are
		// operator-declared. Even when the SSH seam is constructed the chain stays triple-fail-closed: mutation
		// OFF (the module reports read-only + refuses every mutating call), an EMPTY unit allowlist by default
		// (no unit resolves), and empty acknowledged/evidence. The runner is the NATIVE in-process crypto/ssh
		// client (host-key-verified against known_hosts, key-only auth) — the distroless worker has no ssh
		// binary to fork, so the old LocalRunner subprocess path could never execute here; the native runner is
		// never reached while the gate is off and fails closed on any missing known_hosts/key. SelfTest is a
		// BOOT GATE: a nil collaborator is a dark control and must not boot (INV-21). Mutation stays OFF — this
		// is the inert #23 seam, not the flip.
		//
		// ★ THE ACTUATION CREDENTIAL ACQUISITION SITE (TG-153, cmd/worker/main.go:1987 in the audit). Every
		// argument below reads through planeEnv, so on the TRIAGE plane they are all "" — the SSH host and
		// identity are empty, BuildSSHActuator returns nil, and BuildEffectActuator falls to the read-only
		// reference adapter that a deployment with no SSH host declared already gets today. The native runner
		// is still passed a value, but a runner holding an EMPTY key reference has nothing to authenticate
		// with and the leaf it would have served was never built.
		//
		// This is the shape the ticket asks for and the reason it asks for it: the alternative — build the
		// runner with the real key, then guard its use with `if plane == actuation` — leaves the estate-
		// mutating private key resident in the address space of the process that reads untrusted alert and
		// host content. Withholding the REFERENCE means config.SecretRef.Resolve is never called, no OpenBao
		// lookup for tg/actuator is ever issued from this process, and the key never exists in this memory.
		// TG-423: SSH CA/signed-cert auth. On the ACTUATION plane, when TG_SSHCA_ADDR is set (read via planeEnv,
		// so EMPTY on the triage plane — the ssh-CA token is never acquired there, the SAME plane-isolation the
		// bare key above relies on), each actuation presents a short-lived OpenBao-signed CERTIFICATE for the
		// actuation key instead of the long-lived bare key: the target trusts it via sshd's TrustedUserCAKeys
		// and the exposure window shrinks to the cert TTL. UNSET (the default) ⇒ the bare-key runner,
		// byte-identical — arming is the operator's estate step (roll the CA into TrustedUserCAKeys). Fail-closed:
		// a misconfigured enabled engine refuses to BOOT rather than fall back to the static key, and a per-Run
		// signing failure fails that actuation closed (never the bare key). The engine holds only a token
		// REFERENCE; it is resolved lazily per signature, so nothing is read on the triage plane.
		var sshActuationRunner sshactuation.Runner
		if sshcaAddr := strings.TrimSpace(planeEnv("TG_SSHCA_ADDR", "")); sshcaAddr != "" {
			sshcaEngine, scErr := sshca.New(sshca.Config{
				BaseURL:  sshcaAddr,
				Mount:    planeEnv("TG_SSHCA_MOUNT", ""),
				Role:     planeEnv("TG_SSHCA_ROLE", ""),
				TokenRef: config.SecretRef(planeEnv("TG_SSHCA_TOKEN_REF", "")),
				CACert:   planeEnv("TG_SSHCA_CA", ""),
			})
			if scErr != nil {
				log.Fatalf("sshca: SSH CA/signed-cert auth is enabled (TG_SSHCA_ADDR) but the engine will not "+
					"construct — refusing to boot rather than fall back to the static actuation key (TG-423): %v", scErr)
			}
			sshActuationRunner = sshactuation.NewNativeRunnerWithCASigner(
				planeEnv("TG_ACTUATION_SSH_KNOWN_HOSTS", ""),
				config.SecretRef(planeEnv("TG_ACTUATION_SSH_KEY", "")),
				sshcaEngine.SignOne)
			log.Print("actuation: SSH CA/signed-cert auth ARMED (TG-423) — actuations present a short-lived OpenBao-signed certificate; unset TG_SSHCA_ADDR to fall back to the static key")
		} else {
			sshActuationRunner = sshactuation.NewNativeRunner(planeEnv("TG_ACTUATION_SSH_KNOWN_HOSTS", ""), config.SecretRef(planeEnv("TG_ACTUATION_SSH_KEY", "")))
		}
		effectActuator := bootstrap.BuildEffectActuator(chokepoint,
			sshActuationRunner,
			bootstrap.EffectActuatorConfig{
				SSHHost:               planeEnv("TG_ACTUATION_SSH_HOST", ""),
				SSHIdentity:           planeEnv("TG_ACTUATION_SSH_IDENTITY", ""),
				AllowedUnitsSpec:      planeEnv("TG_ACTUATION_ALLOWED_UNITS", ""),
				AllowedContainersSpec: planeEnv("TG_ACTUATION_ALLOWED_CONTAINERS", ""),
			})
		log.Printf("actuation: effect leaf = %s (read-only=%v, may_actuate=%v) — inert while the mode is Shadow",
			effectActuator.Capability(), effectActuator.ReadOnly(), chokepoint.MayActuate())
		// spec/020 T-020-7 (REQ-2007): the OBSERVE-ONLY per-gate verdict trail — one ordered row per interceptor
		// gate into interceptor_gate_verdict (append-only). Nil-safe + emit-error-swallowed, so it can never change
		// a gate outcome; it just lights up the tracer's gate-by-gate walk.
		// Capture the effect leaf + verdict sinks for the regime LaneEffect builder (below): a routed lane
		// must get the IDENTICAL wiring this native-ssh interceptor gets. The gate-verdict sink is a single
		// shared instance used by both the direct interceptor and every builder-produced per-lane interceptor.
		bEffectLeaf = effectActuator
		bVerdictSink = vstore
		// Per-EXECUTION recorder (roadmap P2-1). action_verdict is keyed by the content-addressed action_id
		// first-wins, so re-running one operation persisted nothing — 113 executions collapsed into 28 durable
		// outcomes, making "N independent hands-off heals of class X" unrecordable. Shared by the direct
		// interceptor and every builder-produced per-lane interceptor so no actuation lane is dark.
		bExecutionSink, bGateVerdict, bPreStateSink, bTargetAdmission = wireActuationCollaborators(pool)
		interceptor = wireAuthnCompose(actuate.NewInterceptor(chokepoint, effectActuator, ledger).
			WithVerdictSink(vstore).
			WithExecutionSink(bExecutionSink).
			WithGateVerdictSink(bGateVerdict).
			WithPreStateSink(bPreStateSink).
			// The SHARED actuation-frequency governor (TG-166a). The interceptor already builds a default one,
			// so this line is not what arms the control — it is what makes the direct chain and every routed
			// lane count against ONE window instead of one each.
			WithActuationLimiter(bActuationLimiter).
			WithTargetAdmission(bTargetAdmission), credResolver, getenv) // spec/016 REQ-1604 gate 4d2 (ships dark)
		if err := interceptor.SelfTest(); err != nil {
			log.Fatalf("actuation interceptor: boot self-test failed (unwired chain) — refusing to start: %v", err)
		}
		// Phase-2 keystone (REQ-1520/1521): discharge the boot PROOF obligation. The interceptor SelfTest above
		// proved the interception chain wired, so ProvePreflight marks the mode chokepoint's preflight GREEN —
		// the successor to the proof half of the retired actuate.EnableMutation. It does NOT actuate: the mode is
		// still Shadow (MayActuate stays false), so the worker remains read-only. A green preflight only makes a
		// LATER, operator-authorized, audited mode transition into Semi-auto/Full-auto ADMISSIBLE (that transition
		// gates on this same green preflight). The retired TG_MUTATION_ENABLED env flip is gone — there is no
		// env-armed switch; enabling actuation is a mode transition through the policy engine's RBAC-gated,
		// preflight-gated ModeController, never a boot flag. A failed proof fails the boot CLOSED.
		if err := chokepoint.ProvePreflight(interceptor); err != nil {
			log.Fatalf("boot preflight proof REFUSED (chain unwired) — refusing to start: %v", err)
		}
		log.Print("actuation chokepoint: preflight GREEN (chain proven wired) — mode stays Shadow, worker read-only until an operator escalates the mode")
		// THE POLICY PIPELINE MODE-INVARIANT (REQ-1501, spec/015 T-015-5, paradigm-rule 8). The interceptor
		// SelfTest above proved the ACTUATION chain; this proves the DECISION pipeline resolves an IDENTICAL
		// verdict/band/approve_by across all four modes — a mode-dependent Decide would let the mode silently
		// change what the system CONCLUDES, not merely whether it acts. policy.SelfTest runs a self-contained
		// representative engine (in-memory rules, no pool/env), so it is safe at boot and fails ONLY on a
		// genuine violation; like the actuation preflight, a violation fails the boot CLOSED. TG-505: this guard
		// was documented as a boot preflight but had never been wired (present-not-reaching).
		if err := policy.SelfTest(context.Background()); err != nil {
			log.Fatalf("policy pipeline-guard: boot self-test failed (mode-dependent decision pipeline, REQ-1501) — refusing to start: %v", err)
		}
		log.Print("policy pipeline-guard: preflight GREEN (decision pipeline mode-independent across all four modes, REQ-1501)")
		// Publish the worker's TRUE, live actuation posture (spec/012 REQ-1107) so the grounder — a SEPARATE
		// process whose own gate is read-only by construction — reports it honestly on /v1/whoami +
		// /v1/governance instead of its own always-off gate. It upserts the owner-set MODE plus this worker's
		// ACTUAL chokepoint.MayActuate() and effect-leaf Capability() to the single-writer runtime_posture
		// projection and re-publishes on a heartbeat so updated_at stays fresh; the grounder treats a
		// stale/absent row as UNKNOWN, never a false OFF. Publishing NEVER blocks or kills the worker: a
		// write error is logged (like the estate publish), measurement only, never gating. Re-reading
		// MayActuate() each tick means a runtime halt (breaker/POST /halt → ForceShadow) is reflected within
		// one interval. The mode reader binds LATER in boot (the ModeController wiring), so the immediate
		// boot publish carries mode="" — honest "not yet bound", replaced on the first heartbeat after
		// binding; the reader renders "" as unknown, never invents a mode (TG-112).
		postureStore := db.NewPosturePublishStore(pool)
		publishPosture := func() {
			pctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			mode := ""
			if f := policyModeForMetrics.Load(); f != nil {
				mode = (*f)()
			}
			if perr := postureStore.Publish(pctx, PostureComponent(credentialPlane), mode, chokepoint.MayActuate(), effectActuator.Capability()); perr != nil {
				log.Printf("posture: publish failed: %v (grounder treats the stale/absent row as unknown, never a false OFF)", perr)
			}
		}
		publishPosture() // publish the boot posture immediately
		postureInterval := envDuration("TG_POSTURE_PUBLISH_INTERVAL", 30*time.Second)
		go func() {
			t := time.NewTicker(postureInterval)
			defer t.Stop()
			for range t.C {
				publishPosture()
			}
		}()
		log.Printf("posture: worker publishes its live actuation posture every %s (component=%s, may_actuate=%v, effect=%s)", postureInterval, PostureComponent(credentialPlane), chokepoint.MayActuate(), effectActuator.Capability())
		// Publish the estate graph so the grounder's /v1/estate surface serves the same causal graph the
		// gate reasons over (REQ-516). Publishing never blocks or fails triage: a write error is logged.
		estateWriter := db.NewEstateWriteStore(pool)
		publishEstate = func(g *estate.Graph) {
			if g == nil {
				return
			}
			// STAMPED WITH THIS PROCESS'S PLANE (TG-346). Both workers publish here and their graphs differ
			// by two orders of magnitude; without the stamp the reader picked by timestamp alone and got
			// whichever wrote last.
			if err := estateWriter.Publish(context.Background(), g.Export(), len(estateSources),
				string(credentialPlane)); err != nil {
				log.Printf("estate: publish snapshot failed: %v (kept serving prior)", err)
			}
		}
		// TG-346: PRIME the relay before the first publish. The initial Build ran before the pool existed,
		// so on the actuation plane the relay source errored and the graph is still the impoverished
		// pve-only one — refreshing here, with the loader bound, installs the relayed estate at boot
		// rather than leaving the gate on 17 edges until the first TG_ESTATE_REFRESH_INTERVAL tick.
		if estateRelayArmed {
			primeSources := append(append([]estate.EdgeSource(nil), estateSources...), learner.LearnedSource())
			before := estateHolder.Graph().Len()
			_, primeErrs := estateHolder.Refresh(context.Background(), primeSources, estate.WithDefaultEdgeSchema())
			estateSourcesFailed.Store(int64(len(primeErrs)))
			for _, e := range primeErrs {
				log.Printf("estate: relay prime — source %s failed: %v", e.Source, e.Err)
			}
			log.Printf("estate: relay prime — graph %d -> %d edges", before, estateHolder.Graph().Len())
			feedLiveness(context.Background(), "boot prime", true) // TG-378: first projection, denominator logged
		}
		publishEstate(estateHolder.Graph()) // publish the initial build immediately
		// Publish the credential engine's NON-SECRET coverage + sync state so the console's credential view
		// reads what this worker syncs, across the process boundary (migration 0017). Best-effort like the
		// estate publish: a write error is logged, never fatal. NEVER writes a secret (the SyncRun/coverage
		// types carry only counts + non-secret metadata — a source stores references, never values, INV-13).
		credStateStore := db.NewCredentialStateWriteStore(pool)
		publishCredentialState = func(runs []credential.SyncRun, cov []db.CredentialCoverage) {
			pctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			if perr := credStateStore.Publish(pctx, runs, cov); perr != nil {
				log.Printf("credential engine: publish state failed: %v (kept serving prior)", perr)
			}
		}
		// Install the durable credential-resolution audit sink (migration 0018): every Resolve on the shared
		// resolver now appends one append-only, NON-SECRET credential_resolution row (spec/016 REQ-1617). The
		// resolver was built before the pool existed, so the sink is swapped in here (best-effort append — a
		// projection write never fails an authorized investigation, but the fail-closed refusal always holds).
		credResolver.SetSink(db.NewCredentialResolutionWriteStore(pool))
		log.Printf("durable stores: pgx-backed — infragraph_prediction + governance_ledger + session_risk_audit + action_manifest (chain continues from seq %d)", seq)

		// The Policy Engine's DURABLE stores (spec/015 T-015-12, migration 0019): the append-only
		// policy_decision audit sink, the per-op-class graduation ladder store, the single active-mode store,
		// and the active rules-as-data store. Built here (after the pool) mirroring the credential resolver's
		// late-bound sink. The engine is assembled read-only and wired to these durable stores so a policy
		// decision appends one NON-SECRET, append-only policy_decision row + a governance-ledger record
		// (REQ-1518, INV-19). T-015-13 WIRES it: the ModeController becomes the actuation chokepoint's mode
		// authority (BindMode) — the single source of "may actuate?" (the absorbed MutationGate, REQ-1520) — and
		// the AuditedEngine becomes the interceptor's per-action policy authorizer (WithPolicyDecider). The
		// chokepoint is the ModeController's PreflightChecker, so a mode escalation into Semi-auto/Full-auto gates
		// on the green boot preflight. Mutation stays OFF (mode defaults Shadow). The ruleset load is fail-closed:
		// an absent/unreadable ruleset yields the empty RuleSet (every action → the fail-closed default `approve`,
		// never `auto`).
		pctx := context.Background()
		policyRulesets := db.NewPolicyRulesetStore(pool)
		policyGradStore := db.NewPolicyGraduationStore(pool)
		gradCredits = db.NewGraduationCreditStore(pool)
		// Establish the out-of-box curated Semi-auto baseline on a fresh deployment (absent-only + idempotent;
		// never clobbers an operator ruleset or an earned/operator-tuned op-class; mode-gated — Shadow default,
		// so the seed never actuates until an operator escalates the mode). Returns the effective ruleset.
		policyRuleSet := policy.SeedDefaults(pctx, policyRulesets, policyGradStore, log.Printf)
		// ★ THE BUNDLE-LEVEL QUESTION for vote admission (spec/015 REQ-1516, TG-254): has the operator
		// expressed ANY opinion about who may approve a governed action? Answered ONCE, HERE, over the SAME
		// RuleSet value the policy engine is about to be built from — so "the bundle declares approvers" can
		// never disagree with what approveByFor is able to resolve, and so it is a property of the BUNDLE
		// rather than of whichever action happens to be gated.
		//
		// It exists because an empty approver set is AMBIGUOUS, and the two readings have opposite blast
		// radii. On a bundle that declares approvers, "this action's set is empty" means nobody may approve it
		// (fail closed — the safe reading). On a bundle that declares NONE, the same emptiness means the
		// question was never asked, and enforcing it would refuse every vote on every poll — approve and deny
		// alike — until each session times out at `human:timeout` after runner.VoteWait. That is not a
		// stricter control, it is the poll lane bricked invisibly on an estate that actuates.
		policyApproverRules := rulesDeclaringApprovers(policyRuleSet)
		// THE RATIFIED OVERLAY GOES LIVE HERE (TG-227 blockers 2+3). One synchronous load BEFORE the
		// ladder and engine are built — a grant ratified before this boot is in the composed registry
		// before the first decision is taken — then a refresh loop with a kick from the ratify/revoke
		// verbs. The SAME pass arms the per-class promote thresholds the ladder reads below, so the
		// registry snapshot and the graduation bar can never come from different generations of the
		// table (TG-248: WithPerClassThreshold previously had no caller in any composition root).
		overlayRef := newOverlayRefresher(db.NewOpClassRatifiedStore(pool), log.Printf)
		if err := overlayRef.RefreshOnce(pctx); err != nil {
			log.Printf("opclass overlay: boot load failed (%v) — starting on the embedded registry only; "+
				"the refresh loop keeps retrying every %s", err, envDuration("TG_OPCLASS_OVERLAY_REFRESH", time.Minute))
		}
		policyGrad := buildGraduationLadder(policyGradStore, overlayRef.ThresholdFor)
		// TG-177 coherence: the ratify verb resets a re-ratified/renamed class's DURABLE graduation to
		// approve, but that store write bypasses this ladder's per-process cache. Wire the refresher to evict
		// the reset slug from THIS process's enforcement ladder when it (re)admits the row — set BEFORE the
		// loop starts so no pass reads the callback racily, and the loop runs in every enforcement process, so
		// the reset reaches the gate here on the ratify kick and everywhere else within one refresh interval.
		overlayRef.WithLadderEvict(policyGrad.Forget)
		go overlayRef.Run(context.Background(), envDuration("TG_OPCLASS_OVERLAY_REFRESH", time.Minute))
		bGraduation = policyGrad // captured for the regime LaneEffect builder (identical wiring on routed lanes)
		bLadder = policyGrad     // same ladder, concrete type, for the classifier's ungraduated-class read
		// WIRE the graduation ladder's EARN-PATH into the interceptor (spec/013 REQ-1217, spec/015 REQ-1514):
		// AFTER a governed action executes and its post-state VERIFIES, the interceptor records the run outcome
		// to THIS ladder (the SAME one the policy engine READS via GraduatedVerdict). Without this write the
		// ladder dead-locks — no op-class ever records a clean run, so none can graduate from `approve` to
		// `auto` (the durable policy_graduation table stays empty). It is a post-verify WRITE only; the mode
		// mode chokepoint still gates every execute, so no clean run accrues until an operator
		// escalates the mode. The awx-job async lane feeds the SAME ladder via regimeGradSink below, so both the
		// synchronous native-ssh execute path AND the deferred async-verify path advance one ladder. Wired
		// unconditionally (independent of the policy-engine build below): even a fallback posture that executes
		// governed actuations must accrue its earned trust.
		if interceptor != nil {
			interceptor = interceptor.WithGraduationRecorder(policyGrad)
		}
		// The chokepoint is the ModeController's PreflightChecker: a transition INTO Semi-auto/Full-auto is gated
		// on the green boot preflight (REQ-1520). The AuthorityChecker (RBAC) is now WIRED (REQ-1502) — the LAST
		// gate before an owner-present flip: an operator may transition the mode IFF they are flip-authorized (on
		// the TG_MODE_TRANSITION_OPERATORS allowlist, OR — when no allowlist is set — an authenticated LDAP
		// admin-group / static-admin operator, carried as a trusted signal from the AuthAdminSession surface).
		// Wiring this authority ENABLES an operator-invoked transition; it never auto-transitions anything, and
		// the mode still defaults fail-closed to Shadow (REQ-1519).
		modeAuthority := policy.NewModeTransitionAuthority(policy.ParseOperatorAllowlist(getenv("TG_MODE_TRANSITION_OPERATORS", "")))
		policyModeCtl := policy.NewModeController(pctx, db.NewPolicyModeStore(pool), ledger, modeAuthority, chokepoint, log.Printf)
		// BIND the mode authority into the actuation chokepoint: from here the chokepoint's MayActuate consults
		// the single active mode (the one source of truth). Before this bind the chokepoint had no mode ⇒ it was
		// read-only (fail closed), so the construction-to-bind window never actuated. A rebind is refused.
		// Publish the mode. Without this the estate has no way to see which of the four modes it is in,
		// and every rule about actuation has to ASSUME one.
		modeReader := func() string { return policyModeCtl.Current().String() }
		policyModeForMetrics.Store(&modeReader)
		// TG-506: publish the operator-posture warnings (WarnFor) on the scrape surface. The worker's engine
		// carries no execution deny-floor (no WithFloor call), so nil is the honest floor input — WarnFor then
		// reports allow-all rules + Full-auto mode, the permissive conditions actually settable here. Pure
		// read; suppresses nothing, alters no verdict, never touches the constitutional never-auto floor.
		postureWarnProvider := func() []policy.PolicyWarning {
			return policy.WarnFor(policyRuleSet, nil, policyModeCtl.Current())
		}
		policyPostureWarningsForMetrics.Store(&postureWarnProvider)
		if berr := chokepoint.BindMode(policyModeCtl); berr != nil {
			log.Fatalf("actuation chokepoint: bind mode authority failed (fail-closed): %v", berr)
		}
		// Deploy-time initial mode (TG-140): on a FRESH deployment ONLY, seed the operator-declared initial
		// mode (TG_INITIAL_MODE; unset/invalid → Shadow). Absent-only — never overrides an existing persisted
		// mode, so it is a no-op on an established estate; audited to the ledger; and, for an actuating target,
		// still gated on the green preflight proven above (line ~1093). Fail-closed: any refusal stays Shadow.
		if imRaw := strings.TrimSpace(getenv("TG_INITIAL_MODE", "")); imRaw != "" {
			canon := map[string]string{"shadow": "Shadow", "hitl": "HITL", "semi-auto": "Semi-auto", "semi": "Semi-auto", "full-auto": "Full-auto", "full": "Full-auto"}[strings.ToLower(strings.ReplaceAll(imRaw, "_", "-"))]
			if canon == "" {
				canon = imRaw
			}
			if configured, perr := policy.ParseMode(canon); perr != nil {
				log.Printf("mode: TG_INITIAL_MODE=%q is not a valid mode — ignoring, staying Shadow (fail closed): %v", imRaw, perr)
			} else if serr := policyModeCtl.SeedInitialMode(pctx, configured, "TG_INITIAL_MODE"); serr != nil {
				log.Printf("mode: deploy-time initial mode %s not applied — staying Shadow (fail closed): %v", configured, serr)
			} else if policyModeCtl.Current() != policy.ModeShadow {
				log.Printf("mode: seeded deploy-time initial mode %s on a fresh deployment (TG_INITIAL_MODE); actuation still gated by preflight + policy + floor", policyModeCtl.Current())
			}
		}
		// Boot-posture correctness: the boot publishPosture() above ran BEFORE BindMode, so it wrote
		// may_actuate=false regardless of the persisted mode — the console then showed a TRANSIENT Shadow for up
		// to one ticker interval after every restart (an AWX redeploy per merge), misreading an actuating estate
		// as read-only. Re-publish NOW that the mode authority is bound AND the deploy-time initial mode (if any)
		// is seeded, so the console reflects the TRUE live posture immediately. Idempotent + non-gating (a write
		// error is logged, never fatal), so the extra call is safe.
		publishPosture()
		// The operator-invoked transition surface (spec/015 REQ-1502): POST /v1/mode in the grounder starts
		// modetransition.ModeTransitionWorkflow, which runs THIS activity on the bound controller above — so the
		// flip executes on the ONE controller the chokepoint consults (never a split-brain grounder copy), and its
		// ledger record is written by this single-writer process. Mutation stays OFF (mode Shadow) until posted.
		modeTransitionActs = &modetransition.Activities{D: modetransition.Deps{Controller: policyModeCtl}}
		log.Printf("mode-transition RBAC WIRED: %d explicit flip-authorized operator(s) + LDAP-admin-group fallback; POST /v1/mode gated on authority + green preflight; mode stays %s", modeAuthority.AllowlistSize(), policyModeCtl.Current())
		if policyEng, perr := policy.NewEngine(pctx, policyRuleSet); perr != nil {
			log.Printf("policy engine: build failed (%v) — actuation falls back to the mode chokepoint + never-auto floor only (fail closed): %v", perr, perr)
			// bApproveByConfigured stays FALSE and bApproveByFor stays nil, so vote admission is INERT — and
			// that has to be said out loud, because it is a live exposure, not a footnote to the line above.
			// The alternative is worse: with no engine there is nothing to resolve an approve_by from, so
			// enforcing would refuse every vote on every poll while the operator reads only "policy engine
			// build failed" and reasonably assumes the poll lane still works.
			log.Printf("policy: LIVE EXPOSURE — vote admission is INERT because the policy engine did not build, so no approve_by can be resolved: ANY authenticated operator can approve ANY governed POLL_PAUSE action (the pre-TG-254 behaviour, preserved so a failed engine does not ALSO make every poll unvotable). Fix the ruleset/engine build above to arm approver enforcement (REQ-1516).")
		} else {
			// The audited engine appends every decision to the durable append-only policy_decision table (pgx
			// AuditSink). WIRE it as the interceptor's per-action policy authorizer (an INDEPENDENT layer beneath
			// which the mechanical mode chokepoint still gates, REQ-1521). policyModeCtl.Current supplies the
			// active mode carried into each decision's audit.
			// ★ THE RATE GOVERNOR IS ATTACHED HERE (TG-316), and until now it never was.
			// `WithRateGovernor` had exactly ONE caller in the whole tree and it was a spec acceptance test, so
			// `Engine.rateGov` was nil in every production worker and Refine "degraded to the confidence clamp
			// alone". The `"rate_limit": 30` in core/policy/templates/conservative.json — and any operator rule
			// setting rate_limit — read like an armed control and counted nothing. A configured limit that
			// silently counts nothing is worse than no limit: it answers "is this bounded?" with yes.
			//
			// The clamp direction is auto→approve, i.e. toward MORE human involvement, so arming it cannot
			// admit anything that was previously refused — it can only route an over-frequent auto action to a
			// human. That is why this is safe to switch on directly rather than shipped in a warn mode.
			//
			// It is NOT the same control as the actuation limiter (core/actuate/limiter.go, TG-166), and the two
			// compose rather than duplicate: that one counts EFFECTS at the chokepoint immediately before the
			// effect fires and REFUSES; this one shapes VERDICTS at policy-decide time and re-routes.
			policyRateGov := policy.NewRateGovernor(time.Now)
			policyRateGovForMetrics.Store(&policyRateGov)
			// TG-506: the durable admin engine-toggle (spec/015 REQ-1519, "the operator owns the paranoia
			// dial"). DORMANT unless TG_POLICY_ENGINE_TOGGLE is set — default OFF ⇒ no toggle attached ⇒ the
			// AuditedEngine's engine is always enabled, byte-identical to before. When armed, it loads the
			// admin override from the durable store (so an Override set on the grounder admin plane reaches
			// this decision plane) and a 15s refresh loop re-reads it, so a live admin change takes effect
			// without a worker restart. PROPAGATION WINDOW: the enginetoggle workflow is registered on
			// tg.TaskQueueRunner, so the Override activity runs in the runner-polling process; the OTHER worker
			// plane learns of the change only through its own 15s refresh Load — up to ~15s lag to the plane
			// that executes effects (safe: the never-auto floor still clamps throughout). The never-auto floor
			// (INV-09) is unaffected either way: engine-off routes to `approve` (a human), never `auto`. authz
			// (modeAuthority) IS the live gate on THIS process — the worker runs the workflow's activity, which
			// calls Toggle.Override, so the AuthorityChecker is exercised here (the grounder only STARTS the
			// workflow; it never decides).
			var engineToggle *policy.EngineToggle
			if truthyEnv("TG_POLICY_ENGINE_TOGGLE") {
				engineToggle = policy.NewEngineToggle(modeAuthority, ledger).
					WithLogf(log.Printf).
					WithStore(db.NewPolicyEngineToggleStore(pool))
				if err := engineToggle.Load(pctx); err != nil {
					log.Printf("policy engine toggle: initial durable load failed (%v) — starting from the per-mode default; the refresh loop retries", err)
				}
				go func() {
					tk := time.NewTicker(15 * time.Second)
					defer tk.Stop()
					for {
						select {
						case <-pctx.Done():
							return
						case <-tk.C:
							if err := engineToggle.Load(pctx); err != nil {
								log.Printf("policy engine toggle: refresh load failed (%v) — keeping the last-known override", err)
							}
						}
					}
				}()
				log.Printf("policy engine toggle: ARMED (TG-506, REQ-1519) — admin override loaded from the durable store, refreshed every 15s; the never-auto floor (INV-09) still clamps beneath")
			}
			// REQ-1518 (present-not-reaching sweep, TG-80/TG-509): a policy decision must reach the tamper-EVIDENT
			// hash-chained governance ledger, not only the (tamper-resistant) append-only policy_decision table.
			// Tee the durable-row sink with the LedgerAuditSink so every Decide lands in BOTH halves — the
			// persistence half AND the governance-ledger record this comment (and REQ-1518/design.md) already
			// promise. Policy authorization was the ONLY governance-decision class that reached the table but never
			// the ledger chain; the ledger append is best-effort in lockstep with the row (AuditedEngine.emit).
			policyAudited := policy.NewAuditedEngine(policyEng.WithGraduation(policyGrad).WithRateGovernor(policyRateGov),
				policy.NewTeeAuditSink(db.NewPolicyDecisionWriteStore(pool), policy.NewLedgerAuditSink(ledger))).WithLogf(log.Printf).WithToggle(engineToggle)
			// The single-ledger-writer engine-toggle activity runs on the SAME live toggle (nil when unarmed).
			engineToggleActs = &enginetoggle.Activities{D: enginetoggle.Deps{Toggle: engineToggle, ModeNow: policyModeCtl.Current}}
			// Capture the policy authorizer + active-mode reader for the regime LaneEffect builder, so a routed
			// lane's per-lane interceptor consults the SAME policy Decide before its mode chokepoint (no weaker path).
			bPolicyDecider = policyAudited
			bPolicyModeNow = policyModeCtl.Current
			// The FAITHFUL policy packet-tracer decider (TG-105): the SAME composed pieces the interceptor
			// consults, MINUS the two attachments a hypothetical "may I?" must never touch. It is a SEPARATE
			// engine instance, NOT a re-decoration of policyEng: WithGraduation/WithRateGovernor MUTATE their
			// receiver, and policyEng was just mutated to carry the rate governor two lines up — reusing it
			// would ALIAS that stateful runtime budget into every trace (a read-only query would then consume or
			// reflect live rate state). This fresh engine carries graduation (a trace must reflect earned
			// autonomy) but NO rate governor, and is NOT wrapped in the audited engine, so a trace writes no
			// policy_decision row. The honest gap (no rate simulation) rides on every Result.
			if policyTraceEng, terr := policy.NewEngine(pctx, policyRuleSet); terr != nil {
				log.Printf("policy trace: engine build failed (%v) — POST /v1/policy/trace stays 503 (fail closed)", terr)
			} else {
				policyTraceActs = &policytrace.Activities{Decider: policyTraceEng.WithGraduation(policyGrad)}
				log.Printf("policy trace: WIRED (TG-105) — POST /v1/policy/trace runs the bare composed engine (graduation ON, rate governor OFF, no audit write); rate-governor runtime state is NOT simulated")
			}
			// The approve_by seam the runner's gate asks (TG-254): the SAME graduated engine, unaudited, so
			// resolving "who may approve?" does not append a second policy_decision row per proposal. Before
			// this, the /v1/vote path performed NO approver check at all — any authenticated operator could
			// approve any governed action, and policy.MayApprove had zero production callers.
			// credEngine is the spec/016 credential HUMAN plane: it expands a `group:` approve_by entry to its
			// concrete members AT GATE TIME, because the vote admission runs in workflow code and deliberately has
			// no identity backend (see expandApproveBy). Without it a group-spelled approve_by — the only spelling
			// this repo's own examples use — would name NOBODY, while the boot log still called the control wired.
			bApproveByFor = func(ctx context.Context, q runner.ApproveByQuery) []string {
				return approveByFor(ctx, policyEng, credEngine, policyModeCtl.Current, q)
			}
			// ARM (or deliberately do NOT arm) the admission, from the bundle fact resolved at the ruleset load
			// above. Set HERE, three lines from the resolver, because the two must always travel together: a
			// true with no resolver refuses every vote on every poll, which is the exact bricking this guards.
			// CONFIGURED is "at least one rule DECLARES an approver", not "at least one resolves to a person" —
			// a declared-but-unresolvable regime is still an expressed opinion, so it enforces (and shouts).
			bApproveByConfigured = policyApproverRules > 0
			// LOUD IN ALL THREE STATES, because the operator cannot tell them apart from the outside and each
			// one changes what happens to a parked incident:
			//   (a) UNCONFIGURED — no rule declares approve_by. Admission is INERT: any authenticated operator
			//       can approve anything. That is today's behaviour, not a new hole, but it is a LIVE EXPOSURE
			//       and must read as one, with the exact instruction for arming it.
			//   (b) CONFIGURED but unresolvable — rules declare approvers, yet every entry is a group the human
			//       plane names no member for, so the EXPANDED set names no admissible principal. Enforcement
			//       is ON and those polls are UNVOTABLE. Counting "rules that declare approve_by" alone reports
			//       this as healthy — which is how a fail-closed control ships looking wired while admitting
			//       nobody.
			//   (c) CONFIGURED and resolvable — enforcing, with people who can actually vote.
			resolvableRules := 0
			for _, r := range policyRuleSet.Rules {
				if len(r.ApproveBy) == 0 {
					continue
				}
				if approveByNamesAConcretePrincipal(expandApproveBy(r.ApproveBy, credEngine)) {
					resolvableRules++
				}
			}
			switch {
			case policyApproverRules == 0:
				log.Printf("policy: LIVE EXPOSURE — vote admission is INERT: none of the %d rules in the active bundle declares approve_by, so the deployment has never said who may approve anything, and ANY authenticated operator can approve ANY governed POLL_PAUSE action and release it to the estate at mode %s. This is the pre-TG-254 behaviour, held deliberately: enforcing an empty approver set would instead make EVERY poll unvotable (approve and deny alike) until it times out after %s. Each vote admitted this way is recorded on the governance ledger as human:vote-admitted-unconfigured, so the exposure is countable. TO ARM ENFORCEMENT: declare approve_by on the governing rule(s) (grammar {user:NAME | group:NAME}) — admission then refuses every voter outside the set (REQ-1516).",
					len(policyRuleSet.Rules), policyModeCtl.Current(), runner.VoteWait)
			case resolvableRules == 0:
				log.Printf("policy: vote admission ENFORCING but UNVOTABLE — %d of %d rules declare approve_by, so enforcement is armed, but NONE of them resolves to a concrete principal: every entry is a group the credential human plane names no member for. Those polls can be approved by NOBODY and will stand down at the %s vote timeout (fail closed, REQ-1516). Sync the human-plane group, or name approvers as user:NAME entries.",
					policyApproverRules, len(policyRuleSet.Rules), runner.VoteWait)
			default:
				log.Printf("policy: vote admission ENFORCING (REQ-1516) — %d of %d rules declare approve_by (%d resolve to a concrete principal); a vote is admitted ONLY from a member of the poll's approve_by set, and because the bundle declares an approver regime, an action whose own matched rule names no approver can be approved by NOBODY.",
					policyApproverRules, len(policyRuleSet.Rules), resolvableRules)
			}
			// TG-437: the resolvable-rules count above is NAMESPACE-BLIND — it certifies that approve_by names
			// SOME concrete principal, not that the deployment's actual VOTE CHANNEL can supply one. The Matrix
			// notifier authenticates its own approvers (TG_MATRIX_APPROVERS, Matrix MXIDs) and the vote lane
			// signals the raw MXID as the voter, which runner.VoterAdmitted then checks against approve_by. If
			// those two configs live in disjoint namespaces (MXID vs operator login name), every Matrix vote is
			// refused while everything above reads healthy — measured live 2026-08-10. Name it at boot.
			// Normalize with the SAME alias map the inbound vote lane uses (TG-463), so this asks about the
			// identity actually presented for admission. Shared with the ruleset-WRITE path
			// (rulesetwrite.Deps.OnParsed, wired below) so a write that re-strands the approver — the
			// regression that reopened TG-437 — surfaces at write time, not silently until the next boot.
			logMatrixApproverStranding(splitTokens(getenv("TG_MATRIX_APPROVERS", "")),
				parseVoterAliases(getenv("TG_VOTER_ALIASES", "")), policyRuleSet, credEngine, "boot")
			if interceptor != nil {
				interceptor = interceptor.WithPolicyDecider(policyAudited, policyModeCtl.Current)
				// TG-481 (REQ-1618): feed the SAME object-group membership the credential resolver reads
				// (credEngine.GroupsFor unions membership + the operator-authored object groups from one
				// EstateObjectGroupStore) into the policy EvalInput, so a group-scoped policy rule and a
				// group-scoped credential rule consume one definition — never a second.
				interceptor = interceptor.WithObjectGroupResolver(credEngine.GroupsFor)
			}
			log.Printf("policy engine: WIRED into actuation (policy_decision sink + graduation/mode stores, %d rules, active mode %s) — interceptor consults Decide before the mode chokepoint (T-015-13); mutation stays OFF",
				len(policyRuleSet.Rules), policyModeCtl.Current())
		}

		// The Actuation Regime Engine (spec/017, TG-110): the "through which effect channel?" layer. It
		// COMPOSES over the controls above (interceptor, policy, credential, mode chokepoint) and replaces
		// none of them — every lane is an effect leaf beneath the SAME gates. It is WIRED but INERT: each lane
		// is reachable only through the interceptor's Do (the mode chokepoint refuses at Shadow), and the
		// awx-job lane's actuator re-guards the mode at its own leaf. Nothing below transitions the mode,
		// enables actuation, or launches a job at Shadow. The native-ssh lane re-expresses the SAME effect
		// leaf the interceptor already wires (effectActuator); the awx-job lane stays fail-closed unless the
		// operator declares an AWX base URL + a DISTINCT launch token. Constructed here (after the pool +
		// policy ladder) so resolutions/launches/deferred-verdicts can persist to the append-only 0020 tables
		// and a deferred verify can feed the spec/015 graduation ladder.
		// The deferred verifier gets the SAME estate site authority the interceptor and the propose-path
		// scorer wire (estateHolder.Graph().SiteOf over the LIVE refreshable snapshot, spec/002 REQ-107) —
		// closing the last unscoped verdict-author call site. An unseeded estate derives no sites and the
		// authority then excludes nothing (fail closed, behavior unchanged).
		regimeEngine, asyncLauncher = wireActuationRegime(chokepoint, ledger, effectActuator, policyGrad, pool, policyModeCtl.Current().String(),
			func(host string) (string, bool) { return estateHolder.Graph().SiteOf(host) }, probeReg)

		// The skill store (spec/014): boot-import the compiled registry as production rows (idempotent;
		// a compiled UPGRADE supersedes a prior compiled-import row via the audited Transition, but a
		// GRADUATED store row is never displaced), then hand the composer its snapshot reader. Import
		// failure degrades, never blocks boot — composition falls back to the compiled registry anyway.
		skillDB := db.NewSkillStore(pool)
		// TG-489: initialize/verify the distillate tamper chain BEFORE the boot import appends
		// anything. The report line is the boot log's honest statement of corpus integrity; on a
		// non-OK chain store-backed compose refuses (compiled fallback covers triage) and the
		// import below refuses its appends rather than writing around the chain.
		if rep, cerr := skillDB.EnsureChain(context.Background()); cerr != nil {
			log.Printf("skills: distillate chain: %v", cerr)
		} else {
			log.Printf("skills: %s", rep)
		}
		importCompiledSkills(context.Background(), skillDB, ledger)
		skillRows = skillDB.ProductionRows
		skillWriteActs = &skillwrite.Activities{D: skillwrite.Deps{Store: skillDB, Ledger: ledger}}
		// The SAME durable, chain-continued ledger: an adoption and the skill transition beside it land on
		// one chain, which is what makes "why does the leaf accept this target?" answerable in order.
		manifestDB := db.NewWorldManifestStore(pool)
		manifestWriteActs = &manifestwrite.Activities{D: manifestwrite.Deps{
			Loader: manifestDB, Store: manifestDB, Ledger: ledger,
		}}
		// The earned op-class lane, on the SAME chain-continued ledger: a ratification and the manifest
		// adoption that made its target reachable land in order on one chain, which is what makes "why is
		// this command allowed to run on that host?" answerable at all (spec/028 T-028-7).
		opClassCandidateDB := db.NewOpClassCandidateStore(pool)
		opClassVerbActs = &opclassratify.Activities{D: opclassratify.Deps{
			Loader:  opClassCandidateDB,
			Store:   opClassCandidateDB,
			Ledger:  ledger,
			Overlay: opClassOverlayBackend{s: db.NewOpClassRatifiedStore(pool)},
			Ladder:  db.NewPolicyGraduationStore(pool),
			Export:  embedExporter{},
			// The post-write kick: an operator's ratify/revoke is live in the composed registry within
			// one immediate pass instead of one TTL (TG-227 blocker 2's convergence half).
			Refreshed: overlayRef.Kick,
		}}
		// Config + sealed-secret writes (task #27 Phases C+D): the SAME durable, chain-continued
		// ledger; the LAW clamp is re-validated inside the activity (the authority).
		configWriteActs = &configwrite.Activities{D: configwrite.Deps{
			Ledger: ledger, Config: db.NewCPConfigStore(pool), Secrets: db.NewSealedSecretStore(pool),
			// The DEK rewrap lane (TG-163). Both fields or neither: with no seal backend workerSealer is
			// nil and RewrapSecretsActivity refuses (ErrRewrapUnavailable) instead of reporting a
			// successful no-op over a store it never touched — the report that would get an operator to
			// retire a Transit key version that is still in use.
			Rewrap: sealRewrapStore{s: db.NewSealedSecretStore(pool)}, Sealer: workerSealer,
			// TG-277: log every governed write's per-step latency. This lane had NO latency observability,
			// so a 15s activity timeout on the credential-onboarding path named nothing and the defect was
			// filed against the wrong step. Unconditional rather than threshold-gated: an administrator
			// makes a handful of these writes per install, so the line is cheap, and a threshold would once
			// again leave nothing behind on the run that mattered.
			Observe: logConfigWriteLatency,
		}}
		// The active-ruleset write lane (spec/015 REQ-1503, TG-104): the SAME durable, chain-continued ledger
		// and the SAME active-ruleset store the policy engine loads from — so a replacement validated + ledgered
		// here is the exact document the next Engine.Decide reads. The document is re-validated inside the
		// activity (ParseRuleSet, the authority); a malformed ruleset never becomes the active policy.
		rulesetWriteActs = &rulesetwrite.Activities{D: rulesetwrite.Deps{Store: policyRulesets, Ledger: ledger,
			// TG-437: run the Matrix-approver namespace cross-check on every ruleset write — the SAME
			// logMatrixApproverStranding the boot check runs — so a write that re-strands the approver is
			// surfaced at write time, not silently until the next boot re-runs the boot check.
			OnParsed: func(rs policy.RuleSet) {
				logMatrixApproverStranding(splitTokens(getenv("TG_MATRIX_APPROVERS", "")),
					parseVoterAliases(getenv("TG_VOTER_ALIASES", "")), rs, credEngine, "ruleset-write")
			},
		}}
		// The native-rule write lane (TG-109, spec/016 REQ-1610): the SAME durable, chain-continued ledger
		// as every governed write, and the SAME store the registered native-db sync source reads — so a rule
		// validated + ledgered here is exactly what the next sync serves into resolution. The entry is
		// re-validated inside the activity (ParseRules, exactly one rule — the authority); a malformed row
		// never lands where it would fail every subsequent sync.
		nativeRuleActs = &nativerule.Activities{D: nativerule.Deps{Store: db.NewCredentialNativeRuleStore(pool), Ledger: ledger}}
		// TG-481: the object-group write activity, same single-writer worker + governance ledger.
		objectGroupActs = &objectgroup.Activities{D: objectgroup.Deps{Store: db.NewEstateObjectGroupStore(pool), Ledger: ledger}}
		skillTrials = skillDB
		skillVersionByID = skillDB.GetVersion
		// The finalizer arms the post-graduation regression watch (REQ-1310) — skillDB is also the pgx
		// WatchStore over skill_watch (migration 0010).
		skillTrialActs = &skilltrial.Activities{D: skilltrial.Deps{Trials: skillDB, Store: skillDB, Ledger: ledger, Watch: skillDB}}
		// The judge spine (task #26): the Runner records a compact session_triage row at each terminal
		// outcome (REQ-1106), and the 2-hourly judge cron scores it into session_judgment — the rows
		// ArmScores/JudgedSessionRate already query — then feeds the regression watch. A demotion
		// escalates into the durable escalation queue (the human surface).
		triageDB := db.NewTriageStore(pool)
		triageRecord = triageDB.RecordTriage
		triageMarkCleared = triageDB.MarkCleared // axis A3: the confirmed-clear follow-up mark (migration 0039)
		// axis A3 DENOMINATOR: the triage insert is first-write-wins, so a vote-paused session records its
		// row before it executes and keeps mutated=false. Back-filled at the terminus (see MarkMutated).
		triageMarkMutated = triageDB.MarkMutated
		// The earned-catalog evidence seam (spec/028 REQ-2802, Stage 2). The adapter derives the cluster
		// identity from the OBSERVED op-class/op — never from anything the model declared about itself —
		// and writes the append-only journal row plus the live observing candidacy. Both writes are
		// idempotent by key, so an activity retry can never inflate the incident count that later decides
		// whether an operator is ASKED to grant a capability.
		opclassCandidates := db.NewOpClassCandidateStore(pool)
		recordProposalOccurrence = func(ctx context.Context, occ runner.ProposalOccurrence) error {
			key := opclasscat.CandidateKey(occ.OpClass, occ.Op, nil)
			row := opclasscat.Occurrence{
				CandidateKey: key, ExternalRef: occ.ExternalRef, Host: occ.Host, Target: occ.Target,
				Op: occ.Op, OpClass: occ.OpClass, Rationale: occ.Rationale, UndoSketch: occ.UndoSketch,
				Confidence: occ.Confidence, EvidenceIDs: occ.EvidenceIDs,
				ActorEvidence: occ.ActorEvidence, Outcome: "proposed:shadow", ObservedAt: occ.ObservedAt,
			}
			if err := opclassCandidates.RecordOccurrence(ctx, row); err != nil {
				return err
			}
			return opclassCandidates.UpsertObserving(ctx, key, row)
		}
		// The classifier's prior-verdict band (spec/001 REQ-015, TG-223). The window is operator-declared
		// config-not-code; the default is the predecessor's own bound on how long a verdict is still
		// decision-relevant (reconcile-completed-sessions.py `--very-old-h`, 48h). It is a RECENCY bound only —
		// the long-horizon memory is the graduation ladder, which demotes the class on a deviation and requires
		// N consecutive verified-clean runs to re-earn auto with no time bound at all.
		priorVerdictWindow := envDuration("TG_PRIOR_VERDICT_WINDOW", 48*time.Hour)
		priorVerdicts = priorVerdictReader(wireVerdictVerification(db.NewPriorVerdictStore(pool)), priorVerdictWindow)
		log.Printf("classify: prior-verdict band ARMED — a target carrying a durable %s-recent same-rule-family DEVIATION is polled (spec/001 REQ-015); rule families fold through core/knowledge.CanonicalRule; an absent or unreadable verdict leaves the classification unchanged", priorVerdictWindow)
		// The incident CORRELATION stage (TG-169). It replaces `Correlated = severity == critical` — a
		// property of ONE alert standing in for a claim about the RELATIONSHIP between alerts, which had
		// 81% of live incidents (2,434 of 2,995 admitted alerts are critical) asserting they span multiple
		// systems, and, worse, sent a genuine multi-host cascade built of WARNINGS to the cheapest reasoning.
		//
		// The SPAN is operator-declared config-not-code, because how fast failures propagate is a property
		// of the estate, not of the word "cascade" (the cluster thresholds themselves are constants in
		// core/correlate for exactly that reason — a knob able to lower them is a way back to "everything is
		// correlated"). The default matches the falsifiability window's own cascade bound: minutes, not
		// hours — a host alerting an hour after another is a coincidence, not a propagation.
		correlationSpan := envDuration("TG_CORRELATION_WINDOW", 10*time.Minute)
		correlationStore := db.NewCorrelationStore(pool)
		correlationWindow = func(ctx context.Context, at time.Time) (correlate.Window, error) {
			obs, err := correlationStore.Window(ctx, at, correlationSpan)
			if err != nil {
				return correlate.Window{}, err
			}
			return correlate.Window{Span: correlationSpan, Observations: obs}, nil
		}
		execClassRecord = db.NewExecClassStore(pool).Record
		// TG-385/TG-376: the durable cluster identity every member of one storm JOINs, so a detected cascade
		// collapses to ONE investigation (the elected causal subject) instead of one session per member.
		clusterJoin = db.NewAlertClusterStore(pool).Join
		log.Printf("correlate: CASCADE COLLAPSE ARMED (TG-385/TG-376) — correlated members share a durable alert_cluster identity (migration 0085) and the causal election picks ONE subject to investigate; the rest attach as evidence and open no session")
		log.Printf("execclass: CORRELATION stage ARMED over a %s window on ingest_alert (TG-169) — Correlated is now cross-source/cross-host evidence, not `severity == critical`; every routing decision is recorded with its classifier inputs in exec_class_decision (migration 0058). An unreadable window falls back to the pre-TG-169 severity rule and is marked degraded on the record", correlationSpan)
		// get-incident-history (read-only agent tool): TG's OWN prior sessions on the alerting host, so a
		// recurring incident is RECOGNIZED (the predecessor's biggest correct_diagnosis lever) instead of
		// re-derived from scratch every time. Same-condition matching folds by rule FAMILY inside the tool
		// through the one authority (core/knowledge.CanonicalRule); this wiring only adapts the pgx read.
		// DB-gated by construction: no DSN ⇒ no durable history ⇒ the tool is absent (an inert surface that
		// always answered "no history" would teach the agent to stop asking).
		for _, tl := range incidenthistory.New(incidentHistoryReader(db.NewIncidentHistoryStore(pool))) {
			if err := tools.RegisterFrom("history", tl); err != nil {
				log.Fatalf("register agent tool %s (fail-closed): %v", tl.Name(), err)
			}
		}
		log.Printf("agent: registered the read-only get-incident-history tool — prior same-host/same-family sessions (outcome, op-class, confirmed clear, conclusion) from the durable triage record")
		// get-tracker-history (read-only agent tool): the SHARED incident corpus, which predates TG and which
		// the predecessor has been writing to for its whole production life. get-incident-history closes the
		// recall gap over TG's OWN weeks of sessions; this closes the CORPUS gap. An asymmetric corpus in a
		// head-to-head measures deployment age, not design quality — the same class of confound as running the
		// arms on different models. Registered ONLY when a tracker is actually configured: a tool that always
		// answered "no prior incidents" would teach the agent to stop asking AND silently restore the
		// asymmetry it exists to remove.
		//
		// Keyed on the CAPABILITY, not on the vendor. This block previously resolved the tracker surface
		// at the literal youtrack source type and then type-asserted the concrete *youtrack.Module, so a
		// site running ServiceNow — a tracker fully implementing the four-verb contract — fell to the else
		// arm and logged "no tracker configured", which was FALSE. TG then ran on its own weeks of session
		// history while that site's own incident record, often years of it, sat one API call away. An
		// established estate's ticket archive is the richest source of local knowledge available on day
		// one: it is how the engineers already working there solved this exact fault, in their words.
		trackerHistories := map[string]tracker.History{}
		trackersConfigured := 0
		for _, cp := range moduleReg.Capabilities() {
			if cp.Surface != modules.SurfaceTracker || !cp.Enabled {
				continue
			}
			trackersConfigured++
			regn, rerr := moduleReg.Resolve(modules.SurfaceTracker, cp.SourceType)
			if rerr != nil {
				log.Printf("agent: tracker %s enabled but failed to resolve (%v) — its incident history is NOT readable", cp.SourceType, rerr)
				continue
			}
			if h, okH := regn.Adapter.(tracker.History); okH {
				trackerHistories[cp.SourceType] = h
			}
		}
		switch {
		case len(trackerHistories) > 0:
			// Every history-capable tracker, merged. A site running ServiceNow for ITSM and YouTrack for
			// engineering work has its record split across both; reading one is reading half the memory.
			multi := tracker.NewMultiHistory(trackerHistories)
			for _, tl := range trackerhistory.New(trackerHistoryReader(multi)) {
				if err := tools.RegisterFrom("history", tl); err != nil {
					log.Fatalf("register agent tool %s (fail-closed): %v", tl.Name(), err)
				}
			}
			log.Printf("agent: registered the read-only get-tracker-history tool over %d tracker(s): %s — prior incidents and their HUMAN discussion (writes are refused unless the backend's write flag is set)",
				multi.Len(), strings.Join(multi.Sources(), ", "))
		case trackersConfigured > 0:
			// A tracker IS configured; it simply cannot be searched. That is a different fact from having
			// no tracker, and saying "none configured" here is what hid the ServiceNow gap for as long as
			// it existed.
			log.Printf("agent: get-tracker-history ABSENT — %d tracker(s) configured but none implements the history capability (adapters/tracker.History); TG runs on its own session history only",
				trackersConfigured)
		default:
			log.Printf("agent: get-tracker-history ABSENT — no tracker configured; TG runs on its own session history only")
		}
		escalationStore = db.NewEscalationStore(pool)
		skillJudgeActs = &skilljudge.Activities{D: skilljudge.Deps{
			Model:  gw,
			Store:  triageDB,
			Watch:  skillDB,
			Skills: skillDB,
			Ledger: ledger,
			// The judge reads the LIVE causal graph (TG-202) — the Holder, not a snapshot, so a topology
			// refresh reaches the estate_grounded dimension without a restart. Read-only: the judge asks the
			// graph whether the cause a diagnosis names can reach the host that alerted, and writes nothing
			// to it. Until this line, core/judge had no estate reference at all and a diagnosis blaming a
			// hypervisor the alerting guest does not run on scored exactly like a correct one.
			Estate: estateHolder,
			Escalate: func(ctx context.Context, ref, reason string) error {
				_, err := escalationStore.Enqueue(ctx, ref, 0, time.Now().UTC())
				return err
			},
		}}
		// The flywheel CREATION half (spec/014 REQ-1314): the daily generate -> offline-admit ->
		// trial-start cron. skillDB is the FlywheelStore + MeansReader (the rolling per-dimension judged
		// means over each production version's composing sessions) + TrialStore; the gateway is the
		// generator's Completer; the offline gate scores candidate-vs-production on the skill's own recent
		// judged incidents via the SAME shared judge (skillgen.OfflineRunner — the honest lighter check;
		// the sealed holdout is never read). GENERATE-ONLY and competence-plane: it changes agent prompt
		// content through the audited draft->trial->production state machine and never touches the estate
		// (mutation stays OFF). The generation threshold, sample floor, window, trial shape and the per-run
		// generate/admit caps (TG-63: worst-regressed K skills drafted + oldest J drafts admitted per run,
		// so a global-low dimension can never blow the activity budget) plus the per-trial ARM cap (TG-65:
		// top-K admitted candidates by offline delta, so the arm count and StartTrial's traffic bar stay
		// bounded and a trial can start at bootstrap traffic) are config-not-code (TG_SKILL_GEN_* /
		// TG_SKILL_TRIAL_* / TG_SKILL_OFFLINE_*).
		offlineCfg := skillgen.DefaultOfflineConfig()
		offlineCfg.Window = envDuration("TG_SKILL_GEN_WINDOW", offlineCfg.Window)
		offlineCfg.DiscoveryLimit = envInt("TG_SKILL_OFFLINE_DISCOVERY_LIMIT", offlineCfg.DiscoveryLimit)
		offlineCfg.RegressionSlack = envFloat("TG_SKILL_OFFLINE_REGRESSION_SLACK", offlineCfg.RegressionSlack)
		offlineCfg.MinIncidents = envInt("TG_SKILL_OFFLINE_MIN_INCIDENTS", offlineCfg.MinIncidents)
		// The pre-filter runs on the low-latency "fast" tier by default (REQ-1120 grounding): the "primary"
		// reasoning flagship (~50s, sometimes >the 120s HTTP timeout, on a judge-sized prompt) makes the gate's
		// sequential per-incident calls time out and admit NOTHING. Both arms share the tier so the delta is
		// unaffected. Operator may set it back to "primary" (timeout risk) via TG_SKILL_OFFLINE_MODEL.
		offlineCfg.Model = getenv("TG_SKILL_OFFLINE_MODEL", offlineCfg.Model)
		// LESSON flywheel (REQ-1312): the OPT-IN resolved-incident source. Wired ONLY when BOTH the target skill
		// and its judge dimension are configured — otherwise nil (dormant, the default), leaving the eval-failure
		// flywheel unchanged. When on, ESCALATED incidents draft skill improvements; generate-only; this lane never actuates.
		var lessonSource skillstore.NotableIncidentSource
		if ls, ld := getenv("TG_SKILL_LESSON_SKILL", ""), getenv("TG_SKILL_LESSON_DIMENSION", ""); ls != "" && ld != "" {
			lessonSource = db.NewNotableIncidentStore(pool, ls, ld, envInt("TG_SKILL_LESSON_MAX", 20))
			log.Printf("skillgen: LESSON flywheel ON (opt-in) — escalated incidents draft improvements to skill %q on dimension %q; generate-only; never actuates", ls, ld)
		} else {
			log.Printf("skillgen: LESSON flywheel OFF — set TG_SKILL_LESSON_SKILL + TG_SKILL_LESSON_DIMENSION to have resolved (escalated) incidents draft skill improvements (opt-in)")
		}
		// TG-52 caution feed (part 4): the judge's verbal comments on FAILING sessions (scored low on the
		// dimension - the same failures the caution lane captures) also draft skill improvements, so the
		// flywheel learns from failures, not only from escalations and regressed means. Opt-in + dormant by
		// default; CombineNotableSources folds it alongside the escalation source (either, both, or neither),
		// so generateLessons draws from every wired failure feed. Generate-only; this lane never actuates.
		if cs, cd := getenv("TG_SKILL_CAUTION_SKILL", ""), getenv("TG_SKILL_CAUTION_DIMENSION", ""); cs != "" && cd != "" {
			maxScore := envFloat("TG_SKILL_CAUTION_MAX_SCORE", 2.0)
			cautionSource := db.NewCautionCommentStore(pool, cs, cd, maxScore, envInt("TG_SKILL_CAUTION_MAX", 20))
			lessonSource = skillstore.CombineNotableSources(lessonSource, cautionSource)
			log.Printf("skillgen: CAUTION feed ON (opt-in, TG-52) - sessions scored <=%.1f on dimension %q draft improvements to skill %q carrying the judge's comment; generate-only; never actuates", maxScore, cd, cs)
		}
		skillGenActs = &skillgen.Activities{D: skillgen.Deps{Creation: skillstore.CreationDeps{
			Store:   skillDB,
			Means:   skillDB,
			Trials:  skillDB,
			Ledger:  ledger,
			Model:   skillgen.NewPromptCompleter(gw),
			Runner:  skillgen.OfflineRunner{Model: gw, Store: skillDB, Incidents: triageDB, Cfg: offlineCfg},
			Lessons: lessonSource,
			Cfg: skillstore.CreationConfig{
				Threshold:             envFloat("TG_SKILL_GEN_THRESHOLD", skillstore.DefaultGenThreshold),
				MinSamples:            envInt("TG_SKILL_GEN_MIN_SAMPLES", skillstore.DefaultGenMinSamples),
				Window:                envDuration("TG_SKILL_GEN_WINDOW", 14*24*time.Hour),
				MinSamplesPerArm:      envInt("TG_SKILL_TRIAL_MIN_SAMPLES", 30),
				MinLift:               envFloat("TG_SKILL_TRIAL_MIN_LIFT", 0.2),
				PThreshold:            envFloat("TG_SKILL_TRIAL_P", 0.05),
				TrialDuration:         envDuration("TG_SKILL_TRIAL_DURATION", 14*24*time.Hour),
				FillHorizon:           envDuration("TG_SKILL_TRIAL_FILL_HORIZON", skillstore.DefaultFillHorizon),
				MaxGenSkillsPerRun:    envInt("TG_SKILL_GEN_MAX_SKILLS", skillstore.DefaultMaxGenSkillsPerRun),
				MaxAdmitPerRun:        envInt("TG_SKILL_OFFLINE_MAX_ADMIT_PER_RUN", skillstore.DefaultMaxAdmitPerRun),
				MaxCandidatesPerTrial: envInt("TG_SKILL_TRIAL_MAX_CANDIDATES", skillstore.DefaultMaxCandidatesPerTrial),
			},
		}}}
	} else {
		log.Printf("durable stores: in-memory (no TG_DB_DSN) — predictions + ledger do not survive restart")
	}

	// The armed mutation breaker (Phase-2 readiness review §4.B.3): a post-execution DEVIATION verdict or a
	// chain-integrity gap trips it, and at the threshold (config-not-code, default 1 for the first canary) it
	// FORCES the mode to Shadow in-process (chokepoint.ForceShadow, the absorbed gate.Disable) — the runtime
	// kill the review found missing. It is bound to the (final) governance ledger so an auto-halt is hash-chained
	// like every other decision (INV-19), and attached to the interceptor. INERT under Shadow: Do refuses at the
	// mode chokepoint before it ever executes, so no verdict is produced and the breaker is never touched today.
	// The chokepoint satisfies safety.ShadowForcer — the breaker→kill wire runs through the single source of truth.
	//
	// CROSS-PROCESS (design-wisdom #3): the breaker is backed by the DURABLE pgx store when a DB pool exists, so a
	// trip persists to the shared mutation_breaker_state row (migration 0021) and every sibling worker reads that
	// OPEN state before it actuates — a deviation trip in one worker force-Shadows all of them (the read side is
	// the interceptor's REQ-1210 gate + MutationBreaker.Tripped). Without a DB (an in-memory worker / CI) it falls
	// back to the in-process MemStore fast path — single-worker safe; never actuates regardless. The durable store is
	// the source of truth for the system-wide kill; a store error fails CLOSED (State/Tripped read OPEN).
	var breakerStore breaker.Store = breaker.NewMemStore()
	if dbPool != nil {
		breakerStore = db.NewBreakerStore(dbPool)
		log.Print("mutation breaker: backed by the DURABLE cross-process store (mutation_breaker_state) — a trip force-Shadows every sibling worker")
	} else {
		log.Print("mutation breaker: no DB pool — backed by the in-process store (single-worker; a trip does NOT cross to siblings)")
	}
	mutationBreaker, mbErr := safety.NewMutationBreaker(chokepoint, breakerStore, mutationBreakerThreshold(), ledgerTripRecorder{l: ledger})
	if mbErr != nil {
		log.Fatalf("mutation breaker: arm failed (fail-closed): %v", mbErr)
	}

	// ★ ARM THE PRODUCTION MODEL-PATH BREAKER (TG-221, PORT-FIDELITY-AUDIT finding #24). CONSTITUTION.md:130
	// promises named, observable, persisted circuit breakers on the model-gateway / judge / RAG calls. The
	// machine existed and two lanes consumed it (mutation, cost) — but every production model call went
	// through *model.Gateway with NO breaker, and the guarded per-rung litellm module has no production
	// constructor, so the ported control was never exercised. During a gateway flap the judge cron and the
	// skill generator therefore hammered a dead upstream unbounded.
	//
	// The guard goes on the GATEWAY, not on each caller, for the same reason the CallObserver does: every
	// production caller shares this one *Gateway, so a chokepoint here cannot be bypassed by adding a caller.
	// Assigning the field here (like gw.Obs above) reaches the deps constructed earlier too — they all hold
	// the same pointer, and the worker is still single-threaded at boot.
	//
	// It shares the SAME durable, cross-process store as the mutation breaker (mutation_breaker_state, keyed
	// by name), so a trip is visible to every sibling worker and survives restart — "persisted state", not a
	// per-process counter. One breaker per model TIER (model-<name>): a dead judge tier does not
	// short-circuit a healthy agent tier.
	//
	// DEGRADED MODE (explicit, spec/011 REQ-1010): an OPEN circuit returns a typed breaker_open *ModelError
	// wrapping breaker.ErrOpen — never an empty string with a nil error. The agent loop stops with
	// OutcomeStop and proposes nothing, so the ACTUATION-relevant path fails CLOSED; the judge cron halts its
	// batch with a red activity and leaves every session UNMARKED, so the judging path fails LOUD and no
	// empty scorecard or silently-unjudged session is ever produced. A breaker-STORE error fails OPEN (the
	// call proceeds) and is logged — losing breaker persistence must never block a healthy gateway.
	gw.Breakers = model.NewBreakers(breakerStore,
		breaker.WithThreshold(envInt("TG_MODEL_BREAKER_THRESHOLD", 3)),
		breaker.WithCooldown(envDuration("TG_MODEL_BREAKER_COOLDOWN", 60*time.Second)),
		breaker.WithHalfOpenSuccesses(envInt("TG_MODEL_BREAKER_HALF_OPEN_SUCCESSES", 1)))
	gw.Breakers.Degraded = func(name string, err error) {
		log.Printf("model breaker %s: state store unavailable (%v) — the guard FAILS OPEN (call allowed); "+
			"model calls are unbounded until the store recovers", name, err)
	}
	log.Printf("model breaker armed on the PRODUCTION gateway path (threshold %d, cooldown %s, half-open successes %d) "+
		"— per-tier named breakers over the %s; an open circuit fails CLOSED for actuation (no proposal) and LOUD for judging (no empty scorecard)",
		envInt("TG_MODEL_BREAKER_THRESHOLD", 3), envDuration("TG_MODEL_BREAKER_COOLDOWN", 60*time.Second),
		envInt("TG_MODEL_BREAKER_HALF_OPEN_SUCCESSES", 1), breakerStoreKind(dbPool))

	// ★ ARM THE JUDGE-DEATH DEAD-MAN (TG-222, PORT-FIDELITY-AUDIT #15). The governance monitors could
	// MEASURE a dead judge and could stop nothing — a warning, not a control. This is the actuator: a named
	// breaker over the SAME shared store, tripped by either monitor on a confirmed death, consulted by the
	// trial finalizer before any judged evidence becomes a graduation. Its read FAILS CLOSED, so a store we
	// cannot read halts accrual rather than graduating on a judge whose health is unobservable.
	judgeDeadMan, jdErr := coregov.NewJudgeDeadMan(breakerStore, govHaltRecorder{l: ledger})
	if jdErr != nil {
		log.Fatalf("judge-death dead-man: arm failed (fail-closed): %v", jdErr)
	}
	if skillTrialActs != nil {
		// GRADUATION is the judged-accrual choke point: a candidate skill promoted to production on the
		// strength of a judge's scores. A halted dead-man refuses the whole finalize pass, loudly.
		skillTrialActs.D.JudgeHealth = judgeDeadMan
		log.Print("judge-death dead-man: WIRED to the skill-trial finalizer — a confirmed dead judge REFUSES the graduation pass (no skill graduates on unverified judgment)")
	}
	if interceptor != nil {
		interceptor = interceptor.WithMutationBreaker(mutationBreaker)
	}
	log.Printf("mutation breaker armed (threshold %d) — trips a deviation/chain-gap to chokepoint.ForceShadow; inert while mode is Shadow", mutationBreakerThreshold())

	// Wire the breaker RECOVERY (spec/015 REQ-1523): bind the re-armer into the live ModeController (reached via
	// the mode-transition activity's Controller — the SAME *policy.ModeController the chokepoint consults) so an
	// owner-gated escalation into an actuating mode clears a deviation breaker a prior trip left durably open.
	// This closes the "one trip permanently kills actuation" gap: the trip (breaker→Shadow) and the recovery
	// (escalation→breaker-closed) are now symmetric, both owner-gated, both ledgered. Bound only when the live
	// controller + the durable breaker both exist; a controller-less / breaker-less boot skips it (the breaker
	// is inert there anyway).
	if modeTransitionActs != nil && modeTransitionActs.D.Controller != nil && mutationBreaker != nil {
		modeTransitionActs.D.Controller.BindBreakerRearmer(breakerRearmer{mb: mutationBreaker, ledger: ledger})
		log.Print("mutation breaker: re-arm WIRED to the mode chokepoint — an owner-gated escalation into Semi-auto/Full-auto re-arms a tripped breaker (spec/015 REQ-1523); a trip is recoverable, not a permanent kill")
	}

	// Wire the regime LaneEffect composition seam (spec/017 REQ-1702) so the execute activity dispatches
	// THROUGH the regime engine (SelectLane → LaneEffect → a per-lane spec/013 interceptor) instead of the
	// single hardcoded native-ssh leaf. The builder constructs each per-lane interceptor with the IDENTICAL
	// collaborators the native-ssh interceptor above gets — same mode chokepoint + verdict sinks + graduation
	// recorder + policy decider + mutation breaker, from the SAME captured instances under the SAME conditionals
	// — so a routed lane is never a weaker path than the direct one. ★ This builder is the SINGLE SOURCE of the
	// per-lane wiring and MUST stay in lock-step with the direct interceptor construction above: the boot
	// SelfTest below asserts only the REQUIRED chain (chokepoint + leaf + ledger), so an accidentally-dropped
	// OPTIONAL collaborator (an audit sink / earn hook) would NOT fail it — but the mode chokepoint beneath
	// STILL fails closed, so the routed path can only ever lose an audit/earn hook, never gain permission.
	// Wired only when the DB-present boot built BOTH the effect leaf and the regime engine; otherwise laneEffect
	// stays nil and the execute activity uses the single native-ssh interceptor (behavior-preserving).
	if bEffectLeaf != nil && regimeEngine != nil {
		interceptorBuilder := func(leaf actuation.Actuator) *actuate.Interceptor {
			ic := actuate.NewInterceptor(chokepoint, leaf, ledger)
			// UNCONDITIONAL, and first: every lane must count against the SAME actuation-frequency window as the
			// direct chain (TG-166a). A per-lane private budget would make "3 restarts per target per 10 minutes"
			// mean 3-per-lane, and a loop that alternated lanes would never be throttled at all. There is no
			// `if != nil` guard here on purpose — bActuationLimiter is constructed unconditionally, and a nil
			// argument would be ignored by the seam anyway (the default limiter stays), so the control cannot
			// go dark on a routed lane the way an optional audit hook can.
			ic = ic.WithActuationLimiter(bActuationLimiter)
			ic = ic.WithTargetAdmission(bTargetAdmission) // TG-81 b2: nil-safe seam; a skipped lane is one a loop routes around
			if bVerdictSink != nil {
				ic = ic.WithVerdictSink(bVerdictSink)
			}
			if bExecutionSink != nil {
				ic = ic.WithExecutionSink(bExecutionSink)
			}
			ic = ic.WithPreStateSink(bPreStateSink) // TG-58: nil-safe seam; a lane without it captures nothing
			if bGateVerdict != nil {
				ic = ic.WithGateVerdictSink(bGateVerdict)
			}
			if bGraduation != nil {
				ic = ic.WithGraduationRecorder(bGraduation)
			}
			if bPolicyDecider != nil {
				ic = ic.WithPolicyDecider(bPolicyDecider, bPolicyModeNow)
				// TG-481 (REQ-1618): the regime-lane interceptor consumes the same shared object-group
				// membership as the credential resolver (credEngine.GroupsFor) — one definition, no second.
				ic = ic.WithObjectGroupResolver(credEngine.GroupsFor)
			}
			if mutationBreaker != nil {
				ic = ic.WithMutationBreaker(mutationBreaker)
			}
			return wireAuthnCompose(ic, credResolver, getenv) // spec/016 REQ-1604 gate 4d2 on every routed lane (ships dark)
		}
		// Fail closed: the builder must produce a fully-wired chain (SelfTest asserts every REQUIRED collaborator).
		if serr := interceptorBuilder(bEffectLeaf).SelfTest(); serr != nil {
			log.Fatalf("actuation regime: LaneEffect interceptor builder self-test failed (unwired chain) — refusing to start: %v", serr)
		}
		laneEffect = regime.NewLaneEffect(interceptorBuilder)
		log.Printf("actuation regime ROUTING wired (spec/017 REQ-1702): the execute activity now dispatches through the regime engine — an SSH target routes to the native-ssh lane's IDENTICAL effect chain; other lanes fail closed until configured; mutation stays OFF")
	}

	// The COST/BUDGET spend guard + $-ceiling breaker (spec/013 REQ-1211..1215): the INDEPENDENT sibling of
	// the mutation breaker. It accrues an approximate USD cost for every model completion (approx tokens × a
	// per-model TG_COST_RATE_<model>_PER_1K rate) into DURABLE, cross-process day (UTC) + session accumulators,
	// and when the daily budget (TG_COST_DAILY_BUDGET_USD) or a session ceiling (TG_COST_SESSION_CEILING_USD)
	// is exceeded it TRIPS — force-Shadow (the same kill wire), a 'cost:breaker-trip' ledger note, and a shared
	// OPEN state (migration 0023) so every sibling worker force-Shadows on its next completion. It is wired by
	// WRAPPING the model gateway the agent calls (cost.MeteringCompleter) — the cleanest hook, right where TG
	// already sees the request+response text, so no runner/interceptor code changes to meter spend. It NEVER
	// enables actuation and never weakens the mutation breaker/floor/chokepoint (it only ADDS a spend halt).
	//
	// FAIL-OPEN (deliberate, documented — the inverse of the mutation breaker's fail-CLOSED): the cost breaker
	// guards SPEND, not a safety floor, so an unreadable cost store degrades to "no enforcement" (never a halt)
	// and is LOGGED loudly. A cost-store outage must not halt legitimate ops. Under Shadow the force-Shadow is a
	// no-op (nothing to halt), so — like the mutation breaker — the HALT is inert today; unlike it, the guard
	// still ACCRUES under Shadow (a read-only investigation spends tokens), so it can trip and record now.
	//
	// DISABLED when unconfigured (0/absent budgets AND no rate): the gateway is left un-wrapped — zero overhead,
	// zero behavior change. The daily/session ceilings default to 0 = disabled (a budget guard that is not set
	// must never block work).
	agentModel := agent.Completer(gw)
	var costAcct *cost.Accountant
	costCfg := readCostConfig()
	if costCfg.Enabled() {
		var costStore cost.Store = cost.NewMemStore()
		if dbPool != nil {
			costStore = db.NewCostStore(dbPool)
		}
		acct, cerr := cost.New(costStore, costCfg, chokepoint, costLedgerTripRecorder{l: ledger}, cost.WithLogf(log.Printf))
		if cerr != nil {
			log.Fatalf("cost breaker: arm failed (fail-loud at construction): %v", cerr)
		}
		costAcct = acct
		agentModel = cost.NewMeteringCompleter(gw, costAcct)
		durability := "in-process store (single-worker; a trip does NOT cross to siblings)"
		if dbPool != nil {
			durability = "DURABLE cross-process store (cost_accrual + cost_breaker_state, 0023) — a trip force-Shadows every sibling worker"
		}
		log.Printf("cost breaker armed — daily_budget=$%.2f session_ceiling=$%.2f per_actuation=$%.4f default_rate=$%.4f/1k rates=%d model(s); backed by the %s; FAIL-OPEN (spend guard, not a safety floor) — inert halt while mode is Shadow, still accrues",
			costCfg.DailyBudgetUSD, costCfg.SessionCeilingUSD, costCfg.PerActuationUSD, costCfg.DefaultRate, len(costCfg.Rates), durability)
	} else {
		log.Print("cost breaker: no TG_COST_* rate/budget configured — gateway left un-wrapped (cost tracking DISABLED, honest no-op)")
	}

	agentModel = wireTier3Export(agentModel) // Tier-3 LLM-observability export (REQ-2020), DARK by default — tier3_export_wiring.go
	log.Print(wireGroundnet())               // groundnet federation posture + standing-check (spec/021, TG-128), DARK by default — groundnet_wiring.go

	// Boot-time credential SyncAll + optional scheduled re-sync (TG-109) — wireCredentialSync (credential_sync_wiring.go).
	credentialSyncOne := wireCredentialSync(credEngine, credSources, credCoverage, publishCredentialState)

	// The estate-context investigation tool: the agent's read-only window into the causal graph (upstream /
	// blast radius / common-cause siblings), bound to the holder so every invocation sees the freshest
	// refresh. This is what makes the triage skill's cascade discipline mechanically satisfiable — without it
	// the agent is told to probe "related hosts" it has no way to name.
	for _, tl := range estatetools.New(estateHolder.Graph) {
		if err := tools.RegisterFrom("estate", tl); err != nil {
			log.Fatalf("estate tool %s must register read-only: %v", tl.Name(), err)
		}
	}

	// TG-39: the estate-wide log-CORRELATION tool. When OpenObserve is configured (TG_OPENOBSERVE_URL — the
	// same endpoint the exporter ships to), register the read-only `correlate-logs` tool. It expands an
	// incident host to its blast-radius neighbours from the estate graph and searches the OpenObserve log
	// INDEX across all of them in one bounded query — the cross-host correlation the single-host syslog-ng
	// point tools cannot do (a firewall emits ~131 MB/day, so TG never becomes a raw-syslog destination; the
	// logs are shipped to OpenObserve and this reads the index). Config-not-code: an absent TG_OPENOBSERVE_URL
	// leaves the reader nil and the tool structurally unregistered — no tool, no error — exactly like the
	// exporter and the syslog-ng tools. The stream/host-field knobs resolve through the store-resolving getenv
	// (TG-265) so a value an operator saves in the console binds; blank ⇒ the connector's sane defaults. The
	// reader is offered to the probe registry so its bounded-search self-test answers the openobserve TEST
	// button (the exporter's own stream-list probe was never wired). Mutation stays OFF — this is read-only.
	if ooReader := openobserve.NewReader(
		getenv("TG_OPENOBSERVE_URL", ""),
		config.SecretRef(getenv("TG_OPENOBSERVE_TOKEN_REF", "env:OPENOBSERVE_TOKEN")),
		openobserve.WithStream(getenv("TG_OPENOBSERVE_LOG_STREAM", "")),
		openobserve.WithHostField(getenv("TG_OPENOBSERVE_HOST_FIELD", "")),
	); ooReader != nil {
		ooTools := openobserve.NewCorrelateTools(ooReader, estateHolder.Graph)
		for _, tl := range ooTools {
			if err := tools.RegisterFrom("estate", tl); err != nil {
				log.Fatalf("register agent tool %s (fail-closed): %v", tl.Name(), err)
			}
		}
		// The reader (not the exporter Module) carries the search-path self-test the correlate tool uses;
		// offering it wires that probe to the console's openobserve TEST button (last-offer-wins).
		probeReg.offer("observability", openobserve.SourceType, ooReader)
		log.Printf("agent: registered %d read-only OpenObserve correlate-logs tool(s) over stream %q (host field %q)",
			len(ooTools), ooReader.Stream(), ooReader.HostField())
	}

	// The retrieval plane: a corpus of prior resolved incidents the agent is seeded with as precedent
	// (config-not-code — an operator-exported history via TG_KNOWLEDGE_FILE, until a knowledge store feeds it).
	// Empty/absent ⇒ no retriever ⇒ the agent investigates from the incident alone.
	var retriever knowledge.Retriever
	var knowledgeHolder *knowledge.Holder
	// syncEmbed folds the current corpus into the semantic vector index (best-effort, in the background —
	// an index/embedding failure NEVER blocks a corpus write). A no-op until the semantic plane is wired.
	syncEmbed := func() {}
	// The boot wiring manifest: what the composition root ACTUALLY bound, derived from the values it
	// bound rather than from what any config said was enabled.
	// Install the module configuration keys, derived from each module's published descriptor.
	// Without this the control-plane registry holds ZERO module keys, so a console write of a
	// connector setting is rejected as unknown — and cmd/worker referenced cpconfig not at all, so
	// the write was inert in both directions. core/ does not import modules/, so the keys are
	// PUSHED in here rather than pulled from the catalog by the safety core.
	cpconfig.SetModuleKeys(catalog.ConfigKeys())
	wiringManifest := wiring.New()
	// The hostdiag seam, recorded where the manifest exists (the tools are built far earlier). Bound when
	// the allowlist armed them; declared dark, with the consequence spelled out, when it did not.
	if len(hostDiagTools) > 0 {
		wiring.Bind(wiringManifest, wiring.SeamHostDiag, hostDiagTools)
	} else {
		wiring.Absent[[]agent.Tool](wiringManifest, wiring.SeamHostDiag, wiring.Because{
			Reason: "TG_HOSTDIAG_DEPLOYMENTS is empty, so no host-diagnostics tools are registered",
			Consequence: "the agent cannot read the alerting host at all: every resource and service alert " +
				"is grounded on LibreNMS alone, and a session that could have named the failing unit " +
				"stands down instead",
			Owner: "@ncpjfuzl", Ticket: "TG-271", Expiry: time.Date(2026, time.November, 1, 0, 0, 0, 0, time.UTC),
		})
	}
	// The syslog-ng read seam, recorded here for the same reason (the tools are built at the top of main).
	if len(syslogTools) > 0 {
		wiring.Bind(wiringManifest, wiring.SeamSyslogRead, syslogTools)
	} else {
		wiring.Absent[[]agent.Tool](wiringManifest, wiring.SeamSyslogRead, wiring.Because{
			Reason: "TG_SYSLOGNG_DEPLOYMENTS is empty, so no syslog-ng investigation tools are registered",
			Consequence: "the agent has no device-log window: firewall, switch and router incidents are " +
				"triaged from the alert summary alone, never from the device's own syslog",
			Owner: "@ncpjfuzl", Ticket: "TG-297", Expiry: time.Date(2026, time.November, 2, 0, 0, 0, 0, time.UTC),
		})
	}
	corpusPath := getenv("TG_KNOWLEDGE_FILE", "")
	// The MAINTAINED corpus (corpusPath, worker-written) is unioned with the read-only bootstrap SEED
	// (TG_KNOWLEDGE_SEED_FILE, tracked + deploy-synced) at every load — see knowledge_corpus.go. The split
	// is what makes runtime learning SURVIVE a deploy: the deploy overwrites tracked files (the seed) but
	// never the untracked maintained corpus.
	seedPath := getenv("TG_KNOWLEDGE_SEED_FILE", "")
	// TG-519 Slice C — tamper-ENFORCEMENT for the maintained precedent corpus, the owner-armed ESCALATION of
	// TG-510's evidence-only witness. When armed (TG_CORPUS_ENFORCE), every corpus load verifies the maintained
	// corpus against its latest knowledge-corpus witness BEFORE composing it into trusted retrieval; a corpus
	// that is tampered — OR that cannot be verified at all (no witness store, unreadable witnesses, no witness
	// yet, an unparseable file) — is DROPPED, and the union composes from the SEED corpus alone (see
	// enforcedCorpusPath + corpus_enforcer.go). This is the deliberate INVERSE of TG-510's fail direction:
	// evidence fails safe-WARN (never blocks), enforcement fails safe-DROP (a false verify degrades retrieval to
	// the seed — annoying but SAFE — because a MISSED tamper reaching the agent's trusted context is the real
	// danger). DISARMED ⇒ corpusEnforce is nil and the load path is byte-identical to TG-510 (the maintained
	// corpus always composes). Enforcement verifies against the SAME witness TG-510 records, so it is only
	// meaningful with TG_CORPUS_APPEND_ONLY also armed (that keeps the witnesses fresh); armed without it, the
	// maintained corpus will fail verification after the first unwitnessed write and be dropped — warned below.
	var corpusEnforce *corpusEnforcer
	if truthyEnv("TG_CORPUS_ENFORCE") {
		switch {
		case corpusPath == "":
			log.Printf("corpus enforce: TG_CORPUS_ENFORCE set but TG_KNOWLEDGE_FILE is empty — no maintained corpus to gate; enforcement inert (the seed is already the whole corpus)")
		case dbPool == nil:
			// Armed but nowhere to read witnesses ⇒ every load is unverifiable ⇒ fail-CLOSED drop to seed-only.
			corpusEnforce = &corpusEnforcer{sink: nil}
			log.Printf("!!! corpus enforce: TG_CORPUS_ENFORCE set but no durable store (TG_DB_DSN unset) — the maintained corpus cannot be verified, so enforcement FAILS CLOSED and will DROP it from trusted retrieval (seed-only). Arm TG_DB_DSN + TG_CORPUS_APPEND_ONLY to restore maintained precedent.")
		default:
			corpusEnforce = &corpusEnforcer{sink: db.NewAnchorStore(dbPool).Scoped(knowledge.CorpusAnchorDomain)}
			log.Printf("corpus enforce: ARMED — the maintained knowledge corpus is verified against its witness at every load; a corpus that cannot prove itself is DROPPED from trusted retrieval and composition falls back to the seed alone (TG-519 Slice C, enforcement, fail-CLOSED — the inverse of TG-510 evidence's fail-safe-WARN)")
			if !truthyEnv("TG_CORPUS_APPEND_ONLY") {
				log.Printf("!!! corpus enforce: WARNING — TG_CORPUS_ENFORCE is armed but TG_CORPUS_APPEND_ONLY is NOT, so no writer records fresh witnesses; the maintained corpus will fail verification after the first write and be DROPPED. Arm TG_CORPUS_APPEND_ONLY so every write witnesses the corpus.")
			}
		}
	}
	// loadCorpus parses the seed∪maintained union into a retriever, or nil on error (keep the last good
	// corpus). Function-scoped so every corpus WRITE path (writeback / lessons merge / decay prune) reloads
	// the UNION after writing — never the maintained-only set, which would silently evict the seed from the
	// novelty gate until a restart. TG-519: the maintained path is routed through enforcedCorpusPath so every
	// reload RE-VERIFIES it — a mid-run out-of-band tamper is caught at the next reload and dropped, not just
	// warned.
	loadCorpus := func() *knowledge.LexicalRetriever {
		return loadKnowledgeCorpus(seedPath, enforcedCorpusPath(corpusEnforce, corpusPath, log.Printf), log.Printf)
	}
	// TG-510 Slice A — tamper-EVIDENCE for the maintained precedent corpus. When armed
	// (TG_CORPUS_APPEND_ONLY), every write through persistCorpus records a HEAD anchor of the just-written
	// corpus into the append-only ledger_anchor store (the SAME witness-over-time primitive the governance
	// ledger uses, TG-515) under the knowledge-corpus DOMAIN — a witness the recording principal cannot
	// rewrite (tg_runtime holds INSERT+SELECT but no UPDATE/DELETE, migration 0092). Because every writer
	// RE-READS the file as `existing`, a WRITE-TIME verify runs first (corpusWitness.detectOnWrite): it catches
	// an out-of-band edit present in `existing` BEFORE the write re-witnesses (and would otherwise LAUNDER) it.
	// A periodic verify (wired far below) is the standing net for a tamper no write has yet touched. The
	// witness is shared with the AWX ingest lane (worker_awxplaybooks_ingest.go) so that lane's writes to the
	// same file are witnessed too, not a bypass. EVIDENCE-ONLY and fail-safe: it can only WARN, never block
	// retrieval (enforcement is a later owner-armed slice); DISARMED ⇒ corpusEvidence is nil and persistCorpus
	// is byte-for-byte the old inline write (no chain, no Record, no verify).
	var corpusEvidence *corpusWitness
	if truthyEnv("TG_CORPUS_APPEND_ONLY") {
		switch {
		case dbPool == nil:
			log.Printf("corpus anchor: TG_CORPUS_APPEND_ONLY set but no durable store (TG_DB_DSN unset) — corpus tamper-evidence DISABLED (nowhere to record witnesses)")
		case corpusPath == "":
			log.Printf("corpus anchor: TG_CORPUS_APPEND_ONLY set but TG_KNOWLEDGE_FILE is empty — no maintained corpus to witness; tamper-evidence DISABLED")
		default:
			corpusEvidence = newCorpusWitness(dbPool, envInt("TG_CORPUS_ANCHOR_WINDOW", audit.DefaultAnchorWindow))
			log.Printf("corpus anchor: witnessing the maintained knowledge corpus HEAD on every write into the append-only ledger_anchor store (domain %q), with a WRITE-TIME verify closing read-merge-write laundering — external tamper-evidence (TG-510 Slice A, evidence-only)", knowledge.CorpusAnchorDomain)
		}
	}
	// persistCorpus is the SINGLE chained-write chokepoint for the maintained precedent corpus. Armed, it (1)
	// VERIFIES the freshly-read `existing` against the latest witness BEFORE writing — the write-time limb that
	// closes read-merge-write laundering; (2) writes the corpus atomically (temp+rename); (3) records a HEAD
	// witness of exactly what it wrote. Every maintained-corpus write site passes its just-read `existing` and
	// the `merged`/`kept` result. Disarmed ⇒ just the atomic write (byte-identical to before).
	persistCorpus := func(existing, merged []knowledge.Incident) error {
		if corpusEvidence != nil {
			corpusEvidence.detectOnWrite(existing)
		}
		if err := knowledge.WriteCorpusFile(corpusPath, merged); err != nil {
			return err
		}
		if corpusEvidence != nil {
			corpusEvidence.record(merged)
		}
		return nil
	}
	// TG-52 Reflexion caution lane: a SEPARATE corpus (TG_CAUTION_FILE) of prior attempts on a signature that
	// did NOT verify clean — the failed/deviated/unconfirmed trajectories lessons.Lesson drops. Its own Holder,
	// never unioned with or merged into the precedent corpus; nil when unset (the agent sees precedent only).
	// Populated by appendCautions (below) from the SAME resolved feed the precedent lessons merge reads.
	cautionPath := getenv("TG_CAUTION_FILE", "")
	// HYGIENE (the whole point of TG-52): the caution lane must be a SEPARATE file from the precedent corpus.
	// If an operator points both env vars at the same file, a failed trajectory would land in the precedent
	// corpus the novelty gate and retriever read — exactly the poisoning the lane exists to prevent. Refuse it
	// and disable the lane (fail closed) rather than share the file.
	if cautionPath != "" && cautionPath == corpusPath {
		log.Printf("caution: TG_CAUTION_FILE == TG_KNOWLEDGE_FILE (%s) — REFUSING to share the precedent corpus; caution lane DISABLED (a failed trajectory must never enter the precedent corpus)", cautionPath)
		cautionPath = ""
	}
	cautionHolder := newCautionHolder(cautionPath, log.Printf)
	if corpusPath != "" {
		// TG-519: the boot holder loads through the SAME enforcement gate as every reload — a corpus that was
		// tampered while the worker was down (or one that cannot prove itself) is dropped to seed-only at boot,
		// not admitted into the first session's trusted retrieval.
		knowledgeHolder = newKnowledgeHolder(seedPath, enforcedCorpusPath(corpusEnforce, corpusPath, log.Printf), log.Printf)
		retriever = knowledgeHolder
		// The SEMANTIC channel of the retrieval plane (spec/012 REQ-1110/REQ-1111, TG-40): a query
		// embedding against the pgvector index over knowledge_embedding (migration 0013), RRF-fused with
		// the lexical channel. Strictly additive and fail-open: no embed model, no durable store, or no
		// embedded rows ⇒ EXACTLY the lexical behavior above; a per-query embed/search failure degrades
		// that query to lexical. Embeddings are computed best-effort by a bounded backfill sweep — never
		// fabricated, never blocking a corpus write. All knobs are config-not-code (TG_EMBED_*).
		embedModel := getenv("TG_EMBED_MODEL", "")
		switch {
		case embedModel == "":
			log.Printf("semantic retrieval: disabled — no embed model configured; lexical only")
		case dbPool == nil:
			log.Printf("semantic retrieval: disabled — no durable store (TG_DB_DSN unset); lexical only")
		default:
			estore := db.NewKnowledgeEmbeddingStore(dbPool)
			dim := envInt("TG_EMBED_DIM", knowledge.DefaultEmbedDim)
			if dbDim, derr := estore.Dim(context.Background()); derr != nil {
				log.Printf("semantic retrieval: disabled — embedding column unavailable (%v); lexical only", derr)
			} else if dbDim != dim {
				// A mismatched dimension would mean truncated/padded vectors — a config error, refused loudly.
				log.Fatalf("semantic retrieval: TG_EMBED_DIM=%d does not match the migrated embedding column vector(%d) — the migration's dimension is the law; fix TG_EMBED_DIM (and use an embedding model that produces %d dims)", dim, dbDim, dbDim)
			} else {
				embedder := model.Embedder{Gateway: gw, Model: embedModel}
				minSim := envFloat("TG_EMBED_MIN_SIMILARITY", knowledge.DefaultMinSimilarity)
				// TG-214 HyDE: when armed (TG_RETRIEVE_HYDE), the semantic channel embeds a fast-model
				// hypothetical RESOLUTION as a document instead of the raw symptom query; nil (unarmed) ⇒ the raw
				// query is embedded, byte-identical. A model call in the retrieval path — off by default.
				retriever = &knowledge.FusedRetriever{Base: knowledgeHolder, Index: estore, Embed: embedder, MinSim: minSim,
					Hypothetical: hydeHypothetical(gw, getenv)}
				backfiller := &knowledge.Backfiller{
					Store: estore, Lookup: knowledgeHolder, Embed: embedder, Model: embedModel,
					Dim: dim, Batch: envInt("TG_EMBED_BACKFILL_BATCH", knowledge.DefaultBackfillBatch),
				}
				runEmbedPass := func(ctx context.Context) {
					if _, _, serr := knowledge.SyncIndex(ctx, estore, knowledgeHolder.Snapshot()); serr != nil {
						log.Printf("semantic retrieval: index sync failed: %v (retried next pass; lexical still serves)", serr)
						return
					}
					if res, berr := backfiller.RunOnce(ctx); berr != nil {
						log.Printf("semantic retrieval: embed pass failed: %v (rows stay unembedded; lexical still serves)", berr)
					} else if res.Embedded > 0 {
						log.Printf("semantic retrieval: embedded %d precedent(s) (skipped %d)", res.Embedded, res.Skipped)
					}
				}
				syncEmbed = func() { // best-effort + backgrounded: a corpus write is never blocked on embedding
					go func() {
						sctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
						defer cancel()
						runEmbedPass(sctx)
					}()
				}
				syncEmbed() // fold the boot corpus in immediately
				// The bounded backfill sweep (the falsifiability-scorer loop pattern): every interval, sync
				// refs and embed up to the batch of rows still NULL. Empty ⇒ the 10m default; 0 disables
				// the sweep (corpus writes still embed best-effort).
				iv := strings.TrimSpace(getenv("TG_EMBED_BACKFILL_INTERVAL", ""))
				if iv == "" {
					iv = "10m"
				}
				if d, derr := time.ParseDuration(iv); derr == nil && d > 0 {
					go func() {
						t := time.NewTicker(d)
						defer t.Stop()
						for range t.C {
							sctx, cancel := context.WithTimeout(context.Background(), d)
							runEmbedPass(sctx)
							cancel()
						}
					}()
					log.Printf("semantic retrieval: embedding backfill every %s (batch %d)", d, backfiller.Batch)
				} else {
					log.Printf("semantic retrieval: backfill sweep disabled (TG_EMBED_BACKFILL_INTERVAL=%q); embeddings fold in only on corpus writes", iv)
				}
				log.Printf("semantic retrieval: enabled — model=%s dim=%d min_similarity=%.2f (RRF-fused with lexical; degrades to lexical per-query on embed failure)", embedModel, dim, minSim)
			}
		}
		// Reload the corpus at runtime (an operator or the lessons feed appending a resolved incident takes
		// effect without a restart). Off by default; a parse error keeps the last good corpus.
		if iv := getenv("TG_KNOWLEDGE_REFRESH_INTERVAL", ""); iv != "" {
			if d, err := time.ParseDuration(iv); err == nil && d > 0 {
				go func() {
					t := time.NewTicker(d)
					defer t.Stop()
					for range t.C {
						knowledgeHolder.Set(loadCorpus())
						syncEmbed() // a reloaded corpus re-syncs the vector index (best-effort)
					}
				}()
				log.Printf("knowledge: corpus reload every %s", d)
			}
		}
	} else if getenv("TG_EMBED_MODEL", "") != "" {
		log.Printf("semantic retrieval: disabled — TG_EMBED_MODEL set but no knowledge corpus (TG_KNOWLEDGE_FILE unset); nothing to retrieve over")
	}
	// TG-50: deterministic multi-query retrieval — when armed, retrieve over rule-broadened query variants (the
	// original + a host-relaxed one) and RRF-fuse them, adding recall from the broadened variant's different
	// neighbours (most valuable over the semantic-fused base), no model in the path. OFF by default ⇒ the base
	// retriever serves directly, byte-identical. Wraps AFTER the base is finalized, so a corpus reload still
	// updates the underlying holder the wrapper delegates to.
	if retriever != nil && truthyEnv("TG_RETRIEVE_MULTIQUERY") {
		retriever = &knowledge.MultiQueryRetriever{Base: retriever}
		log.Print("knowledge: multi-query retrieval ARMED (TG-50) — rule-broadened variants, RRF-fused")
	}

	// TG-53: incident-knowledge GraphRAG — when armed, broaden retrieval by the alerting host's estate BLAST
	// RADIUS (the hosts that fail WITH it) and RRF-fuse, so precedent on a topologically-coupled host is lifted
	// above an equally-lexical precedent on an unrelated host. OFF by default ⇒ the base retriever serves
	// directly, byte-identical. Runs each blast-radius host through the full fused+multiquery pipeline (so it
	// wraps BENEATH the rerank stage below), and reads estateHolder.Graph() LIVE per query so a graph refresh
	// takes effect without a restart.
	if retriever != nil && truthyEnv("TG_RETRIEVE_GRAPH_PRECEDENT") {
		retriever = &knowledge.GraphExpandRetriever{
			Base:       retriever,
			MaxHosts:   envInt("TG_RETRIEVE_GRAPH_MAX_HOSTS", knowledge.DefaultGraphExpandHosts),
			BlastHosts: estateBlastHosts(estateHolder, envInt("TG_RETRIEVE_GRAPH_DEPTH", 2)),
		}
		log.Print("knowledge: graph-expand retrieval ARMED (TG-53) — blast-radius host broadening, RRF-fused")
	}

	// TG-50: cross-encoder RERANK. When a rerank endpoint is configured (TG_RERANK_URL — the TEI reranker,
	// BAAI/bge-reranker-v2-m3, on the GPU aux node), pull a WIDE candidate set from the base and reorder it to a
	// precise top-k with a real cross-encoder that judges (incident ↔ precedent) relevance jointly. Wraps
	// OUTERMOST, so it reranks the final fused + multi-query + graph-expanded cut. OFF by default (URL unset ⇒
	// base ranking, byte-identical); a reranker outage/timeout degrades to the base order, never failing a
	// retrieval. The endpoint host is deploy config (TG_RERANK_URL), never a committed estate hostname.
	if retriever != nil {
		if rurl := strings.TrimSpace(getenv("TG_RERANK_URL", "")); rurl != "" {
			widen := envInt("TG_RERANK_WIDEN", knowledge.DefaultRerankWiden)
			retriever = &knowledge.RerankRetriever{
				Base:     retriever,
				Reranker: &rerank.TEIClient{BaseURL: rurl},
				WidenTo:  widen,
			}
			log.Printf("knowledge: cross-encoder rerank ARMED (TG-50) — widen to %d then rerank to top-k, degrades to base on outage", widen)
		}
	}

	// TG-50: LLM QUERY-REWRITE — reformulate the incident into a crisp retrieval query with a fast model, then
	// run the whole fused + multi-query + graph + rerank stack on the rewrite. Wraps OUTERMOST (the rewrite
	// happens once, before all retrieval). OFF by default (TG_RETRIEVE_QUERY_REWRITE unset ⇒ the raw query,
	// byte-identical); a generation error degrades to the original query, never failing a retrieval. A MODEL
	// CALL in the retrieval path (latency), armed only by explicit operator choice.
	if retriever != nil {
		if rw := queryRewrite(gw, getenv); rw != nil {
			retriever = &knowledge.QueryRewriteRetriever{Base: retriever, Rewrite: rw}
			log.Print("knowledge: LLM query-rewrite ARMED (TG-50) — reformulates the query before retrieval, degrades to raw on error")
		}
	}

	// reconcileLessons is the LESSONS half of the shared recency/decay pass (spec/018, design-wisdom #11): it
	// prunes precedents whose PROVENANCE age exceeds the retention horizon (TG_LESSONS_MAX_AGE) from the durable
	// corpus, so a stale lesson's influence decays to zero (it leaves the retrieval set). It shares lessonsMu
	// with the append path so a reconcile tick and a feed-append tick never race on the corpus file. It stays a
	// no-op until the lessons feed + corpus + a positive max-age are configured (assigned in the block below).
	var lessonsMu sync.Mutex
	reconcileLessons := func() {}
	// The lessons persistence hop — closes the learn→retrieve loop: a resolved-incident feed
	// (TG_LESSONS_SOURCE_FILE; today an operator export, in Phase 2 the close-out path) is distilled to its
	// CONFIRMED-CLEAN subset (core/lessons) and merged into the durable corpus (TG_KNOWLEDGE_FILE) the retriever
	// reloads — so a verified resolution becomes citable precedent for the next similar incident, and a
	// deviation/partial/unconfirmed outcome never poisons the corpus. Requires the corpus file (there is no
	// durable place to persist a lesson without it) and is a no-op unless the feed contributes a NET-NEW
	// confirmed-clean lesson. The read-merge-write is serialized by lessonsMu so a boot pass and an interval
	// tick (or, later, a concurrent close-out) never race on the corpus file.
	// EVERY path records at the lessons seam, including the implicit else. Before this the outer `if`
	// had no else at all: with TG_LESSONS_SOURCE_FILE unset nothing ran and NOTHING WAS LOGGED, so the
	// corpus quietly froze while the boot log still reported "corpus loaded — 670 prior incidents".
	darkLessons := func(reason string) {
		wiring.Absent[struct{}](wiringManifest, wiring.SeamLessonsFeed, wiring.Because{
			Reason:      reason,
			Consequence: "the corpus grows only from TG's own confirmed-clean heals; curated and imported operator knowledge has no path in",
			Owner:       "@ncpjfuzl", Ticket: "TG-239 (MECH-201)", Expiry: time.Date(2026, time.October, 30, 0, 0, 0, 0, time.UTC),
		})
	}
	if src := getenv("TG_LESSONS_SOURCE_FILE", ""); src != "" {
		switch {
		case corpusPath == "":
			darkLessons("TG_LESSONS_SOURCE_FILE is set but TG_KNOWLEDGE_FILE is empty")
			log.Printf("lessons: TG_LESSONS_SOURCE_FILE set but TG_KNOWLEDGE_FILE is empty — no durable corpus to persist into; lessons feed disabled")
		case knowledgeHolder == nil:
			darkLessons("the knowledge corpus holder is unavailable")
			log.Printf("lessons: knowledge corpus unavailable — lessons feed disabled")
		default:
			// The recency/decay retention horizon (spec/018): a positive TG_LESSONS_MAX_AGE prunes precedents
			// older than it from the corpus (via reconcileLessons, below) AND stops the append path from
			// re-adding a stale lesson still present in the feed — so the two never tug-of-war. 0 ⇒ decay OFF.
			lessonsMaxAge := envDuration("TG_LESSONS_MAX_AGE", 0)
			// The live path: Bind the closure that actually grows the corpus, so liveness is derived from
			// the bound value rather than asserted by reaching this branch.
			appendLessons := func() {
				lessonsMu.Lock()
				defer lessonsMu.Unlock()
				sf, err := os.Open(src)
				if err != nil {
					log.Printf("lessons: resolved-incident feed %s unreadable: %v (skipped)", src, err)
					return
				}
				resolved, perr := lessons.ParseResolved(sf)
				sf.Close()
				if perr != nil {
					log.Printf("lessons: resolved-incident feed %s rejected: %v (skipped, corpus untouched)", src, perr)
					return
				}
				if lessonsMaxAge > 0 { // never re-add a lesson the reconciliation would immediately prune
					resolved = lessons.Reconcile(resolved, time.Now(), lessonsMaxAge).Fresh
				}
				// The current corpus on disk (empty if the file is not yet written).
				var existing []knowledge.Incident
				if cf, err := os.Open(corpusPath); err == nil {
					existing, _ = knowledge.ParseCorpus(cf)
					cf.Close()
				}
				merged, added := lessons.Merge(existing, resolved)
				// Observed BEFORE the early return, and that ordering is the point: a feed carrying 400
				// resolved incidents that merges zero is the starvation case, and it is exactly the path
				// that returns here having logged nothing at all.
				wiringYield.Observe(wiring.SeamLessonsFeed, len(resolved), added, time.Now().UTC())
				if added == 0 {
					return // nothing confirmed-clean and net-new — leave the corpus (and its file) untouched
				}
				// Atomic write + tamper-evidence witness, through the single maintained-corpus chokepoint.
				if werr := persistCorpus(existing, merged); werr != nil {
					log.Printf("lessons: %v (skipped)", werr)
					return
				}
				knowledgeHolder.Set(loadCorpus()) // reload the seed∪maintained union — never the maintained-only set (the seed must stay visible to the novelty gate after a write)
				syncEmbed()                       // a new lesson becomes semantically retrievable too (best-effort, never blocking)
				log.Printf("lessons: distilled %d new confirmed-clean lesson(s) into %s", added, corpusPath)
			}
			appendLessons() // fold the feed in once at boot
			// TG-52 caution lane population: the SAME resolved feed (src), distilling the FAILED/deviated/
			// unconfirmed trajectories lessons.Distill DROPS (lessons.DistillCautions) into the SEPARATE
			// caution corpus (cautionPath) — never the precedent corpus. Serialized on its own cautionMu; a
			// no-op when TG_CAUTION_FILE is unset or the caution holder is absent. Mirrors appendLessons.
			var cautionMu sync.Mutex
			appendCautions := func() {
				if cautionPath == "" || cautionHolder == nil {
					return
				}
				cautionMu.Lock()
				defer cautionMu.Unlock()
				sf, err := os.Open(src)
				if err != nil {
					return // appendLessons already logged the feed's unreadability this pass
				}
				resolved, perr := lessons.ParseResolved(sf)
				sf.Close()
				if perr != nil {
					return // appendLessons already logged the rejection
				}
				var existing []knowledge.Incident
				if cf, cerr := os.Open(cautionPath); cerr == nil {
					existing, _ = knowledge.ParseCorpus(cf)
					cf.Close()
				}
				merged, added := lessons.CautionMerge(existing, resolved)
				if added == 0 {
					return // nothing net-new — leave the caution corpus (and its file) untouched
				}
				// The caution lane is a SEPARATE corpus (its own file, its own would-be domain); it shares the
				// atomic write primitive but is NOT anchored in Slice A — persistCorpus (TG-510) witnesses only
				// the maintained PRECEDENT corpus that reaches trusted retrieval. Anchoring the caution lane is a
				// clean follow-up: its own 'caution-corpus' domain + its own verify job.
				if werr := knowledge.WriteCorpusFile(cautionPath, merged); werr != nil {
					log.Printf("caution: %v (skipped)", werr)
					return
				}
				// Arm the reload the SAME way newCautionHolder arms the initial holder (knowledge_corpus.go:
				// "loadKnowledgeCorpus arms reloads") — otherwise this fold-in silently reverts the caution
				// lane to flat/no-floor, un-arming TG-50's min-score and TG-508's IDF tags for this lane the
				// moment a caution is distilled. Byte-identical while both flags are unset.
				cautionHolder.Set(knowledge.NewLexicalRetriever(merged).SetMinScore(envFloat("TG_RETRIEVE_MIN_SCORE", 0)).SetIDFTags(truthyEnv("TG_RETRIEVAL_IDF_TAGS")))
				log.Printf("caution: distilled %d new caution(s) into %s (failed/deviated trajectories — separate from precedent)", added, cautionPath)
			}
			appendCautions() // fold the caution lane in at boot too
			if iv := getenv("TG_LESSONS_REFRESH_INTERVAL", ""); iv != "" {
				if d, err := time.ParseDuration(iv); err == nil && d > 0 {
					go func() {
						t := time.NewTicker(d)
						defer t.Stop()
						for range t.C {
							appendLessons()
							appendCautions()
						}
					}()
					log.Printf("lessons: resolved-incident feed folded in every %s", d)
				}
			}
			wiring.Bind(wiringManifest, wiring.SeamLessonsFeed, appendLessons)
			// The lessons recency/decay reconciliation (spec/018): prune precedents older than the retention
			// horizon (lessonsMaxAge, above) from the corpus so a stale lesson's influence decays to zero. OFF
			// unless TG_LESSONS_MAX_AGE is positive. Serialized with the append path via lessonsMu; fired by the decay cron.
			if lessonsMaxAge > 0 {
				reconcileLessons = func() {
					lessonsMu.Lock()
					defer lessonsMu.Unlock()
					sf, err := os.Open(src)
					if err != nil {
						log.Printf("lessons decay: feed %s unreadable: %v (skipped)", src, err)
						return
					}
					resolved, perr := lessons.ParseResolved(sf)
					sf.Close()
					if perr != nil {
						log.Printf("lessons decay: feed %s rejected: %v (skipped, corpus untouched)", src, perr)
						return
					}
					rec := lessons.Reconcile(resolved, time.Now(), lessonsMaxAge)
					if len(rec.StaleRefs) == 0 {
						return // nothing aged past the horizon
					}
					var existing []knowledge.Incident
					if cf, cerr := os.Open(corpusPath); cerr == nil {
						existing, _ = knowledge.ParseCorpus(cf)
						cf.Close()
					}
					kept, removed := lessons.PruneStaleFromCorpus(existing, rec.StaleRefs)
					if removed == 0 {
						return // the stale lessons were not in the corpus — nothing to rewrite
					}
					// Atomic write + tamper-evidence witness, through the single maintained-corpus chokepoint.
					if werr := persistCorpus(existing, kept); werr != nil {
						log.Printf("lessons decay: %v (skipped)", werr)
						return
					}
					knowledgeHolder.Set(loadCorpus()) // reload the seed∪maintained union after the prune — never the maintained-only set
					syncEmbed()                       // the pruned corpus re-syncs the vector index (best-effort, never blocking)
					log.Printf("lessons decay: pruned %d stale precedent(s) older than %s from %s", removed, lessonsMaxAge, corpusPath)
				}
				log.Printf("lessons decay: provenance-pruning armed (retention horizon %s) — fired by the decay cron", lessonsMaxAge)
			}
		}
	} else {
		// THE MISSING ELSE — this branch did not exist, and its absence is the whole finding. With the
		// feed unset the corpus silently freezes while the boot log still says "corpus loaded".
		darkLessons("TG_LESSONS_SOURCE_FILE is not configured in this deployment")
		log.Printf("lessons: NO resolved-incident feed configured (TG_LESSONS_SOURCE_FILE unset) — the "+
			"corpus grows only from TG's own confirmed-clean heals; curated knowledge has no path in (seam %s dark)", wiring.SeamLessonsFeed)
	}

	// ── THE TRACKER-HISTORY IMPORT LANE (TG-244) ─────────────────────────────────────────────────────
	//
	// The COMPOUNDING half of tracker history. get-tracker-history (registered above) is the RECALL half —
	// a read-only tool that FETCHES prior incidents on demand but persists nothing, so recall never
	// compounds: every session re-reads the same tickets and the human resolutions the site's engineers
	// already wrote for these exact faults never reach the retriever's ranking. This lane distils that
	// history into ranked ProvenanceTrackerImport corpus rows (core/trackerimport) and merges them into the
	// maintained corpus the retriever reloads, so the estate's own ticket archive becomes precedent that
	// RANKS. It writes ONLY the maintained corpus; the tracker is read-only by construction (the History
	// capability has one method, and it searches).
	//
	// DARK WHEN NO HISTORY-CAPABLE TRACKER IS CONFIGURED, or when there is no durable corpus
	// (TG_KNOWLEDGE_FILE) to compound into — mirroring how get-tracker-history's config gates it and how
	// SeamLessonsFeed gates the sibling write lane. Serialized with the export/decay/writeback paths via
	// lessonsMu so the four never race the corpus file. The history-capable set is rediscovered from the
	// module registry here (the get-tracker-history block's map is out of scope), keyed on the CAPABILITY.
	darkTrackerImport := func(reason string) {
		wiring.Absent[struct{}](wiringManifest, wiring.SeamTrackerImport, wiring.Because{
			Reason:      reason,
			Consequence: "the estate's own incident tracker is never distilled into ranked precedent: its engineers' human resolutions stay fetch-only via get-tracker-history, re-read every session and never reaching the retriever's ranking",
			Owner:       "@ncpjfuzl", Ticket: "TG-244", Expiry: time.Date(2026, time.October, 30, 0, 0, 0, 0, time.UTC),
		})
	}
	importHistories := map[string]tracker.History{}
	for _, cp := range moduleReg.Capabilities() {
		if cp.Surface != modules.SurfaceTracker || !cp.Enabled {
			continue
		}
		regn, rerr := moduleReg.Resolve(modules.SurfaceTracker, cp.SourceType)
		if rerr != nil {
			continue // the get-tracker-history block already logged this same resolve failure
		}
		if h, okH := regn.Adapter.(tracker.History); okH {
			importHistories[cp.SourceType] = h
		}
	}
	switch {
	case corpusPath == "":
		darkTrackerImport("no durable corpus (TG_KNOWLEDGE_FILE) to compound tracker history into")
		log.Printf("tracker-import: TG_KNOWLEDGE_FILE is empty — no durable corpus to persist imported precedent into; import lane disabled (seam %s dark)", wiring.SeamTrackerImport)
	case knowledgeHolder == nil:
		darkTrackerImport("the knowledge corpus holder is unavailable")
		log.Printf("tracker-import: knowledge corpus unavailable — import lane disabled (seam %s dark)", wiring.SeamTrackerImport)
	case len(importHistories) == 0:
		darkTrackerImport("no history-capable tracker is configured")
		log.Printf("tracker-import: no history-capable tracker configured (adapters/tracker.History) — nothing to import; import lane disabled (seam %s dark)", wiring.SeamTrackerImport)
	default:
		importReader := tracker.NewMultiHistory(importHistories)
		importLimit := envInt("TG_TRACKER_IMPORT_LIMIT", 20)
		importTrackerHistory := func() {
			lessonsMu.Lock()
			defer lessonsMu.Unlock()
			var existing []knowledge.Incident
			if cf, err := os.Open(corpusPath); err == nil {
				existing, _ = knowledge.ParseCorpus(cf)
				cf.Close()
			}
			ctx, cancel := context.WithTimeout(context.Background(), envDuration("TG_TRACKER_IMPORT_TIMEOUT", 2*time.Minute))
			defer cancel()
			merged, res := trackerimport.Run(ctx, existing, importReader, importLimit)
			// The seam's runtime yield: incidents distilled OFFERED vs precedents actually merged PRODUCED. A
			// large OFFERED with a zero PRODUCED is the starvation case (all duplicates, all screened out, or
			// all downhill) and reads as such rather than as a healthy idle lane.
			wiringYield.Observe(wiring.SeamTrackerImport, res.Offered, res.Produced, time.Now().UTC())
			for _, fail := range res.Failures {
				log.Printf("tracker-import: source read failed (skipped; that shape leaves the corpus untouched): %s", fail)
			}
			if res.Dropped > 0 {
				log.Printf("tracker-import: %d imported row(s) DROPPED by screen.Scrub (a neutralized injection or a leaked secret) — never written un-scrubbed", res.Dropped)
			}
			if !res.Changed {
				return // nothing net-new after screening and the downhill-protected merge — leave the corpus file untouched
			}
			// Atomic write + tamper-evidence witness, through the single maintained-corpus chokepoint.
			if werr := persistCorpus(existing, merged); werr != nil {
				log.Printf("tracker-import: %v (skipped)", werr)
				return
			}
			knowledgeHolder.Set(loadCorpus()) // reload the seed∪maintained union so imported precedent is retrievable now
			syncEmbed()                       // and semantically retrievable too (best-effort, never blocking)
			log.Printf("tracker-import: distilled %d historical incident(s) across %d shape(s) into %d new precedent(s) in %s", res.Offered, res.QueriesRun, res.Produced, corpusPath)
		}
		// Fold the estate's tracker history in once at boot, ASYNCHRONOUSLY: unlike the lessons feed's
		// boot pass (a local file read), this one makes a tracker API call per corpus shape, so running it
		// inline would stall worker startup for up to the import timeout. It is bounded by that timeout and
		// serialized on lessonsMu, so a slow tracker delays only the first import, never the boot.
		go importTrackerHistory()
		if iv := getenv("TG_TRACKER_IMPORT_INTERVAL", ""); iv != "" {
			if d, err := time.ParseDuration(iv); err == nil && d > 0 {
				go func() {
					t := time.NewTicker(d)
					defer t.Stop()
					for range t.C {
						importTrackerHistory()
					}
				}()
				log.Printf("tracker-import: estate tracker history re-imported every %s", d)
			}
		}
		wiring.Bind(wiringManifest, wiring.SeamTrackerImport, importTrackerHistory)
		log.Printf("agent: tracker-history IMPORT lane armed over %d history-capable tracker(s): %s — distils prior human resolutions into ranked ProvenanceTrackerImport precedent (read-only on the tracker; writes only the maintained corpus)",
			importReader.Len(), strings.Join(importReader.Sources(), ", "))
	}

	// ── THE WORLD-MODEL DISCOVERY PASS (spec/027 REQ-2705) ───────────────────────────────────────────
	//
	// This lane existed, fully built and unit-tested, and was WIRED BY NOTHING. Production ran with
	// manifest_entry at 0 rows: no entity was ever drafted, so the console's manifest surface said
	// "Discovery has not drafted anything yet" — true, and read as "it looked and found nothing" when the
	// truth was that it had never looked. Its own tests were green the whole time, because they call Run
	// directly and nothing else ever did.
	//
	// SAFE BY CONSTRUCTION, and worth stating because this lane touches the allowlist's source. It does
	// exactly two things: DRAFTS what is newly present and marks STALE what has disappeared. A draft is
	// inert — it materializes nothing until an operator approves it — and a disappearance never retires a
	// grant, because a source that blinks is not evidence a unit is gone (worlddiscovery's own header
	// reasons this out at length). So arming it cannot widen what TG may do; only an operator can.
	darkDiscovery := func(reason string) {
		wiring.Absent[struct{}](wiringManifest, wiring.SeamWorldDiscovery, wiring.Because{
			Reason: reason,
			Consequence: "no entity is ever drafted: manifest_entry stays empty, the console's manifest " +
				"surface stays permanently blank, and the earned-catalog ladder has no top rung",
			Owner: "@ncpjfuzl", Ticket: "TG-227 (spec/027 REQ-2705)", Expiry: time.Date(2026, time.October, 30, 0, 0, 0, 0, time.UTC),
		})
	}
	// 30m, matching the wiki-compile lane below and deploy/docker-compose.yml. The Go default was 0 —
	// declared-dark — so the lane wired in !826 ran ONLY for deployments using that compose file, and any
	// other deployment silently reproduced the empty-manifest state !826 existed to fix. A safe default
	// belongs in the binary; the compose value is then explicit agreement rather than the only thing
	// holding the lane up. Setting the env to 0 still turns it off deliberately, and that path is still
	// declared dark with its reason.
	// The SERVICE-OBSERVING half, recorded separately from the lane itself. world.discovery being LIVE
	// was never the same as world discovery working: with no service probe wired, TypeService has no
	// producer and two of the three adoption kinds are unreachable while this same block logs "armed".
	if len(discoverySources) > 0 {
		wiring.Bind(wiringManifest, wiring.SeamDiscoveryService, discoverySources)
		// The probes are EdgeSources, so their yield is observed where the estate is rebuilt: hosts
		// probed vs service edges actually returned. A probe that reaches every host and returns no
		// units is the shape this seam was created for — and it is invisible in an edge total that
		// mixes it with netbox and librenms.
		// Wrap each probe so its yield is counted WHERE IT ACTUALLY RUNS, inside estate.Build. Counting
		// by re-invoking Edges() from the refresh loop would have been a second full round of SSH probes
		// per refresh — the observer paying the observed cost twice.
		for i := range discoverySources {
			discoverySources[i] = yieldingEdgeSource{
				inner: discoverySources[i],
				hosts: discoveryHostCounts[i], // recorded at construction; NEVER a type assertion for an
				// accessor the probe does not have, which compiles and panics the worker at boot.
				observe: func(hosts, edges int) {
					wiringYield.Observe(wiring.SeamDiscoveryService, hosts, edges, time.Now().UTC())
				},
			}
		}
	} else {
		wiring.Absent[[]estate.EdgeSource](wiringManifest, wiring.SeamDiscoveryService, wiring.Because{
			Reason: "no service-observing discovery probe is configured (TG_DISCOVERY_SYSTEMD_HOSTS and " +
				"TG_DISCOVERY_DOCKER_HOSTS are both empty)",
			Consequence: "estate.TypeService has no producer, so no KindUnit or KindContainer entry can " +
				"ever be drafted for adoption and TG_ACTUATION_ALLOWED_UNITS stays the only grant path",
			Owner: "@ncpjfuzl", Ticket: "TG-247", Expiry: time.Date(2026, time.August, 31, 0, 0, 0, 0, time.UTC),
		})
		log.Printf("discovery: NO service-observing probe configured — the world model can draft hosts "+
			"and guests but never a unit or container (seam %s dark)", wiring.SeamDiscoveryService)
	}
	discoveryEvery := envDuration("TG_WORLD_DISCOVERY_INTERVAL", 30*time.Minute)
	switch {
	case discoveryEvery <= 0:
		darkDiscovery("TG_WORLD_DISCOVERY_INTERVAL is unset or zero")
		log.Printf("world discovery: NOT armed (TG_WORLD_DISCOVERY_INTERVAL unset) — nothing will ever be "+
			"drafted into the world model, so #manifest stays empty (seam %s dark)", wiring.SeamWorldDiscovery)
	case dbPool == nil:
		darkDiscovery("no database pool — the manifest store is unavailable")
		log.Printf("world discovery: NOT armed — no durable manifest store (seam %s dark)", wiring.SeamWorldDiscovery)
	case len(estateSources) == 0:
		// A pass with no sources would observe zero entities and, worse, diff an EMPTY snapshot against
		// the manifest — marking every approved entry stale. worlddiscovery.Run refuses with ErrNoSources
		// rather than doing that, and declaring it dark here says WHY rather than logging that refusal
		// once per interval forever.
		darkDiscovery("no estate discovery sources are configured (TG_NETBOX_URL / TG_LIBRENMS_DEPLOYMENTS)")
		log.Printf("world discovery: NOT armed — no estate sources configured, so a pass would observe "+
			"nothing and could only mark existing entries stale (seam %s dark)", wiring.SeamWorldDiscovery)
	default:
		discoveryJob := worlddiscovery.Job{
			// The pair the seam-yield register alarms on: entities OBSERVED by the sources vs drafts
			// WRITTEN. A pass that sees the estate and drafts nothing is a broken lane wearing a healthy
			// "armed every 30m" boot line.
			OnPass: func(res worlddiscovery.Result) {
				wiringYield.Observe(wiring.SeamWorldDiscovery, res.Observed, res.Drafted, time.Now().UTC())
			},
			Sources: append(append([]estate.EdgeSource(nil), estateSources...), learner.LearnedSource()),
			Store:   db.NewWorldManifestStore(dbPool),
			Ledger:  ledger,
		}
		wiring.Bind(wiringManifest, wiring.SeamWorldDiscovery, discoveryJob)
		go worlddiscovery.RunPeriodically(context.Background(), discoveryJob, discoveryEvery, func(err error) {
			log.Printf("world discovery: pass failed: %v (the manifest is unchanged)", err)
		})
		log.Printf("world discovery: armed every %s over %d source(s) — drafts are INERT until an operator "+
			"approves them, and a disappearance marks stale, never retires", discoveryEvery, len(discoveryJob.Sources))
	}

	// ── THE WIKI COMPILER (MECH-211, ported from the predecessor's scripts/wiki-compile.py) ──────────
	//
	// Turns what TG has RECORDED into what an operator can READ: one page per host it has triaged,
	// compiled from the spine, written as one atomically-replaced envelope the grounder serves.
	//
	// Why a compile and not a query: the console's host surface currently filters a 200-row ESTATE-WIDE
	// session window client-side, which for 78 hosts cannot give most of them a single incident, and it
	// takes its host list from the estate GRAPH — so a machine TG has triaged but discovery never
	// registered gets no page at all. A per-host compile inverts both: a host earns a page by having been
	// dealt with, and its incidents come from a per-host read with no shared window.
	//
	// The compile itself is PURE and clock-free (core/wikicompile, guarded by TestPackageIsClockFree), so
	// the same spine produces byte-identical articles and "did anything change?" stays answerable. The
	// predecessor stamps a timestamp into every article body and consequently rewrites all 86 of its files
	// nightly; here the timestamp lives on the envelope.
	//
	// Observe-only: it reads the spine and writes one file. It reaches no actuator and never traverses the
	// mode chokepoint.
	// firstWikiCompile is invoked after the wiring report below, not at arm time — see the note at its
	// assignment. nil when the lane is not configured.
	var firstWikiCompile func()
	darkWiki := func(reason string) {
		wiring.Absent[struct{}](wiringManifest, wiring.SeamWikiCompile, wiring.Because{
			Reason: reason,
			Consequence: "no per-host article is compiled: the console's host surface stays a client-side " +
				"filter over a 200-row estate-wide window, and hosts absent from the estate graph get no page",
			Owner: "@ncpjfuzl", Ticket: "TG-239 (MECH-211)", Expiry: time.Date(2026, time.October, 30, 0, 0, 0, 0, time.UTC),
		})
	}
	if wikiPath := getenv("TG_WIKI_ARTICLES_FILE", ""); wikiPath != "" {
		hist := db.NewIncidentHistoryStore(dbPool)
		estateStore := db.NewEstateReadStore(dbPool)
		deps := wikiCompileDeps{
			Roster:       hist.WikiHostRoster,
			SourceCounts: hist.WikiSourceCounts,
			PriorFor:     hist.PriorSessions,
			RuleSessions: hist.WikiRuleSessions,
			Decisions:    hist.WikiDecisionTallies,
			Seams: func() []wikicompile.SeamStatus {
				// Report() returns only the DARK findings; All() is the closed set. A seam in All() with
				// no finding is live — derived rather than assumed, so a new seam appears here the moment
				// it joins the set, live or dark, without anyone remembering to add it.
				findings, _ := wiringManifest.Report(time.Now().UTC())
				byName := make(map[string]wiring.Finding, len(findings))
				for _, f := range findings {
					byName[string(f.Seam)] = f
				}
				specs := wiring.All()
				out := make([]wikicompile.SeamStatus, 0, len(specs))
				for _, sp := range specs {
					st := wikicompile.SeamStatus{
						Name: string(sp.ID), Consequence: sp.Consequence,
						Critical: sp.Criticality == wiring.Critical,
					}
					if f, dark := byName[string(sp.ID)]; dark {
						st.Dark = true
						st.Detail = f.Reason()
					}
					out = append(out, st)
				}
				return out
			},
			Ratified: func(ctx context.Context) (map[string]bool, error) {
				rows, err := db.NewOpClassRatifiedStore(dbPool).LiveOverlay(ctx)
				if err != nil {
					return nil, err
				}
				out := make(map[string]bool, len(rows))
				for _, r := range rows {
					out[r.OpClass] = true
				}
				return out, nil
			},
			Candidates: func(ctx context.Context) (map[string]string, error) {
				cs, err := db.NewOpClassCandidateStore(dbPool).LiveCandidates(ctx)
				if err != nil {
					return nil, err
				}
				out := make(map[string]string, len(cs))
				for _, c := range cs {
					out[c.OpClass] = string(c.Status)
				}
				return out, nil
			},
			Edges: func(ctx context.Context) ([]wikicompile.HostEdge, error) {
				row, err := estateStore.Latest(ctx)
				if err != nil {
					return nil, err
				}
				out := make([]wikicompile.HostEdge, 0, len(row.Graph.Edges))
				for _, e := range row.Graph.Edges {
					out = append(out, wikicompile.HostEdge{
						From: e.FromName, To: e.ToName,
						Rel: e.Rel, Confidence: e.Confidence,
					})
				}
				return out, nil
			},
			Corpus: func() []knowledge.Incident {
				if knowledgeHolder == nil {
					return nil
				}
				return knowledgeHolder.Snapshot()
			},
			Now: time.Now,
		}
		compileWiki := func() {
			hosts, n, err := compileWikiArticles(context.Background(), wikiPath, deps)
			if err != nil {
				// The PREVIOUS envelope stays in place. A partial wiki would silently retire every host
				// this run could not see; stale-but-complete beats fresh-and-truncated, and the surface
				// renders compiled_at so the staleness is visible rather than assumed away.
				log.Printf("wiki compile: FAILED, previous articles left in place: %v", err)
				return
			}
			// The PAIR, not the count: "12 articles" reads fine until you learn the roster held 78.
			wiringYield.Observe(wiring.SeamWikiCompile, hosts, n, time.Now().UTC())
			log.Printf("wiki compile: %d host article(s) written to %s (roster offered %d host(s))", n, wikiPath, hosts)
		}
		// THE FIRST COMPILE IS DEFERRED UNTIL AFTER THE WIRING REPORT, deliberately. The lane-health page
		// renders which seams are live, and the manifest is only complete once every Bind/Absent site has
		// run — several of which are hundreds of lines below this one. Compiling here would publish a page
		// declaring those later seams dark, which is precisely the false positive the report's own
		// ordering guard exists to prevent (deploy.TestWiringReportIsTakenAfterEveryBind).
		firstWikiCompile = compileWiki
		wiring.Bind(wiringManifest, wiring.SeamWikiCompile, compileWiki)
		iv := envDuration("TG_WIKI_COMPILE_INTERVAL", 30*time.Minute)
		if iv > 0 {
			go func() {
				t := time.NewTicker(iv)
				defer t.Stop()
				for range t.C {
					compileWiki()
				}
			}()
			log.Printf("wiki compile: armed every %s (articles %s)", iv, wikiPath)
		}
	} else {
		// The else that the lessons lane was missing, present from the start here for the same reason:
		// an unconfigured lane that logs nothing is indistinguishable from a working one.
		darkWiki("TG_WIKI_ARTICLES_FILE is not configured in this deployment")
		log.Printf("wiki compile: NOT configured (TG_WIKI_ARTICLES_FILE unset) — no per-host articles are "+
			"compiled; the console's host surface stays a client-side window (seam %s dark)", wiring.SeamWikiCompile)
	}

	// The read-only playbooks-as-knowledge lane (spec/017 T-017-5 follow-on): a fail-safe cron that pulls AWX
	// job templates + inventory READ-ONLY (re-read by id), ingests them into the knowledge corpus as retrieval
	// DATA (never an executable capability), and folds them into the semantic index over the UNION of the live
	// corpus + the runbooks (SyncIndex prunes refs absent from the corpus it is handed, so the union never
	// drops a lesson). It launches NOTHING — a surfaced runbook re-enters only as a proposal through the full
	// interceptor chain. Disabled unless TG_AWXPLAYBOOKS_* is configured; a cron error never crashes the worker.
	armAWXPlaybooksIngest(dbPool, knowledgeHolder, probeReg, &lessonsMu)

	// The actor-attribution plane (spec/023 — WHO is the actor behind the observed change?). The ruleset is
	// loadable rules-as-data (core/attribution). The EMBEDDED default is GENERIC (the portable
	// taxonomy→disposition mapping ONLY — no site principals, no pool carve-outs baked into the binary);
	// site-specific principals + carve-outs come from an operator OVERRIDE document mounted at
	// TG_ATTRIBUTION_CONFIG (a COMPLETE ruleset that REPLACES the default when present + readable). The
	// platform's OWN actuation identity per domain is derived from the credential configuration (never a
	// hardcoded token string, so self-recognition survives a token rotation); each domain reader is
	// config-gated (REQ-2306): an unconfigured domain has no reader and reads unattributable (REQ-2303). A
	// parse failure of EITHER fails CLOSED to the empty mapping (every non-unattributable attribution
	// escalates to the approver graph, REQ-2308) — never a permissive fallback.
	attributionDoc := attribution.DefaultConfigDocument()
	if p := getenv("TG_ATTRIBUTION_CONFIG", ""); p != "" {
		if b, rerr := os.ReadFile(p); rerr == nil {
			attributionDoc = b
			log.Printf("attribution: loaded operator ruleset override from %s (replaces the generic embedded default)", p)
		} else {
			log.Printf("attribution: TG_ATTRIBUTION_CONFIG=%s unreadable (%v) — using the generic embedded default (no site principals/carve-outs)", p, rerr)
		}
	} else {
		log.Printf("attribution: TG_ATTRIBUTION_CONFIG unset — using the generic embedded default (declare site principals/carve-outs via an override to sanction admins/pool hosts)")
	}
	attributionMapping, attributionCfg, aerr := attribution.ParseConfig(attributionDoc)
	if aerr != nil {
		log.Printf("attribution: ruleset failed to parse (%v) — failing CLOSED to the empty mapping (every non-unattributable attribution escalates)", aerr)
		attributionMapping = attribution.Mapping{}
		attributionCfg = attribution.Config{Sanctioned: map[string][]string{}, SelfActors: map[string]string{}, SelfReaders: map[string][]string{}}
	}
	// THE FINDINGS REACH THE LEDGER, not just stdout. Collected across the three checks below and appended
	// ONCE, after the readers are registered — the domain gaps are not knowable until then.
	var gapUncoveredHosts []string
	var gapTLSDetail string
	if disagreeTLS, d := pveTLSFlagDisagreement(truthyEnv("TG_PVE_INSECURE"), truthyEnv("TG_PROXMOX_INSECURE"), pveLivenessTLSFlagKey(planeEnv)); disagreeTLS {
		gapTLSDetail = d
	}

	// CARVE-OUT HOST COVERAGE — report the guests whose harness cycle will now escalate instead of resolving.
	//
	// matchCarveOut matches hosts EXACTLY (case-folded, no glob/CIDR — correct for an authorization rule), so
	// the carve-out host list and the estate's actuation guest pool are two lists that must agree, edited in
	// different places, with nothing comparing them. A guest added to TG_PROXMOX_ALLOWED_GUESTS but not to a
	// carve-out stops resolving to authorized-test: its injector-change + TG-heal pair becomes the
	// {AttributedAuthorized, AttributedSelf} contradiction, which escalates to a human. That is an AUTONOMY
	// loss, it is silent, and on one host it looks indistinguishable from estate noise.
	//
	// A WARNING, NOT A REFUSAL. Uncovered guests are the SAFE direction (TG asks rather than acts), and the
	// shipped default has no carve-outs at all, so failing closed here would refuse to boot a stock install.
	// The line names the hosts so the gap is legible instead of statistical.
	if pool := splitTokens(getenv("TG_PROXMOX_ALLOWED_GUESTS", "")); len(pool) > 0 {
		covered, uncovered := attribution.CarveOutHostCoverage(attributionCfg, pool, time.Now().UTC())
		gapUncoveredHosts = uncovered
		if len(uncovered) > 0 {
			log.Printf("attribution: carve-out host coverage %d/%d — NOT covered: %s. A harness cycle on an "+
				"uncovered guest resolves to an unadjudicated contradiction and escalates to a human "+
				"(authorized-test needs the guest named in a currently-valid carve-out)",
				len(covered), len(pool), strings.Join(uncovered, ", "))
		} else {
			log.Printf("attribution: carve-out host coverage %d/%d — every allowlisted guest is named by a currently-valid carve-out", len(covered), len(pool))
		}
	}

	// ★ THE CARVE-OUT EXPIRY, SAID OUT LOUD AT EVERY BOOT. A carve-out is a bounded suspension of the
	// security path, and the bound is now mandatory (REQ-2309) — which means it has a date on which the
	// learning regime stops: past it, the injector's sanctioned faults stop resolving to authorized-test and
	// revert toward stand-down, withholding actuation. That is the SAFE direction, and it is also completely
	// invisible from the outside: the symptom is "the estate stopped auto-healing", with a config file that
	// still lists every host. Reported here and as a gauge below so the lapse is a scheduled, visible event
	// rather than a mystery. Nothing here refuses to boot: an expired carve-out is a correctness-preserving
	// state, not a broken one.
	for _, e := range attribution.CarveOutExpiries(attributionCfg, time.Now().UTC()) {
		switch {
		case e.Expired:
			log.Printf("attribution: carve-out %q (domain %q) is EXPIRED (valid_until %s) — its actors no "+
				"longer resolve to authorized-test, so harness faults on its hosts now stand down instead of "+
				"healing and clean-run accrual has STOPPED for them; renew the bound to resume",
				e.ID, e.Domain, e.ValidUntil.Format(time.RFC3339))
		case e.Renew:
			log.Printf("attribution: carve-out %q (domain %q) lapses in %.1f days (valid_until %s) — renew it "+
				"before then or harness faults on its hosts will begin standing down instead of healing",
				e.ID, e.Domain, e.Remaining.Hours()/24, e.ValidUntil.Format(time.RFC3339))
		default:
			log.Printf("attribution: carve-out %q (domain %q) valid for another %.0f days (valid_until %s)",
				e.ID, e.Domain, e.Remaining.Hours()/24, e.ValidUntil.Format(time.RFC3339))
		}
	}

	var actorReaders []actorevidence.Reader
	// Per-domain accounting for the readers below, surfaced on /metrics as tg_actor_evidence_{reads,rows,
	// read_failures}_total{domain}. See core/attribution/readertally for why a zero-row read is recorded
	// explicitly rather than omitted.
	actorTally := readertally.New()
	if pveURL := getenv("TG_PVE_URL", ""); pveURL != "" {
		// Self-identity: the ACTUATION token's principal (user@realm!tokenid) — the identity TG's own heals
		// appear as in the PVE task log (e.g. root@pam!tg-actuate). Derived from the resolved ACTUATION
		// credential (TG_PROXMOX_TOKEN_REF — never the estate-read token and never a hardcoded string), so
		// self-recognition keys on the identity that actually actuates and survives a token rotation.
		if self := resolveSelfActor(getenv); self != "" {
			attributionCfg.SelfActors["pve"] = self
		}
		// The reader authenticates with a SEPARATE least-privilege READ-ONLY token (REQ-2306/INV-13) — never
		// the tg-actuate write token. Gate on the RESOLVED token, not merely the ref: compose always sets the
		// ref (with an empty value), so a ref that resolves empty must NOT register a reader that would 401
		// every read. Mirror the estate transport's opt-in TLS
		// skip (TG_PVE_INSECURE) — without it the default client's TLS verification fails and the reader is
		// silently inert.
		roRef := getenv("TG_PVE_RO_TOKEN_REF", "")
		if roTok, rerr := config.SecretRef(roRef).Resolve(); roRef != "" && rerr == nil && strings.TrimSpace(roTok) != "" {
			ropts := []pveattr.Option{pveattr.WithTimeout(8 * time.Second)}
			if truthyEnv("TG_PVE_INSECURE") {
				ropts = append(ropts, pveattr.WithHTTPClient(estateHTTPClient(true)))
				reportTLSSkip(pveURL)
			}
			actorReaders = append(actorReaders, pveattr.New(pveURL, config.SecretRef(roRef), ropts...))
			log.Printf("attribution: PVE task-log reader armed (read-only token) — WHO-CAUSED-THIS active for PVE guest lifecycle")
		} else {
			log.Printf("attribution: TG_PVE_RO_TOKEN_REF unset or resolves empty — the PVE actor-evidence reader is NOT registered (config-gated; PVE subjects read unattributable)")
		}
	}
	// The journal/sudo actor-evidence reader (spec/023 REQ-2314, the SECOND domain): reads a host's own
	// journal for privileged sudo actions over the native host-key-verified read-only SSH runner, resolving
	// the per-host identity through the SAME credential engine hostdiag uses. Config-gated on an operator
	// allowlist (TG_JOURNAL_DEPLOYMENTS) AND a mandatory known_hosts file (TG_JOURNAL_KNOWN_HOSTS — unset ⇒
	// the native runner fails closed on every read). Both unset ⇒ the reader is not registered (journal
	// subjects read unattributable).
	// PLANE-SCOPED (TG-153): the journal reader SSHes an estate host and reads its log text. Triage plane only.
	actorReaders = wireK8sAuditReader(actorReaders, credResolver, planeEnv, getenv) // spec/023 T-023-9 (config-gated)
	if jAccess := journal.ParseAccess(planeEnv("TG_JOURNAL_DEPLOYMENTS", "")); len(jAccess) > 0 {
		jRunner := syslogng.NewNativeRunner(getenv(journal.KnownHostsEnv, ""))
		sshSessions := truthyEnv(journal.SSHSessionsEnv)
		actorReaders = append(actorReaders, journal.New(jAccess, jRunner, credResolver, journal.WithSSHSessions(sshSessions)))
		// The self-identity is wired whether or not the SSH source is armed: it costs nothing when unused,
		// and arming the source WITHOUT it would make every one of TG's own SSH heals read
		// attributed-suspicious — a security escalation on itself, and suspicion masks every other candidate.
		//
		// PLANE-SCOPED (TG-153): resolveSelfSSHActor RESOLVES AND PARSES the actuation private key to derive
		// its public fingerprint. That is a full read of the estate-mutating key into this process's memory —
		// on the triage plane, exactly the thing that must never happen — so it reads through planeEnv and
		// fails closed to "" there, which is the documented no-self-identity behaviour. The consequence is
		// stated below rather than hidden: on a split deployment the triage worker cannot recognise TG's own
		// SSH heals in the journal, so if SSH-session evidence is armed it must be armed with the self-actor
		// declared some other way (see docs/THREAT-MODEL.md §5, bounded-blast-radius).
		if self := resolveSelfSSHActor(planeEnv); self != "" {
			attributionCfg.SelfActors["journal"] = self
		}
		// TG-453/TG-457: recognise TG's OWN read-only investigation identities in the journal domain. hostdiag's
		// classify-SSH logins into a faulted host, and syslogng's device-log reads off a per-site syslog server,
		// land in the subject host's auth journal DURING triage; either would otherwise read attributed-suspicious
		// (not the actuation self-actor, not sanctioned, not a carve-out) and security-escalate TG's own
		// investigation — refusing a legitimately-approved heal (the live defect). Derived from the hostdiag +
		// syslogng KEYS the same credential-not-token way the actuation self-actor is; resolves here on the triage
		// plane (which holds those keys) and to none on the actuation plane (which withholds them).
		if readers := resolveSelfSSHReaders(planeEnv); len(readers) > 0 {
			attributionCfg.SelfReaders["journal"] = readers
			log.Printf("attribution: recognised %d of TG's OWN read-only investigation identit(ies) in the "+
				"journal domain (hostdiag classify-SSH + syslogng log reads) — TG's own diagnostic logins are "+
				"excluded from actor attribution, never attributed-suspicious (TG-453/TG-457)", len(readers))
		}
		src := "sudo only"
		if sshSessions {
			src = "sudo + SSH key fingerprints"
			if attributionCfg.SelfActors["journal"] == "" {
				// PLANE-AWARE (TG-353). The self-identity is derived by reading the ACTUATION private key,
				// which the plane split deliberately withholds from a triage process — so on that plane an
				// empty self-actor is the DESIGNED outcome, not a fault. The comment above already says so;
				// this warning used to contradict it, telling the triage operator that "every heal TG
				// performs over SSH will read attributed-suspicious" and to "fix the key ref".
				//
				// Both halves were wrong there. A triage worker performs no heals — actuation runs on the
				// other plane, where the key resolves — so the misreporting condition cannot arise on the
				// process printing the message. And "fix the key ref" means handing an actuation credential
				// to the triage plane, which ValidateFor then refuses at boot: the remedy undoes the control.
				//
				// Same shape as the UpstreamProbeUnreadable false page (TG-344): a correct security posture
				// reported as a fault, with a remedy that weakens it.
				if credentialPlane.HoldsActuation() {
					log.Printf("attribution: SSH-session evidence is ARMED but TG's own SSH identity did NOT resolve " +
						"(TG_ACTUATION_SSH_KEY) — every heal TG performs over SSH will read attributed-suspicious. " +
						"This is the one combination that actively misreports; fix the key ref or unset " +
						journal.SSHSessionsEnv)
				} else {
					log.Printf("attribution: SSH-session evidence is armed and TG's own SSH identity is NOT resolved " +
						"here — EXPECTED on this plane, which the credential split withholds the actuation key " +
						"from (TG-153). This worker performs no heals, so nothing it attributes is misreported; " +
						"it simply cannot recognise TG's own SSH heals in the journal. Do NOT add the key ref " +
						"here — declare the self-actor another way (docs/THREAT-MODEL.md §5)")
				}
			}
		}
		log.Printf("attribution: journal reader armed across %d access rule(s), sources: %s — WHO-CAUSED-THIS active for host privileged actions", len(jAccess), src)
	} else {
		log.Printf("attribution: TG_JOURNAL_DEPLOYMENTS unset — the journal actor-evidence reader is NOT registered (config-gated; journal subjects read unattributable)")
	}
	// The AWX job-history reader (spec/023 REQ-2306, T-023-10): attributes automation-driven changes — which
	// AWX job ran against the target host, and WHO launched it (launched_by/created_by). Gated on an EXPLICIT
	// opt-in (TG_AWX_ACTOREVIDENCE) rather than the AWX address/token alone, because those are already set for
	// the machine-plane credential source — arming a reader changes triage semantics (transparency-gated), so
	// it must be a deliberate operator act. Uses the same read-only-scoped TG_AWX_TOKEN_REF, resolved at use.
	if truthyEnv("TG_AWX_ACTOREVIDENCE") {
		if awxAddr := strings.TrimSpace(getenv("TG_AWX_ADDR", "")); awxAddr != "" {
			actorReaders = append(actorReaders, awxattr.New(awxAddr, config.SecretRef(getenv("TG_AWX_TOKEN_REF", ""))))
			log.Printf("attribution: AWX job-history reader armed — WHO-CAUSED-THIS active for automation-driven changes")
		} else {
			log.Printf("attribution: TG_AWX_ACTOREVIDENCE set but TG_AWX_ADDR empty — AWX actor-evidence reader NOT registered (fail closed)")
		}
	}
	// The NetBox changelog reader (spec/023 REQ-2306, T-023-10): attributes CMDB edits — who changed the
	// target device (the /api/core/object-changes/ user + action). Same explicit opt-in gate
	// (TG_NETBOX_ACTOREVIDENCE); uses TG_NETBOX_URL + the read-only TG_NETBOX_TOKEN_REF.
	if truthyEnv("TG_NETBOX_ACTOREVIDENCE") {
		if nbURL := strings.TrimSpace(getenv("TG_NETBOX_URL", "")); nbURL != "" {
			actorReaders = append(actorReaders, netboxattr.New(nbURL, config.SecretRef(getenv("TG_NETBOX_TOKEN_REF", ""))))
			log.Printf("attribution: NetBox changelog reader armed — WHO-CAUSED-THIS active for CMDB changes")
		} else {
			log.Printf("attribution: TG_NETBOX_ACTOREVIDENCE set but TG_NETBOX_URL empty — NetBox actor-evidence reader NOT registered (fail closed)")
		}
	}
	// The GitOps MR-history reader (spec/023 REQ-2306, T-023-11): attributes declarative-deploy changes — who
	// merged a deploy-manifest MR. Explicit opt-in (TG_GITOPSMR_ACTOREVIDENCE); needs the GitLab instance URL
	// + project id/path + a READ-ONLY project token (never the deploy/admin token). Gated closed if any of the
	// three is empty. Optional TG_GITOPSMR_BRANCH (default main) / TG_GITOPSMR_MANIFEST_PREFIX (default deploy/).
	if truthyEnv("TG_GITOPSMR_ACTOREVIDENCE") {
		glURL := strings.TrimSpace(getenv("TG_GITLAB_URL", ""))
		glProj := strings.TrimSpace(getenv("TG_GITLAB_PROJECT", ""))
		glTokRef := strings.TrimSpace(getenv("TG_GITLAB_RO_TOKEN_REF", ""))
		if glURL != "" && glProj != "" && glTokRef != "" {
			gopts := []gitopsmr.Option{}
			if b := strings.TrimSpace(getenv("TG_GITOPSMR_BRANCH", "")); b != "" {
				gopts = append(gopts, gitopsmr.WithTargetBranch(b))
			}
			if p := strings.TrimSpace(getenv("TG_GITOPSMR_MANIFEST_PREFIX", "")); p != "" {
				gopts = append(gopts, gitopsmr.WithManifestPrefix(p))
			}
			actorReaders = append(actorReaders, gitopsmr.New(glURL, glProj, config.SecretRef(glTokRef), gopts...))
			log.Printf("attribution: GitOps MR-history reader armed — WHO-CAUSED-THIS active for declarative-deploy changes")
		} else {
			log.Printf("attribution: TG_GITOPSMR_ACTOREVIDENCE set but TG_GITLAB_URL/PROJECT/RO_TOKEN_REF incomplete — GitOps MR reader NOT registered (fail closed)")
		}
	}

	// P5-1 — LET THE AGENT SEE THE EVIDENCE IT ALREADY COLLECTS (spec/023 REQ-2312/2313).
	//
	// TG computes actor evidence on every session and the reasoning context has never contained any of it:
	// measured live the day this shipped, 465 of 1228 triage rows carried actor evidence and 508 a resolved
	// taxonomy, while the agent package referenced attribution ZERO times. The evidence reached an audit
	// signal map and the A7 axis, and nothing the model could read — which is the shape behind the two
	// largest judged deficits, correct_diagnosis and evidence_grounded: the system HOLDS the evidence and
	// never binds it to the claim.
	//
	// It is a TOOL, not a pre-loaded context block, and that distinction is a safety property rather than a
	// style choice. A pre-built evidence record would be marked Captured+Recent by the orchestrator and is
	// target-relevant by construction, so it would satisfy BOTH the INV-11 silent-cognition guard and the
	// execute-time evidence gate — letting a session that gathered NOTHING keep its auto-resolve and actuate.
	// As a tool the agent spends a cycle and genuinely gathers it, so the binding is honest, and the payload
	// inherits the loop's input screen (REQ-1012) which is exactly what REQ-2313 demands of rendered evidence.
	//
	// It reads through the SAME readers the attribution activity uses — one collection path, so the tool and
	// the mechanical taxonomy can never disagree about what the audit trail said.
	if len(actorReaders) > 0 {
		attrWindow := attributionCfg.Window
		if attrWindow <= 0 {
			attrWindow = 30 * time.Minute // the compiled ceiling, mirroring AttributeActivity
		}
		readActorEvidence := makeActorEvidenceReader(actorReaders, actorTally)
		for _, tl := range actorevidencetool.New(readActorEvidence, attrWindow) {
			if err := tools.RegisterFrom("host", tl); err != nil {
				log.Fatalf("actor-evidence tool %s must register read-only: %v", tl.Name(), err)
			}
		}
		log.Printf("agent: registered the read-only actor-evidence tool (%d domain reader(s), window %s) — "+
			"the agent can now CITE who acted on a host instead of asserting it (P5-1)", len(actorReaders), attrWindow)
	}
	// The identity/auth enrichment resolver (spec/023 REQ-2315..2319): promotes confirmed live admins and
	// demotes disabled ones over a per-session copy of the sanctioned set. Reuses the SAME FreeIPA/LDAP
	// service bind the approver-sync uses (TG_LDAP_*). Config-gated on TG_LDAP_URLS; unset ⇒ no enrichment
	// (exactly the static Phase-1 behavior). Advisory/fail-open — a construction error is logged, never fatal.
	// ARMED READERS WITH NO IDENTITY DECLARED FOR THEIR DOMAIN — reported, never inferred.
	//
	// Two absences, both driven by a missing config row and neither announced by anything else:
	//   no sanctioned principals for a domain -> EVERY actor in it reads attributed-suspicious
	//   no self-actor for a domain            -> TG'S OWN actions there read attributed-suspicious, i.e. TG
	//                                            raises a security escalation on itself, and because suspicion
	//                                            DOMINATES, that reading masks every other candidate
	//
	// SelfActors is populated for exactly one domain in the tree ("pve", from the actuation credential above),
	// so arming any other reader lands in the second case immediately.
	//
	// A WARNING, NOT AN INFERENCE. Matching an actor against any SelfActors value would let a credential
	// stolen in one domain be auto-excused in another; per-domain identity is the control, not an oversight.
	// ★ THE DOMAINS TG ACTUATES IN. Stated here because the composition root is what wires actuation — the
	// attribution package must not infer it, and a self-identity is only meaningful where TG can ACT.
	//
	//   pve      — the actuation token performs guest lifecycle (vzstart/vzstop) and it lands in the task log
	//   journal  — the actuation SSH key logs into guests, and sshd records the login
	//   awx      — opschema.json declares effect_kind "awx-launch", so TG's own runs land in AWX job history
	//
	// netbox is DELIBERATELY ABSENT: the reader declares ReadOnly() and nothing in the tree posts, puts,
	// patches or deletes there, so TG can never appear in a NetBox changelog and has no self-identity to
	// match. Demanding one would send an operator to wire something with no effect.
	if len(actorReaders) > 0 {
		armed := make([]string, 0, len(actorReaders))
		for _, r := range actorReaders {
			armed = append(armed, r.Domain())
		}
		for _, g := range attribution.DomainConfigGaps(attributionCfg, armed, tgActuatesIn) {
			switch {
			case g.NoSelfActor && g.NoSanctioned:
				log.Printf("attribution: domain %q has an ARMED reader but NO self-actor and NO sanctioned principals — every actor there reads attributed-suspicious, INCLUDING TG's own actions (security-escalate, and suspicion masks every other candidate). The self-identity is derived from the domain's CREDENTIAL at the composition root (only \"pve\" is wired today), never from the ruleset; sanctioned principals ARE declared in the ruleset", g.Domain)
			case g.NoSelfActor:
				log.Printf("attribution: domain %q has an ARMED reader but NO self-actor — TG's OWN actions there read attributed-suspicious and escalate as a security event. Self-identity is derived from the domain's CREDENTIAL at the composition root (only \"pve\" is wired today), never from the ruleset", g.Domain)
			case g.NoSanctioned:
				log.Printf("attribution: domain %q has an ARMED reader but NO sanctioned principals — every non-TG actor there reads attributed-suspicious", g.Domain)
			}
		}
	}

	// ONE governance-ledger row per boot, and ONLY when something is wrong. The ledger is served
	// (/v1/ledger), append-only and hash-chained, so a finding placed there is timestamped, chain-positioned
	// and visible from the console — unlike the log lines above, which live only in `docker logs` on a host
	// an operator has no reason to open. A clean config writes nothing; the append is best-effort and never
	// blocks boot, because a diagnostic that can stop the control plane is a worse defect than the gap it
	// reports.
	if ledger != nil {
		var domainGaps []attribution.DomainConfigGap
		if len(actorReaders) > 0 {
			armed := make([]string, 0, len(actorReaders))
			for _, r := range actorReaders {
				armed = append(armed, r.Domain())
			}
			domainGaps = attribution.DomainConfigGaps(attributionCfg, armed, tgActuatesIn)
		}
		if err := appendConfigGapReport(ledger, gapUncoveredHosts, domainGaps, gapTLSDetail,
			attribution.CarveOutExpiryRisk(attributionCfg)); err != nil {
			log.Printf("config: gap report could not be appended to the governance ledger (non-blocking): %v", err)
		}
	}

	var sanctionResolver actorevidence.SanctionResolver
	if ldapURLs := getenv("TG_LDAP_URLS", ""); strings.TrimSpace(ldapURLs) != "" {
		var urls []string
		for _, u := range strings.Split(ldapURLs, ",") {
			if u = strings.TrimSpace(u); u != "" {
				urls = append(urls, u)
			}
		}
		if r, rerr := ldapident.New(ldapident.Config{
			URLs:            urls,
			BindDNRef:       config.SecretRef(getenv("TG_LDAP_BIND_DN", "env:LDAP_BIND_DN")),
			BindPasswordRef: config.SecretRef(getenv("TG_LDAP_BIND_PW", "env:LDAP_BIND_PW")),
			CACertRef:       config.SecretRef(getenv("TG_LDAP_CA", "")),
			UserBaseDN:      getenv("TG_LDAP_USER_BASE", ""),
		}); rerr == nil {
			sanctionResolver = r
			log.Printf("attribution: LDAP/FreeIPA identity resolver armed — dynamic sanctioning (promote live admins, demote disabled credentials) active")
		} else {
			log.Printf("attribution: LDAP identity resolver NOT armed (%v) — dynamic sanctioning disabled, static sanctioned list governs", rerr)
		}
	} else {
		log.Printf("attribution: TG_LDAP_URLS unset — the LDAP identity resolver is NOT registered (config-gated; static sanctioned list governs)")
	}

	// TG-466 slice 2: the confighash grounded mutation signal's read seam (Deps.GuestConfigChangedWithin,
	// temporal/runner/activities.go). Gated on confighashReader != nil — NOT the flag alone (a review finding:
	// dbPool+flag let an operator end up half-armed, flag ON but TG_PVE_URL/TG_PVE_RO_TOKEN_REF missing, with
	// the READ wired against a baseline NOTHING ever swept — fail-safe, ChangedWithin on an untouched table
	// always answers false, but a silent, permanent false-negative nothing told the operator to suspect). The
	// decision + boot line are extracted to confighashReadArmed (confighash_feed.go) so this exact half-armed
	// shape has a direct unit test. nil ⇒ AttributeActivity never calls this, Observation stays the zero
	// value, and the covered-but-empty escalation (REQ-2304 half 2) stays exactly as unreachable as today.
	var guestConfigChangedWithin func(context.Context, string, time.Duration) (bool, error)
	armed, confighashReadLog := confighashReadArmed(dbPool != nil, confighashReader != nil, truthyEnv("TG_PVE_CONFIGHASH_ENABLED"), credentialPlane != credential.ProcessPlaneActuation)
	if armed {
		guestConfigChangedWithin = db.NewGuestConfigBaselineStore(dbPool).ChangedWithin
	}
	log.Printf("%s", confighashReadLog)

	// recentAlertHosts backs the common-cause sibling corroboration (axis A2): which of a host set hold an
	// OPEN incident (a raise with no recovery, bounded by ingest.MaxOpenIncident). This replaced the
	// row-recency ActiveHosts read on 2026-07-29: a sibling down for twenty minutes stops producing rows and
	// vanished from a 15-minute window — measured blind to 82% of open incidents (9 of 11), and incident-
	// scoped suppression would have removed the very repeat rows the window depended on (140 of 203). The
	// open-incident read is suppression-immune by construction and shares its semantics with the actuation
	// verifier's baseline arm, so corroboration and verification now agree on what "already broken" means.
	// Nil when no DB is wired (the in-memory oracle) ⇒ corroboration is inert (fail-open).
	var recentAlertHosts func(context.Context, []string) (map[string]bool, error)
	if dbPool != nil {
		alertHist := db.NewAlertHistoryStore(dbPool)
		recentAlertHosts = func(ctx context.Context, hosts []string) (map[string]bool, error) {
			return alertHist.ActiveByOpenIncident(ctx, hosts, time.Now().UTC(), coreingest.MaxOpenIncident)
		}
	}
	// The armed revert's durable state (spec/029 T-029-2, REQ-2901). Conditional on the pool but
	// NOT optional in effect: with no database there is no durable arm, so an eligible op-class
	// under a pool-less worker REFUSES the forward (fail closed via the nil seam) — never wire a
	// store around a nil pool (a typed-nil interface would defeat exactly that refusal).
	var commitConfirmStore runner.CommitConfirmRecorder
	var commitConfirmDB *db.CommitConfirmStore
	var commitConfirmExecs runner.CommitConfirmExecutionReader
	if dbPool != nil {
		commitConfirmDB = db.NewCommitConfirmStore(dbPool)
		commitConfirmStore = commitConfirmDB
		// The consult's terminus read (T-029-3): the same per-run record the interceptor's
		// execution sink writes, opened read-side here.
		commitConfirmExecs = db.NewActionExecutionStore(dbPool)
	}
	// TG-394 slice 3: TG's OWN dependency-capability wiring (embed / journal-evidence / secrets / tracker /
	// notify), resolved from TG's config ONCE at boot and shared by two consumers — the runner's per-session
	// degraded-set stamp (deps.DegradedCapabilities, below) and the reachability metrics job (withSelfDepReachable,
	// further down). No estate identifiers are compiled in: the capability→host wiring is TG's config, resolved
	// against the live graph at read time.
	selfDepReachCaps := selfDepReachCapabilities(getenv, journalDepGlobs(journal.ParseAccess(planeEnv("TG_JOURNAL_DEPLOYMENTS", ""))))
	// TG-52: a nil INTERFACE (not a typed-nil *Holder) when no caution corpus is wired, so runner.caution's
	// `a.D.Cautions == nil` guard holds and never dereferences a nil holder.
	var cautionRetriever runner.CautionRetriever
	if cautionHolder != nil {
		cautionRetriever = cautionHolder
	}
	logPackBootAttestation(tools)
	deps := runner.Deps{
		Model:            agentModel, // the LiteLLM gateway, WRAPPED by the cost meter when a budget is configured
		Tools:            tools,
		Limits:           agent.DefaultLimits(),
		SkillRows:        skillRows,
		SkillTrials:      skillTrials,
		SkillVersionByID: skillVersionByID,
		CommitConfirm:    commitConfirmStore, // the armed revert's durable state (spec/029 T-029-2) — see its construction above
		Executions:       commitConfirmExecs, // the confirm consult's terminus read (T-029-3, REQ-2902)
		// REQ-2906: a failed revert trips the mutation breaker. The breaker is constructed above
		// (nil only when its store is absent); the activity errs loudly on a nil seam.
		BreakerTrip: func(bctx context.Context, reason string) error {
			if mutationBreaker == nil {
				return fmt.Errorf("mutation breaker not constructed")
			}
			_, err := mutationBreaker.Trip(bctx, reason)
			return err
		},
		Retriever:  retriever,
		XMLContext: truthyEnv("TG_RETRIEVE_XML_CONTEXT"), // TG-50: XML precedent block when armed; plain text default
		// TG-214 retrieval-sufficiency (CRAG-analog): when armed, an inadequate kept set renders an explicit
		// "no adequate precedent" signal instead of weak hits. OFF by default ⇒ byte-identical precedent block.
		Sufficiency:          truthyEnv("TG_RETRIEVE_SUFFICIENCY"),
		SufficiencyMinCosine: envFloat("TG_RETRIEVE_SUFFICIENCY_MIN_COSINE", 0), // <=0 ⇒ knowledge.StrongSemanticSimilarity
		// TG-86 slice 2b: fold estate-doc grounding into <estate>, estate identifiers redacted (TG-486), when armed (nil ⇒ OFF, byte-identical).
		EstateDocs: estateDocSeedGrounding(getenv, log.Printf, func() []string { return estateHolder.Graph().FreshObservableNames() }),

		Cautions:          cautionRetriever, // TG-52 caution lane (nil ⇒ no caution block); SEPARATE from Retriever
		Observe:           func(host string, at time.Time) { learner.Observe(learn.AlertObservation{Host: host, At: at}) },
		Metrics:           obsRegistry,           // OBSERVE-ONLY: the agent-loop/verify/classify metrics emitter (never gates)
		AgentSteps:        agentStepSink,         // OBSERVE-ONLY: scrubbed per-ReAct-cycle transcript (spec/020 T-020-8)
		AgentStepEvidence: agentStepEvidenceSink, // OBSERVE-ONLY: the screened tool payload behind each step (TG-272)
		SessionSpans:      sessionSpanSink,       // OBSERVE-ONLY: the session's ordered spans, to the trace store (TG-44)
		// NOT observe-only: the read-lane volume bound the agent loop consults before every estate read
		// (TG-165). One governor per process — see its construction beside the chokepoint.
		Recon: reconGovernor,
		// Prediction-eligible ⇔ the host resolves in the estate graph. Until the topology readers seed it the
		// graph is empty, so every host is (correctly) ineligible and classification fails closed to a poll.
		PredictionEligible: func(host string) bool { _, ok := estateHolder.Graph().Resolve(host); return ok },
		// A criticality-tier (P0) host is never silently AUTO. Declared config, config-not-code.
		CriticalityTier: func(host string) bool { _, ok := critHosts[host]; return ok },
		// A restart of a platform-owned control-plane service is vetoed to a poll. Declared config.
		SelfProtectedService: selfProtected,
		// A staged-canary (host, op) is forced to POLL_PAUSE so the first mutations require a human vote
		// (REQ-009). Declared config, config-not-code; nil-safe (empty ⇒ nothing pinned, inert).
		CanaryPinned: canaryPins.Match,
		// An op-class that has not earned AUTO will be composed to `approve` by the policy engine at execute
		// time. Telling the classifier lets it POLL, so that approval can actually be given — otherwise the
		// action is refused with nobody ever asked (measured: 13 such sessions in 24h).
		// THE PER-RUNG TRUTH TABLE (spec/028 REQ-2807/2809). This is the ONE translation between the ladder's
		// policy.Level and the runner's decoupled LadderRung, so "graduated?" and "needs a notice?" can never
		// be wired to disagree — a disagreement whose failure mode is silent: an auto_notice class acting with
		// nobody paged.
		//
		//   policy.LevelApprove    -> RungApprove     poll, so the approval the engine wants is askable
		//   policy.LevelAutoNotice -> RungAutoNotice  no poll; AUTO_NOTICE band floor (acts and pages)
		//   policy.LevelAuto       -> RungAuto        no poll, no floor (acts silently)
		//
		// An unrecognised level maps to RungApprove (fail closed), matching the ladder's own treatment of a
		// corrupt persisted level.
		// A named function, not an inline literal, for ONE reason: an aliveness oracle must be able to drive
		// the SAME code the shipped binary wires. A truth table re-typed inside a test proves the test's copy
		// is right and says nothing about the binary — which is exactly how a control ships unreachable.
		LadderRungFor: func(opClass string) runner.LadderRung { return ladderRungFor(bLadder, opClass) },
		// WHO may approve a POLL_PAUSE poll, and WHETHER the bundle declares an approver regime at all
		// (spec/015 REQ-1516, TG-254). Both are resolved at gate time over the policy engine and carried into
		// the workflow's history, so the vote-wait admits the voter from a RECORDED set rather than a live read
		// (Temporal determinism).
		//
		// The pair is what makes the control safe to ship to an ALREADY-RUNNING deployment. Under a configured
		// bundle an empty set admits nobody — the fail-closed direction, and the permissive default is the
		// defect this closes. Under an UNCONFIGURED bundle (no rule declares approve_by anywhere, which is what
		// the live estate runs today) admission is INERT instead, because refusing an empty set there would
		// make every poll unvotable rather than making any action safer.
		ApproveByFor:        bApproveByFor,
		ApproveByConfigured: bApproveByConfigured,
		// The actor-attribution plane (spec/023): the registered domain readers, the taxonomy→disposition
		// rules-as-data, and the deterministic attributor's config (self-identity from the credential
		// configuration, sanctioned principals + carve-outs from the ruleset).
		ActorReaders:       actorReaders,
		AttributionMapping: attributionMapping,
		AttributionConfig:  attributionCfg,
		SanctionResolver:   sanctionResolver, // spec/023 identity/auth enrichment; nil ⇒ static sanction only
		// TG-466 slice 2: the grounded positive observed-mutation signal (modules/cmdb/pve/confighash). nil
		// (TG_PVE_CONFIGHASH_ENABLED unset — the ship-dark default) ⇒ AttributeActivity passes the zero
		// Observation and the covered-but-empty escalation stays unreachable, byte-identical to today.
		GuestConfigChangedWithin: guestConfigChangedWithin,

		// A wide predicted estate blast radius ceilings the action at AUTO_NOTICE (never silent AUTO). The
		// blast radius is computed over the causal estate graph; today the graph is empty so no host is wide
		// (correct — an empty estate makes no wide claim), and this goes live as the topology readers seed it.
		BlastRadiusWide: func(host string) bool {
			g := estateHolder.Graph()
			e, ok := g.Resolve(host)
			if !ok {
				return false
			}
			return len(g.BlastRadius(e, 3)) >= blastWide
		},
		// Common-cause SIBLING corroboration (axis A2 blast-radius precision): SiblingsOf returns a host's estate
		// co-tenants; RecentAlertHosts reports which of them are also alerting. GateActivity keeps the alert-class
		// common-cause gate ONLY when >=2 co-tenants alert too, so an isolated hosted-guest down does not fan a
		// speculative 26-54-host sibling cascade. Both read live state; nil-safe (fail-open in the activity).
		SiblingsOf: siblingsOfReader(estateHolder),
		// The rollback necessity probe's SERVICE lane (TG-464): a `systemctl is-active` read over the
		// actuation identity — nil on the triage plane (planeEnv withholds the key), wired on actuate.
		ServiceActive: serviceActiveReader(planeEnv("TG_ACTUATION_SSH_KNOWN_HOSTS", ""), planeEnv("TG_ACTUATION_SSH_IDENTITY", ""), config.SecretRef(planeEnv("TG_ACTUATION_SSH_KEY", ""))),
		// TG-483: the terminus collateral re-check's two seams. BlastMembers enumerates the causal
		// blast-radius MEMBERS (same graph, same depth as BlastRadiusWide's width test — the set the heal
		// can plausibly have perturbed, anchor included); empty until the topology readers seed the graph,
		// which the activity reports as UNKNOWN, never as an all-clear. CollateralOpenedSince asks TG's own
		// durable per-delivery alert capture (first-surfaced-since semantics, incident's own rule-family
		// excluded) — wired only with a database, like every durable read here.
		BlastMembers: func(host string) []string {
			g := estateHolder.Graph()
			e, ok := g.Resolve(host)
			if !ok {
				return nil
			}
			out := []string{e.Name}
			for _, imp := range g.BlastRadius(e, 3) {
				out = append(out, imp.Entity.Name)
			}
			return out
		},
		// (CollateralOpenedSince — the read half of this pair — is bound below, only with a database.)
		// TG-200 (A2/A6): the compact persistent-world-model seed block for the alerting host, built over the
		// SAME live estate graph. Reads the graph fresh per incident (a promotion/decay applies to the next
		// investigation); "" until the topology readers seed the graph, so the <estate> seed is inert today.
		EstateSeed: func(host string) string { return estateSeedBlock(estateHolder.Graph(), host) },
		// TG-394 slice 3 (part 4): which of TG's OWN dependency capabilities are currently degraded — any with a
		// backing host that has no fresh edge in the estate graph. Stamped on the session record so a lexical-only
		// investigation (its embed backend unreachable) is legible afterwards. Reads the SAME live graph the
		// reachability metric does, over the boot-resolved capability wiring; "" wiring resolves to the empty set,
		// so an unwired/compose-internal deployment records "nothing degraded" honestly rather than guessing.
		DegradedCapabilities: func() []string { return degradedCapabilitySet(estateHolder.Graph(), selfDepReachCaps) },
		RecentAlertHosts:     recentAlertHosts,
		Gate: &predict.PredictionGate{
			Store: predStore, // pgx-backed (durable) when TG_DB_DSN is set, else the in-memory oracle twin
			// TG-378: the seal-time state precondition's reader. nil (no DB) ⇒ the gate REFUSES any
			// op-class declaring requires_target_state — unknown is not not-running.
			GuestRunning: guestRunningReader(guestLivenessStore.Load(), guestLivenessBound),
			// The prediction gate reads the multi-source causal estate graph (core/estate), not a flat
			// adjacency map. The graph is seeded per-source-isolated; the NetBox/LibreNMS/PVE topology
			// readers are wired next, so today it is empty and an unresolvable target fails closed on
			// eligibility — the correct, non-vacuous behavior, not the empty-map dead capability it replaces.
			Model: &predict.InfragraphModel{
				EstateProvider: estateHolder.Graph,
				Graph:          predict.NewDependencyGraph(map[string][]string{}), // retained for the shuffled control path
				MaxDepth:       3,
				// axis A2 (blast-radius precision): drop blast-radius/sibling impacts below this path-product
				// confidence. 0 (default) = behavior-preserving (keeps every impact); tune toward ~0.70 to cut the
				// low-confidence far/learned-edge false positives. The same threshold gates the negative control.
				MinConfidence: envFloat("TG_PREDICT_MIN_CONFIDENCE", 0),
			},
			Mode: predict.ModeEnforce,
		},
		Ledger:           ledger,
		ManifestSink:     manifestSink,
		ManifestBackfill: manifestBackfill,
		Mutation:         chokepoint,
		// The wired-by-construction actuation chain + the durable readers the execute activity uses to
		// reconstruct the governed Request from state. nil (no DB) ⇒ the execute activity is a no-op. The
		// grounded-territory set is EMPTY here: a territory ack only GATES ops that classify INTO a high-stakes
		// territory (territory.Permit matches a Target/Op/OpClass keyword) — the curated restart/reload family
		// carries no such keyword, so the empty ack alone does NOT refuse them. The real fresh-deploy fail-closed
		// for those classes is the compound of: Shadow mode (default), the EMPTY effect-leaf allowlist, the
		// per-incident novelty poll, and an empty estate — NOT the territory ack. A Phase-2 flip populates the
		// territory acks deliberately, per territory (config-not-code), for the ops that DO classify into one.
		Interceptor:  interceptor,
		RegimeEngine: regimeEngine,                   // spec/017: route the execute dispatch through SelectLane → LaneEffect
		LaneEffect:   laneEffect,                     // nil (no DB/engine) ⇒ the execute activity falls back to Interceptor.Do
		AsyncLaunch:  asyncLaunchSeam(asyncLauncher), // TG-122 slice 0: nil-guarded (the typed-nil-in-interface trap, poll_queue.go)
		// awx-launch op-class → AWX template id (config-not-code, TG_AWXJOB_ALLOWLIST). FAIL-CLOSED: unset/empty/
		// ambiguous ⇒ resolves nothing ⇒ an awx op cannot encode a launch ⇒ refused. AWX is unconfigured here.
		AWXTemplateForOpClass: awxTemplateResolver(getenv("TG_AWXJOB_ALLOWLIST", "")),
		// TG-122 slice 3: the k8s-declarative op-class → gitops-mr ProposeSpec resolver (config-not-code,
		// plane-scoped — the map names repos whose api-scoped PATs live in the plane-scoped allowlist). Empty
		// map ⇒ every declarative op fails closed (no MR opened). Dark today: no op-class declares effect_kind
		// k8s-declarative, so this resolver is never consulted.
		GitOpsMRProposeForOpClass: gitopsProposeResolver(planeEnv("TG_GITOPSMR_PROPOSE_MAP", "")),

		Manifests:    manifestReader,
		Predictions:  predReader,
		Verdicts:     verdictReader,
		Pending:      pendingWriter,
		Acknowledged: map[territory.Territory]bool{},
		// The compact terminal triage record (REQ-1106) — the judge cron's input. nil without a DB.
		TriageRecord: triageRecord,
		// The confirmed-clear follow-up mark (axis A3, migration 0039) — nil without a DB.
		TriageMarkCleared: triageMarkCleared,
		TriageMarkMutated: triageMarkMutated,
		// The earned-catalog evidence seam (spec/028 REQ-2802). nil without a DB (documented inert).
		RecordProposalOccurrence: recordProposalOccurrence,
		// The classifier's prior-verdict band (spec/001 REQ-015) — nil without a DB (inert, unchanged bands).
		PriorVerdicts: priorVerdicts,
		// The pre-context CORRELATION stage (TG-169): the evidence the topology decision routes on, and the
		// audit trail it never had. Both nil without a DB ⇒ the pre-TG-169 severity fallback, recorded as
		// degraded rather than silently passed off as "nothing else was broken".
		CorrelationWindow: correlationWindow,
		ExecClassRecord:   execClassRecord,
		// TG-385/TG-376: the durable cluster join (DB-gated; nil ⇒ no collapse, every member investigates) and
		// the estate-graph oracle the causal election reads FRESH per session (empty graph ⇒ earliest-arrival
		// fallback, still one session per storm). GraphTopology reads estateHolder.Graph per call.
		ClusterJoin:     clusterJoin,
		ClusterTopology: runner.GraphTopology(estateHolder.Graph),
		Stages:          stageTally, // TG-380: record the correlate stage's offered/eligible/acted triple (same tally as suppress)
	}
	// The estate-derived site vocabulary for the verifier's coincidental-cross-site filter (spec/002 REQ-107):
	// SiteOf over the LIVE refreshable estate snapshot, so a topology refresh updates the vocabulary without a
	// restart. An estate with no seeded site entities derives nothing, and the verdict then excludes nothing —
	// config-not-code, failing closed exactly as before this seam existed. This is the mechanic whose absence
	// let a 59-second sensor flap at the OTHER site demote restart-container auto→approve (ledger seq 6555).
	wireEstateSignals(&deps, estateHolder) // site + guest + pve-node signals (TG-78); see estate_signals_wire.go
	// The post-execution observer (spec/013 verifiability gate): after a (future, gated) mutation the
	// deterministic verifier diffs the committed prediction against the REAL post-state read here — never nil,
	// which would make every action verify as match (the blind-verifier bug the readiness review flagged #1).
	// It reads the currently-firing alerts from LibreNMS (read-only, the same active-alert surface the poller
	// uses) and maps them to verify.ObservedAlert. It runs ONLY after an execution; in a non-actuating
	// mode the interceptor refuses before execute, so nothing reaches this until the mode permits. Unset (no LibreNMS) ⇒ the
	// execute activity supplies an EMPTY observation and the verdict still computes deterministically.
	if obsDeps := librenmsDeployments(getenv("TG_LIBRENMS_DEPLOYMENTS", "")); len(obsDeps) > 0 {
		obsSrc := librenms.NewAlertSource(obsDeps, librenms.WithAlertHTTPClient(estateHTTPClient(truthyEnv("TG_LIBRENMS_INSECURE"))))
		deps.PostStateObserve = func(ctx context.Context, targetHost, site string) ([]verify.ObservedAlert, bool) {
			envs, _, ferr := obsSrc.FetchActive(ctx) // obsSrc has no minAge gate (withheld always 0 here)
			if ferr != nil {
				return nil, false // fail-closed (TG-182): a read error is NOT an empty post-state — the verifier must
				// withhold its verdict (no false `match`, no graduation credit), exactly as ClearObserve does below.
			}
			out := make([]verify.ObservedAlert, 0, len(envs))
			for _, e := range envs {
				out = append(out, verify.ObservedAlert{Host: e.Host, Rule: e.AlertRule, Site: e.Site})
			}
			return out, true
		}
		// ClearObserve is the ConfirmedClear reader: the SAME read-only LibreNMS active-alert surface, but with
		// the fetch error SURFACED (ok=false) instead of collapsed to empty. The close-out clear-check must
		// distinguish "observed the host quiet" from "could not observe" — a transient LibreNMS outage returning
		// empty must NEVER read as a clear (that would false auto-close AND de-novel on zero evidence).
		deps.ClearObserve = func(ctx context.Context, host, site string) ([]verify.ObservedAlert, bool) {
			envs, _, ferr := obsSrc.FetchActive(ctx) // obsSrc has no minAge gate (withheld always 0 here)
			if ferr != nil {
				return nil, false // fail-closed: a read error is NOT a clear
			}
			out := make([]verify.ObservedAlert, 0, len(envs))
			for _, e := range envs {
				out = append(out, verify.ObservedAlert{Host: e.Host, Rule: e.AlertRule, Site: e.Site})
			}
			return out, true
		}
		log.Printf("actuation: post-execution verifier reads live LibreNMS active alerts (read-only; inert until an execution occurs)")
	}
	// The clear-confirm BELT (TG-124 Plan B): bind the durable recovery-transition log so the Runner's
	// ConfirmedClear check can confirm on TG's OWN captured provider recovery push (ingest_transition, written
	// by the front door) even when the LibreNMS re-pull lags past the bound — the observed writeback-miss case.
	// nil pool ⇒ the seam stays nil ⇒ the belt is inert (the re-pull governs alone, exactly today's behavior).
	if dbPool != nil {
		deps.RecoveredSince = db.NewTransitionLogStore(dbPool).RecoveredSince
		// TG-483: the terminus collateral read — durable per-delivery capture, mapped to the runner-local
		// hit type (the runner never imports core/db).
		colStore := db.NewAlertLogStore(dbPool)
		deps.CollateralOpenedSince = func(ctx context.Context, hosts []string, excludeHost, excludeRule string, since time.Time) ([]runner.CollateralHit, error) {
			rows, err := colStore.CollateralOpenedSince(ctx, hosts, excludeHost, excludeRule, since)
			if err != nil {
				return nil, err
			}
			out := make([]runner.CollateralHit, 0, len(rows))
			for _, r := range rows {
				out = append(out, runner.CollateralHit{Host: r.Host, AlertRule: r.AlertRule})
			}
			return out, nil
		}
		// The verifier's HOST-arm baseline (the 2026-07-28 false deviation, ledger 5153-5155): hosts already
		// holding an OPEN incident in TG's own ingest ledger when an action executes. Durable — it does not
		// share the LibreNMS HTTP surface's failure mode, which is the seam the pair baseline walked through.
		// Bounded by MaxOpenIncident on the same lost-recovery reasoning as suppression.
		deps.OpenIncidents = db.OpenIncidentsBaseline(db.NewAlertHistoryStore(dbPool), coreingest.MaxOpenIncident)
		wirePriorSessionMemory(&deps, dbPool)
		wireTransactionPlanStore(&deps, dbPool) // spec/030: the plan lane's durable recorder (fail-closed at RecordPlanActivity when absent)
		// WIRE THE GRADUATION PROMOTE TO THE SESSION TERMINUS (REQ-1223). The interceptor still records the
		// immediate outcomes it can trust — a deviation demotes and trips the breaker at once — but a verified
		// `match` no longer promotes there: that read is ~1s old against a monitoring surface whose poll cycle is
		// minutes long, so it cannot tell a heal that worked from one whose consequences have not surfaced. The
		// CLEAN RUN is asserted at the terminus instead, off the same confirmed-clean facts the novelty writeback
		// already trusts. Adapts the decoupled bool seam onto the ladder's RunOutcome so the runner never imports
		// core/policy. WITHOUT THIS WIRING NOTHING PROMOTES AT ALL — the ladder would dead-lock exactly as it did
		// before the interceptor earn-path was first wired.
		if bGraduation != nil {
			ladder := bGraduation
			credits := gradCredits // nil without a DSN ⇒ the claim is skipped, see below
			deps.RecordGraduation = func(ctx context.Context, opClass, externalRef string, cleanRun bool) error {
				outcome := policy.OutcomeUnverified // executed but not confirmable ⇒ breaks the streak, never promotes
				if cleanRun {
					outcome = policy.OutcomeVerifiedClean
				}
				// EXACTLY-ONCE CREDIT (TG-266, REQ-2804). Claim BEFORE the increment, and ONLY for the
				// promoting outcome: a streak-breaking outcome is a safety action and must never be
				// withheld by a bookkeeping key. Without a durable store the claim is skipped rather than
				// refused — an in-memory deployment has no ladder to protect (its state dies with it).
				if outcome == policy.OutcomeVerifiedClean && credits != nil {
					claimed, cerr := credits.Claim(ctx, opClass, externalRef, outcome.String())
					if cerr != nil {
						return fmt.Errorf("graduation credit unclaimable for %s/%s, run does NOT promote: %w", opClass, externalRef, cerr)
					}
					if !claimed {
						log.Printf("graduation: %s already credited for %s — replay or re-run, streak unchanged (REQ-2804)", opClass, externalRef)
						return nil
					}
				}
				// GRADUATION-WRITER: claimed — the promoting outcome is gated by credits.Claim above (exactly-once,
				// REQ-2804) and grounded by migration-0064 against the action_execution row keyed on externalRef; a
				// non-promoting outcome is a safety signal recorded unconditionally (see TG-436 seam guard).
				_, err := ladder.Record(ctx, opClass, outcome)
				return err
			}
			log.Printf("graduation: PROMOTE path wired to the session terminus (REQ-1223) — a clean run is " +
				"asserted from the confirmed-clear terminus, never from the immediate post-execution read; a " +
				"deviation still demotes at the interceptor")
		} else {
			// Loud, because the failure is invisible: with no seam NOTHING can ever promote, and the symptom is
			// simply that no op-class ever graduates — the same dead-lock that predated the earn-path wiring.
			log.Printf("graduation: NO promote path wired (no ladder) — no op-class can graduate; " +
				"the ladder is inert for promotion")
		}
		log.Printf("clear-confirm belt: RecoveredSince reads the durable recovery-transition log (ingest_transition)")
		log.Printf("terminus collateral re-check ARMED (TG-483): blast-radius members x durable alert capture (ingest_alert_occurrence); unseeded graph reads as UNKNOWN, never all-clear")
	}
	// The rolling DISCOVERY CORPUS (design-wisdom #10): the in-memory buffer the falsify Scorer captures every
	// live-scored DEVIATION into (keyed by deviation signature), and the verify-sourced disproof signal the
	// estate decay-on-disproof pass reads. Constructed unconditionally so the decay cron can snapshot it; it is
	// injected into the Scorer below only when the writeback is armed, and drained to eval/discovery-corpus.json
	// by the flush cron (#10's deferred wiring hop). Bounded (TG_DISCOVERY_CORPUS_CAP; 0 ⇒ the package default).
	discoveryCorpus := falsify.NewMemDiscoveryCorpus(envInt("TG_DISCOVERY_CORPUS_CAP", 0))
	// TG-206: back the in-process corpus with the durable pgx store when a DB is wired, so a live-scored
	// misprediction — and its cross-incident reproduction count, the promotion-gating signal — survives a
	// restart instead of being lost with the worker's memory. Without a DSN it stays the in-memory-only
	// behaviour (honest zeros, never a panic). The Mem buffer still serves the in-process flush drain; the
	// durable store adds persistence (dual write).
	var discoveryWriter falsify.DiscoveryWriter = discoveryCorpus
	if dbPool != nil {
		discoveryWriter = dualDiscoveryWriter{mem: discoveryCorpus, durable: db.NewDiscoveryStore(dbPool)}
		log.Printf("falsifiability: discovery corpus is DURABLE (discovery_deviation) — reproduction counts survive a restart")
	}
	// The verify-time FALSIFIABILITY WRITEBACK (#23 evidenced-readiness prep / #26 grounding deepening): the
	// production caller the predict → verdict → score chain never had, so SignalRatio / the grounding
	// scorecard finally reads REAL scored predictions. Every N (TG_FALSIFIABILITY_SCORE_INTERVAL) it takes the
	// committed-but-unscored predictions whose observation window has elapsed — so the cascade has had time to
	// manifest — observes the LIVE post-incident alerts through the SAME
	// read-only surface the interceptor's verifier uses (deps.PostStateObserve), and writes back the
	// confusion-matrix score + the mechanical verdict (a deviation is never-auto by construction) + one
	// windowed cascade-stats aggregate (INV-22). It fires on the READ-ONLY / propose path: a prediction is
	// committed BEFORE any action and scored AFTER observation, so this NEVER depends on mutation being ON —
	// it scores, it never actuates. Armed only with BOTH a durable store (a DSN) AND a live observer (a
	// LibreNMS deployment); without either it stays dark and the scorecard honestly reports zeros. Best-effort
	// throughout: a scoring error logs and the loop continues — it never crashes the worker or mutates the estate.
	if iv := getenv("TG_FALSIFIABILITY_SCORE_INTERVAL", "5m"); iv != "" && falsifyUnscored != nil && falsifyScores != nil && deps.PostStateObserve != nil {
		if d, derr := time.ParseDuration(iv); derr == nil && d > 0 {
			// THE LEARNED OBSERVATION WINDOW (spec/002 REQ-110, TG-220 / port-fidelity finding #20). Both bounds
			// are operator-visible: FLOOR is the window an edge with no observed history gets (the predecessor's
			// 900s DEFAULT_WINDOW_S, deliberately LONGER than the 10m constant it replaces), MAX caps what one
			// outlier observation can do (the predecessor had no cap and could strand a row unscored forever).
			windowFloor := envDuration("TG_FALSIFIABILITY_WINDOW_FLOOR", falsify.DefaultWindowFloor)
			windowCap := envDuration("TG_FALSIFIABILITY_WINDOW_MAX", falsify.DefaultWindowCap)
			latencyLookback := envDuration("TG_FALSIFIABILITY_LATENCY_LOOKBACK", falsify.DefaultLatencyLookback)
			// The retired constant. It is NOT silently honoured as the new floor: it meant "the whole window" and
			// could be set BELOW 900s, which is exactly the miss-manufacturing direction TG-220 closes.
			if legacy := getenv("TG_FALSIFIABILITY_WINDOW", ""); legacy != "" {
				log.Printf("falsifiability writeback: TG_FALSIFIABILITY_WINDOW=%s is RETIRED and IGNORED — the "+
					"observation window is now learned per edge, max(floor, 2x p95) capped; set "+
					"TG_FALSIFIABILITY_WINDOW_FLOOR / TG_FALSIFIABILITY_WINDOW_MAX instead", legacy)
			}
			scorer := &falsify.Scorer{
				Unscored: falsifyUnscored, Scores: falsifyScores, ForecastVerdicts: falsifyVerdicts,
				CascadeStats: falsifyCascade, Observe: falsify.Observer(deps.PostStateObserve),
				Discovery:   discoveryWriter, // capture each forecast-graded deviation into the rolling corpus (#10), durably backed when a DB is wired (TG-206)
				WindowFloor: windowFloor, WindowCap: windowCap, LatencyLookback: latencyLookback,
				Batch: envInt("TG_FALSIFIABILITY_BATCH", 200),
			}
			// The DURABLE latency evidence the window is learned from: TG's own front-door ledger (ingest_alert),
			// per ordered (primary → dependent) host pair — the same key and the same stream the estate's learned
			// confidence tier already uses. Deterministic code, no model call. Unwired (no DSN) or unreadable ⇒
			// every edge stays on the FLOOR, so the fail direction is "wait longer", never "score early".
			if dbPool != nil {
				scorer.Latency = db.CascadeLatencies(db.NewCascadeLatencyStore(dbPool), windowCap, falsify.LatencySampleCap)
			}
			// THE COMMIT-TIME BASELINE (Phase C4): the (host,rule) pairs + open-incident hosts already firing at
			// each prediction's CommittedAt, read back from TG's own durable ingest ledger — the same two-arm
			// shape the interceptor captures pre-execution (REQ-1228), anchored by the ledger instead of a live
			// snapshot. Without it every ambient alert reads as a forecast's failed cascade, which is how
			// prediction_verdict reached 19/19 deviation all-time. The scorer refuses to author a forecast
			// verdict outside an established baseline and skips (retries) a prediction whose baseline read fails.
			if dbPool != nil {
				scorer.Baseline = db.FalsifyBaseline(db.NewAlertHistoryStore(dbPool), coreingest.MaxOpenIncident)
			}
			// The estate-derived site vocabulary (spec/002 REQ-107): the coincidental-cross-site filter keys on
			// SiteOf over the LIVE estate snapshot — a host provably on the OTHER site is background noise, an
			// unknown-site host is never excluded (fail closed).
			scorer.HostSite = func(host string) (string, bool) { return estateHolder.Graph().SiteOf(host) }
			go func() {
				t := time.NewTicker(d)
				defer t.Stop()
				for range t.C {
					sctx, cancel := context.WithTimeout(context.Background(), d)
					res, serr := scorer.ScoreDue(sctx)
					switch {
					case serr != nil:
						log.Printf("falsifiability writeback: score pass failed: %v (retry next tick)", serr)
					case res.Scored > 0:
						log.Printf("falsifiability writeback: scored %d prediction(s) [real_tp=%d control_tp=%d forecast_deviations=%d executed=%d skipped=%d deferred=%d widest_window=%s] — measurement only; never actuates",
							res.Scored, res.SumRealTP, res.SumControlTP, res.Deviations, res.Executed, res.Skipped, res.Deferred, res.WidestWindow)
					case res.Deferred > 0:
						// Loud, because a deferral streak is otherwise INVISIBLE: nothing failed and nothing scored,
						// so a pathologically wide learned window would look exactly like an idle estate. Bounded by
						// TG_FALSIFIABILITY_WINDOW_MAX, so this can never run forever.
						log.Printf("falsifiability writeback: %d prediction(s) deferred — inside their LEARNED observation window (widest %s, cap %s); nothing failed, the cascade evidence is not in yet",
							res.Deferred, res.WidestWindow, windowCap)
					}
					cancel()
				}
			}()
			log.Printf("falsifiability writeback: verify-time scoring every %s (observation window LEARNED per edge: max(%s, 2x p95 observed cascade latency), capped %s, latency lookback %s) — reads live post-incident alerts; never actuates",
				d, windowFloor, windowCap, latencyLookback)
			// The DISCOVERY-CORPUS FLUSH cron (design-wisdom #10 deferred hop): periodically DRAIN the in-memory
			// rolling corpus the Scorer just captured into into the durable eval/discovery-corpus.json via the
			// provided pure fn eval.IngestCaptured, so captured deviations survive the rolling cap and the process
			// (the three-set flywheel's discovery set). The in-memory Snapshot is CUMULATIVE, so the cron feeds
			// IngestCaptured only the per-signature reproduction DELTA since the last successful flush (tracked in
			// flushed) — repeated flushes never double-count. Fail-safe: a load/save error logs and the loop
			// continues; it NEVER crashes the worker and NEVER mutates the estate (measurement-plane only).
			discoveryFile := getenv("TG_DISCOVERY_CORPUS_FILE", "eval/discovery-corpus.json")
			flushInterval := envDuration("TG_DISCOVERY_FLUSH_INTERVAL", 0) // OFF by default — opt-in, needs a writable/mounted corpus path
			flushed := map[string]int{}
			flushDiscovery := func() {
				snap := discoveryCorpus.Snapshot()
				var batch []falsify.CapturedDeviation
				for _, cd := range snap {
					if delta := cd.Reproductions - flushed[cd.Record.DeviationKey()]; delta > 0 {
						b := cd
						b.Reproductions = delta
						batch = append(batch, b)
					}
				}
				if len(batch) == 0 {
					return // nothing new captured since the last flush
				}
				corpus, lerr := eval.LoadDiscoveryCorpus(discoveryFile)
				if lerr != nil {
					log.Printf("discovery-corpus flush: load %s failed: %v (retry next tick)", discoveryFile, lerr)
					return
				}
				added := corpus.IngestCaptured(batch)
				if serr := corpus.Save(discoveryFile); serr != nil {
					log.Printf("discovery-corpus flush: save %s failed: %v (in-memory retained; retry next tick)", discoveryFile, serr)
					return
				}
				for _, cd := range snap {
					flushed[cd.Record.DeviationKey()] = cd.Reproductions // advance the drained baseline
				}
				if dropped := discoveryCorpus.Dropped(); len(dropped) > 0 {
					log.Printf("discovery-corpus flush: note %d signature(s) rolled off the in-memory cap since boot", len(dropped))
				}
				log.Printf("discovery-corpus flush: drained %d capture-delta record(s) (%d new case(s)) into %s", len(batch), added, discoveryFile)
			}
			if flushInterval > 0 {
				go func() {
					t := time.NewTicker(flushInterval)
					defer t.Stop()
					for range t.C {
						flushDiscovery()
					}
				}()
				log.Printf("discovery-corpus flush: draining to %s every %s (measurement-plane; never actuates)", discoveryFile, flushInterval)
			} else {
				log.Printf("discovery-corpus flush: disabled (TG_DISCOVERY_FLUSH_INTERVAL unset) — deviations still captured in-memory + read by the decay pass, but not persisted to %s", discoveryFile)
			}
		} else if derr != nil {
			log.Printf("falsifiability writeback: invalid TG_FALSIFIABILITY_SCORE_INTERVAL %q — scoring disabled", iv)
		}
	} else if falsifyUnscored != nil && deps.PostStateObserve == nil {
		log.Printf("falsifiability writeback: idle — no live post-incident observer (TG_LIBRENMS_DEPLOYMENTS unset); the grounding scorecard honestly reports zeros")
	}
	// The read-only CONFIDENCE CALIBRATOR (spec/020 T-020-15, REQ-2021): periodically join the persisted agent
	// confidence (session_triage.confidence, migration 0024) to the LLM-free verified falsify outcome
	// (infragraph_prediction, by external_ref, migration 0026) and log the reliability curve (Brier/ECE/MCE).
	// OBSERVE-ONLY — it adjudicates nothing and gates nothing; the policy min_confidence clamp stays OFF until an
	// operator judges the reliability trustworthy (INV-22). Armed only with a DSN; without one it stays dark.
	// Best-effort: a read error logs and the loop continues — it NEVER crashes the worker or mutates the estate.
	// Today it honestly logs "no evidence yet" (the confidence + external_ref plumbing is new; 0 paired rows)
	// until fresh triage sessions flow.
	if iv := getenv("TG_CALIBRATION_INTERVAL", "15m"); iv != "" && dbPool != nil {
		if d, derr := time.ParseDuration(iv); derr == nil && d > 0 {
			calibJob := calibratejob.Job{
				Reader: db.NewCalibrationReadStore(dbPool),
				Bins:   envInt("TG_CALIBRATION_BINS", 10),
				Limit:  envInt("TG_CALIBRATION_SAMPLE_LIMIT", 5000),
				// EmitTo, not LogReliability: the curve goes to /metrics AS WELL AS the log, so the answer to
				// "does the agent's confidence mean anything" is visible to anyone who was not reading worker
				// output at the moment the pass ran.
				Emit: calibratejob.EmitTo(obsRegistry),
			}
			// RunPeriodically, NOT an inline ticker loop: it runs one pass IMMEDIATELY and is covered by an
			// oracle that fails if that first pass is ever deferred again (temporal/calibrate/periodic_test.go).
			// A bare `for range t.C` left the calibration gauges ABSENT for a full interval after every worker
			// start — and absent is not zero, so `tg_confidence_samples == 0` could not observe it. Measured
			// live: a worker recreated at 21:07 published nothing until 21:22, while the dashboard showed the
			// PREVIOUS container's value carried forward by Prometheus' 5-minute lookback.
			go calibratejob.RunPeriodically(context.Background(), calibJob, d, func(cerr error) {
				log.Printf("confidence calibrator: pass failed: %v (retry next tick)", cerr)
			})
			// THE SECOND REFERENCE CLASS (TG-335). The curve above scores blast-radius EXACTNESS
			// (fp = 0 AND fn = 0), which is a specific claim rather than "the diagnosis was right", and the
			// confidence alerts instruct the reader to compare the two before calling the agent overconfident.
			// That comparison was impossible from the running system: one outcome was computed, `outcome` was a
			// label with one value, and OutcomeDiagnosisCorrect was a constant nothing produced.
			//
			// Same job, same emitter, different reference class — so the two curves cannot drift apart in how
			// they are computed, only in what they are computed ABOUT. Both stay observe-only.
			diagJob := calibratejob.Job{
				Reader: db.DiagnosisSampleReader{
					Store: db.NewCalibrationReadStore(dbPool),
					// The rubric is 1-5 and a reliability curve needs a boolean, so this threshold is a
					// judgement. 4 is "the judge found the diagnosis correct" with the top two bands counting;
					// it is configurable precisely because it is a choice, and it is published beside the
					// curve's own base rate so a reader can see how easy the target is.
					CleanScore: envFloat("TG_CALIBRATION_DIAGNOSIS_CLEAN_SCORE", 4),
				},
				Bins:  envInt("TG_CALIBRATION_BINS", 10),
				Limit: envInt("TG_CALIBRATION_SAMPLE_LIMIT", 5000),
				Emit:  calibratejob.EmitFor(metrics.OutcomeDiagnosisCorrect, obsRegistry),
			}
			go calibratejob.RunPeriodically(context.Background(), diagJob, d, func(cerr error) {
				log.Printf("confidence calibrator (diagnosis): pass failed: %v (retry next tick)", cerr)
			})
			log.Printf("confidence calibrator: reliability scoring every %s — observe-only, min_confidence gate stays OFF until calibrated", d)
		} else if derr != nil {
			log.Printf("confidence calibrator: invalid TG_CALIBRATION_INTERVAL %q — calibration disabled", iv)
		}
	}
	// Ledger-head anchor tamper-evidence (TG-80 P1#1 record + TG-509 consuming verify), carved into
	// wireLedgerAnchor (ledger_anchor_wiring.go); pure relocation.
	wireLedgerAnchor(dbPool)
	// TG-510 Slice A consuming half — periodic corpus tamper-evidence verify against its recorded witnesses,
	// carved into wireCorpusVerify (corpus_verify_wiring.go); pure relocation.
	wireCorpusVerify(corpusEvidence, corpusPath)
	// THE EARNED-CATALOG CLUSTERING PASS (spec/028 REQ-2811/REQ-2812, epic TG-227 Stage 2). Recurring
	// free-form proposals accrue into op-class CANDIDATES an operator later ratifies from an evidence
	// dossier — the predecessor EARNED its autonomy; TG demanded its authorship, and this is the seam that
	// closes that gap. OBSERVE-ONLY: the pass can advance a candidate to ratify_ready and no further, so no
	// bug here can manufacture a capability (the state machine has no edge to `ratified` at all).
	//
	// Ready is LIVE (TG-227 blocker 1): the estate blast-radius walk supplies coverage through the same
	// holder the prediction path reads, so the resolver sees the CURRENT graph on every pass. The
	// fail-closed legs live where they always did — MeetsRatifyReady holds a candidate below the gate on
	// ambiguous family, uncomputable coverage, or an unstamped screen; a nil graph is simply 0 coverage.
	if dbPool != nil {
		// TG-267, second half: every catalog pair still absent after assembly registers as
		// declared-but-disabled, so the projection carries the WHOLE catalog and "off" stops being
		// indistinguishable from "invisible".
		if n := declareCatalogAbsent(moduleReg); n > 0 {
			log.Printf("module registry: %d catalog module(s) declared but not constructed this boot (registered disabled)", n)
		}
		// The worker publishes its module enablement so the API process can stop guessing (TG-251). The
		// interval is the freshness contract: the grounder treats rows older than 3x this as unknown.
		go runCapabilityProjection(context.Background(), moduleReg, db.NewCapabilityProjectionStore(dbPool),
			envDuration("TG_CAPABILITY_PROJECTION_INTERVAL", time.Minute), log.Printf)
		// The credential-onboarding work list (TG-274), published the same way and for the same reason: the
		// inventory sources live HERE and the console is served by the grounder. Every source that can
		// report its credential->scope bindings joins the screen by implementing Discovered().
		if disc := discoverableSources(credSources); len(disc) > 0 {
			go runCredentialBindingProjection(context.Background(), disc,
				db.NewCredentialBindingProjectionStore(dbPool),
				envDuration("TG_CAPABILITY_PROJECTION_INTERVAL", time.Minute), log.Printf)
		}
	}
	// Earned-catalog clustering pass (spec/028 REQ-2811/REQ-2812, epic TG-227 Stage 2), carved into
	// wireOpclassClustering (opclass_clustering_wiring.go); pure relocation.
	wireOpclassClustering(dbPool, ledger, estateHolder)
	// Shared recency/decay reconciliation (design-wisdom #11): lessons provenance-prune, learn half-life,
	// estate decay-on-disproof, carved into wireDecayReconciliation (decay_reconciliation_wiring.go); pure
	// relocation.
	wireDecayReconciliation(dbPool, reconcileLessons, learner, discoveryCorpus, estateHolder, publishEstate)
	// The NOVELTY gate reads the prior-incident count for a (host, rule) signature from the knowledge corpus
	// (the prior-resolved-incident history the retriever already ranks over). A loaded corpus lets novelty be
	// POSITIVELY established: a never-seen (host, rule) → count 0 forces a poll (the first time a class is ever
	// seen a human enters the loop, spec/001). No corpus ⇒ PriorIncidents stays nil ⇒ novelty is UNKNOWN and
	// the gate does NOT fire (no false positives — a missing corpus never invents a poll; the mechanical floor
	// still governs). This activates the classifier's novelty gate, which was inert without a data source.
	if knowledgeHolder != nil {
		deps.PriorIncidents = func(host, alertRule string) (int, bool) {
			return knowledgeHolder.Count(host, alertRule), true
		}
	} else {
		// S5 (flywheel-audit): a missing/unreadable corpus disables novelty fleet-wide. That is the deliberate
		// fail-SAFE-on-this-axis design (unknown ⇒ don't invent a poll, no false positives) — but it must be
		// VISIBLE, or a forgotten TG_KNOWLEDGE_FILE silently removes the one control meant to force a human onto
		// a never-seen (host,rule). Warn loudly; actuation stays governed by graduation + band + floor + mode.
		log.Printf("WARNING knowledge: no prior-incident corpus (TG_KNOWLEDGE_FILE unset or the corpus failed to load) — the per-incident NOVELTY gate is DISABLED fleet-wide (the spec/001 first-sight-human poll will NOT fire). Set TG_KNOWLEDGE_FILE to a corpus to restore it; actuation remains governed by graduation, risk band, the never-auto floor, and the mode chokepoint.")
	}
	// The novelty WRITEBACK feeder (TG-124): the LIVE close-out counterpart to the operator-export lessons
	// feed (appendLessons, above), carved into wireNoveltyWriteback (novelty_writeback_wiring.go); pure
	// relocation.
	wireNoveltyWriteback(&deps, knowledgeHolder, corpusPath, &lessonsMu, persistCorpus, loadCorpus, syncEmbed)
	// The investigation reasons WITH the authoritative CMDB record when NetBox is registered (INV-17: a
	// resolved, enabled CMDB capability) — the read-only reconciliation step. Fail-open: an unregistered CMDB
	// leaves CMDBResolve nil, and a lookup miss/error returns found=false, so a CMDB problem never blocks triage.
	if cmdbReader, cerr := resolve.CMDB(moduleReg, "netbox"); cerr == nil {
		deps.CMDBResolve = func(ctx context.Context, kind, id string) (cmdb.Entity, bool) {
			e, rerr := cmdbReader.Resolve(ctx, kind, id)
			if rerr != nil {
				return cmdb.Entity{}, false
			}
			return e, true
		}
	}
	// THE ENTRY TICKET — the incident's own ticket, which four separate capabilities depend on: the
	// investigation reads it, the terminal reconcile close-out transitions it, the learned scheduled-reboot
	// lane consults it, and the dedup stage asks it whether a parent incident is still open.
	//
	// This was bound only when EXACTLY ONE tracker was enabled, with NO else arm and no seam. Two
	// configured trackers — ServiceNow for ITSM and YouTrack for engineering, the ordinary shape at an
	// established site — took all four dark at once and nothing anywhere said so. The fourth is not a
	// degradation but a wrong answer until TG-354: with gate.OpenIssue nil, core/suppression/dedup.go
	// suppressed a re-fire whose parent ticket had RESOLVED under the reason "duplicate of an open incident
	// within window", asserting an openness nothing checked; the dedup stage now fails toward surfacing (a
	// suppression must be BACKED by a confirmed-open parent), so a STALE unconfirmable re-fire escalates —
	// whenever openness can't be confirmed, not only after RESOLVED — while TG-459 still dedups a re-fire that
	// arrives within the short recency sub-window (a rapid duplicate of a still-open incident). The count loop
	// carried the same last-wins bug the
	// notifier's did — it kept only the LAST enabled source type while counting all of them.
	trackerSrcs := make([]string, 0, 2)
	for _, cp := range moduleReg.Capabilities() {
		if cp.Surface == modules.SurfaceTracker && cp.Enabled {
			trackerSrcs = append(trackerSrcs, cp.SourceType)
		}
	}
	sort.Strings(trackerSrcs) // deterministic ownership-resolution order across boots
	trackerCount := len(trackerSrcs)
	trackersByName := make(map[string]tracker.Tracker, trackerCount)
	var trackerResolveErrs []string
	for _, src := range trackerSrcs {
		tr, terr := resolve.Tracker(moduleReg, src)
		if terr != nil {
			trackerResolveErrs = append(trackerResolveErrs, fmt.Sprintf("%s: %v", src, terr))
			continue
		}
		trackersByName[src] = tr
	}
	if len(trackerResolveErrs) > 0 {
		log.Printf("tracker: %d of %d enabled tracker(s) failed to resolve — %s",
			len(trackerResolveErrs), trackerCount, strings.Join(trackerResolveErrs, "; "))
	}

	darkTracker := func(why wiring.Because) tracker.Tracker {
		return wiring.Absent[tracker.Tracker](wiringManifest, wiring.SeamTrackerEntry, why)
	}
	var entryTracker tracker.Tracker
	switch {
	case trackerCount == 0:
		entryTracker = darkTracker(wiring.Because{
			Reason:      "no tracker is configured in this deployment",
			Consequence: "no ticket is read, transitioned, or consulted for dedup openness",
			Owner:       "@ncpjfuzl", Ticket: "TG-245", Expiry: time.Date(2026, time.August, 31, 0, 0, 0, 0, time.UTC),
		})
		log.Printf("tracker: NONE configured — the entry ticket is not read and no close-out is written (seam %s dark)", wiring.SeamTrackerEntry)
	case len(trackersByName) == 0:
		entryTracker = darkTracker(wiring.Because{
			Reason: fmt.Sprintf("all %d enabled tracker(s) failed to resolve: %s",
				trackerCount, strings.Join(trackerResolveErrs, "; ")),
			Consequence: "no ticket is read, transitioned, or consulted for dedup openness",
			Owner:       "@ncpjfuzl", Ticket: "TG-245", Expiry: time.Date(2026, time.August, 31, 0, 0, 0, 0, time.UTC),
		})
		log.Printf("tracker: all %d enabled tracker(s) failed to resolve — the entry ticket is NOT readable", trackerCount)
	case len(trackersByName) == 1 && trackerCount == 1:
		// Bind derives liveness from the value, so a nil tracker here reports dark whatever the count said.
		entryTracker = wiring.Bind(wiringManifest, wiring.SeamTrackerEntry, trackersByName[trackerSrcs[0]])
		log.Printf("tracker: entry ticket read/transitioned via %s", trackerSrcs[0])
	default:
		// ROUTED BY REF OWNERSHIP, never fanned out. An external ref belongs to exactly one tracker, so a
		// write goes only to the tracker that holds it — broadcasting a TransitionState would resolve an
		// unrelated incident in a second system on nothing more than an id-shape coincidence.
		router := tracker.NewMultiTracker(trackersByName)
		entryTracker = wiring.Bind(wiringManifest, wiring.SeamTrackerEntry, tracker.Tracker(router))
		log.Printf("tracker: entry ticket ROUTED across %d tracker(s): %s (a ref is read/written only in the tracker that owns it)",
			router.Len(), strings.Join(router.Sources(), ", "))
	}
	// The entry-ticket creator's reconciling pass (TG-490), carved into wireTrackerCreate
	// (tracker_create_wiring.go); pure relocation.
	wireTrackerCreate(dbPool, trackersByName, trackerSrcs, entryTracker)
	if entryTracker != nil {
		deps.TrackerRead = func(ctx context.Context, id string) (tracker.Issue, bool) {
			iss, rerr := entryTracker.Open(ctx, id)
			// Fail-open is correct here and is precisely why the pair matters: every lookup returns
			// (zero, false) on error and triage carries on, so a tracker that has stopped answering is
			// indistinguishable from an estate whose incidents carry no tickets — unless someone counts.
			wiringYield.Observe(wiring.SeamTrackerEntry, 1, boolCount(rerr == nil), time.Now().UTC())
			if rerr != nil {
				return tracker.Issue{}, false // read-only and fail-open: a tracker problem never blocks triage
			}
			return iss, true
		}
		// The TERMINAL reconcile close-out (spec/003) transitions the ticket at a finished session — a
		// tracker write (annotate/transition), never an estate mutation. nil ⇒ the reconcile records no
		// close-out (fail-safe).
		deps.Tickets = runner.NewTrackerTransitioner(entryTracker)
	}
	// The human channel: deliver the governance notice/poll to on-call across EVERY enabled notifier
	// (MECH-719). Best-effort and fail-open: nil ⇒ no delivery, and NotifyActivity swallows a delivery
	// error so a notifier outage never fails the Runner. Paging is the Phase-0/1 human-in-the-loop
	// channel, not an estate mutation (never mutation-gated).
	//
	// Until 2026-08-01 a sink was bound ONLY when exactly one notifier was enabled, on the reading that
	// several channels are "ambiguous for a single bound decision". They are not: INV-12 binds a VOTE to
	// the decision it answers, which is a property of the vote, not a cap on how many places the question
	// is asked. What the old shape actually produced was silence — an operator configuring matrix AND SMS
	// AND email, buying redundancy on the page that wakes them, got nothing delivered at all, and each
	// channel they added made the silence more certain. The count loop carried the matching bug: it kept
	// only the LAST enabled source type, so it could never have named more than one channel anyway.
	notifierSrcs := make([]string, 0, 2)
	for _, cp := range moduleReg.Capabilities() {
		if cp.Surface == modules.SurfaceNotifier && cp.Enabled {
			notifierSrcs = append(notifierSrcs, cp.SourceType)
		}
	}
	sort.Strings(notifierSrcs) // deterministic attempt order, so boot logs and error text do not reshuffle
	notifierCount := len(notifierSrcs)

	// Resolve each enabled notifier INDEPENDENTLY. One broken vendor must not take the others down with
	// it: three configured channels and one unresolvable adapter is still two live pages.
	notifierSinks := make([]notifier.Notifier, 0, notifierCount)
	var notifierResolveErrs []string
	for _, src := range notifierSrcs {
		nf, nerr := resolve.Notifier(moduleReg, src)
		if nerr != nil {
			notifierResolveErrs = append(notifierResolveErrs, fmt.Sprintf("%s: %v", src, nerr))
			continue
		}
		notifierSinks = append(notifierSinks, nf)
	}
	if len(notifierResolveErrs) > 0 {
		log.Printf("notifier: %d of %d enabled notifier(s) failed to resolve and will NOT be delivered to — %s",
			len(notifierResolveErrs), notifierCount, strings.Join(notifierResolveErrs, "; "))
	}
	// EVERY branch records at the wiring seam, including the implicit else. The shape is a switch with a
	// default rather than an if/else-if precisely because the missing else WAS the defect: with zero
	// notifiers configured nothing ran, deps.Notify stayed nil, and every governance notice — including a
	// judge-death page that fired on 2026-08-01 — degraded to log.Printf on stdout, reaching no operator
	// and reported by nothing. `deploy.TestNotifierWiringDeclaresInEveryBranch` fails if a branch is added
	// here without a Bind or an Absent, so this cannot silently regrow a dark path.
	darkNotify := func(why wiring.Because) func(context.Context, notifier.Notice) error {
		return wiring.Absent[func(context.Context, notifier.Notice) error](wiringManifest, wiring.SeamGovNotify, why)
	}
	switch {
	case notifierCount > 0 && len(notifierSinks) == 0:
		// Enabled, and not one of them resolved. Previously the single-notifier arm of this shape left
		// deps.Notify nil with NO log line at all — a second dark path in the same twelve lines, which
		// nobody had named until the seam forced every branch to answer.
		deps.Notify = darkNotify(wiring.Because{
			Reason: fmt.Sprintf("all %d enabled notifier(s) failed to resolve: %s",
				notifierCount, strings.Join(notifierResolveErrs, "; ")),
			Consequence: "governance notices and approval pages reach no operator",
			Owner:       "@ncpjfuzl", Ticket: "TG-239", Expiry: time.Date(2026, time.August, 31, 0, 0, 0, 0, time.UTC),
		})
		log.Printf("notifier: all %d enabled notifier(s) failed to resolve — governance notices NOT delivered", notifierCount)
	case notifierCount == 1 && len(notifierSinks) == 1:
		// Bind derives liveness from the value, so a nil sink here reports dark no matter what the count
		// said. The count and the binding are different facts; only the binding pages anyone.
		nf := notifierSinks[0]
		deps.Notify = wiring.Bind(wiringManifest, wiring.SeamGovNotify,
			func(ctx context.Context, n notifier.Notice) error {
				err := nf.Notify(ctx, n)
				// One notice OFFERED; one PRODUCED only if it actually landed. A seam that counted the
				// attempt as the outcome would report a healthy yield through a total outage.
				wiringYield.Observe(wiring.SeamGovNotify, 1, boolCount(err == nil), time.Now().UTC())
				return err
			})
		log.Printf("notifier: governance notices/polls delivered via %s", notifierSrcs[0])
	case len(notifierSinks) > 0:
		// FAN-OUT. Success if ANY channel delivered; an all-channel failure is an error, loudly — the
		// composite refuses to report success when the notice reached nobody, which is the same defect
		// class as a Page() that returns nil while the escalation lands in a log file.
		fan := notifier.NewFanout(notifierSinks...)
		deps.Notify = wiring.Bind(wiringManifest, wiring.SeamGovNotify,
			func(ctx context.Context, n notifier.Notice) error {
				rep, ferr := fan.NotifyReport(ctx, n)
				// OFFERED counts the channels attempted, PRODUCED the ones that landed — so a fan-out
				// degrading from three channels to one is visible as a widening gap rather than as an
				// unchanged "delivered" boolean.
				wiringYield.Observe(wiring.SeamGovNotify, rep.Attempted, rep.Delivered, time.Now().UTC())
				// A PARTIAL delivery is a success — the human was reached — so a dead channel can only
				// reach an operator through this line. Returning it as an error instead would escalate on
				// a page that actually worked.
				if len(rep.Failures) > 0 {
					log.Printf("notifier fan-out: decision %s delivered to %d of %d channel(s); failed — %s",
						n.DecisionID, rep.Delivered, rep.Attempted, strings.Join(rep.Failures, "; "))
				}
				return ferr
			})
		log.Printf("notifier: governance notices/polls FANNED OUT to %d channel(s): %s (delivery succeeds if any one lands)",
			fan.Len(), strings.Join(fan.Sources(), ", "))
	default:
		deps.Notify = darkNotify(wiring.Because{
			Reason:      "no notifier is configured in this deployment",
			Consequence: "governance notices and the judge-death page reach no operator surface",
			Owner:       "@ncpjfuzl", Ticket: "TG-239", Expiry: time.Date(2026, time.August, 31, 0, 0, 0, 0, time.UTC),
		})
		log.Printf("notifier: NONE configured — governance notices and pages reach no operator (seam %s dark)", wiring.SeamGovNotify)
	}
	// TG-386: arm the proposal-less-handoff page. A substantive investigation that concludes "a human is
	// needed, no safe action exists" is routed to the notifier — but ONLY when the operator opts in, because
	// it opens a new outward paging path. INERT by default: unset ⇒ the workflow still schedules the notice
	// (its eligibility is recorded) but NotifyActivity does not deliver it.
	deps.HandoffNotify = truthyEnv("TG_HANDOFF_NOTIFY_ENABLED")
	if deps.HandoffNotify {
		log.Printf("runner: proposal-less-handoff page ARMED (TG_HANDOFF_NOTIFY_ENABLED) — a substantive no-action conclusion pages on-call")
	}

	// The prober set for the console's Test button.
	//
	// The notifiers are already offered by the registry sweep above (they are registered modules, and
	// resolve.Notifier only type-asserts the same Adapter this offered). Nothing to add here.
	moduleProbers := probeReg.set()

	// The scheduled module-probe sweep (see cmd/worker/probe_sweep.go for why the console TEST button alone
	// is not enough), carved into wireModuleProbeSweep (module_probe_sweep_wiring.go); pure relocation.
	wireModuleProbeSweep(moduleProbers, notifierSinks)

	// ── THE INBOUND VOTE LANE (seam vote.inbound) ────────────────────────────────────────────────────
	//
	// Every notifier module implements ResolveVote and NOTHING called any of them — six implementations,
	// fully unit-tested, with no production caller. So TG posted MSC3381 approval polls a human could
	// click, and the click reached nothing; votes only ever arrived through the console.
	//
	// Armed only for Matrix today, because Matrix is the one backend with a pull-based inbound API the
	// worker can read without an exposed webhook. Slack/Teams/email need an inbound HTTP route, which is
	// a different (and more exposed) design decision — declared dark rather than half-built.
	darkVotes := func(why wiring.Because) {
		wiring.Absent[func()](wiringManifest, wiring.SeamVoteInbound, why)
	}
	var matrixSink notifier.Notifier
	for _, sink := range notifierSinks {
		if sink.SourceType() == "matrix" {
			matrixSink = sink
		}
	}
	switch {
	case matrixSink == nil:
		darkVotes(wiring.Because{
			Reason:      "no matrix notifier is configured, and it is the only backend with an inbound reader",
			Consequence: "an approval poll posted to chat cannot be answered there; every vote must go through the console",
			Owner:       "@ncpjfuzl", Ticket: "TG-251", Expiry: time.Date(2026, time.September, 1, 0, 0, 0, 0, time.UTC),
		})
		log.Printf("votes: no inbound reader — approval polls are answerable only in the console (seam %s dark)", wiring.SeamVoteInbound)
	case dbPool == nil:
		// Without the pending projection there is no way to resolve a decision's sealed action id, and a
		// vote that does not name its action is recorded-and-ignored by the Runner (INV-12). Delivering
		// votes that can never count would be worse than not reading them.
		darkVotes(wiring.Because{
			Reason:      "no database pool — the sealed action id for an open decision cannot be resolved",
			Consequence: "inbound votes could not name the action they decide, so the Runner would ignore every one",
			Owner:       "@ncpjfuzl", Ticket: "TG-251", Expiry: time.Date(2026, time.September, 1, 0, 0, 0, 0, time.UTC),
		})
		log.Printf("votes: inbound reader NOT armed — no durable pending-decision store (seam %s dark)", wiring.SeamVoteInbound)
	default:
		pendingRead := db.NewPendingStore(dbPool)
		// TG-463 (B26): the voter-alias normalizer — presented chat identity → the canonical login the
		// frozen approve_by entries carry. Resolution only, never a wider set; unknown ⇒ passthrough ⇒
		// the frozen set refuses exactly as before. A silent armed control is indistinguishable from an
		// unwired one, so both postures log.
		voterAliases := parseVoterAliases(getenv("TG_VOTER_ALIASES", ""))
		if len(voterAliases) > 0 {
			log.Printf("votes: voter-alias normalizer ARMED (TG-463) — %d alias(es) map chat identities to canonical approve_by logins; unknown identities pass through and the frozen set refuses them", len(voterAliases))
		} else {
			log.Printf("votes: voter-alias normalizer PASSTHROUGH (TG_VOTER_ALIASES unset/empty) — chat voters must match the frozen approve_by entries verbatim")
		}
		lane := &matrixVoteLane{
			homeserver: getenv("TG_MATRIX_HOMESERVER", ""),
			tokenRef:   config.SecretRef(getenv("TG_MATRIX_TOKEN_REF", "env:MATRIX_TOKEN")),
			rooms:      voteRoomSet(getenv("TG_MATRIX_DEFAULT_ROOM", ""), getenv("TG_MATRIX_ROOMS", "")),
			http:       &http.Client{Timeout: 30 * time.Second},
			resolve:    matrixSink.ResolveVote,
			normalize:  func(presented string) string { return normalizeVoter(voterAliases, presented) },
			// SERVER-SIDE action resolution. The answer id a client returns could be forged by any
			// approver; TG's own projection cannot be.
			actionFor: func(ctx context.Context, decisionID string) (string, bool) {
				open, oerr := pendingRead.OpenDecisions(ctx)
				if oerr != nil {
					return "", false
				}
				for _, d := range open {
					if d.ExternalRef == decisionID {
						return d.ActionID, true
					}
				}
				return "", false
			},
			signal: func(ctx context.Context, decisionID, actionID string, approve bool, voter string) error {
				return c.SignalWorkflow(ctx, tg.WorkflowID(decisionID), "", runner.VoteSignalName,
					runner.VoteSignal{Approve: approve, Voter: voter, ActionID: actionID})
			},
			observe: func(offered, delivered int) {
				wiringYield.Observe(wiring.SeamVoteInbound, offered, delivered, time.Now().UTC())
			},
		}
		wiring.Bind(wiringManifest, wiring.SeamVoteInbound, lane)
		votePoll := envDuration("TG_MATRIX_VOTE_POLL_INTERVAL", 15*time.Second)
		go lane.run(context.Background(), votePoll)
		log.Printf("votes: inbound matrix reader armed (every %s) — an approval poll clicked in chat now reaches the waiting decision", votePoll)
	}
	// The dropped-escalation requeue lane (spec/003 BEH-3) wired into the worker (Gulli ch12 — recovery must
	// be REACHABLE): an orphaned poll the reconciler requeues, or a judge-demotion escalation, is fired on a
	// cadence by the FireDue cron so it re-escalates / pages / stands down instead of sitting in the queue
	// forever. Constructed only with a durable store (a DSN) — without one there is nowhere durable to enqueue,
	// so the lane is inert. Nothing here mutates the estate: it re-enters the gated pipeline via the
	// authenticated signal and pages humans (mutation stays OFF).
	var escalationController *coreesc.Controller
	if escalationStore != nil {
		// The re-check re-decides on the LIVE condition: still-active ⇒ re-escalate + page the approver graph;
		// recovered ⇒ defer closure. With no live active-alert oracle wired the condition FAILS SAFE to
		// still-active (escalate to a human — never silently drop an unresolved incident). The pager is the
		// human notifier channel (Approval=false — an escalation PAGE, not a poll); no notifier ⇒ a logging pager.
		reCheckCap := envInt("TG_ESCALATION_RECHECK_CAP", 3)
		escalationController = coreesc.NewController(escalationStore, failSafeActive{},
			wiring.Bind(wiringManifest, wiring.SeamEscalationPage, notifierPager{
				notify: deps.Notify,
				yield: func(offered, produced int) {
					wiringYield.Observe(wiring.SeamEscalationPage, offered, produced, time.Now().UTC())
				},
			}), reCheckCap)
		// The reconcile→escalation re-check hand-off (spec/003 REQ-206): an UNRESOLVED reconcile decision (an
		// orphaned poll) is requeued into THIS lane for a delayed re-check — rate-capped by the per-incident cap
		// (ScheduleReCheck stands down to a human at the cap). The delay is config-not-code.
		reCheckDelay := envDuration("TG_ESCALATION_RECHECK_DELAY", 15*time.Minute)
		deps.ReCheckSchedule = func(ctx context.Context, ref string, attempts int) error {
			_, err := escalationController.ScheduleReCheck(ctx, ref, attempts, time.Now().Add(reCheckDelay))
			return err
		}
		log.Printf("escalation requeue lane: durable store wired (per-incident cap %d, re-check delay %s) — fires via the FireDue cron, pages via the notifier; never actuates", reCheckCap, reCheckDelay)
	} else {
		// Found by deploy.TestEveryWiringConditionalDeclaresInAllBranches, which generalised the
		// notifier-specific guard into the rule it was an instance of. Without a DSN there is no durable
		// queue, so the escalation lane is legitimately inert — but "inert by design" and "silently
		// unrecorded" must not look the same, which is the entire lesson of the two live dark components.
		wiring.Absent[coreesc.Pager](wiringManifest, wiring.SeamEscalationPage, wiring.Because{
			Reason:      "no TG_DB_DSN: there is no durable escalation queue to page from",
			Consequence: "a re-check that would have paged an approver has nowhere to be enqueued",
			Owner:       "@ncpjfuzl", Ticket: "TG-239", Expiry: time.Date(2026, time.October, 30, 0, 0, 0, 0, time.UTC),
		})
		log.Printf("escalation requeue lane: NO durable store (TG_DB_DSN unset) — the lane is inert (seam %s dark)", wiring.SeamEscalationPage)
	}

	// Tier-1 suppression's FIRST gate: operator-declared maintenance/chaos freeze windows (config-not-code,
	// TG_SUPPRESSION_FREEZE_FILE). An alert inside an active, in-scope window is an EXPECTED effect of declared
	// maintenance and is suppressed before spending a session — even at critical severity (the operator knows
	// it is coming). Wired only when windows are declared; otherwise the chain stays nil and every incident is
	// investigated (fail-open). Each decision is hash-chained into the governance ledger (INV-19).
	freezePath := getenv("TG_SUPPRESSION_FREEZE_FILE", "")
	windows := freezeWindows(freezePath)
	// The SECOND freeze source (spec/019, TG-411): live MAINTENANCE windows read from the estate's own
	// scheduler. Cronicle has run on the box for weeks, the connector is complete and its spec Ratified, but
	// no composition root ever constructed it — so the sensor yielded nothing. Unset TG_CRONICLE_DEPLOYMENTS
	// ⇒ no providers ⇒ the sensor stays dark (inert). When declared, an active sanctioned maintenance window
	// is projected into the freeze plane on each reload below, so a change's expected alerts are suppressed.
	cronicleProvs := cronicleProviders(getenv("TG_CRONICLE_DEPLOYMENTS", ""))
	rules := suppressRules(getenv("TG_SUPPRESSION_RULES_FILE", ""))
	var dedupWindow time.Duration
	if dw := getenv("TG_SUPPRESSION_DEDUP_WINDOW", ""); dw != "" {
		if d, derr := time.ParseDuration(dw); derr == nil && d > 0 {
			dedupWindow = d
		} else {
			log.Printf("suppression: invalid TG_SUPPRESSION_DEDUP_WINDOW %q — dedup disabled", dw)
		}
	}
	patterns := suppressPatterns(getenv("TG_SUPPRESSION_PATTERNS_FILE", ""))
	schedules := suppressSchedules(getenv("TG_SUPPRESSION_SCHEDULES_FILE", ""))
	// Asymmetric scheduled-reboot window [fire − pre-buffer, fire + post-window]: a reboot alert normally
	// arrives AFTER the fire (detection lag + reboot duration), so the post-window (default 10m) is wider than
	// the pre-buffer (default 5m) — the predecessor's DEFAULT_PRE_BUFFER_MINUTES / DEFAULT_WINDOW_MINUTES.
	rebootPre := 5 * time.Minute
	if rt := getenv("TG_SUPPRESSION_REBOOT_PRE_BUFFER", ""); rt != "" {
		if d, derr := time.ParseDuration(rt); derr == nil && d > 0 {
			rebootPre = d
		}
	}
	rebootWin := 10 * time.Minute
	if rt := getenv("TG_SUPPRESSION_REBOOT_WINDOW", ""); rt != "" {
		if d, derr := time.ParseDuration(rt); derr == nil && d > 0 {
			rebootWin = d
		}
	}
	folds := foldPolicies(getenv("TG_SUPPRESSION_FOLDS_FILE", ""))
	// The reboot-class ALLOWLIST as data (config-not-code, the predecessor's REBOOT_RULE_PATTERNS): which
	// alert rules the scheduled-reboot lane may apply to at all. Unset ⇒ the compiled default set.
	deps.RebootRules = suppression.RebootRules{Patterns: splitList(getenv("TG_SUPPRESSION_REBOOT_RULES", ""))}
	// The LEARNED scheduled-reboot lane (spec/005 REQ-409..411, TG-219). It LANDS DARK — the predecessor's
	// TIER1_SCHED_REBOOT_ENABLED discipline: an operator arms it deliberately, and until then TG honors only
	// operator-DECLARED schedules exactly as before. Armed, it observes reboot-class alerts the chain did NOT
	// suppress, registers a recurring CLEAN-boot pattern as OBSERVING, promotes it to LIVE only after two
	// verified in-window occurrences, and unlearns it on proof that it silenced a real incident.
	var rebootStore persist.ScheduledRebootStore
	if dbPool != nil {
		rebootStore = db.NewScheduledReboots(dbPool) // TG-225: durable learned-schedule persistence
	}
	rebootLearner, demotionLookup, evidenceStore, demoter := learnedRebootLane(
		truthyEnv("TG_SUPPRESSION_LEARN_ENABLED"), rebootPre, rebootWin, entryTracker, deps.Notify, ledger, rebootStore)
	// freezePath != "" arms the chain even with ZERO windows declared today: without it, a freeze file that
	// is empty at boot leaves the whole tier-1 gate nil and the reload below has nothing to reload into.
	if freezePath != "" || len(cronicleProvs) > 0 || len(windows) > 0 || len(folds) > 0 || len(rules) > 0 || len(patterns) > 0 || len(schedules) > 0 || dedupWindow > 0 || rebootLearner != nil {
		gate := &runner.LiveSuppressGate{
			Folds: folds, FoldFreshness: 100 * 365 * 24 * time.Hour, // operator-declared policies have no learned staleness — only the valid window gates
			Schedules: schedules, RebootPreBuffer: rebootPre, RebootWindow: rebootWin,
			Patterns: patterns, Rules: rules,
			Learn: rebootLearner, Demotions: demotionLookup, Evidence: evidenceStore,
			LearnRenewFor: suppression.DefaultLearnValidity,
			Window:        dedupWindow, Ledger: ledger, Log: runner.NewRecentTriageLog(dedupWindow),
			Stages: stageTally, // TG-380: record the suppress stage's offered/eligible/acted triple
		}
		// ARMED BY THE FILE, NOT BY ITS CURRENT CONTENTS. This used to be `if len(windows) > 0`, which made
		// the gate nil forever whenever the file was empty at boot — and the file is empty at boot precisely
		// when nobody has declared a window yet, which is every ordinary day. A window declared later could
		// then never take effect, because there was nothing to reload into.
		if freezePath != "" || len(cronicleProvs) > 0 {
			// The freeze plane now has TWO config-not-code sources, merged on every reload: operator-declared
			// windows in the freeze FILE, and live MAINTENANCE windows read from the declared Cronicle
			// instances (spec/019, TG-411). mergedFreezeWindows re-reads BOTH so a window declared in either
			// takes effect without a restart. Cronicle FAILS CLOSED — an unreadable schedule contributes
			// nothing (the estate stays open to triage), never a stale freeze.
			mergedFreezeWindows := func() []suppression.FreezeWindow {
				w := freezeWindows(freezePath)
				if len(cronicleProvs) > 0 {
					w = append(w, cronicleFreezeWindows(context.Background(), cronicleProvs, time.Now())...)
				}
				return w
			}
			fg := &suppression.FreezeGate{Windows: mergedFreezeWindows()}
			gate.Freeze = fg
			// RE-READ WITHOUT A RESTART. A file window carries an absolute Start/End and the file was read
			// exactly ONCE, at boot — so declaring a maintenance window meant editing JSON on the box AND
			// restarting the very process that would observe it. A Cronicle maintenance window is recurrence-
			// based and its ACTIVE span moves with the clock, so it too must be re-derived on a cadence.
			// Restarting the worker is itself a disruption during maintenance, and a file written once decays
			// as its windows expire. Measured 2026-08-06: "tier-1 gate active — 0 freeze, 0 fold(s),
			// 0 schedule(s), 0 pattern(s), 0 rule(s)" while the wiring register reported the chain STARVED on
			// 162 offered alerts.
			//
			// Every source returns NOTHING on an unreadable/malformed input, so a broken file or an
			// unreachable scheduler re-opens the estate to full triage rather than silently freezing it — the
			// safe direction, and the reason a reload can be unattended. Only a CHANGE is logged.
			freezeEvery := envDuration("TG_SUPPRESSION_FREEZE_RELOAD", time.Minute)
			if freezeEvery > 0 {
				go func(every time.Duration, held int) {
					t := time.NewTicker(every)
					defer t.Stop()
					for range t.C {
						if n := fg.Replace(mergedFreezeWindows()); n != held {
							log.Printf("suppression: freeze windows reloaded — %d active (was %d); a window declared in "+
								"the freeze file or as a Cronicle maintenance window takes effect within %s, no restart", n, held, every)
							held = n
						}
					}
				}(freezeEvery, len(fg.Snapshot()))
				srcs := "freeze file"
				switch {
				case freezePath == "":
					srcs = "Cronicle maintenance windows"
				case len(cronicleProvs) > 0:
					srcs = "freeze file + Cronicle maintenance windows"
				}
				log.Printf("suppression: freeze windows re-read every %s (%s; %d active at boot) — an operator can "+
					"declare a maintenance window WITHOUT restarting the worker", freezeEvery, srcs, len(fg.Snapshot()))
			}
		}
		// The SCHEDULED demote pass — the unlearning half. A learned pattern proven to have silenced an
		// incident that needed action is already demoted in-path; this pass turns that proof into the durable,
		// audited, auto-expiring analysis-only row the chain consults on every later decision (spec/005
		// REQ-411, spec/004 REQ-301/304). Learning ships with unlearning or neither ships.
		if rebootLearner != nil {
			demoteEvery := envDuration("TG_SUPPRESSION_DEMOTE_INTERVAL", time.Hour)
			go func() {
				t := time.NewTicker(demoteEvery)
				defer t.Stop()
				for range t.C {
					n, derr := gate.DemotePass(context.Background(), demoter, 24*time.Hour, time.Now())
					if derr != nil {
						log.Printf("suppression(demote pass): %v (retry next tick)", derr)
						continue
					}
					if n > 0 {
						log.Printf("suppression(demote pass): %d learned pattern(s) demoted to analysis-only on suppression-miss evidence", n)
					}
				}
			}()
			log.Printf("suppression: LEARNED reboot lane ARMED — observe→verify→promote (threshold %d, validity %s), demote pass every %s",
				suppression.PromotionThreshold, suppression.DefaultLearnValidity, demoteEvery)
		}
		// When a tracker is wired, dedup only holds while the anchor incident is CONFIRMED still OPEN: a
		// re-fire whose parent ticket has RESOLVED — or whose state cannot be read (a read error is not an
		// open-confirmation) — is a genuine new incident and escalates, at ANY age (TG-354). The TG-459
		// short-recency fallback does NOT apply on this wired-tracker path: it fires ONLY when NO tracker is
		// wired at all (gate.OpenIssue nil), where openness is genuinely unknowable and a fresh re-fire is a
		// rapid duplicate of a plausibly-still-open incident.
		if entryTracker != nil {
			gate.OpenIssue = func(issueRef string) bool {
				iss, rerr := entryTracker.Read(context.Background(), issueRef)
				if rerr != nil {
					return false
				}
				return iss.State == tracker.StateOpen || iss.State == tracker.StateInProgress
			}
		}
		deps.Suppress = gate
		suppGate.Store(gate) // expose the gate's decision counts to the telemetry loop
		wiring.Bind(wiringManifest, wiring.SeamSuppression, gate)
		// Report the freeze count from the ARMED gate (file windows + live Cronicle maintenance windows), not
		// the boot-time file slice, so the line reflects the whole freeze plane — this is the number TG-411's
		// acceptance watches stop reading zero once a Cronicle maintenance window is active.
		freezeCount := len(windows)
		if gate.Freeze != nil {
			freezeCount = len(gate.Freeze.Snapshot())
		}
		log.Printf("suppression: tier-1 gate active — %d freeze, %d fold(s), %d schedule(s), %d pattern(s), %d rule(s), dedup %s", freezeCount, len(folds), len(schedules), len(patterns), len(rules), dedupWindow)
	} else {
		// THE MISSING ELSE, and this plane had no seam at all until 2026-08-01. Every TG_SUPPRESSION_* key
		// ships empty (deploy/.env.example, deploy/docker-compose.yml), so the default deployment assembles
		// no chain — and said nothing anywhere. Dark is the FAIL-OPEN direction here (no chain means every
		// incident is investigated, never fewer), which is exactly why it stayed invisible: nothing breaks,
		// TG just burns a session on alerts an operator already declared expected.
		wiring.Absent[struct{}](wiringManifest, wiring.SeamSuppression, wiring.Because{
			Reason: "no TG_SUPPRESSION_* source is configured (freeze windows, folds, rules, patterns, " +
				"schedules and dedup window are all empty)",
			Consequence: "every incident is investigated, including ones inside declared maintenance " +
				"windows and known flap patterns — fail-open, so nothing is missed, but a full triage " +
				"session is spent on alerts the operator has already said to expect",
			Owner: "@ncpjfuzl", Ticket: "TG-239 (MECH-001)", Expiry: time.Date(2026, time.October, 30, 0, 0, 0, 0, time.UTC),
		})
		log.Printf("suppression: NO tier-1 chain configured — every incident is investigated (fail-open); "+
			"declared maintenance and known flaps will each spend a session (seam %s dark)", wiring.SeamSuppression)
	}
	// The seam report is taken HERE, after every Bind/Absent site, and that ordering is LOAD-BEARING.
	// Its first production run reported escalation.page as "dark-unrecorded" while that seam was in
	// fact bound 23 lines further down: a FALSE POSITIVE, which is the worst failure a detector can
	// have, because crying wolf trains everyone to ignore it. deploy.TestWiringReportIsTakenAfterEveryBind
	// enforces the ordering so it cannot silently regress.
	// The seam report: durable, not a log line. Findings carry each seam's CONSEQUENCE verbatim so the
	// row says what the darkness costs, not merely that it exists.
	wiringFindings, wiringSamples := wiringManifest.Report(time.Now().UTC())
	for _, f := range wiringFindings {
		log.Printf("wiring: %s", f.Reason())
	}
	if err := appendWiringDarkReport(ledger, wiringFindings); err != nil {
		log.Printf("wiring: dark-seam report could not be appended to the governance ledger (non-blocking): %v", err)
	}
	// Hand the gauge to the export loop. This line replaces `_ = wiringSamples`, whose comment promised an
	// export "below" that did not exist — the control built to catch silently-dark seams was itself
	// silently dark in its alerting limb, while temporal/worlddiscovery ran unwired in production.
	wiringSampleSet.Store(&wiringSamples)

	// The YIELD half of the same report. At boot every seam reads UNOBSERVED, which is correct and is the
	// point: it names exactly which seams have no runtime coverage yet, instead of an uninstrumented
	// register reporting a clean estate. The periodic re-report below is where starvation actually shows.
	// Observe before reporting, so the first report reflects the gate's real counts rather than the
	// boot-time UNOBSERVED placeholder.
	observeSuppressionYield()
	yieldFindings, yieldSamples := wiringYield.Report(time.Now().UTC())
	if txt := wiring.YieldReportText(yieldFindings); txt != "" {
		log.Printf("wiring %s", txt)
	}
	wiringYieldSampleSet.Store(&yieldSamples)
	// Re-report on a cadence: starvation is a RUNTIME fact and a boot-time snapshot cannot see it. The
	// interval is config-not-code; 0 disables the re-report but never the register, so the gauges stay
	// live for a dashboard even when the log line is off.
	if every := envDuration("TG_WIRING_YIELD_INTERVAL", 30*time.Minute); every > 0 {
		go func() {
			t := time.NewTicker(every)
			defer t.Stop()
			for range t.C {
				observeSuppressionYield()
				fs, ss := wiringYield.Report(time.Now().UTC())
				wiringYieldSampleSet.Store(&ss)
				if txt := wiring.YieldReportText(fs); txt != "" {
					log.Printf("wiring %s", txt)
				}
			}
		}()
		log.Printf("wiring: seam-yield re-reported every %s — a seam that is bound, running and producing NOTHING is named there", every)
	}

	// NOW the manifest is complete, so the wiki's first compile can render an honest lane-health page.
	if firstWikiCompile != nil {
		firstWikiCompile()
	}

	acts := runner.NewActivities(deps)

	// ★ THE TRIAGE QUEUE (TG-153). On the triage or `both` plane this is a real Temporal worker on tg.runner,
	// registering the Runner workflow and every triage activity — above all InvestigateActivity, which drives
	// the LLM agent over untrusted alert/syslog/host content. On the ACTUATION plane it is the off-plane stub:
	// nothing below registers anywhere, and this process never polls tg.runner, so no untrusted-content
	// activity can be delivered to the process that holds the estate-mutating key. The stub REFUSES to Start,
	// so a wiring slip that tried to run it fails the boot rather than producing a silently dark worker.
	var w planeWorker = newOffPlaneWorker(tg.TaskQueueRunner, credentialPlane)
	if credentialPlane.HoldsTriage() {
		w = worker.New(c, tg.TaskQueueRunner, runnerWorkerOptions(envInt))
	}
	w.RegisterWorkflow(runner.RunnerWorkflow)
	// The operator-facing MANUAL ROLLBACK lane (TG-462): seals the inverse of an executed forward action and
	// drives it through the SAME governed chain (POLL_PAUSE → approval → interceptor, InvertsActionID set);
	// its execute step dispatches to tg.actuate, its activities register via the canonical list.
	w.RegisterWorkflow(runner.RollbackWorkflow)
	// The armed revert's dead-man window (spec/029 T-029-2/3): RunnerWorkflow starts it as a child —
	// confirmed started BEFORE the effect executes — and ABANDONS it at close, so the confirm window
	// outlives the triage session. Its confirm/hold bookkeeping runs on the triage queue; the ONE
	// step that reaches an effect leaf — the fired inverse's SealRollbackExecuteActivity — is
	// dispatched onto tg.actuate exactly like the forward execute and the manual rollback.
	w.RegisterWorkflow(runner.CommitConfirmWorkflow)
	w.RegisterWorkflow(runner.TransactionPlanWorkflow) // spec/030 (TG-58): all-or-nothing plan; inert until a recipe is declared
	// THE ORPHAN SWEEP (spec/029 T-029-3; the TG-82 review-#1 obligation): an armed commit_confirm
	// row whose deadline passed with slack has LOST its timer (the child never started, died, or
	// its resolve is stuck in a store outage) — a ghost dead-man that looks live and blocks
	// re-arms. Re-adopt each by starting the SAME deterministic-ID child with a short residual
	// window: WorkflowID dedup refuses the start while a live twin runs (never two timers over one
	// effect), and a COMPLETED twin that failed its resolve simply gets a fresh consult. Triage
	// plane only (DB read + workflow starts).
	if credentialPlane.HoldsTriage() && commitConfirmDB != nil {
		// The boot log names the armed control (the read-the-boot-log discipline: what shipped must
		// say so) — per-adoption lines are rare by design, and a silent armed sweep would be
		// indistinguishable from an unwired one.
		log.Printf("commit-confirm: orphan sweep ARMED — re-adopting armed windows >2m past deadline every 5m (deterministic child IDs make live twins unadoptable)")
		go func() {
			t := time.NewTicker(5 * time.Minute)
			defer t.Stop()
			for range t.C {
				sctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
				rows, err := commitConfirmDB.OverdueArmed(sctx, 2*time.Minute, 50)
				if err != nil {
					log.Printf("commit-confirm sweep: overdue scan failed: %v", err)
					cancel()
					continue
				}
				for _, r := range rows {
					_, serr := c.ExecuteWorkflow(sctx, client.StartWorkflowOptions{
						ID:        "commit-confirm:" + r.ExternalRef + ":" + r.ActionID,
						TaskQueue: tg.TaskQueueRunner,
					}, runner.CommitConfirmWorkflow, runner.CommitConfirmInput{
						// TG-461: thread the incident signature so a RE-ADOPTED orphan service window can still
						// durable-confirm (RecoveredSince scopes to it); without it a re-adopted window HOLDs forever.
						ActionID: r.ActionID, ExternalRef: r.ExternalRef, WindowSeconds: 60, AlertRule: r.AlertRule,
					})
					var startedErr *serviceerror.WorkflowExecutionAlreadyStarted
					switch {
					case serr == nil:
						log.Printf("commit-confirm sweep: re-adopted orphaned armed window %s/%s (deadline was %s)",
							r.ActionID, r.ExternalRef, r.DeadlineAt.Format(time.RFC3339))
					case errors.As(serr, &startedErr):
						// a live twin holds the window — exactly the healthy case
					default:
						log.Printf("commit-confirm sweep: re-adopt of %s/%s failed: %v", r.ActionID, r.ExternalRef, serr)
					}
				}
				cancel()
			}
		}()
	}
	// EVERY Runner activity registers through the ONE canonical list (runner.RegisterActivities) — the
	// same call the eval + acceptance harnesses make, so a workflow-referenced activity missing from
	// this composition root is structurally impossible. Two prod stalls on 2026-07-18
	// (RecordPendingActivity, then ResolvePendingActivity) came from hand-maintained per-site lists
	// drifting; register_test.go now proves the canonical list covers every *Activities method.
	runner.RegisterActivities(w, acts)

	// THE MODULE TEST LANE (TG-253). Registered unconditionally: the workflow is read-only, and whether a
	// given module can be tested is decided by whether a PROBER exists for it, not by whether the lane is
	// registered. A lane registered only when some module happens to be configured is a lane that reports
	// "no test implemented" for a reason the operator cannot see.
	//
	// Distinctly named at the SYMBOL (TestModuleWorkflow/TestModuleActivity) because Temporal registers by
	// bare function name — a plain Transition*/Test* would collide with another lane and panic this worker
	// at boot, which is the 2026-07-17 boot-loop this repo has already paid for.
	moduleTestActs := &moduletest.Activities{D: moduletest.Deps{Probers: moduleProbers}}
	w.RegisterWorkflow(moduletest.TestModuleWorkflow)
	// The credential engine's "Sync now" lane (TG-109): the SyncEngine lives in THIS process; the grounder
	// starts the workflow. Registered unconditionally — with no engine the activity answers "not wired".
	credSyncActs := &credentialsync.Activities{D: credentialsync.Deps{Syncer: credentialSyncSeam{fn: credentialSyncOne}}}
	w.RegisterWorkflow(credentialsync.CredentialSyncWorkflow)
	w.RegisterActivity(credSyncActs.SyncSourceActivity)
	w.RegisterActivity(moduleTestActs.TestModuleActivity)
	// REPORT THE DENOMINATOR, NOT JUST THE COUNT. "1 prober" was true for weeks while twenty-nine dialogs
	// promised a test, and the number alone could not distinguish a small fleet from a broken filter. The
	// declined list names every CONSTRUCTED module whose dialog will honestly report "no test is
	// implemented", so the gap is enumerable at boot rather than discovered by an operator pressing the
	// button.
	log.Printf("module test: %d of %d constructed module(s) can prove themselves — %v",
		len(moduleProbers), probeReg.constructed(), probeReg.keys())
	// THE CROSS-CHECK, against the catalog rather than against ourselves.
	//
	// The previous two lines report what the registry knows, and a registry can only report on modules it
	// was offered. The failure THAT cannot see is a module which implements the capability and whose
	// dialog promises a test, but whose instance no composition-root line ever hands over — the probe
	// then exists, is unit-tested, is green, and is reached by nothing. That is this codebase's signature
	// defect reproduced one level up inside the fix for it, so it is checked explicitly and out loud.
	//
	// A descriptor that promises a test and is absent here is EITHER not configured in this deployment
	// (expected and harmless) OR wired to nothing (a defect). The worker cannot always tell which, so it
	// prints the list and says so plainly rather than picking the flattering reading.
	if described, cerr := catalog.All(); cerr == nil {
		promises := map[string]bool{}
		for _, d := range described {
			if strings.TrimSpace(d.Test.Verb) != "" {
				promises[d.Surface+"/"+d.SourceType] = true
			}
		}
		// THE DEFECT CASE, SEPARATED FROM THE EXPECTED ONE. A module that promises a test and has no
		// prober is either simply not configured in this deployment (expected, and most of the list) or
		// CONSTRUCTED AND OFFERED BY NOTHING — a probe that exists, is unit-tested, is green, and is
		// reached by no line of the composition root. Those are very different facts and printing them
		// together would bury the one that is a bug; the registry knows which modules were built, so it
		// can tell them apart rather than leaving a reader to guess.
		var unwired, unconfigured []string
		for key := range promises {
			if _, ok := moduleProbers[key]; ok {
				continue
			}
			if probeReg.seen[key] {
				unwired = append(unwired, key)
			} else {
				unconfigured = append(unconfigured, key)
			}
		}
		if len(unwired) > 0 {
			sort.Strings(unwired)
			log.Printf("module test: DEFECT — %d module(s) are CONSTRUCTED and promise a TEST verb but no "+
				"composition-root line offered them a probe: %v. Their dialogs promise an action nothing "+
				"performs.", len(unwired), unwired)
		}
		if len(unconfigured) > 0 {
			sort.Strings(unconfigured)
			log.Printf("module test: %d described module(s) are not configured in this deployment, so their "+
				"TEST reports \"no test is implemented\": %v", len(unconfigured), unconfigured)
		}
		// Constructed-with-no-probe is only worth reporting for modules that HAVE a dialog. The registry
		// also holds connectors with no descriptor at all — the push-only receivers, the model provider
		// declarations, the config-free exporters — and they have no TEST button to be honest or dishonest
		// about. Listing them made a finished surface read as eleven outstanding gaps.
		var gaps []string
		for _, k := range probeReg.declinedKeys() {
			if promises[k] {
				gaps = append(gaps, k)
			}
		}
		if len(gaps) > 0 {
			log.Printf("module test: %d constructed module(s) with a dialog publish NO probe: %v", len(gaps), gaps)
		}
	}
	if skillWriteActs != nil {
		w.RegisterWorkflow(skillwrite.TransitionWorkflow)
		w.RegisterActivity(skillWriteActs.TransitionActivity)
	}
	if manifestWriteActs != nil {
		// Distinctly named at the SYMBOL (ManifestTransitionWorkflow/Activity): Temporal registers by
		// bare function name, so a plain Transition* here would collide with skillwrite's above and
		// panic this worker at boot — the 2026-07-17 boot-loop. Both are on the names guard list.
		w.RegisterWorkflow(manifestwrite.ManifestTransitionWorkflow)
		w.RegisterActivity(manifestWriteActs.ManifestTransitionActivity)
	}
	if opClassVerbActs != nil {
		// Distinctly named at the SYMBOL (OpClassVerbWorkflow/Activity) for the same bare-function-name
		// collision reason as the two lanes above — on the names guard list.
		w.RegisterWorkflow(opclassratify.OpClassVerbWorkflow)
		w.RegisterActivity(opClassVerbActs.OpClassVerbActivity)
	}
	if policyTraceActs != nil {
		// The faithful policy packet-tracer (TG-105): POST /v1/policy/trace starts THIS workflow so the
		// grounder's answer comes from the worker's ONE engine, never a grounder-side copy. Distinctly named at
		// the symbol (PolicyTraceWorkflow/Activity) — on the bare-function-name collision guard list. Read-only:
		// it evaluates and returns, actuating nothing and writing no audit row.
		w.RegisterWorkflow(policytrace.PolicyTraceWorkflow)
		w.RegisterActivity(policyTraceActs.PolicyTraceActivity)
	}
	if configWriteActs != nil {
		// Distinctly-named workflows (the bare-function-name collision guard lives in
		// temporal/skilltrial/finalizer_names_test.go — these are on that list).
		w.RegisterWorkflow(configwrite.ConfigWriteWorkflow)
		w.RegisterWorkflow(configwrite.SecretPutWorkflow)
		w.RegisterWorkflow(configwrite.SecretRewrapWorkflow)
		w.RegisterActivity(configWriteActs.ApplyConfigActivity)
		w.RegisterActivity(configWriteActs.PutSecretActivity)
		w.RegisterActivity(configWriteActs.RewrapSecretsActivity)
	}
	if modeTransitionActs != nil {
		// The operator-invoked autonomy-mode transition (spec/015 REQ-1502) — the LAST gate before the
		// mutation flip. Distinctly named (the bare-function-name collision guard is on the finalizer names
		// list). It runs on the chokepoint-bound controller; mutation stays OFF until an operator posts a flip.
		w.RegisterWorkflow(modetransition.ModeTransitionWorkflow)
		w.RegisterActivity(modeTransitionActs.ApplyModeTransitionActivity)
	}
	if engineToggleActs != nil {
		// The operator-invoked policy-engine enable/disable (spec/015 REQ-1519) — the warn-don't-block admin
		// toggle. Distinctly named (finalizer-names guard). Runs on the worker's live EngineToggle (single
		// ledger writer); a nil bound toggle (unarmed) makes the activity fail closed with ErrNoToggle.
		w.RegisterWorkflow(enginetoggle.EngineToggleWorkflow)
		w.RegisterActivity(engineToggleActs.ApplyEngineToggleActivity)
	}
	if rulesetWriteActs != nil {
		// The operator-invoked active-ruleset replacement (spec/015 REQ-1503, TG-104): validated + ledgered +
		// persisted in the single-writer worker. Distinctly named (the bare-function-name collision guard is on
		// the finalizer names list). Mutation is unaffected — a ruleset is rules-as-data; the mode chokepoint
		// still gates every actuation.
		w.RegisterWorkflow(rulesetwrite.RulesetWriteWorkflow)
		w.RegisterActivity(rulesetWriteActs.ApplyRulesetWriteActivity)
	}
	if nativeRuleActs != nil {
		// The operator-invoked native credential-rule write (TG-109, spec/016 REQ-1610): validated
		// (ParseRules, exactly one rule) + ledgered + persisted in the single-writer worker. Distinctly
		// named (the bare-function-name collision guard is on the finalizer names list). Mutation is
		// unaffected — a rule row only feeds read-only credential resolution through the sync source.
		w.RegisterWorkflow(nativerule.NativeRuleWriteWorkflow)
		w.RegisterActivity(nativeRuleActs.ApplyNativeRuleWriteActivity)
	}
	if objectGroupActs != nil {
		// The operator-invoked object-group write (TG-481, spec/016): validated (name/patterns) + ledgered +
		// persisted in the single-writer worker. Distinctly named (on the finalizer names list). Mutation is
		// unaffected — a group row only ADDS read-only credential-resolution membership.
		w.RegisterWorkflow(objectgroup.ObjectGroupWriteWorkflow)
		w.RegisterActivity(objectGroupActs.ApplyObjectGroupWriteActivity)
	}
	// The skill-flywheel finalizer/judge/generator crons + the escalation FireDue cron, carved into
	// wireSkillCrons (skill_cron_wiring.go); pure relocation.
	wireSkillCrons(w, c, skillTrialActs, skillJudgeActs, skillGenActs, escalationController)

	// ★ ARM THE GOVERNANCE MONITORS (TG-222, PORT-FIDELITY-AUDIT #15). Judge-liveness and the frontier
	// cross-check were code-complete with no constructor, no caller, and workflows defined nowhere — so a
	// judge that died mid-campaign silently invalidated the comparison window, the exact multi-week-undetected
	// class the finding names. Both now have a constructor here, a schedule, and a workflow that exists.
	//
	// DB-GATED: both read the durable session/judgment tables, so without a pool there is nothing to measure
	// and an armed-but-blind monitor would report a healthy judged fraction over zero sessions — a false
	// all-clear, worse than an honest absence. The frontier arm additionally needs a model TIER DISTINCT from
	// the local judge's: a cross-check on the same tier is the judge grading itself, which reintroduces the
	// blind spot it exists to close (docs/PORT-FIDELITY-AUDIT §3-8), so an equal tier is refused and logged.
	if dbPool != nil {
		govRead := db.NewGovernanceReadStore(dbPool,
			envDuration("TG_JUDGE_LIVENESS_WINDOW", 24*time.Hour), envInt("TG_JUDGE_LIVENESS_LIMIT", 500))
		govEsc := govEscalator{notify: deps.Notify}
		if escalationStore != nil {
			govEsc.enqueue = func(ctx context.Context, ref, _ string) error {
				_, eerr := escalationStore.Enqueue(ctx, ref, 0, time.Now().UTC())
				return eerr
			}
		}
		govActs := &tggov.Activities{
			Monitor: &coregov.JudgeLivenessMonitor{
				Sessions: govRead, Judgments: govRead, Escalation: govEsc, Halt: judgeDeadMan,
				// RECOVERY, bound to the same dead-man that halts (2026-08-06). JudgeDeadMan.Rearm calls
				// itself "the ONLY path back" and nothing outside a test called it: the mutation breaker's
				// re-arm is wired to the owner-gated mode chokepoint, this one was wired to nothing. The
				// first real halt was therefore permanent — measured live, the flywheel had graduated
				// nothing since 2026-07-31 while the generator and trial arm kept producing. The release
				// is gated on the same judge-independent measurement that trips it, over the same minimum
				// sample, above a higher fraction (hysteresis), so it is proof of life and not a timer.
				Rearm:  judgeDeadMan,
				Window: envDuration("TG_JUDGE_LIVENESS_WINDOW", 24*time.Hour),
				// The LAG lower bound excludes sessions the judge's 2-hourly cadence has not had time to reach:
				// counting them would depress the fraction and page a HEALTHY judge as dead (the -2h bound
				// restored in !61). Defaulted from the judge's own cadence, not a hand-picked number.
				Lag: envDuration("TG_JUDGE_LIVENESS_LAG", 2*time.Hour),
			},
		}
		frontierTier := strings.TrimSpace(getenv("TG_FRONTIER_JUDGE_MODEL", ""))
		localTier := judge.DefaultParams().Model
		switch {
		case frontierTier == "":
			log.Print("frontier cross-check: TG_FRONTIER_JUDGE_MODEL unset — the model-INDEPENDENT anchor is NOT armed; " +
				"judge-death detection rests on the local liveness fraction alone, which a judge writing no rows at all can evade")
		// IDENTITY, NOT SPELLING (TG-356). The name check below is necessary and NOT sufficient: a litellm
		// model_name is an alias, and several point at one upstream model on this estate — measured
		// 2026-08-06, `judge` and `fallback-deepseek` both resolve to deepseek/deepseek-v4-pro, and
		// `primary`, `fast` and `opus-cc` all resolve to openai/opus-cc. So two DIFFERENT tier names can be
		// the same model, and arming on such a pair gives the judge itself as its own independent anchor —
		// silently, past a guard that reported OK.
		//
		// Resolution is best-effort by design: an unreachable gateway must not stop the worker from arming a
		// cross-check that may well be independent. But an UNVERIFIED independence claim is said out loud
		// rather than implied by silence, because "the guard passed" and "the guard could not check" are
		// different facts and only one of them is reassuring.
		case func() bool {
			rctx, rcancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer rcancel()
			same, resolved, rerr := gw.SameUpstreamModel(rctx, frontierTier, localTier)
			switch {
			case rerr != nil || !resolved:
				log.Printf("frontier cross-check: could not resolve %q and %q to upstream models (%v) — "+
					"arming on the NAME check alone; independence is UNVERIFIED. Two different tier names "+
					"can be one model (TG-356)", frontierTier, localTier, rerr)
				return false
			case same:
				return true
			default:
				log.Printf("frontier cross-check: independence VERIFIED — %q and %q resolve to different "+
					"upstream models", frontierTier, localTier)
				return false
			}
		}():
			log.Printf("frontier cross-check: REFUSING to arm — TG_FRONTIER_JUDGE_MODEL (%q) resolves to the "+
				"SAME upstream model as the local judge tier (%q) under a different alias. That is the judge "+
				"grading itself with an extra name on it (TG-356)", frontierTier, localTier)

		case frontierTier == localTier:
			log.Printf("frontier cross-check: REFUSING to arm — TG_FRONTIER_JUDGE_MODEL (%q) equals the local judge tier, "+
				"which is the judge grading itself and reintroduces the blind spot the anchor exists to close", frontierTier)
		default:
			govActs.CrossCheck = &coregov.FrontierCrossCheckMonitor{
				// TG-356: the pair source is GATED on independence being re-verified. The boot-time
				// resolution above is one-shot and races litellm's listener; this re-attempts it on every
				// scheduled run until it resolves, and REFUSES the run outright if the two tiers turn out
				// to be one upstream model under two aliases.
				Pairs: &independenceGatedPairs{
					inner: &tggov.ModelPairSource{
						Sample: govRead, Model: agentModel, Tier: frontierTier,
						Limit: envInt("TG_FRONTIER_CROSSCHECK_SAMPLE", 20),
					},
					resolve: func(ctx context.Context) (bool, bool, error) {
						return gw.SameUpstreamModel(ctx, frontierTier, localTier)
					},
					logf:     log.Printf,
					frontier: frontierTier,
					local:    localTier,
				},
				Escalation: govEsc,
				Halt:       judgeDeadMan,
			}
			log.Printf("frontier cross-check: armed on tier %q (local judge %q), sample %d per run — "+
				"independence is re-verified on EVERY run until it resolves, and the run REFUSES if the two "+
				"turn out to be one upstream model (TG-356)",
				frontierTier, localTier, envInt("TG_FRONTIER_CROSSCHECK_SAMPLE", 20))
		}
		if gerr := armGovernanceSchedules(context.Background(), c.ScheduleClient(), w, govActs, log.Printf); gerr != nil {
			// Non-fatal, like every other cron arm — but the consequence is named, and the CI dead-man
			// (eval/ci/check-governance-schedules.sh) fails when this wiring leaves the tree at all.
			log.Printf("governance schedules: arm failed: %v — judge death is UNDETECTED until the next boot; "+
				"judged accrual is NOT halted by a monitor that never runs", gerr)
		}
	} else {
		log.Print("governance monitors: no DB pool — judge-liveness and the frontier cross-check are NOT armed " +
			"(nothing durable to measure; an armed-but-blind monitor would report a healthy fraction over zero sessions)")
	}

	// TG-80 P1.1: the Temporal-native half of the ledger-HEAD anchor (ledger_anchor_wiring.go), registered on
	// the SAME tg.runner worker `w` and governance schedules just used above — harmless on the actuation-only
	// plane (w is the no-op stub there) and self-contained on dbPool, exactly like wireLedgerAnchor's DB-only
	// witness earlier in boot.
	wireLedgerAnchorTemporalWitness(c, w, dbPool)

	// The worker admin surface (Phase-2 readiness review §4.B.2/§2): a runtime kill-switch (POST /halt →
	// gate.Disable) and a read-only /metrics exposition, on a separate internal port. The halt bearer is
	// resolved from TG_ADMIN_TOKEN_REF; unresolved ⇒ /halt is not registered (fail closed) and only /metrics
	// is served. This surface has NO enable path — /halt can only ever turn mutation MORE off.
	adminAddr := getenv("TG_WORKER_ADMIN_ADDR", ":8444")
	haltToken := ""
	if tok, terr := config.SecretRef(getenv("TG_ADMIN_TOKEN_REF", "env:TG_ADMIN_TOKEN")).Resolve(); terr == nil {
		haltToken = tok
	} else {
		log.Printf("worker kill-switch: TG_ADMIN_TOKEN_REF not resolvable (%v) — POST /halt disabled (fail closed), /metrics still served", terr)
	}
	// actorTally is passed only when readers were armed: an estate with no evidence reader has nothing to
	// report on, and emitting an empty family there would be a series that can never move.
	// The shared breaker store is handed to the admin surface so EVERY named breaker — above all the
	// per-tier PRODUCTION MODEL breakers armed above — carries a circuit_breaker_state series. Without this
	// the model breaker would be persisted but not observable, which is only half of what CONSTITUTION.md:130
	// promises and is how a tripped model plane stays invisible (TG-221).
	adm := newWorkerAdmin(chokepoint, mutationBreaker, costAcct, ledger, haltToken).
		// THE SPEND GUARD'S CONFIGURATION, not just its accountant. With no budget set there IS no
		// accountant, and every cost gauge was inside `if a.cost != nil` — so the posture that most needs
		// publishing was the one that published nothing (measured: 0 series on dc1tg01 while 3.18M
		// model tokens had been spent).
		withCostConfig(costCfg).
		// WHICH PLANE THIS IS (TG-112). Both worker processes published component="worker" and nothing
		// on the posture series told them apart.
		withPlane(string(credentialPlane)).
		// THE ACTUATION GOVERNOR (TG-286). It has always been on the real path; it has never been readable.
		withActuationLimiter(func() actuate.LimiterStats { return bActuationLimiter.Stats() }).
		withPolicyRateGovernor(func() (policy.RateGovernorStats, bool) {
			g := policyRateGovForMetrics.Load()
			if g == nil {
				return policy.RateGovernorStats{}, false
			}
			return (*g).Stats(), true
		}).
		// THE MUTATION GATE'S INPUT (TG-343). Read live from the holder rather than on a cadence: the
		// graph is in memory, so a scrape cannot read a stale copy, and a zero here is the gate having
		// nothing to refuse against rather than nothing having happened.
		withEstateSize(startEstateSizeJob(estateHolder, func() int { return int(estateSourcesFailed.Load()) })).
		// TG-394: how many of TG's OWN dependency hosts share one hypervisor — from the same live graph. Slice 1
		// covers the journal-evidence hosts (declared in TG_JOURNAL_DEPLOYMENTS as globs; 5 of the 7 concentrated
		// hosts in the pve03 incident were these). No estate identifiers are compiled in — the set is TG's own
		// config resolved against the live estate.
		withSelfDepConcentration(startSelfDepConcentrationMultiJob(estateHolder,
			selfDepCapabilities(getenv, journalDepGlobs(journal.ParseAccess(planeEnv("TG_JOURNAL_DEPLOYMENTS", "")))))).
		// TG-394 slice 3: the LIVE per-capability reachability + tg_capability_degraded rollup, over the SAME live
		// graph and the SAME boot-resolved capability wiring the runner's session degraded-set stamp reads. This is
		// the signal that was missing when TG's embedding backend went unreachable and retrieval silently ran
		// lexical-only for 11h12m with nothing reporting a reduced capability.
		withSelfDepReachable(startSelfDepReachabilityJob(estateHolder, selfDepReachCaps)).
		// TG's estate-doc grounding coverage on the surface Prometheus scrapes (TG-86 slice 1b): the ingest
		// built in slice 1 wired into the running worker; unconfigured (TG_ESTATE_DOCS_DIR unset) it emits
		// nothing, so an ungrounded deployment reads as absent rather than a silent zero.
		withEstateDocCoverage(startEstateDocCoverageJob(getenv, log.Printf)).
		// TG'S FASTEST DETECTOR, MEASURED (TG-350 follow-through). It had no series at all: its loop logs
		// only when it mints, so a quiet estate, a blind fetch and a dead goroutine were one observation —
		// and it went 147 hours without producing while 12 alerts about the guests it watches arrived from
		// other sources during the pve03 cascade.
		withPVELivenessYield(func() []metrics.Sample { return pveLivenessReg.samples(time.Now().UTC()) }).
		// TG-315: the collector's register, chained even when it is dark.
		withAuthlogYield(func() []metrics.Sample { return authlogReg.samples(time.Now().UTC()) }).
		// THE DECISION PLANE, ON THE SURFACE PROMETHEUS SCRAPES (TG-380). The gate has tallied its
		// outcomes all along; the rendering was reachable only from the observability export loop, which
		// needs TG_OBSERVABILITY_EXPORT_INTERVAL (empty in production) AND an enabled exporter (none), so
		// no `tg_suppression*` series has ever existed on this deployment.
		withSuppressionDecisions(func() []metrics.Sample {
			g := suppGate.Load()
			if g == nil {
				return nil // not armed yet — emitting zeros would assert a gate that is not there
			}
			return suppressionDecisionSamples(g.Counts(), time.Now().UTC())
		}).
		// THE DECISION-STAGE TRIPLE, ON THE SCRAPE SURFACE (TG-380). offered/eligible/acted per stage so a
		// zero is interpretable — the pve03 cascade's stages were invisible in real time. Slice 1 wires
		// suppress; the tally renders whatever stages have recorded, nothing when idle.
		withStageDecisions(stageTally.Samples).
		// THE OPERATOR-POSTURE WARNINGS, ON THE SCRAPE SURFACE (TG-506). policy.WarnFor had no production
		// caller — an allow-all rule or Full-auto mode warned no one. Now visible as tg_policy_posture_warning.
		withPolicyPostureWarnings(func() []metrics.Sample {
			f := policyPostureWarningsForMetrics.Load()
			if f == nil {
				return nil // policy engine not built yet — nothing to report
			}
			return policyPostureWarningSamples((*f)(), time.Now().UTC())
		}).
		// THE HUMAN APPROVAL QUEUE (TG-173). The rate governor answers load by routing MORE decisions to
		// the operator; until now nothing counted them, so a flood of the human gate and a quiet estate
		// published identically. Same cadence knob as the intake dead-man — both are liveness of a queue.
		// THE LIVE-DB-LEAK TRIPWIRE (TG-190a, CONSTITUTION 4.9: "synthetic canaries against an isolated
		// throwaway DB — live-DB-leak counter must stay 0"). Built BEFORE the canary injector on purpose:
		// the hazard a canary carries is a synthetic row reaching the LIVE corpus the judge scores and the
		// flywheel learns from, and shipping the injector first would run that hazard unobserved. Emitted
		// on every scrape including with no pool, so an unwired store cannot read as a clean database.
		withSyntheticLeak(func() []metrics.Sample {
			return collectSyntheticLeak(context.Background(), syntheticLeakStoreOrNil(dbPool))
		}).
		withPollQueue(startPollQueueJob(
			context.Background(), pollQueueStoreOrNil(dbPool),
			envDuration("TG_POLL_QUEUE_INTERVAL", 30*time.Second),
		)).
		// CAN TG READ THE HOSTS IT IS ASKED ABOUT? (TG-271). The host-diagnostic lane failed on 100% of its
		// calls for weeks because known_hosts covered 16 of 38 alerted hosts, and every read returned a
		// valid-looking "(host was unreachable)" sentinel that no invocation count could distinguish from
		// success. Built from the SAME env var the tools read, so the gauge cannot measure a different file.
		withKnownHostsCoverage(func() func() []metrics.Sample {
			v, entries := knownHostsCoverageInputs(getenv("TG_HOSTDIAG_KNOWN_HOSTS", ""))
			return startKnownHostsCoverageJob(
				context.Background(), alertedHostStoreOrNil(dbPool), v, entries,
				envDuration("TG_HOSTDIAG_COVERAGE_WINDOW", 30*24*time.Hour),
				envDuration("TG_HOSTDIAG_COVERAGE_INTERVAL", time.Hour),
				dnsResolvable,
			)
		}()).
		// WHAT THE UPSTREAM HAD (TG-344). The alert source doubles as a read-only prober: it counts what
		// each LibreNMS currently has firing WITHOUT admitting anything, so the arrived-count finally has
		// a denominator. Resolved from TG_LIBRENMS_DEPLOYMENTS — the list ingest itself uses — because a
		// push-only deployment has no poller and is precisely the one that needs the denominator; see
		// upstreamProbeSourceFor. No deployments configured emits nothing and says so.
		withUpstreamProbe(startUpstreamProbeJob(
			context.Background(), upstreamProberOrNil(upstreamProbeSourceFor(
				credentialPlane.HoldsTriage(),
				upstreamProbeSource,
				librenmsDeployments(getenv("TG_LIBRENMS_DEPLOYMENTS", "")),
				estateHTTPClient(truthyEnv("TG_LIBRENMS_INSECURE")),
			)),
			envDuration("TG_UPSTREAM_PROBE_INTERVAL", 2*time.Minute),
		)).
		// TG'S OWN INPUT (TG-336). A nil pool yields a reader that emits nothing and says so at boot,
		// rather than a worker that panics or — worse — one that silently watches nothing.
		withIngestFreshness(startIngestFreshnessJob(
			context.Background(), ingestFreshnessStoreOrNil(dbPool), declaredIngestSources(moduleReg),
			envDuration("TG_INGEST_FRESHNESS_INTERVAL", 2*time.Minute),
			envDuration("TG_INGEST_FRESHNESS_WINDOW", 7*24*time.Hour),
		)).
		// THE PREMISE BEHIND TG-302 (TG-345). That decision declined to seal agent_step_evidence at rest on
		// the measured fact that the corpus holds no credential material. That is a property of what the
		// estate's hosts print, not of the design, so it needs a watcher or it expires silently.
		withEvidenceShape(startEvidenceShapeJob(
			context.Background(), evidenceShapeStoreOrNil(dbPool),
			envDuration("TG_EVIDENCE_SHAPE_INTERVAL", 15*time.Minute),
		)).
		withPredictionWidth(startPredictionWidthJob(
			context.Background(), predictionWidthStoreOrNil(dbPool),
			envInt("TG_BLAST_RADIUS_WIDE_THRESHOLD", 8),
			envDuration("TG_PREDICTION_WIDTH_INTERVAL", 15*time.Minute),
		)).
		withCategoryCoverage(startCategoryCoverageJob(
			context.Background(), categoryCoverageStoreOrNil(dbPool),
			envDuration("TG_CATEGORY_COVERAGE_INTERVAL", 15*time.Minute),
		)).
		withLoopClosure(startLoopClosureJob(
			context.Background(), loopClosureStoreOrNil(dbPool),
			envDuration("TG_LOOP_CLOSURE_INTERVAL", 15*time.Minute),
		)).
		withLedgerShape(startLedgerShapeJob(
			context.Background(), ledgerShapeStoreOrNil(dbPool),
			envDuration("TG_LEDGER_SHAPE_INTERVAL", 15*time.Minute),
		)).
		// THE OWNER-SET MODE, published (see admin.go). Read through the atomic hand-off rather than
		// captured directly, because the controller is constructed ~2,900 lines above inside the DB-backed
		// branch and may legitimately be absent (no pool ⇒ no controller ⇒ fail-closed Shadow).
		withPolicyMode(func() string {
			if c := policyModeForMetrics.Load(); c != nil {
				return (*c)()
			}
			return "Shadow" // no controller bound: the chokepoint is fail-closed, and so is this reading
		}).
		// THE WIRING REGISTERS (TG-250), on /metrics unconditionally. Both sample sets are already
		// maintained above as atomic hand-offs for the exporter loop; this reads the SAME pointers, so
		// there is no second source of truth and no extra work per scrape. Reading them here rather than
		// re-computing also means a scrape can never trigger the register's own side effects.
		withWiringRegisters(func() []observability.Sample {
			var out []observability.Sample
			if ws := wiringSampleSet.Load(); ws != nil {
				out = append(out, *ws...)
			}
			if ys := wiringYieldSampleSet.Load(); ys != nil {
				out = append(out, *ys...)
			}
			return out
		}).
		withSSHCredential(sshCredReport).
		withBreakerStore(breakerStore).
		// The read lane, so POST /halt stops RECON as well as mutation and the recon counters are alertable
		// (TG-165). Without this the kill switch keeps stopping only the half that Shadow already stopped.
		withRecon(reconGovernor).
		// THE OUTBOUND LANE (TG-160). The meter installed at the top of main() counts destinations and
		// bytes; without this line those counts stay inside the process and an off-allowlist connection is
		// visible only to whoever happens to read the journal. A control nobody can alert on is a control
		// nobody has.
		withEgressMeter(egressMeter)
	if len(actorReaders) > 0 {
		adm = adm.withActorTally(actorTally)
	}
	// ★ ARM THE AXIS SAMPLER. db.AxisReadStore.Aggregate had exactly ONE caller in the tree (cmd/axisscore),
	// so A1 recall and the per-source detection-latency distribution existed only when a human ran a CLI. The
	// interval is env-gated and defaults to 15m: long enough that a 7-day aggregate is cheap, short enough that
	// a regression is visible the same shift it appears in.
	if dbPool != nil {
		axisWindow := envDuration("TG_AXIS_SAMPLE_WINDOW", 7*24*time.Hour)
		axisEvery := envDuration("TG_AXIS_SAMPLE_INTERVAL", 15*time.Minute)
		sampler := newAxisSampler(axisWindow)
		startAxisSampler(context.Background(), sampler, db.NewAxisReadStore(dbPool), axisEvery, log.Printf)
		adm = adm.withAxisSampler(sampler)
		log.Printf("axis sampler: publishing A1 recall + per-source detection latency to /metrics every %s over a %s window "+
			"(previously reachable only by running cmd/axisscore by hand)", axisEvery, axisWindow)
		// TG-180: the estate-observation census — how many live hosts TG can actually SEE, silence split from
		// health. Boot-load, DB-backed (needs the connected pool, so it lives in this dbPool != nil block, not
		// the boot-safe sampler chain); the live host set is read fresh (FreshHostNames, TG-449) and refreshes
		// with the graph, the fired-history refreshes on the next deploy.
		adm = adm.withObservationCensus(startObservationCensusJob(
			context.Background(),
			func() []string { return estateHolder.Graph().FreshObservableNames() },
			db.NewAxisReadStore(dbPool).LastAlertByHost,
			envDuration("TG_OBSERVATION_CENSUS_WINDOW", 14*24*time.Hour),
			envDuration("TG_OBSERVATION_CENSUS_REFRESH", 15*time.Minute),
			time.Now, log.Printf))
		// TG-180 PART 2: the fault-injection PROBE — the census's null test. This publishes the coverage-of-the-
		// unmeasured scorecard dimension (how much of the census's blindness has been TESTED, not merely asserted)
		// and declares the probe's arming posture. It is DEFAULT-OFF: TG_OBSERVE_PROBE_ENABLED gates the (owner-
		// armed, not-wired-here) injection loop, so with it unset the numerator is 0 and NOTHING is injected. Same
		// live host set + window as the census, so the unobservable denominator agrees with tg_observation_census.
		adm = adm.withObservationProbe(startObservationProbeJob(
			context.Background(),
			func() []string { return estateHolder.Graph().FreshObservableNames() },
			db.NewAxisReadStore(dbPool).LastAlertByHost,
			db.NewObservationProbeStore(dbPool).ProbeConfirmedHosts,
			envDuration("TG_OBSERVATION_CENSUS_WINDOW", 14*24*time.Hour),
			envDuration("TG_OBSERVATION_CENSUS_REFRESH", 15*time.Minute),
			truthyEnv("TG_OBSERVE_PROBE_ENABLED"),
			time.Now, log.Printf,
			// TG-180: persist each census refresh (migration 0106) so the grounder's axis scorer publishes
			// coverage-of-the-unmeasured from durable rows, not only Prometheus.
			db.NewObservationCoverageStore(dbPool).Record))
		// TG-180 PART 2, the PERTURBING arm (DARK by default). The gauge above PUBLISHES the coverage-of-the-
		// unmeasured; this starts the loop that actually TESTS it by injecting a real fault on a guinea-pig. It is
		// default-OFF twice over: unless the owner supplies the pool + snapshot node + allowlist (TG_OBSERVE_PROBE_*)
		// it returns without a goroutine, and even configured it injects only when TG_OBSERVE_PROBE_ENABLED is set.
		// The unobservable seam reuses the SAME census (live host set + window) the gauge's denominator is computed
		// from, so the loop probes exactly the population the coverage number counts — no new DB connection.
		startObservationProbeLoop(
			context.Background(), dbPool,
			func() []string {
				lf, err := db.NewAxisReadStore(dbPool).LastAlertByHost(context.Background())
				if err != nil {
					log.Printf("observation probe loop: unobservable census read failed (%v) — no candidates this cycle", err)
					return nil
				}
				window := envDuration("TG_OBSERVATION_CENSUS_WINDOW", 14*24*time.Hour)
				return observe.Census(estateHolder.Graph().FreshObservableNames(), lf, time.Now().Add(-window)).HostsInState(observe.Unobservable)
			},
			time.Now, log.Printf)
	}
	// The carve-out expiry gauges: emitted whenever carve-outs are declared, independent of whether any
	// reader was armed — an unarmed estate can still hold a security-path suspension that is about to lapse.
	if len(attributionCfg.CarveOuts) > 0 {
		adm = adm.withCarveOuts(attributionCfg.CarveOuts)
	}
	startWorkerAdmin(adminAddr, adm)

	// ★ THE ACTUATION QUEUE (TG-153). RunnerWorkflow schedules ExecuteActivity — the only Runner activity that
	// can reach a credential which mutates the estate — onto tg.actuate (temporal/runner/workflow.go). This is
	// the worker that serves it.
	//
	// Under the DEFAULT `both` plane this process starts BOTH workers, so the dispatch is served in the same
	// process, by the same deps, as it was before TG-153: an existing single-worker deployment upgrades with
	// no configuration change and no behavioural difference. Under `triage` this block does not run at all —
	// the process does not poll tg.actuate, and an actuation task is not refused but undeliverable. Under
	// `actuation` it is the ONLY worker started, and it registers exactly one activity.
	//
	// It is started (not Run) so the run loop below still owns the process lifetime and one interrupt stops
	// both. A failure to start is FATAL: silently continuing would leave every gated action queued forever on
	// a queue nothing polls, which looks like "TG proposed and nothing happened" — the least debuggable
	// possible failure of an actuation path.
	if credentialPlane.HoldsActuation() {
		aw := worker.New(c, tg.TaskQueueActuate, worker.Options{})
		runner.RegisterActuationActivities(aw, acts)
		if err := aw.Start(); err != nil {
			log.Fatalf("actuation worker failed to start on queue %s: %v — every gated action would queue forever on a queue nothing polls", tg.TaskQueueActuate, err)
		}
		defer aw.Stop()
		log.Printf("actuation plane: worker up on queue=%s (plane=%s) — the estate-mutating activity runs HERE; mutation itself stays gated by the mode chokepoint (may_actuate=%v)", tg.TaskQueueActuate, credentialPlane, chokepoint.MayActuate())
	} else {
		log.Printf("actuation plane: this process does NOT poll %s (plane=%s) — it holds no estate-mutating credential, so an actuation task is not merely refused here, it is undeliverable", tg.TaskQueueActuate, credentialPlane)
	}

	if !credentialPlane.HoldsTriage() {
		// Actuation-only process: there is no tg.runner worker to Run, so block on the interrupt channel and
		// let the started actuation worker serve until the process is signalled.
		log.Printf("actuation-only worker up — queue=%s temporal=%s may_actuate=%v; NO untrusted-content reader and NO triage activity is registered in this process", tg.TaskQueueActuate, hostPort, chokepoint.MayActuate())
		<-worker.InterruptCh()
		return
	}
	log.Printf("read-only Runner worker up — queue=%s temporal=%s may_actuate=%v plane=%s", tg.TaskQueueRunner, hostPort, chokepoint.MayActuate(), credentialPlane)
	if err := w.Run(worker.InterruptCh()); err != nil {
		log.Fatalf("worker exited: %v", err)
	}
}

// ---------------------------------------------------------------------------------------------------------
// Actuation Regime Engine wiring (spec/017, TG-110). The engine answers "through which effect channel?" and
// COMPOSES over the already-built controls (interceptor spec/013, policy spec/015, credential spec/016, the
// mode chokepoint core/safety); it authorizes nothing, authenticates nothing, and lifts no floor. It is
// WIRED but INERT: every lane is reachable only through the interceptor's Do (which refuses at Shadow), and
// the awx-job lane re-guards the mode at its own leaf. Nothing here transitions the mode, enables actuation,
// or launches a job at Shadow — the default/absent/corrupt mode stays Shadow (may_actuate=false).
// ---------------------------------------------------------------------------------------------------------
