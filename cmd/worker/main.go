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
	"encoding/json"
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
	tracker "github.com/territory-grounder/grounder/adapters/tracker"
	"github.com/territory-grounder/grounder/agent"
	"github.com/territory-grounder/grounder/agent/skills"
	"github.com/territory-grounder/grounder/core/actuate"
	"github.com/territory-grounder/grounder/core/attribution"
	"github.com/territory-grounder/grounder/core/audit"
	"github.com/territory-grounder/grounder/core/breaker"
	"github.com/territory-grounder/grounder/core/config"
	"github.com/territory-grounder/grounder/core/cost"
	"github.com/territory-grounder/grounder/core/credential"
	"github.com/territory-grounder/grounder/core/db"
	coreesc "github.com/territory-grounder/grounder/core/escalation"
	"github.com/territory-grounder/grounder/core/estate"
	"github.com/territory-grounder/grounder/core/execclass"
	"github.com/territory-grounder/grounder/core/falsify"
	"github.com/territory-grounder/grounder/core/judge"
	"github.com/territory-grounder/grounder/core/knowledge"
	"github.com/territory-grounder/grounder/core/learn"
	"github.com/territory-grounder/grounder/core/lessons"
	"github.com/territory-grounder/grounder/core/observe"
	"github.com/territory-grounder/grounder/core/persist"
	"github.com/territory-grounder/grounder/core/policy"
	"github.com/territory-grounder/grounder/core/predict"
	"github.com/territory-grounder/grounder/core/preflight"
	"github.com/territory-grounder/grounder/core/regime"
	"github.com/territory-grounder/grounder/core/risk"
	"github.com/territory-grounder/grounder/core/safety"
	"github.com/territory-grounder/grounder/core/skillstore"
	"github.com/territory-grounder/grounder/core/suppression"
	"github.com/territory-grounder/grounder/core/territory"
	tracepkg "github.com/territory-grounder/grounder/core/trace"
	"github.com/territory-grounder/grounder/core/verify"
	"github.com/territory-grounder/grounder/eval"
	"github.com/territory-grounder/grounder/modules"
	awxattr "github.com/territory-grounder/grounder/modules/actorevidence/awx"
	"github.com/territory-grounder/grounder/modules/actorevidence/gitopsmr"
	"github.com/territory-grounder/grounder/modules/actorevidence/journal"
	"github.com/territory-grounder/grounder/modules/actorevidence/ldapident"
	netboxattr "github.com/territory-grounder/grounder/modules/actorevidence/netbox"
	pveattr "github.com/territory-grounder/grounder/modules/actorevidence/pve"
	"github.com/territory-grounder/grounder/modules/actuation/awxjob"
	proxmoxactuation "github.com/territory-grounder/grounder/modules/actuation/proxmox"
	sshactuation "github.com/territory-grounder/grounder/modules/actuation/ssh"
	"github.com/territory-grounder/grounder/modules/bootstrap"
	"github.com/territory-grounder/grounder/modules/cmdb/netbox"
	"github.com/territory-grounder/grounder/modules/cmdb/pve"
	"github.com/territory-grounder/grounder/modules/credsource/openbao"
	estatetools "github.com/territory-grounder/grounder/modules/estate"
	"github.com/territory-grounder/grounder/modules/ingest/librenms"
	"github.com/territory-grounder/grounder/modules/knowledge/awxplaybooks"
	"github.com/territory-grounder/grounder/modules/observability/hostdiag"
	"github.com/territory-grounder/grounder/modules/observability/syslogng"
	"github.com/territory-grounder/grounder/modules/resolve"
	"github.com/territory-grounder/grounder/modules/telemetry"
	tg "github.com/territory-grounder/grounder/temporal"
	calibratejob "github.com/territory-grounder/grounder/temporal/calibrate"
	"github.com/territory-grounder/grounder/temporal/configwrite"
	escsched "github.com/territory-grounder/grounder/temporal/escalation"
	"github.com/territory-grounder/grounder/temporal/modetransition"
	"github.com/territory-grounder/grounder/temporal/runner"
	"github.com/territory-grounder/grounder/temporal/skillgen"
	"github.com/territory-grounder/grounder/temporal/skilljudge"
	"github.com/territory-grounder/grounder/temporal/skilltrial"
	"github.com/territory-grounder/grounder/temporal/skillwrite"
)

func getenv(k, def string) string {
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

// freezeWindows reads operator-declared maintenance/chaos freeze windows from a JSON file (config-not-code):
// [{"scope":"host-or-rule-or-empty","start":"RFC3339","end":"RFC3339","reason":"..."}]. An empty path, an
// unreadable/parse-broken file, or a malformed/inverted row yields no window — fail toward investigating (a
// freeze is a deliberate declaration; absent one, the alert is triaged, never silently dropped).
func freezeWindows(path string) []suppression.FreezeWindow {
	if path == "" {
		return nil
	}
	b, err := os.ReadFile(path)
	if err != nil {
		log.Printf("suppression: cannot read freeze file %q: %v (no freeze windows)", path, err)
		return nil
	}
	var rows []struct{ Scope, Start, End, Reason string }
	if err := json.Unmarshal(b, &rows); err != nil {
		log.Printf("suppression: bad freeze file %q: %v (no freeze windows)", path, err)
		return nil
	}
	var out []suppression.FreezeWindow
	for _, r := range rows {
		start, e1 := time.Parse(time.RFC3339, r.Start)
		end, e2 := time.Parse(time.RFC3339, r.End)
		if e1 != nil || e2 != nil || !end.After(start) {
			continue // skip malformed / inverted windows
		}
		out = append(out, suppression.FreezeWindow{Scope: r.Scope, Start: start, End: end, Reason: r.Reason})
	}
	return out
}

// suppressRules reads operator-declared active-memory suppress rules from a JSON file (config-not-code):
// [{"host":"glob","rule":"glob","reason":"..."}] (path.Match globs; either side "*" for any). An empty
// path, a broken file, or a CATCH-ALL rule (both patterns "*") is refused — a catch-all would silence every
// non-critical alert, so it is dropped with a warning rather than suppressing the whole estate. A malformed
// glob matches nothing (the stage fails open), and critical/unknown severity is never suppressed by a rule.
func suppressRules(path string) []suppression.SuppressRule {
	if path == "" {
		return nil
	}
	b, err := os.ReadFile(path)
	if err != nil {
		log.Printf("suppression: cannot read rules file %q: %v (no operator rules)", path, err)
		return nil
	}
	var rows []struct{ Host, Rule, Reason string }
	if err := json.Unmarshal(b, &rows); err != nil {
		log.Printf("suppression: bad rules file %q: %v (no operator rules)", path, err)
		return nil
	}
	var out []suppression.SuppressRule
	for _, r := range rows {
		if r.Host == "*" && r.Rule == "*" {
			log.Printf("suppression: refusing a catch-all operator rule (host=* rule=*) — it would suppress the whole estate")
			continue
		}
		out = append(out, suppression.SuppressRule{HostPattern: r.Host, RulePattern: r.Rule, Reason: r.Reason})
	}
	return out
}

// suppressPatterns reads operator-declared known-transient patterns from a JSON file (config-not-code):
// [{"alert_rule":"...","estate":"...","confidence":0.8}]. A DECLARED pattern (no LastSeen) has no recency
// gate, but the stage still requires confidence >= 0.7 AND a transient-nature keyword in the rule
// (flap/blip/recover/…) to suppress — so a standing fault like "DiskFull" is never auto-suppressed. An empty
// path, a broken file, or a rule-less row yields no pattern.
func suppressPatterns(path string) []suppression.TransientPattern {
	if path == "" {
		return nil
	}
	b, err := os.ReadFile(path)
	if err != nil {
		log.Printf("suppression: cannot read patterns file %q: %v (no patterns)", path, err)
		return nil
	}
	var rows []struct {
		AlertRule  string  `json:"alert_rule"`
		Estate     string  `json:"estate"`
		Confidence float64 `json:"confidence"`
	}
	if err := json.Unmarshal(b, &rows); err != nil {
		log.Printf("suppression: bad patterns file %q: %v (no patterns)", path, err)
		return nil
	}
	var out []suppression.TransientPattern
	for _, r := range rows {
		if r.AlertRule == "" {
			continue
		}
		out = append(out, suppression.TransientPattern{AlertRule: r.AlertRule, Estate: r.Estate, Confidence: r.Confidence})
	}
	return out
}

// suppressSchedules reads operator-declared recurring reboot schedules from a JSON file (config-not-code):
// [{"host":"...","cron":"0 3 * * *","timezone":"Europe/Athens","valid_from":"RFC3339","valid_until":"RFC3339"}].
// A declared schedule is registered LIVE (an operator declaration IS the authorization — no observe-before-
// live), so a reboot-class alert on that host inside the DST-correct cron window (± tolerance) is suppressed.
// A row without a host or cron is skipped. Empty ⇒ no scheduled-reboot suppression.
func suppressSchedules(path string) []suppression.Schedule {
	if path == "" {
		return nil
	}
	b, err := os.ReadFile(path)
	if err != nil {
		log.Printf("suppression: cannot read schedules file %q: %v (no schedules)", path, err)
		return nil
	}
	var rows []struct {
		Host       string `json:"host"`
		Cron       string `json:"cron"`
		Timezone   string `json:"timezone"`
		ValidFrom  string `json:"valid_from"`
		ValidUntil string `json:"valid_until"`
	}
	if err := json.Unmarshal(b, &rows); err != nil {
		log.Printf("suppression: bad schedules file %q: %v (no schedules)", path, err)
		return nil
	}
	var out []suppression.Schedule
	for _, r := range rows {
		if r.Host == "" || r.Cron == "" {
			continue
		}
		sc := suppression.Schedule{Host: r.Host, Cron: r.Cron, Timezone: r.Timezone, Status: suppression.SchLive}
		if t, e := time.Parse(time.RFC3339, r.ValidFrom); e == nil {
			sc.ValidFrom = t
		}
		if t, e := time.Parse(time.RFC3339, r.ValidUntil); e == nil {
			sc.ValidUntil = t
		}
		out = append(out, sc)
	}
	return out
}

// foldPolicies reads operator-declared blast-radius fold policies from a JSON file (config-not-code):
// [{"host":"child-host-or-*","rule":"child-rule-or-*","site":"...","valid_from":"RFC3339","valid_until":"RFC3339"}].
// A matching CHILD alert is folded — posted as a notice, no session — while the policy is valid. An
// operator-declared policy is treated as verified-at-load (LastVerifiedAt = now) with an effectively-infinite
// freshness, because it has no learned staleness failure mode; only its valid window gates it. A catch-all
// (host=* rule=*) is refused so a config slip cannot fold the whole estate into silent notices.
func foldPolicies(path string) []suppression.SuppressionPolicy {
	if path == "" {
		return nil
	}
	b, err := os.ReadFile(path)
	if err != nil {
		log.Printf("suppression: cannot read folds file %q: %v (no fold policies)", path, err)
		return nil
	}
	var rows []struct {
		Host       string `json:"host"`
		Rule       string `json:"rule"`
		Site       string `json:"site"`
		ValidFrom  string `json:"valid_from"`
		ValidUntil string `json:"valid_until"`
	}
	if err := json.Unmarshal(b, &rows); err != nil {
		log.Printf("suppression: bad folds file %q: %v (no fold policies)", path, err)
		return nil
	}
	now := time.Now()
	var out []suppression.SuppressionPolicy
	for _, r := range rows {
		if r.Host == "" && r.Rule == "" {
			continue
		}
		if r.Host == "*" && r.Rule == "*" {
			log.Printf("suppression: refusing a catch-all fold policy (host=* rule=*) — it would fold the whole estate to notices")
			continue
		}
		p := suppression.SuppressionPolicy{HostScope: r.Host, RuleScope: r.Rule, Site: r.Site, LastVerifiedAt: now}
		if t, e := time.Parse(time.RFC3339, r.ValidFrom); e == nil {
			p.ValidFrom = t
		}
		if t, e := time.Parse(time.RFC3339, r.ValidUntil); e == nil {
			p.ValidUntil = t
		}
		out = append(out, p)
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
		c.Transport = &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, // internal self-signed estate endpoint (opt-in)
		}
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
	return []preflight.SecretEntry{
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
		biz("TG_TEAMS_TOKEN_REF"), biz("TG_TWILIO_TOKEN_REF"), biz("TG_YOUTRACK_TOKEN_REF"),
		// Exempt — substrate/bootstrap credentials + public material (REQ-2401).
		exempt("TG_OPENBAO_TOKEN_REF"), exempt("TG_OPENBAO_ROLE_ID_REF"), exempt("TG_OPENBAO_SECRET_ID_REF"),
		exempt("TG_OPENBAO_WRAP_TOKEN_REF"), exempt("TG_OPENBAO_JWT_REF"), exempt("TG_LDAP_CA"),
		exempt("TG_OIDC_CLIENT_ID_REF"), exempt("TG_LANGFUSE_PUBLIC_REF"),
	}
}

func truthyEnv(k string) bool {
	switch strings.ToLower(strings.TrimSpace(getenv(k, ""))) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
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

// resolveSelfActor resolves the platform's own actuation identity from the ACTUATION credential
// (TG_PROXMOX_TOKEN_REF) — deliberately NOT the estate-READ token (TG_PVE_TOKEN_REF): self-recognition
// must key on the identity that actually actuates, or TG reads its OWN heals as third-party changes
// (suspicious) on non-pool hosts. Kept as a seam over a getenv-like func so a test can pin that the source
// is the actuation ref, not the read ref (regression guard for b9212f8). Empty on an unresolvable/malformed
// token — the caller then registers no self identity and self-recognition is simply inert (safe).
func resolveSelfActor(get func(k, def string) string) string {
	tok, err := config.SecretRef(get("TG_PROXMOX_TOKEN_REF", "")).Resolve()
	if err != nil {
		return ""
	}
	return selfPrincipalFromToken(tok)
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

// envDuration reads an operator-declared positive duration (config-not-code); blank/invalid/non-positive ⇒ def.
func envDuration(k string, def time.Duration) time.Duration {
	v, err := time.ParseDuration(strings.TrimSpace(getenv(k, "")))
	if err != nil || v <= 0 {
		return def
	}
	return v
}

func main() {
	log.SetPrefix("tg-worker: ")
	log.SetFlags(log.LstdFlags | log.LUTC)

	// Credential delivery (spec/022 REQ-2200/REQ-2204, TG-156/TG-157): make the process's own SecretRefs
	// resolvable as bao: references from OpenBao. Done FIRST, before any secret resolves (the credential
	// preflight below, the model gateway key, per-target bundles). Substrate OFF by default (TG_OPENBAO_ADDR
	// unset) ⇒ behaviour-preserving no-op. Fail-closed: a misconfigured/unreachable enabled substrate refuses
	// to start rather than let a declared bao: secret degrade to a plaintext fallback.
	if err := openbao.WireDelivery(getenv("TG_OPENBAO_ADDR", ""), getenv("TG_OPENBAO_TOKEN_REF", ""), getenv("TG_OPENBAO_CA", ""), log.Printf); err != nil {
		log.Fatalf("credential delivery: %v", err)
	}

	// Credential plane split (spec/022 REQ-2203, TG-157): the read-only triage plane must never co-hold an
	// actuation credential. Assert at boot that the configured read-triage references (estate reads + the
	// read-scoped substrate token) are DISJOINT from the actuation references (the SSH mutate key, proxmox/AWX
	// write-tokens) — a config mistake that recombined them fails closed here, beneath the OpenBao role split.
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
	if err := planes.Validate(); err != nil {
		log.Fatalf("%v", err)
	}
	log.Printf("credential plane split: %s", planes.Summary())

	// Actuation is OFF by construction — this worker is read-only (Phase 0/1). The mode-driven actuation
	// chokepoint (the absorbed MutationGate, REQ-1520) starts with NO mode authority ⇒ MayActuate is false
	// (fail closed); the real ModeController is BOUND later (after the durable stores exist), and it defaults to
	// Shadow, so the worker stays read-only unless an operator later escalates the mode. The retired
	// TG_MUTATION_ENABLED knob is gone — enabling actuation is a mode transition, never an env flag.
	chokepoint := safety.NewChokepoint(nil)
	if chokepoint.MayActuate() {
		log.Fatal("actuation posture is ON at boot — refusing to start the read-only worker")
	}

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
	sshCredReport := preflight.CheckSSHKeys(preflight.SSHKeyRefsFromEnv(func(k string) string { return getenv(k, "") }))
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
	{
		policy := preflight.ParseSecretPolicy(getenv("TG_SECRET_POLICY", "off"))
		rep := preflight.CheckSecretPolicy(workerSecretEntries(func(k string) string { return getenv(k, "") }))
		if policy == preflight.PolicyWarn {
			for _, v := range rep.Violations {
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

	gw := model.NewGateway(getenv("TG_LITELLM_URL", "http://litellm:4000"), config.SecretRef(getenv("TG_LITELLM_KEY_REF", "env:LITELLM_MASTER_KEY")))
	tools := agent.NewReadOnlyToolSet()
	// Ground the agent in OBSERVED estate state: register the read-only LibreNMS investigation tools
	// (device status, event log, active alerts) from the same declared deployments the ingest fleet uses.
	// Without these the agent triages by inference alone (evidence_grounded floored); with them it reads the
	// live device before proposing. Every tool is GET-only — Register refuses a non-read-only tool (INV-17).
	if lnmsDeps := librenmsDeployments(getenv("TG_LIBRENMS_DEPLOYMENTS", "")); len(lnmsDeps) > 0 {
		lnmsTools := librenms.NewTools(lnmsDeps, estateHTTPClient(truthyEnv("TG_LIBRENMS_INSECURE")))
		for _, tl := range lnmsTools {
			if err := tools.Register(tl); err != nil {
				log.Fatalf("register agent tool %s (fail-closed): %v", tl.Name(), err)
			}
		}
		log.Printf("agent: registered %d read-only LibreNMS investigation tools across %d deployment(s)", len(lnmsTools), len(lnmsDeps))
	}

	// Ground the agent in OBSERVED device logs: register the read-only syslog-ng investigation tools
	// (get-host-logs, search-host-logs) from the declared per-site syslog servers (TG_SYSLOGNG_DEPLOYMENTS).
	// This is the firewall/switch/router syslog window the predecessor's cisco-asa-specialist and
	// triage-researcher had and TG lacked. Config-not-code: absent config ⇒ no tools, no error. Every tool
	// is read-only (Register refuses a non-read-only tool, INV-17) and reads a FIXED argv over host-key-
	// verified SSH — no shell, mutation stays OFF. The nil runner selects the production SSH runner.
	if sgServers := syslogng.ParseServers(getenv("TG_SYSLOGNG_DEPLOYMENTS", "")); len(sgServers) > 0 {
		sgTools := syslogng.NewTools(sgServers, nil)
		for _, tl := range sgTools {
			if err := tools.Register(tl); err != nil {
				log.Fatalf("register agent tool %s (fail-closed): %v", tl.Name(), err)
			}
		}
		log.Printf("agent: registered %d read-only syslog-ng investigation tools across %d server(s)", len(sgTools), len(sgServers))
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
		JiraURL:                 getenv("TG_JIRA_URL", ""),
		JiraEmail:               getenv("TG_JIRA_EMAIL", ""),
		JiraTokenRef:            getenv("TG_JIRA_TOKEN_REF", "env:JIRA_TOKEN"),
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
	if err := bootstrap.RegisterNotifiers(moduleReg, bootstrap.NotifierConfig{
		MatrixHomeserver:    getenv("TG_MATRIX_HOMESERVER", ""),
		MatrixTokenRef:      getenv("TG_MATRIX_TOKEN_REF", "env:MATRIX_TOKEN"),
		MatrixApprovers:     splitTokens(getenv("TG_MATRIX_APPROVERS", "")),
		MatrixRooms:         keyValueMap(getenv("TG_MATRIX_ROOMS", "")),
		MatrixDefaultRoom:   getenv("TG_MATRIX_DEFAULT_ROOM", ""),
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

	// The credential/identity engine (spec/016), instantiated LIVE from operator config: a SyncEngine over the
	// native fallback + every configured READ-ONLY source (OpenBao/Vault, AWX, Semaphore on the machine plane;
	// LDAP/FreeIPA on the human plane). Each source's creds are SecretRef references, never literals (INV-13). A
	// source whose config is absent is skipped; a source whose config is PARTIAL/invalid FAILS THE BOOT closed
	// (a misconfigured credential source must never silently drop and let actuation resolve a wrong/blank
	// identity). The engine is HELD for future actuation resolution + the grounder read surface; mutation stays
	// OFF — this is read-only credential resolution, Phase-1-safe.
	credEngine, credSources, err := bootstrap.BuildSyncEngine(bootstrap.CredentialConfig{
		NativeRules:          getenv("TG_CREDENTIAL_NATIVE_RULES", ""),
		HostDiagDeployments:  getenv("TG_HOSTDIAG_DEPLOYMENTS", ""),
		OpenBaoAddr:          getenv("TG_OPENBAO_ADDR", ""),
		OpenBaoSourceID:      getenv("TG_OPENBAO_SOURCE_ID", ""),
		OpenBaoAuthMethod:    getenv("TG_OPENBAO_AUTH_METHOD", ""),
		OpenBaoTokenRef:      getenv("TG_OPENBAO_TOKEN_REF", ""),
		OpenBaoRoleIDRef:     getenv("TG_OPENBAO_ROLE_ID_REF", ""),
		OpenBaoSecretIDRef:   getenv("TG_OPENBAO_SECRET_ID_REF", ""),
		OpenBaoWrapTokenRef:  getenv("TG_OPENBAO_WRAP_TOKEN_REF", ""),
		OpenBaoJWTRef:        getenv("TG_OPENBAO_JWT_REF", ""),
		OpenBaoJWTRole:       getenv("TG_OPENBAO_JWT_ROLE", ""),
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
	for _, rs := range credSources {
		log.Printf("credential engine: registered source %q (plane=%s, precedence=%d) — read-only sync", rs.ID, rs.Plane, rs.Precedence)
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

	// Read-only HOST-DIAGNOSTICS tools (the predecessor's SSH df/free/systemctl investigation): SSH the
	// alerting host and run a FIXED read-only diagnostic so the agent can GROUND a resource alert instead of
	// escalating blind. The allowlist (TG_HOSTDIAG_DEPLOYMENTS) gates WHETHER the tools exist; the per-host SSH
	// identity is resolved through credResolver (fail-closed) — the SAME allowlist also feeds the engine's
	// native hostdiag source, so a host resolves to exactly the (user, keyref) it reached before, now audited.
	if hdAccess := hostdiag.ParseAccess(getenv("TG_HOSTDIAG_DEPLOYMENTS", "")); len(hdAccess) > 0 {
		hdTools := hostdiag.NewTools(hdAccess, nil, credResolver)
		for _, tl := range hdTools {
			if err := tools.Register(tl); err != nil {
				log.Fatalf("register agent tool %s (fail-closed): %v", tl.Name(), err)
			}
		}
		log.Printf("agent: registered %d read-only host-diagnostics tools across %d access rule(s) — identity via the credential engine", len(hdTools), len(hdAccess))
	}

	// The tier-1 suppression gate is constructed later (it needs the tracker + config); the telemetry loop
	// reads its decision counts through this atomic pointer, set when the gate is built. nil ⇒ no suppression
	// samples (the gate is not wired).
	var suppGate atomic.Pointer[runner.LiveSuppressGate]

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
							samples = append(samples, telemetry.SuppressionSamples(g.Counts(), time.Now())...)
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

	// Build the causal estate graph the prediction gate reasons over, seeded from the configured CMDB
	// topology sources (config-not-code — a source is added only when its endpoint is declared). Each source
	// is per-source-isolated: an unconfigured or failing source contributes nothing rather than aborting the
	// others, and a fetch error is surfaced (logged), never silently presented as an empty truth. A target
	// that does not resolve still fails closed on eligibility — the correct behavior, not a vacuous prediction.
	var estateSources []estate.EdgeSource
	if nbURL := getenv("TG_NETBOX_URL", ""); nbURL != "" {
		nb := netbox.New(nbURL, config.SecretRef(getenv("TG_NETBOX_TOKEN_REF", "env:NETBOX_TOKEN")))
		estateSources = append(estateSources, netbox.NewEstateSource(nb, getenv("TG_NETBOX_CASCADE_ALERT", "HostDown")))
	}
	if deps := librenmsDeployments(getenv("TG_LIBRENMS_DEPLOYMENTS", "")); len(deps) > 0 {
		topts := []librenms.TopoOption{librenms.WithExpectedAlerts(getenv("TG_LIBRENMS_CASCADE_ALERT", "DeviceDown"))}
		if truthyEnv("TG_LIBRENMS_INSECURE") {
			topts = append(topts, librenms.WithTopologyHTTPClient(estateHTTPClient(true)))
			log.Printf("estate: LibreNMS TLS verification DISABLED (TG_LIBRENMS_INSECURE=true) — internal self-signed endpoint")
		}
		estateSources = append(estateSources, librenms.NewEstateSource(deps, topts...))
	}
	if pveURL := getenv("TG_PVE_URL", ""); pveURL != "" {
		popts := []pve.Option{pve.WithExpectedAlerts(getenv("TG_PVE_CASCADE_ALERT", "HostDown"))}
		if truthyEnv("TG_PVE_INSECURE") {
			popts = append(popts, pve.WithHTTPClient(estateHTTPClient(true)))
			log.Printf("estate: PVE TLS verification DISABLED (TG_PVE_INSECURE=true) — internal self-signed endpoint")
		}
		estateSources = append(estateSources, pve.New(pveURL, config.SecretRef(getenv("TG_PVE_TOKEN_REF", "env:PVE_API_TOKEN")), popts...))
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
	initialGraph, estateErrs := estate.Build(context.Background(), estateSources)
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
	// configWriteActs executes console-ordered config overrides + sealed-secret commits in THIS
	// process (task #27 Phases C+D, REQ-523/524) — same single-ledger-writer discipline. nil without
	// a DB: the workflows are then not registered at all.
	var configWriteActs *configwrite.Activities
	// modeTransitionActs executes an operator-invoked autonomy-mode transition in THIS process on the
	// single chokepoint-bound ModeController (spec/015 REQ-1502) — the LAST gate before the mutation flip.
	// nil without a DB: the transition workflow is then not registered at all (POST /v1/mode fails closed).
	var modeTransitionActs *modetransition.Activities
	// The trial engine's collaborators (spec/014 REQ-1306/1308); nil without a DB.
	var skillTrials skillstore.TrialStore
	var skillVersionByID func(context.Context, int64) (skillstore.Version, error)
	var skillTrialActs *skilltrial.Activities
	// The durable judge spine (task #26, spec/012 REQ-1106): the Runner's terminal triage-record
	// writer + the 2-hourly judge cron. Both nil without a DB — the record activity is then a
	// fail-open no-op and the cron is not registered (sessions stay honestly unjudged).
	var triageRecord func(context.Context, judge.TriageRow) error
	var skillJudgeActs *skilljudge.Activities
	// The flywheel CREATION half (spec/014 REQ-1314): the daily generator cron that fires
	// GenerateCandidates -> AdmitToTrial -> StartTrial from the durable judge signal. nil without a DB —
	// the cron is then not registered (no durable means/drafts to act on). Generate-only, mutation OFF.
	var skillGenActs *skillgen.Activities
	if iv := getenv("TG_ESTATE_REFRESH_INTERVAL", ""); iv != "" && len(estateSources) > 0 {
		if d, err := time.ParseDuration(iv); err == nil && d > 0 {
			go func() {
				t := time.NewTicker(d)
				defer t.Stop()
				for range t.C {
					// re-read the live sources AND fold in the learner's current co-occurrences.
					sources := append(append([]estate.EdgeSource(nil), estateSources...), learner.LearnedSource())
					for _, e := range estateHolder.Refresh(context.Background(), sources) {
						log.Printf("estate refresh: source %s failed: %v (kept prior edges)", e.Source, e.Err)
					}
					publishEstate(estateHolder.Graph()) // republish the refreshed graph for the read API
				}
			}()
			log.Printf("estate: periodic topology refresh every %s (with the learned tier)", d)
		} else {
			log.Printf("estate: invalid TG_ESTATE_REFRESH_INTERVAL %q — refresh disabled", iv)
		}
	}

	// LibreNMS alert intake is PUSH by default: LibreNMS transports POST each alert to /v1/ingest/librenms
	// authenticated by a per-source static bearer (AuthIngestPush), exactly like Alertmanager — the earlier
	// belief that "LibreNMS's transport cannot sign TG's HMAC ingest, so native pull is the only path" is
	// obsolete now that the bearer path exists. The two servers (NL, GR) share the one endpoint and are
	// discriminated by the payload's `site` field. This active-alert PULL is a DEPRECATED opt-in FALLBACK for
	// an air-gapped / transport-less deployment: it stays OFF unless TG_LIBRENMS_ALERT_POLL_INTERVAL is set to
	// a duration. When enabled, each tick fetches every configured deployment's firing alerts (state=1,
	// read-only) and mints ONE triage RunnerWorkflow per alert, deduped by workflow id (REJECT_DUPLICATE) so a
	// still-firing alert is triaged exactly once — and only when a deployment is also configured. Best-effort
	// throughout: a fetch or start failure logs and continues — the poller NEVER crashes the worker, and it
	// never mutates the estate (mutation stays OFF).
	if iv := getenv("TG_LIBRENMS_ALERT_POLL_INTERVAL", ""); iv != "" {
		alertDeps := librenmsDeployments(getenv("TG_LIBRENMS_DEPLOYMENTS", ""))
		if d, err := time.ParseDuration(iv); err == nil && d > 0 && len(alertDeps) > 0 {
			alertSrc := librenms.NewAlertSource(alertDeps, librenms.WithAlertHTTPClient(estateHTTPClient(truthyEnv("TG_LIBRENMS_INSECURE"))))
			go func() {
				t := time.NewTicker(d)
				defer t.Stop()
				for range t.C {
					ctx, cancel := context.WithTimeout(context.Background(), d)
					envs, ferr := alertSrc.FetchActive(ctx)
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
					if minted > 0 || already > 0 {
						log.Printf("librenms alert poll: %d new triage(s), %d already-firing skipped", minted, already)
					}
					cancel()
				}
			}()
			log.Printf("librenms: active-alert pull (opt-in fallback) every %s across %d deployment(s) (read-only, mutation OFF) — the primary intake is PUSH to /v1/ingest/librenms", d, len(alertDeps))
		} else if len(alertDeps) == 0 {
			log.Printf("librenms: alert pull idle — no TG_LIBRENMS_DEPLOYMENTS configured")
		}
	} else {
		log.Printf("librenms: alert intake is PUSH — LibreNMS transports POST /v1/ingest/librenms (per-source bearer, AuthIngestPush); active-alert pull disabled (set TG_LIBRENMS_ALERT_POLL_INTERVAL to a duration to re-enable the opt-in fallback)")
	}

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
		log.Printf("canary poll policy: %d pinned (host,op) rule(s) — forced POLL_PAUSE (human vote), mutation OFF", n)
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
	var manifestSink runner.ManifestSink           // durable sealed-manifest writer when a DSN is configured, else nil
	var manifestBackfill runner.ManifestBackfiller // lifecycle-label backfiller (approval_choice/verdict), same store
	var agentStepSink tracepkg.AgentStepSink       // scrubbed per-ReAct-cycle transcript writer (spec/020 T-020-8), else nil
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
		// Collaborators captured for LaneEffect's interceptor BUILDER — it must build a per-lane spec/013
		// interceptor with the IDENTICAL wiring the native-ssh interceptor gets, so a routed lane preserves
		// every gate. Assigned where each is constructed in the DB-present block below.
		bEffectLeaf    actuation.Actuator
		bVerdictSink   actuate.VerdictSink
		bGateVerdict   tracepkg.GateVerdictSink
		bGraduation    actuate.GraduationRecorder
		bPolicyDecider actuate.PolicyDecider
		bPolicyModeNow func() policy.Mode
		manifestReader runner.ManifestReader
		predReader     runner.PredictionReader
		verdictReader  runner.VerdictReader
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
		// escalationStore is the durable dropped-escalation requeue lane, hoisted so the FireDue cron + the
		// reconcile→escalation re-check hand-off share ONE lane; nil without a DSN ⇒ the escalation lane is
		// inert (there is nowhere durable to enqueue a re-check).
		escalationStore *db.EscalationStore
	)
	// dbPool is the shared runtime pool, hoisted so later planes (the semantic retrieval index) can reuse
	// it; nil without a DSN — every consumer nil-checks and degrades honestly.
	var dbPool *db.Pool
	if dsn := getenv("TG_DB_DSN", ""); dsn != "" {
		pool, err := db.Connect(context.Background(), dsn)
		if err != nil {
			log.Fatalf("durable stores: connect %v", err)
		}
		defer pool.Close()
		dbPool = pool
		pstore := db.NewPredictionStore(pool)
		predStore = pstore
		predReader = pstore
		// Continue the governance chain from its persisted tail, and mirror every new decision to the DB
		// write-through — so the tamper-evident audit trail is unbroken across restarts (INV-19).
		lstore := db.NewLedgerStore(pool)
		seq, hash, terr := lstore.Tail(context.Background())
		if terr != nil {
			log.Fatalf("governance ledger: read tail %v", terr)
		}
		ledger = audit.NewLedgerFromTail(seq, hash).WithSink(lstore).WithRiskSink(db.NewRiskAuditStore(pool))
		mstore := db.NewManifestStore(pool)
		manifestSink = mstore
		manifestBackfill = mstore // same pgx store also backfills approval_choice/verdict (spec/020 T-020-4)
		manifestReader = mstore
		agentStepSink = db.NewAgentStepStore(pool) // scrubbed per-cycle transcript (spec/020 T-020-8), observe-only
		vstore := db.NewVerdictStore(pool)
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
		falsifyVerdicts = vstore
		// the durable pending-decisions projection (REQ-519): the console reads what this worker writes
		// across the process boundary, so it MUST be the shared pgx store, never in-memory.
		pendingWriter = db.NewPendingStore(pool)
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
		effectActuator := bootstrap.BuildEffectActuator(chokepoint,
			sshactuation.NewNativeRunner(getenv("TG_ACTUATION_SSH_KNOWN_HOSTS", ""), config.SecretRef(getenv("TG_ACTUATION_SSH_KEY", ""))),
			bootstrap.EffectActuatorConfig{
				SSHHost:               getenv("TG_ACTUATION_SSH_HOST", ""),
				SSHIdentity:           getenv("TG_ACTUATION_SSH_IDENTITY", ""),
				AllowedUnitsSpec:      getenv("TG_ACTUATION_ALLOWED_UNITS", ""),
				AllowedContainersSpec: getenv("TG_ACTUATION_ALLOWED_CONTAINERS", ""),
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
		bGateVerdict = db.NewGateVerdictStore(pool)
		interceptor = actuate.NewInterceptor(chokepoint, effectActuator, ledger).
			WithVerdictSink(vstore).
			WithGateVerdictSink(bGateVerdict)
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
		// Publish the worker's TRUE, live mutation posture (spec/012 REQ-1107) so the grounder — a SEPARATE
		// process whose own gate is read-only by construction — reports it honestly on /v1/whoami +
		// /v1/governance instead of its own always-off gate. It upserts this worker's ACTUAL gate.Enabled()
		// and effect-leaf Capability() to the single-writer runtime_posture projection and re-publishes on a
		// heartbeat so updated_at stays fresh; the grounder treats a stale/absent row as UNKNOWN, never a
		// false OFF. Publishing NEVER blocks or kills the worker: a write error is logged (like the estate
		// publish), measurement only, never gating. Re-reading gate.Enabled() each tick means a runtime halt
		// (breaker/POST /halt → gate.Disable) is reflected within one interval, so the posture stays live.
		postureStore := db.NewPosturePublishStore(pool)
		publishPosture := func() {
			pctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			if perr := postureStore.Publish(pctx, "worker", chokepoint.MayActuate(), effectActuator.Capability()); perr != nil {
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
		log.Printf("posture: worker publishes its live mutation posture every %s (component=worker, may_actuate=%v, effect=%s)", postureInterval, chokepoint.MayActuate(), effectActuator.Capability())
		// Publish the estate graph so the grounder's /v1/estate surface serves the same causal graph the
		// gate reasons over (REQ-516). Publishing never blocks or fails triage: a write error is logged.
		estateWriter := db.NewEstateWriteStore(pool)
		publishEstate = func(g *estate.Graph) {
			if g == nil {
				return
			}
			if err := estateWriter.Publish(context.Background(), g.Export(), len(estateSources)); err != nil {
				log.Printf("estate: publish snapshot failed: %v (kept serving prior)", err)
			}
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
		// Establish the out-of-box curated Semi-auto baseline on a fresh deployment (absent-only + idempotent;
		// never clobbers an operator ruleset or an earned/operator-tuned op-class; mode-gated — Shadow default,
		// so the seed never actuates until an operator escalates the mode). Returns the effective ruleset.
		policyRuleSet := policy.SeedDefaults(pctx, policyRulesets, policyGradStore, log.Printf)
		policyGrad := policy.NewLadder(policy.DefaultPromoteThreshold, policyGradStore, log.Printf)
		bGraduation = policyGrad // captured for the regime LaneEffect builder (identical wiring on routed lanes)
		// WIRE the graduation ladder's EARN-PATH into the interceptor (spec/013 REQ-1217, spec/015 REQ-1514):
		// AFTER a governed action executes and its post-state VERIFIES, the interceptor records the run outcome
		// to THIS ladder (the SAME one the policy engine READS via GraduatedVerdict). Without this write the
		// ladder dead-locks — no op-class ever records a clean run, so none can graduate from `approve` to
		// `auto` (the durable policy_graduation table stays empty). It is a post-verify WRITE only; the mode
		// chokepoint (mutation OFF) still gates every execute, so no clean run accrues until an operator
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
		log.Printf("mode-transition RBAC WIRED: %d explicit flip-authorized operator(s) + LDAP-admin-group fallback; POST /v1/mode gated on authority + green preflight; mode stays %s (mutation OFF)", modeAuthority.AllowlistSize(), policyModeCtl.Current())
		if policyEng, perr := policy.NewEngine(pctx, policyRuleSet); perr != nil {
			log.Printf("policy engine: build failed (%v) — actuation falls back to the mode chokepoint + never-auto floor only (fail closed): %v", perr, perr)
		} else {
			// The audited engine appends every decision to the durable append-only policy_decision table (pgx
			// AuditSink). WIRE it as the interceptor's per-action policy authorizer (an INDEPENDENT layer beneath
			// which the mechanical mode chokepoint still gates, REQ-1521). policyModeCtl.Current supplies the
			// active mode carried into each decision's audit.
			policyAudited := policy.NewAuditedEngine(policyEng.WithGraduation(policyGrad),
				db.NewPolicyDecisionWriteStore(pool)).WithLogf(log.Printf)
			// Capture the policy authorizer + active-mode reader for the regime LaneEffect builder, so a routed
			// lane's per-lane interceptor consults the SAME policy Decide before its mode chokepoint (no weaker path).
			bPolicyDecider = policyAudited
			bPolicyModeNow = policyModeCtl.Current
			if interceptor != nil {
				interceptor = interceptor.WithPolicyDecider(policyAudited, policyModeCtl.Current)
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
		regimeEngine = wireActuationRegime(chokepoint, ledger, effectActuator, policyGrad, pool, policyModeCtl.Current().String())

		// The skill store (spec/014): boot-import the compiled registry as production rows (idempotent;
		// a compiled UPGRADE supersedes a prior compiled-import row via the audited Transition, but a
		// GRADUATED store row is never displaced), then hand the composer its snapshot reader. Import
		// failure degrades, never blocks boot — composition falls back to the compiled registry anyway.
		skillDB := db.NewSkillStore(pool)
		importCompiledSkills(context.Background(), skillDB, ledger)
		skillRows = skillDB.ProductionRows
		skillWriteActs = &skillwrite.Activities{D: skillwrite.Deps{Store: skillDB, Ledger: ledger}}
		// Config + sealed-secret writes (task #27 Phases C+D): the SAME durable, chain-continued
		// ledger; the LAW clamp is re-validated inside the activity (the authority).
		configWriteActs = &configwrite.Activities{D: configwrite.Deps{
			Ledger: ledger, Config: db.NewCPConfigStore(pool), Secrets: db.NewSealedSecretStore(pool),
		}}
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
		escalationStore = db.NewEscalationStore(pool)
		skillJudgeActs = &skilljudge.Activities{D: skilljudge.Deps{
			Model:  gw,
			Store:  triageDB,
			Watch:  skillDB,
			Skills: skillDB,
			Ledger: ledger,
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
		skillGenActs = &skillgen.Activities{D: skillgen.Deps{Creation: skillstore.CreationDeps{
			Store:  skillDB,
			Means:  skillDB,
			Trials: skillDB,
			Ledger: ledger,
			Model:  skillgen.NewPromptCompleter(gw),
			Runner: skillgen.OfflineRunner{Model: gw, Store: skillDB, Incidents: triageDB, Cfg: offlineCfg},
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
	// back to the in-process MemStore fast path — single-worker safe, mutation OFF regardless. The durable store is
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
			if bVerdictSink != nil {
				ic = ic.WithVerdictSink(bVerdictSink)
			}
			if bGateVerdict != nil {
				ic = ic.WithGateVerdictSink(bGateVerdict)
			}
			if bGraduation != nil {
				ic = ic.WithGraduationRecorder(bGraduation)
			}
			if bPolicyDecider != nil {
				ic = ic.WithPolicyDecider(bPolicyDecider, bPolicyModeNow)
			}
			if mutationBreaker != nil {
				ic = ic.WithMutationBreaker(mutationBreaker)
			}
			return ic
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

	// Turn the credential engine ON: run an initial read-only SyncAll now (best-effort — a source that is
	// unreachable/denied fails closed and contributes nothing, NEVER fatal, exactly like the estate publish),
	// publish the non-secret coverage + sync state, then optionally re-sync on a schedule
	// (TG_CREDENTIAL_SYNC_INTERVAL, OFF by default like the observability export loop). Mutation stays OFF —
	// this resolves identities read-only; it never actuates.
	if len(credSources) > 0 {
		runCredentialSync := func() {
			sctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
			defer cancel()
			runs, serr := credEngine.SyncAll(sctx)
			if serr != nil {
				log.Printf("credential engine: sync error: %v (prior converged state retained, mutation OFF)", serr)
			}
			cov := make([]db.CredentialCoverage, 0, len(runs))
			for _, r := range runs {
				credCoverage[r.SourceID] += r.Added - r.Removed
				if credCoverage[r.SourceID] < 0 {
					credCoverage[r.SourceID] = 0 // defensive: coverage can never be negative
				}
				cov = append(cov, db.CredentialCoverage{SourceID: r.SourceID, Plane: r.Plane, Targets: credCoverage[r.SourceID]})
			}
			publishCredentialState(runs, cov)
		}
		runCredentialSync() // initial sync at boot
		if iv := getenv("TG_CREDENTIAL_SYNC_INTERVAL", ""); iv != "" {
			if d, derr := time.ParseDuration(iv); derr == nil && d > 0 {
				go func() {
					t := time.NewTicker(d)
					defer t.Stop()
					for range t.C {
						runCredentialSync()
					}
				}()
				log.Printf("credential engine: scheduled re-sync every %s (read-only, mutation OFF)", d)
			} else {
				log.Printf("credential engine: invalid TG_CREDENTIAL_SYNC_INTERVAL %q — scheduled sync disabled (the initial sync still ran)", iv)
			}
		} else {
			log.Printf("credential engine: scheduled re-sync disabled (TG_CREDENTIAL_SYNC_INTERVAL unset) — synced once at boot")
		}
	}

	// The estate-context investigation tool: the agent's read-only window into the causal graph (upstream /
	// blast radius / common-cause siblings), bound to the holder so every invocation sees the freshest
	// refresh. This is what makes the triage skill's cascade discipline mechanically satisfiable — without it
	// the agent is told to probe "related hosts" it has no way to name.
	for _, tl := range estatetools.New(estateHolder.Graph) {
		if err := tools.Register(tl); err != nil {
			log.Fatalf("estate tool %s must register read-only: %v", tl.Name(), err)
		}
	}

	// The retrieval plane: a corpus of prior resolved incidents the agent is seeded with as precedent
	// (config-not-code — an operator-exported history via TG_KNOWLEDGE_FILE, until a knowledge store feeds it).
	// Empty/absent ⇒ no retriever ⇒ the agent investigates from the incident alone.
	var retriever knowledge.Retriever
	var knowledgeHolder *knowledge.Holder
	// syncEmbed folds the current corpus into the semantic vector index (best-effort, in the background —
	// an index/embedding failure NEVER blocks a corpus write). A no-op until the semantic plane is wired.
	syncEmbed := func() {}
	corpusPath := getenv("TG_KNOWLEDGE_FILE", "")
	// The MAINTAINED corpus (corpusPath, worker-written) is unioned with the read-only bootstrap SEED
	// (TG_KNOWLEDGE_SEED_FILE, tracked + deploy-synced) at every load — see knowledge_corpus.go. The split
	// is what makes runtime learning SURVIVE a deploy: the deploy overwrites tracked files (the seed) but
	// never the untracked maintained corpus.
	seedPath := getenv("TG_KNOWLEDGE_SEED_FILE", "")
	// loadCorpus parses the seed∪maintained union into a retriever, or nil on error (keep the last good
	// corpus). Function-scoped so every corpus WRITE path (writeback / lessons merge / decay prune) reloads
	// the UNION after writing — never the maintained-only set, which would silently evict the seed from the
	// novelty gate until a restart.
	loadCorpus := func() *knowledge.LexicalRetriever {
		return loadKnowledgeCorpus(seedPath, corpusPath, log.Printf)
	}
	if corpusPath != "" {
		knowledgeHolder = knowledge.NewHolder(loadCorpus())
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
				retriever = &knowledge.FusedRetriever{Base: knowledgeHolder, Index: estore, Embed: embedder, MinSim: minSim}
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
	if src := getenv("TG_LESSONS_SOURCE_FILE", ""); src != "" {
		switch {
		case corpusPath == "":
			log.Printf("lessons: TG_LESSONS_SOURCE_FILE set but TG_KNOWLEDGE_FILE is empty — no durable corpus to persist into; lessons feed disabled")
		case knowledgeHolder == nil:
			log.Printf("lessons: knowledge corpus unavailable — lessons feed disabled")
		default:
			// The recency/decay retention horizon (spec/018): a positive TG_LESSONS_MAX_AGE prunes precedents
			// older than it from the corpus (via reconcileLessons, below) AND stops the append path from
			// re-adding a stale lesson still present in the feed — so the two never tug-of-war. 0 ⇒ decay OFF.
			lessonsMaxAge := envDuration("TG_LESSONS_MAX_AGE", 0)
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
				if added == 0 {
					return // nothing confirmed-clean and net-new — leave the corpus (and its file) untouched
				}
				// Atomic write: temp file + rename, so a reader (the reload loop) never sees a torn corpus.
				tmp := corpusPath + ".tmp"
				out, err := os.Create(tmp)
				if err != nil {
					log.Printf("lessons: cannot write corpus temp %s: %v (skipped)", tmp, err)
					return
				}
				if werr := knowledge.WriteCorpus(out, merged); werr != nil {
					out.Close()
					os.Remove(tmp)
					log.Printf("lessons: corpus serialize failed: %v (skipped)", werr)
					return
				}
				out.Close()
				if rerr := os.Rename(tmp, corpusPath); rerr != nil {
					os.Remove(tmp)
					log.Printf("lessons: cannot replace corpus %s: %v (skipped)", corpusPath, rerr)
					return
				}
				knowledgeHolder.Set(loadCorpus()) // reload the seed∪maintained union — never the maintained-only set (the seed must stay visible to the novelty gate after a write)
				syncEmbed()                       // a new lesson becomes semantically retrievable too (best-effort, never blocking)
				log.Printf("lessons: distilled %d new confirmed-clean lesson(s) into %s", added, corpusPath)
			}
			appendLessons() // fold the feed in once at boot
			if iv := getenv("TG_LESSONS_REFRESH_INTERVAL", ""); iv != "" {
				if d, err := time.ParseDuration(iv); err == nil && d > 0 {
					go func() {
						t := time.NewTicker(d)
						defer t.Stop()
						for range t.C {
							appendLessons()
						}
					}()
					log.Printf("lessons: resolved-incident feed folded in every %s", d)
				}
			}
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
					tmp := corpusPath + ".tmp"
					out, cerr := os.Create(tmp)
					if cerr != nil {
						log.Printf("lessons decay: cannot write corpus temp %s: %v (skipped)", tmp, cerr)
						return
					}
					if werr := knowledge.WriteCorpus(out, kept); werr != nil {
						out.Close()
						os.Remove(tmp)
						log.Printf("lessons decay: corpus serialize failed: %v (skipped)", werr)
						return
					}
					out.Close()
					if rerr := os.Rename(tmp, corpusPath); rerr != nil {
						os.Remove(tmp)
						log.Printf("lessons decay: cannot replace corpus %s: %v (skipped)", corpusPath, rerr)
						return
					}
					knowledgeHolder.Set(loadCorpus()) // reload the seed∪maintained union after the prune — never the maintained-only set
					syncEmbed()                       // the pruned corpus re-syncs the vector index (best-effort, never blocking)
					log.Printf("lessons decay: pruned %d stale precedent(s) older than %s from %s", removed, lessonsMaxAge, corpusPath)
				}
				log.Printf("lessons decay: provenance-pruning armed (retention horizon %s) — fired by the decay cron", lessonsMaxAge)
			}
		}
	}

	// The read-only playbooks-as-knowledge lane (spec/017 T-017-5 follow-on): a fail-safe cron that pulls AWX
	// job templates + inventory READ-ONLY (re-read by id), ingests them into the knowledge corpus as retrieval
	// DATA (never an executable capability), and folds them into the semantic index over the UNION of the live
	// corpus + the runbooks (SyncIndex prunes refs absent from the corpus it is handed, so the union never
	// drops a lesson). It launches NOTHING — a surfaced runbook re-enters only as a proposal through the full
	// interceptor chain. Disabled unless TG_AWXPLAYBOOKS_* is configured; a cron error never crashes the worker.
	armAWXPlaybooksIngest(dbPool, knowledgeHolder)

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
		attributionCfg = attribution.Config{Sanctioned: map[string][]string{}, SelfActors: map[string]string{}}
	}
	var actorReaders []actorevidence.Reader
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
		// every read. PVE serves a self-signed cert on :8006, so mirror the estate transport's opt-in TLS
		// skip (TG_PVE_INSECURE) — without it the default client's TLS verification fails and the reader is
		// silently inert.
		roRef := getenv("TG_PVE_RO_TOKEN_REF", "")
		if roTok, rerr := config.SecretRef(roRef).Resolve(); roRef != "" && rerr == nil && strings.TrimSpace(roTok) != "" {
			ropts := []pveattr.Option{pveattr.WithTimeout(8 * time.Second)}
			if truthyEnv("TG_PVE_INSECURE") {
				ropts = append(ropts, pveattr.WithHTTPClient(estateHTTPClient(true)))
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
	if jAccess := journal.ParseAccess(getenv("TG_JOURNAL_DEPLOYMENTS", "")); len(jAccess) > 0 {
		jRunner := syslogng.NewNativeRunner(getenv(journal.KnownHostsEnv, ""))
		actorReaders = append(actorReaders, journal.New(jAccess, jRunner, credResolver))
		log.Printf("attribution: journal/sudo reader armed across %d access rule(s) — WHO-CAUSED-THIS active for host privileged actions", len(jAccess))
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
	// The identity/auth enrichment resolver (spec/023 REQ-2315..2319): promotes confirmed live admins and
	// demotes disabled ones over a per-session copy of the sanctioned set. Reuses the SAME FreeIPA/LDAP
	// service bind the approver-sync uses (TG_LDAP_*). Config-gated on TG_LDAP_URLS; unset ⇒ no enrichment
	// (exactly the static Phase-1 behavior). Advisory/fail-open — a construction error is logged, never fatal.
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

	deps := runner.Deps{
		Model:            agentModel, // the LiteLLM gateway, WRAPPED by the cost meter when a budget is configured
		Tools:            tools,
		Limits:           agent.DefaultLimits(),
		SkillRows:        skillRows,
		SkillTrials:      skillTrials,
		SkillVersionByID: skillVersionByID,
		Retriever:        retriever,
		Observe:          func(host string, at time.Time) { learner.Observe(learn.AlertObservation{Host: host, At: at}) },
		Metrics:          obsRegistry,   // OBSERVE-ONLY: the agent-loop/verify/classify metrics emitter (never gates)
		AgentSteps:       agentStepSink, // OBSERVE-ONLY: scrubbed per-ReAct-cycle transcript (spec/020 T-020-8)
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
		// The actor-attribution plane (spec/023): the registered domain readers, the taxonomy→disposition
		// rules-as-data, and the deterministic attributor's config (self-identity from the credential
		// configuration, sanctioned principals + carve-outs from the ruleset).
		ActorReaders:       actorReaders,
		AttributionMapping: attributionMapping,
		AttributionConfig:  attributionCfg,
		SanctionResolver:   sanctionResolver, // spec/023 identity/auth enrichment; nil ⇒ static sanction only

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
		Gate: &predict.PredictionGate{
			Store: predStore, // pgx-backed (durable) when TG_DB_DSN is set, else the in-memory oracle twin
			// The prediction gate reads the multi-source causal estate graph (core/estate), not a flat
			// adjacency map. The graph is seeded per-source-isolated; the NetBox/LibreNMS/PVE topology
			// readers are wired next, so today it is empty and an unresolvable target fails closed on
			// eligibility — the correct, non-vacuous behavior, not the empty-map dead capability it replaces.
			Model: &predict.InfragraphModel{
				EstateProvider: estateHolder.Graph,
				Graph:          predict.NewDependencyGraph(map[string][]string{}), // retained for the shuffled control path
				MaxDepth:       3,
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
		RegimeEngine: regimeEngine, // spec/017: route the execute dispatch through SelectLane → LaneEffect
		LaneEffect:   laneEffect,   // nil (no DB/engine) ⇒ the execute activity falls back to Interceptor.Do
		// awx-launch op-class → AWX template id (config-not-code, TG_AWXJOB_ALLOWLIST). FAIL-CLOSED: unset/empty/
		// ambiguous ⇒ resolves nothing ⇒ an awx op cannot encode a launch ⇒ refused. AWX is unconfigured here.
		AWXTemplateForOpClass: awxTemplateResolver(getenv("TG_AWXJOB_ALLOWLIST", "")),

		Manifests:    manifestReader,
		Predictions:  predReader,
		Verdicts:     verdictReader,
		Pending:      pendingWriter,
		Acknowledged: map[territory.Territory]bool{},
		// The compact terminal triage record (REQ-1106) — the judge cron's input. nil without a DB.
		TriageRecord: triageRecord,
	}
	// The post-execution observer (spec/013 verifiability gate): after a (future, gated) mutation the
	// deterministic verifier diffs the committed prediction against the REAL post-state read here — never nil,
	// which would make every action verify as match (the blind-verifier bug the readiness review flagged #1).
	// It reads the currently-firing alerts from LibreNMS (read-only, the same active-alert surface the poller
	// uses) and maps them to verify.ObservedAlert. It runs ONLY after an execution; under mutation OFF the
	// interceptor refuses before execute, so this is NEVER called today (inert). Unset (no LibreNMS) ⇒ the
	// execute activity supplies an EMPTY observation and the verdict still computes deterministically.
	if obsDeps := librenmsDeployments(getenv("TG_LIBRENMS_DEPLOYMENTS", "")); len(obsDeps) > 0 {
		obsSrc := librenms.NewAlertSource(obsDeps, librenms.WithAlertHTTPClient(estateHTTPClient(truthyEnv("TG_LIBRENMS_INSECURE"))))
		deps.PostStateObserve = func(ctx context.Context, targetHost, site string) []verify.ObservedAlert {
			envs, ferr := obsSrc.FetchActive(ctx)
			if ferr != nil {
				return []verify.ObservedAlert{} // a read error ⇒ empty observation (the verdict still computes deterministically)
			}
			out := make([]verify.ObservedAlert, 0, len(envs))
			for _, e := range envs {
				out = append(out, verify.ObservedAlert{Host: e.Host, Rule: e.AlertRule, Site: e.Site})
			}
			return out
		}
		// ClearObserve is the ConfirmedClear reader: the SAME read-only LibreNMS active-alert surface, but with
		// the fetch error SURFACED (ok=false) instead of collapsed to empty. The close-out clear-check must
		// distinguish "observed the host quiet" from "could not observe" — a transient LibreNMS outage returning
		// empty must NEVER read as a clear (that would false auto-close AND de-novel on zero evidence).
		deps.ClearObserve = func(ctx context.Context, host, site string) ([]verify.ObservedAlert, bool) {
			envs, ferr := obsSrc.FetchActive(ctx)
			if ferr != nil {
				return nil, false // fail-closed: a read error is NOT a clear
			}
			out := make([]verify.ObservedAlert, 0, len(envs))
			for _, e := range envs {
				out = append(out, verify.ObservedAlert{Host: e.Host, Rule: e.AlertRule, Site: e.Site})
			}
			return out, true
		}
		log.Printf("actuation: post-execution verifier reads live LibreNMS active alerts (read-only, inert while mutation OFF)")
	}
	// The clear-confirm BELT (TG-124 Plan B): bind the durable recovery-transition log so the Runner's
	// ConfirmedClear check can confirm on TG's OWN captured provider recovery push (ingest_transition, written
	// by the front door) even when the LibreNMS re-pull lags past the bound — the observed writeback-miss case.
	// nil pool ⇒ the seam stays nil ⇒ the belt is inert (the re-pull governs alone, exactly today's behavior).
	if dbPool != nil {
		deps.RecoveredSince = db.NewTransitionLogStore(dbPool).RecoveredSince
		log.Printf("clear-confirm belt: RecoveredSince reads the durable recovery-transition log (ingest_transition)")
	}
	// The rolling DISCOVERY CORPUS (design-wisdom #10): the in-memory buffer the falsify Scorer captures every
	// live-scored DEVIATION into (keyed by deviation signature), and the verify-sourced disproof signal the
	// estate decay-on-disproof pass reads. Constructed unconditionally so the decay cron can snapshot it; it is
	// injected into the Scorer below only when the writeback is armed, and drained to eval/discovery-corpus.json
	// by the flush cron (#10's deferred wiring hop). Bounded (TG_DISCOVERY_CORPUS_CAP; 0 ⇒ the package default).
	discoveryCorpus := falsify.NewMemDiscoveryCorpus(envInt("TG_DISCOVERY_CORPUS_CAP", 0))
	// The verify-time FALSIFIABILITY WRITEBACK (#23 evidenced-readiness prep / #26 grounding deepening): the
	// production caller the predict → verdict → score chain never had, so SignalRatio / the grounding
	// scorecard finally reads REAL scored predictions. Every N (TG_FALSIFIABILITY_SCORE_INTERVAL) it takes the
	// committed-but-unscored predictions whose observation window (TG_FALSIFIABILITY_WINDOW) has elapsed —
	// so the cascade has had time to manifest — observes the LIVE post-incident alerts through the SAME
	// read-only surface the interceptor's verifier uses (deps.PostStateObserve), and writes back the
	// confusion-matrix score + the mechanical verdict (a deviation is never-auto by construction) + one
	// windowed cascade-stats aggregate (INV-22). It fires on the READ-ONLY / propose path: a prediction is
	// committed BEFORE any action and scored AFTER observation, so this NEVER depends on mutation being ON —
	// it scores, it never actuates. Armed only with BOTH a durable store (a DSN) AND a live observer (a
	// LibreNMS deployment); without either it stays dark and the scorecard honestly reports zeros. Best-effort
	// throughout: a scoring error logs and the loop continues — it never crashes the worker or mutates the estate.
	if iv := getenv("TG_FALSIFIABILITY_SCORE_INTERVAL", "5m"); iv != "" && falsifyUnscored != nil && falsifyScores != nil && deps.PostStateObserve != nil {
		if d, derr := time.ParseDuration(iv); derr == nil && d > 0 {
			window := envDuration("TG_FALSIFIABILITY_WINDOW", 10*time.Minute)
			scorer := &falsify.Scorer{
				Unscored: falsifyUnscored, Scores: falsifyScores, Verdicts: falsifyVerdicts,
				CascadeStats: falsifyCascade, Observe: falsify.Observer(deps.PostStateObserve),
				Discovery: discoveryCorpus, // capture each scored deviation into the rolling discovery corpus (#10)
				Window:    window, Batch: envInt("TG_FALSIFIABILITY_BATCH", 200),
			}
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
						log.Printf("falsifiability writeback: scored %d prediction(s) [real_tp=%d control_tp=%d deviations=%d] — measurement only, mutation OFF",
							res.Scored, res.SumRealTP, res.SumControlTP, res.Deviations)
					}
					cancel()
				}
			}()
			log.Printf("falsifiability writeback: verify-time scoring every %s (observation window %s) — reads live post-incident alerts, mutation OFF", d, window)
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
				log.Printf("discovery-corpus flush: draining to %s every %s (measurement-plane, mutation OFF)", discoveryFile, flushInterval)
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
				Emit:   calibratejob.LogReliability,
			}
			go func() {
				t := time.NewTicker(d)
				defer t.Stop()
				for range t.C {
					cctx, cancel := context.WithTimeout(context.Background(), d)
					if _, cerr := calibJob.Run(cctx); cerr != nil {
						log.Printf("confidence calibrator: pass failed: %v (retry next tick)", cerr)
					}
					cancel()
				}
			}()
			log.Printf("confidence calibrator: reliability scoring every %s — observe-only, min_confidence gate stays OFF until calibrated", d)
		} else if derr != nil {
			log.Printf("confidence calibrator: invalid TG_CALIBRATION_INTERVAL %q — calibration disabled", iv)
		}
	}
	// The shared RECENCY/DECAY reconciliation (design-wisdom #11, Gulli ch14 — periodic reconciliation): ONE
	// periodic pass ages the THREE learned stores so recent evidence dominates and reality-contradicted state
	// fades — (1) lessons prune by provenance age, (2) core/learn co-occurrence counts decay on a half-life,
	// (3) estate learned edges a fresh verify DISPROOF contradicts lose confidence / age out. It is
	// COMPETENCE-plane only: it ages LEARNED state and NEVER touches the estate itself, actuates, or gates —
	// mutation stays OFF. OFF by default (TG_DECAY_INTERVAL unset); every step is fail-safe — a panic is
	// recovered and logged, so a decay error can never crash the worker. All knobs are config-not-code.
	if iv := getenv("TG_DECAY_INTERVAL", ""); iv != "" {
		if d, derr := time.ParseDuration(iv); derr == nil && d > 0 {
			learnHalfLife := envDuration("TG_LEARN_HALFLIFE", 30*24*time.Hour)
			edgeDecayFactor := envFloat("TG_ESTATE_DECAY_FACTOR", estate.DefaultDecayFactor)
			runDecay := func() {
				defer func() {
					if r := recover(); r != nil {
						log.Printf("decay: reconciliation pass panicked: %v (recovered — worker unaffected, mutation OFF)", r)
					}
				}()
				now := time.Now()
				// 1) lessons: prune precedents older than the retention horizon (no-op unless configured).
				reconcileLessons()
				// 2) core/learn: half-life the co-occurrence counts so old evidence fades toward zero.
				if st := learner.Decay(now, learnHalfLife); st.Pairs > 0 || st.Pruned > 0 {
					log.Printf("decay: learn half-life (%s) decayed %d co-occurrence pair(s), pruned %d faded", learnHalfLife, st.Pairs, st.Pruned)
				}
				// 3) estate: decay-on-disproof over the LEARNED tier. The disproof hosts are the surprise-hosts +
				// rule-mismatch hosts off the typed core/verify.VerdictDetail the falsify Scorer captured
				// (discoveryCorpus). Applied to a CLONE then atomically swapped, so a concurrent prediction read
				// never sees a half-mutated graph; only Source==incident edges decay (ground truth is untouched).
				if hosts := disproofHosts(discoveryCorpus.Snapshot()); len(hosts) > 0 {
					if newG, rep := estateHolder.Graph().DecayOnDisproof(estate.Disproof{Hosts: hosts, At: now}, estate.DecayOptions{Factor: edgeDecayFactor}); rep.Decayed > 0 {
						estateHolder.Set(newG)
						publishEstate(newG)
						log.Printf("decay: estate decay-on-disproof decayed %d learned edge(s), aged out %d (from %d disproof host(s)) — competence-plane, mutation OFF", rep.Decayed, rep.AgedOut, len(hosts))
					}
				}
			}
			runDecay() // one reconciliation pass at boot
			go func() {
				t := time.NewTicker(d)
				defer t.Stop()
				for range t.C {
					runDecay()
				}
			}()
			log.Printf("decay: shared recency/decay reconciliation every %s (lessons provenance-prune, learn half-life %s, estate decay-on-disproof factor %.2f) — competence-plane, mutation OFF", d, learnHalfLife, edgeDecayFactor)
		} else {
			log.Printf("decay: invalid TG_DECAY_INTERVAL %q — reconciliation disabled", iv)
		}
	}
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
	// feed (appendLessons, above). When the terminal reconcile confirms a CLEAN resolution, ReconcileActivity
	// emits the resolved incident through this seam; it is distilled (lessons.Merge → the SAME confirmed-clean
	// gate the export/decay paths use) and merged into the durable corpus the retriever reloads — so a
	// graduated op-class's next same-shape incident is no longer flagged NOVEL (it now has a precedent row
	// knowledge.Count sees, keyed on the EXACT (host, rule) the classifier read). The read-merge-write-reload is
	// serialized with the export/decay paths via lessonsMu so the three never race the corpus file. Requires a
	// durable corpus (TG_KNOWLEDGE_FILE) to persist into; without one there is nowhere to record a precedent, so
	// the seam stays nil (fail-safe — the writeback is simply skipped, novelty behaves exactly as before). It
	// writes ONLY the knowledge corpus — never the estate, never gated by the mutation chokepoint.
	if knowledgeHolder != nil && corpusPath != "" {
		deps.LearnResolved = func(_ context.Context, ri lessons.ResolvedIncident) error {
			lessonsMu.Lock()
			defer lessonsMu.Unlock()
			var existing []knowledge.Incident
			if cf, err := os.Open(corpusPath); err == nil {
				existing, _ = knowledge.ParseCorpus(cf)
				cf.Close()
			}
			// Distill through the SAME confirmed-clean gate the operator export uses; a non-clean or already-known
			// record contributes 0 and leaves the corpus (and its file) untouched — an idempotent no-op.
			merged, added := lessons.Merge(existing, []lessons.ResolvedIncident{ri})
			if added == 0 {
				// Writeback DIAGNOSTICS (TG-124): the reconcile gate already passed (Verdict=match, ConfirmedClear),
				// yet Merge added nothing — either lessons.Lesson rejected the record (blank external_ref/action) or
				// the (host, rule) is ALREADY a precedent. Both are otherwise-silent no-ops that read identically to a
				// gate failure from outside, so name the drop reason to make the observed writeback miss diagnosable.
				already := false
				for _, e := range existing {
					if e.Host == ri.Host && e.AlertRule == ri.AlertRule {
						already = true
						break
					}
				}
				log.Printf("novelty writeback: distilled-but-DROPPED %s (host=%s rule=%s action=%q): already_known=%v (0 added — no precedent recorded)", ri.ExternalRef, ri.Host, ri.AlertRule, ri.Action, already)
				return nil
			}
			// Atomic write: temp file + rename, so a concurrent reader (the reload loop) never sees a torn corpus.
			tmp := corpusPath + ".tmp"
			out, err := os.Create(tmp)
			if err != nil {
				return fmt.Errorf("novelty writeback: cannot write corpus temp %s: %w", tmp, err)
			}
			if werr := knowledge.WriteCorpus(out, merged); werr != nil {
				out.Close()
				os.Remove(tmp)
				return fmt.Errorf("novelty writeback: corpus serialize failed: %w", werr)
			}
			out.Close()
			if rerr := os.Rename(tmp, corpusPath); rerr != nil {
				os.Remove(tmp)
				return fmt.Errorf("novelty writeback: cannot replace corpus %s: %w", corpusPath, rerr)
			}
			knowledgeHolder.Set(loadCorpus()) // reload the seed∪maintained union after the write — never the maintained-only set (the seed must stay visible to the novelty gate)
			syncEmbed()                       // the new precedent becomes semantically retrievable too (best-effort, never blocking)
			log.Printf("novelty writeback: distilled resolved incident %s into %s (host=%s rule=%s)", ri.ExternalRef, corpusPath, ri.Host, ri.AlertRule)
			return nil
		}
		log.Printf("novelty writeback: live close-out feeder armed (corpus %s) — a confirmed-clean resolution de-novels its (host, rule)", corpusPath)
	}
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
	// The investigation also reasons WITH the incident's entry ticket when EXACTLY ONE tracker is enabled
	// (the entry tracker is otherwise ambiguous). Read-only; fail-open on any miss/error.
	trackerSrc, trackerCount := "", 0
	for _, cp := range moduleReg.Capabilities() {
		if cp.Surface == modules.SurfaceTracker && cp.Enabled {
			trackerSrc, trackerCount = cp.SourceType, trackerCount+1
		}
	}
	var entryTracker tracker.Tracker
	if trackerCount == 1 {
		if tr, terr := resolve.Tracker(moduleReg, trackerSrc); terr == nil {
			entryTracker = tr
			deps.TrackerRead = func(ctx context.Context, id string) (tracker.Issue, bool) {
				iss, rerr := tr.Open(ctx, id)
				if rerr != nil {
					return tracker.Issue{}, false
				}
				return iss, true
			}
			// The TERMINAL reconcile close-out (spec/003) transitions THIS tracker's ticket at a finished
			// session — a tracker write (annotate/transition), never an estate mutation. Wired only for the
			// single enabled tracker; nil otherwise ⇒ the reconcile records no close-out (fail-safe).
			deps.Tickets = runner.NewTrackerTransitioner(tr)
		}
	}
	// The human channel: deliver the governance notice/poll to on-call when EXACTLY ONE notifier is enabled
	// (multiple is ambiguous for a single bound decision — INV-12). Best-effort and fail-open: nil ⇒ no
	// delivery, and NotifyActivity swallows a delivery error so a notifier outage never fails the Runner.
	// Paging is the Phase-0/1 human-in-the-loop channel, not an estate mutation (never mutation-gated).
	notifierSrc, notifierCount := "", 0
	for _, cp := range moduleReg.Capabilities() {
		if cp.Surface == modules.SurfaceNotifier && cp.Enabled {
			notifierSrc, notifierCount = cp.SourceType, notifierCount+1
		}
	}
	if notifierCount == 1 {
		if nf, nerr := resolve.Notifier(moduleReg, notifierSrc); nerr == nil {
			deps.Notify = func(ctx context.Context, n notifier.Notice) error { return nf.Notify(ctx, n) }
			log.Printf("notifier: governance notices/polls delivered via %s", notifierSrc)
		}
	} else if notifierCount > 1 {
		log.Printf("notifier: %d notifiers enabled — ambiguous single channel, governance notices not delivered", notifierCount)
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
		escalationController = coreesc.NewController(escalationStore, failSafeActive{}, notifierPager{notify: deps.Notify}, reCheckCap)
		// The reconcile→escalation re-check hand-off (spec/003 REQ-206): an UNRESOLVED reconcile decision (an
		// orphaned poll) is requeued into THIS lane for a delayed re-check — rate-capped by the per-incident cap
		// (ScheduleReCheck stands down to a human at the cap). The delay is config-not-code.
		reCheckDelay := envDuration("TG_ESCALATION_RECHECK_DELAY", 15*time.Minute)
		deps.ReCheckSchedule = func(ctx context.Context, ref string, attempts int) error {
			_, err := escalationController.ScheduleReCheck(ctx, ref, attempts, time.Now().Add(reCheckDelay))
			return err
		}
		log.Printf("escalation requeue lane: durable store wired (per-incident cap %d, re-check delay %s) — fires via the FireDue cron, pages via the notifier, mutation OFF", reCheckCap, reCheckDelay)
	}
	// Tier-1 suppression's FIRST gate: operator-declared maintenance/chaos freeze windows (config-not-code,
	// TG_SUPPRESSION_FREEZE_FILE). An alert inside an active, in-scope window is an EXPECTED effect of declared
	// maintenance and is suppressed before spending a session — even at critical severity (the operator knows
	// it is coming). Wired only when windows are declared; otherwise the chain stays nil and every incident is
	// investigated (fail-open). Each decision is hash-chained into the governance ledger (INV-19).
	windows := freezeWindows(getenv("TG_SUPPRESSION_FREEZE_FILE", ""))
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
	if len(windows) > 0 || len(folds) > 0 || len(rules) > 0 || len(patterns) > 0 || len(schedules) > 0 || dedupWindow > 0 {
		gate := &runner.LiveSuppressGate{
			Folds: folds, FoldFreshness: 100 * 365 * 24 * time.Hour, // operator-declared policies have no learned staleness — only the valid window gates
			Schedules: schedules, RebootPreBuffer: rebootPre, RebootWindow: rebootWin,
			Patterns: patterns, Rules: rules,
			Window: dedupWindow, Ledger: ledger, Log: runner.NewRecentTriageLog(dedupWindow),
		}
		if len(windows) > 0 {
			gate.Freeze = &suppression.FreezeGate{Windows: windows}
		}
		// When a tracker is wired, dedup only holds while the anchor incident is still OPEN: a re-fire whose
		// parent ticket has RESOLVED is a genuine new incident and escalates. Fail-open — a read error treats
		// the incident as closed, so the re-fire is investigated rather than silently deduped.
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
		log.Printf("suppression: tier-1 gate active — %d freeze, %d fold(s), %d schedule(s), %d pattern(s), %d rule(s), dedup %s", len(windows), len(folds), len(schedules), len(patterns), len(rules), dedupWindow)
	}
	acts := runner.NewActivities(deps)

	w := worker.New(c, tg.TaskQueueRunner, worker.Options{})
	w.RegisterWorkflow(runner.RunnerWorkflow)
	// EVERY Runner activity registers through the ONE canonical list (runner.RegisterActivities) — the
	// same call the eval + acceptance harnesses make, so a workflow-referenced activity missing from
	// this composition root is structurally impossible. Two prod stalls on 2026-07-18
	// (RecordPendingActivity, then ResolvePendingActivity) came from hand-maintained per-site lists
	// drifting; register_test.go now proves the canonical list covers every *Activities method.
	runner.RegisterActivities(w, acts)
	if skillWriteActs != nil {
		w.RegisterWorkflow(skillwrite.TransitionWorkflow)
		w.RegisterActivity(skillWriteActs.TransitionActivity)
	}
	if configWriteActs != nil {
		// Distinctly-named workflows (the bare-function-name collision guard lives in
		// temporal/skilltrial/finalizer_names_test.go — these are on that list).
		w.RegisterWorkflow(configwrite.ConfigWriteWorkflow)
		w.RegisterWorkflow(configwrite.SecretPutWorkflow)
		w.RegisterActivity(configWriteActs.ApplyConfigActivity)
		w.RegisterActivity(configWriteActs.PutSecretActivity)
	}
	if modeTransitionActs != nil {
		// The operator-invoked autonomy-mode transition (spec/015 REQ-1502) — the LAST gate before the
		// mutation flip. Distinctly named (the bare-function-name collision guard is on the finalizer names
		// list). It runs on the chokepoint-bound controller; mutation stays OFF until an operator posts a flip.
		w.RegisterWorkflow(modetransition.ModeTransitionWorkflow)
		w.RegisterActivity(modeTransitionActs.ApplyModeTransitionActivity)
	}
	if skillTrialActs != nil {
		w.RegisterWorkflow(skilltrial.FinalizerWorkflow)
		w.RegisterActivity(skillTrialActs.FinalizeActivity)
		// The finalizer CRON: idempotent start — an already-running cron is the desired state.
		if _, serr := c.ExecuteWorkflow(context.Background(), client.StartWorkflowOptions{
			ID: skilltrial.WorkflowID, TaskQueue: tg.TaskQueueRunner, CronSchedule: skilltrial.CronSchedule,
			WorkflowIDReusePolicy: enumspb.WORKFLOW_ID_REUSE_POLICY_ALLOW_DUPLICATE,
		}, skilltrial.FinalizerWorkflow); serr != nil {
			var started *serviceerror.WorkflowExecutionAlreadyStarted
			if !errors.As(serr, &started) {
				log.Printf("skill-trial finalizer: cron start failed: %v (trials will not finalize until next boot)", serr)
			}
		} else {
			log.Printf("skill-trial finalizer: cron armed (%s)", skilltrial.CronSchedule)
		}
	}
	if skillJudgeActs != nil {
		w.RegisterWorkflow(skilljudge.JudgeWorkflow)
		w.RegisterActivity(skillJudgeActs.JudgeBatchActivity)
		// The judge CRON: idempotent start — an already-running cron is the desired state.
		if _, serr := c.ExecuteWorkflow(context.Background(), client.StartWorkflowOptions{
			ID: skilljudge.WorkflowID, TaskQueue: tg.TaskQueueRunner, CronSchedule: skilljudge.CronSchedule,
			WorkflowIDReusePolicy: enumspb.WORKFLOW_ID_REUSE_POLICY_ALLOW_DUPLICATE,
		}, skilljudge.JudgeWorkflow); serr != nil {
			var started *serviceerror.WorkflowExecutionAlreadyStarted
			if !errors.As(serr, &started) {
				log.Printf("session judge: cron start failed: %v (sessions will not be judged until next boot)", serr)
			}
		} else {
			log.Printf("session judge: cron armed (%s) — judging up to %d sessions per run", skilljudge.CronSchedule, skilljudge.BatchLimit)
		}
	}
	if skillGenActs != nil {
		// Distinctly-named workflow (the bare-function-name collision guard lives in
		// temporal/skilltrial/finalizer_names_test.go — GeneratorWorkflow is on that list).
		w.RegisterWorkflow(skillgen.GeneratorWorkflow)
		w.RegisterActivity(skillGenActs.GenerateActivity)
		// The generator CRON: idempotent start — an already-running cron is the desired state.
		if _, serr := c.ExecuteWorkflow(context.Background(), client.StartWorkflowOptions{
			ID: skillgen.WorkflowID, TaskQueue: tg.TaskQueueRunner, CronSchedule: skillgen.CronSchedule,
			WorkflowIDReusePolicy: enumspb.WORKFLOW_ID_REUSE_POLICY_ALLOW_DUPLICATE,
		}, skillgen.GeneratorWorkflow); serr != nil {
			var started *serviceerror.WorkflowExecutionAlreadyStarted
			if !errors.As(serr, &started) {
				log.Printf("skill generator: cron start failed: %v (candidates will not be generated until next boot)", serr)
			}
		} else {
			log.Printf("skill generator: cron armed (%s) — generate-only, competence-plane, mutation OFF", skillgen.CronSchedule)
		}
	}
	if escalationController != nil {
		// The escalation FireDue CRON (spec/003 wired, Gulli ch12): fires every DUE re-check so an enqueued
		// escalation actually re-escalates/pages/stands down. Distinctly-named workflow (Temporal registers by
		// bare function name). Idempotent start — an already-running cron is the desired state. A FireDue error
		// is captured in the activity Result and never crashes the worker.
		escActs := &escsched.Activities{D: escsched.Deps{Controller: escalationController}}
		w.RegisterWorkflow(escsched.FireDueWorkflow)
		w.RegisterActivity(escActs.FireDueActivity)
		if _, serr := c.ExecuteWorkflow(context.Background(), client.StartWorkflowOptions{
			ID: escsched.WorkflowID, TaskQueue: tg.TaskQueueRunner, CronSchedule: escsched.CronSchedule,
			WorkflowIDReusePolicy: enumspb.WORKFLOW_ID_REUSE_POLICY_ALLOW_DUPLICATE,
		}, escsched.FireDueWorkflow); serr != nil {
			var started *serviceerror.WorkflowExecutionAlreadyStarted
			if !errors.As(serr, &started) {
				log.Printf("escalation FireDue: cron start failed: %v (enqueued escalations will not fire until next boot)", serr)
			}
		} else {
			log.Printf("escalation FireDue: cron armed (%s) — fires due re-checks/re-escalations/pages, mutation OFF", escsched.CronSchedule)
		}
	}

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
	startWorkerAdmin(adminAddr, newWorkerAdmin(chokepoint, mutationBreaker, costAcct, ledger, haltToken).withSSHCredential(sshCredReport))

	log.Printf("read-only Runner worker up — queue=%s temporal=%s may_actuate=%v", tg.TaskQueueRunner, hostPort, chokepoint.MayActuate())
	if err := w.Run(worker.InterruptCh()); err != nil {
		log.Fatalf("worker exited: %v", err)
	}
}

// disproofHosts collects the deduplicated, sorted hosts a captured verify DEVIATION contradicts — the
// surprise-hosts (observed, unpredicted) plus the rule-mismatch hosts (predicted host, unpredicted rule),
// both read off the typed core/verify.VerdictDetail the falsify Scorer captured into the discovery corpus.
// These feed the estate decay-on-disproof pass; only LEARNED edges incident to them decay, so a surprise
// host with no learned edge is a harmless no-op. Read-only over the snapshot — it never drains the corpus.
func disproofHosts(captured []falsify.CapturedDeviation) []string {
	set := map[string]struct{}{}
	for _, cd := range captured {
		for _, h := range cd.Record.SurpriseHosts {
			if h != "" {
				set[h] = struct{}{}
			}
		}
		for _, m := range cd.Record.Mismatches {
			if m.Host != "" {
				set[m.Host] = struct{}{}
			}
		}
	}
	out := make([]string, 0, len(set))
	for h := range set {
		out = append(out, h)
	}
	sort.Strings(out)
	return out
}

// mutationBreakerThreshold reads the operator-declared breaker trip threshold (config-not-code). The first
// canary uses 1 (a single deviation halts) per the readiness review; a non-positive/invalid value falls
// back to 1 — fail toward the tightest setting, never a looser one.
func mutationBreakerThreshold() int {
	n, err := strconv.Atoi(strings.TrimSpace(getenv("TG_MUTATION_BREAKER_THRESHOLD", "1")))
	if err != nil || n < 1 {
		return 1
	}
	return n
}

// ledgerTripRecorder binds a mutation-breaker auto-trip to the governance ledger (safety.TripRecorder):
// when the breaker trips and disables the gate, the halt is hash-chained like every other governance
// decision (INV-19). A nil ledger is a no-op.
type ledgerTripRecorder struct{ l *audit.Ledger }

func (r ledgerTripRecorder) RecordTrip(reason string) {
	if r.l == nil {
		return
	}
	if _, err := r.l.Append(audit.GovDecision{
		Decision: "safety:breaker-trip",
		Reason:   reason,
		ActionID: "mutation-breaker-trip",
		Withheld: true, // autonomy withheld — the breaker turned mutation off
	}); err != nil {
		log.Printf("mutation breaker: trip applied but ledger append failed: %v", err)
	}
}

// breakerRearmer implements policy.BreakerRearmer: the RECOVERY counterpart to ledgerTripRecorder. When an
// owner-gated mode transition escalates INTO an actuating mode, the ModeController calls Rearm to clear a
// deviation breaker a prior trip left durably open (spec/015 REQ-1523) — without which one (possibly false)
// trip permanently refuses all actuation even after the mode is restored. It appends the audit record
// BEFORE clearing the breaker (audit-before-effect, mirroring the mode transition itself); an append failure
// returns the error and leaves the breaker OPEN (fail-safe — actuation stays halted, never half-enabled). It
// lives in the worker, the single process holding the armed breaker, its shared cross-process store, and the
// ledger writer, so one worker's re-arm closes the shared row for every sibling.
type breakerRearmer struct {
	mb     *safety.MutationBreaker
	ledger *audit.Ledger
}

func (r breakerRearmer) Rearm(ctx context.Context) error {
	if r.mb == nil {
		return nil
	}
	if r.ledger != nil {
		if _, err := r.ledger.Append(audit.GovDecision{
			Decision: "safety:breaker-rearm",
			Reason:   "deviation breaker re-armed on an owner-gated escalation into an actuating mode (spec/015 REQ-1523) — trip cleared so actuation can resume",
			ActionID: "mutation-breaker-rearm",
		}); err != nil {
			return fmt.Errorf("breaker re-arm audit append failed, breaker left open: %w", err)
		}
	}
	return r.mb.Rearm(ctx)
}

// costLedgerTripRecorder binds a COST-breaker auto-trip to the governance ledger (cost.TripRecorder): when
// the daily budget or a session ceiling is exceeded and the breaker force-Shadows, the halt is hash-chained
// like every other governance decision (INV-19). Distinct from the mutation breaker's recorder by its
// 'cost:breaker-trip' decision label so a spend halt is auditable apart from a safety halt. A nil ledger is
// a no-op.
type costLedgerTripRecorder struct{ l *audit.Ledger }

func (r costLedgerTripRecorder) RecordTrip(reason string) {
	if r.l == nil {
		return
	}
	if _, err := r.l.Append(audit.GovDecision{
		Decision: "cost:breaker-trip",
		Reason:   reason,
		ActionID: "cost-breaker-trip",
		Withheld: true, // autonomy withheld — the spend guard forced the mode to Shadow
	}); err != nil {
		log.Printf("cost breaker: trip applied but ledger append failed: %v", err)
	}
}

// readCostConfig reads the operator-declared spend policy (config-not-code) from TG_COST_* env into a
// cost.Config. Money defaults to 0 = DISABLED (a budget/rate that is not set never enforces — the spend
// guard's fail-open posture). Per-model rates come from every TG_COST_RATE_<model>_PER_1K variable (the
// <model> is the gateway tier the agent calls, e.g. "fast" / "primary"); TG_COST_DEFAULT_RATE_PER_1K is
// the fallback for a model with no explicit rate.
func readCostConfig() cost.Config {
	return cost.Config{
		Rates:             readCostRates(),
		DefaultRate:       envFloat("TG_COST_DEFAULT_RATE_PER_1K", 0),
		PerActuationUSD:   envFloat("TG_COST_PER_ACTUATION_USD", 0),
		DailyBudgetUSD:    envFloat("TG_COST_DAILY_BUDGET_USD", 0),
		SessionCeilingUSD: envFloat("TG_COST_SESSION_CEILING_USD", 0),
	}
}

// readCostRates scans the environment for TG_COST_RATE_<model>_PER_1K variables, extracting the per-model
// USD-per-1k-tokens rate keyed by <model>. A non-positive/invalid value is skipped (config-not-code; a
// zero rate contributes no cost, so there is no reason to record it).
func readCostRates() map[string]float64 {
	const (
		prefix = "TG_COST_RATE_"
		suffix = "_PER_1K"
	)
	rates := map[string]float64{}
	for _, kv := range os.Environ() {
		eq := strings.IndexByte(kv, '=')
		if eq < 0 {
			continue
		}
		key, val := kv[:eq], kv[eq+1:]
		if !strings.HasPrefix(key, prefix) || !strings.HasSuffix(key, suffix) {
			continue
		}
		model := strings.TrimSuffix(strings.TrimPrefix(key, prefix), suffix)
		if model == "" {
			continue
		}
		if v, err := strconv.ParseFloat(strings.TrimSpace(val), 64); err == nil && v > 0 {
			rates[model] = v
		}
	}
	return rates
}

// failSafeActive is the escalation condition oracle DEFAULT (core/escalation.ConditionChecker): with no
// live post-condition reader wired it treats every incident as STILL ACTIVE, so a due re-check re-escalates
// to a human rather than silently dropping an unresolved incident (fail SAFE toward escalation — never
// toward closure). A live LibreNMS active-alert oracle can later replace it to also DEFER a genuinely
// recovered incident. It reads nothing and mutates nothing.
type failSafeActive struct{}

func (failSafeActive) StillActive(context.Context, string) (bool, error) { return true, nil }

// notifierPager pages an approver tier through the human notifier channel (core/escalation.Pager) — an
// escalation PAGE, not an approval poll (Approval=false). Paging is the Phase-0/1 human-in-the-loop
// channel, never an estate mutation and never mutation-gated. A nil notifier ⇒ a LOGGING pager: the
// re-escalation is recorded to the log (best-effort) rather than lost, so the FireDue lane still records
// its decisions when no channel is configured.
type notifierPager struct {
	notify func(ctx context.Context, n notifier.Notice) error
}

func (p notifierPager) Page(ctx context.Context, externalRef, tier string) error {
	body := "escalation re-check for " + externalRef + " — paging " + tier
	if p.notify == nil {
		log.Printf("escalation: %s (no notifier wired — page recorded to log only)", body)
		return nil
	}
	return p.notify(ctx, notifier.Notice{DecisionID: externalRef, Body: body, Approval: false})
}

// importCompiledSkills idempotently seeds the skill store from the compiled registry (spec/014
// REQ-1304): identities first (conservative-remediation pinned — the hard floor), then each compiled
// body as a production row. A compiled UPGRADE (new version in code) supersedes a prior compiled-import
// row through the audited Transition; a GRADUATED store row is never displaced. Degrades on error —
// composition falls back to the compiled registry regardless.
func importCompiledSkills(ctx context.Context, st *db.SkillStore, lg *audit.Ledger) {
	compiled := skills.Default().All()
	imported := 0
	for i, sk := range compiled {
		pinned := sk.Name == "conservative-remediation"
		if err := st.PutSkill(ctx, skillstore.Skill{Name: sk.Name, Kind: "behavioral", Pinned: pinned, Position: i}); err != nil {
			log.Printf("skills: import identity %s: %v (degraded — compiled fallback covers it)", sk.Name, err)
			continue
		}
		// The compiled registry's selectors are Go funcs; their declarative equivalent for the store row
		// is the closed-vocabulary predicate. Compiled `always` skills map to the empty predicate; the
		// exec-class-scoped ones to the standard/deep pair (the compiled func remains authoritative for
		// composition of compiled-origin bodies — this row is the console/library representation).
		aw := skillstore.AppliesWhen{}
		if !sk.AppliesWhen(skills.Context{Phase: skills.PhaseInvestigate, ExecClass: execclass.FastAgent}) {
			aw = skillstore.AppliesWhen{ExecClasses: []string{string(execclass.StandardAgent), string(execclass.DeepInvestigation)}}
		}
		if cur, ok, err := st.ProductionVersion(ctx, sk.Name); err == nil && ok &&
			cur.Source == "compiled-import" && cur.Version != sk.Version {
			if _, terr := skillstore.Transition(ctx, st, lg, cur.ID, skillstore.StatusRetired,
				"compiled registry upgraded to v"+sk.Version); terr != nil {
				log.Printf("skills: supersede compiled %s v%s: %v", sk.Name, cur.Version, terr)
				continue
			}
		}
		if err := st.ImportCompiledVersion(ctx, sk.Name, sk.Version, sk.Body, aw); err != nil {
			// After a successful supersede-retire a failed import leaves the skill with NO production
			// row (crash window). Composition is unaffected (total compiled fallback) and the next boot
			// heals it (the NOT EXISTS guard admits the import), but retry once and log LOUDLY so the
			// console's library view being production-less for this skill is never a silent mystery.
			if rerr := st.ImportCompiledVersion(ctx, sk.Name, sk.Version, sk.Body, aw); rerr != nil {
				log.Printf("skills: import %s v%s FAILED TWICE: %v — library shows no production row until the next boot (composition unaffected)", sk.Name, sk.Version, rerr)
				continue
			}
		}
		imported++
	}
	log.Printf("skills: store seeded from the compiled registry (%d/%d skills)", imported, len(compiled))
}

// ---------------------------------------------------------------------------------------------------------
// Actuation Regime Engine wiring (spec/017, TG-110). The engine answers "through which effect channel?" and
// COMPOSES over the already-built controls (interceptor spec/013, policy spec/015, credential spec/016, the
// mode chokepoint core/safety); it authorizes nothing, authenticates nothing, and lifts no floor. It is
// WIRED but INERT: every lane is reachable only through the interceptor's Do (which refuses at Shadow), and
// the awx-job lane re-guards the mode at its own leaf. Nothing here transitions the mode, enables actuation,
// or launches a job at Shadow — the default/absent/corrupt mode stays Shadow (may_actuate=false).
// ---------------------------------------------------------------------------------------------------------

// wireActuationRegime constructs the regime resolver (config-not-code rules over the SHARED estate object-
// model), the native-ssh + awx-job lanes, the append-only regime audit writer, and — when an AWX launch
// client is declared — the GLOBAL deferred-verify channel + its poll cron. It fails the boot CLOSED on a
// malformed rule/allowlist (a bad regime mapping must never route a target down an undefined channel) and
// logs the exact posture: regime wired, lanes registered, the awx-job actuator state (real vs fail-closed),
// mode Shadow, may_actuate=false. sshLeaf is the SAME effect leaf the interceptor already wires.
// nativeSSHLaneFor builds the regime native-ssh lane. By DEFAULT it is the STATIC single-host / read-only leaf
// the interceptor already wires (behaviour-preserving). When TG_ACTUATION_SSH_PER_TARGET is set (REQ-1717,
// P3-B2) it instead returns a PER-TARGET lane: each action's effect leaf binds to the action's OWN target host,
// authenticating with the operator's configured ACTUATION identity + key (TG_ACTUATION_SSH_IDENTITY /
// TG_ACTUATION_SSH_KEY — DISTINCT from any read/hostdiag identity, preserving the credential plane-split)
// verified against the fleet-wide TG_ACTUATION_SSH_KNOWN_HOSTS. It stays DORMANT until explicitly armed: the
// flag is OFF by default; the mode chokepoint (Shadow) refuses every Exec until the owner flips mutation on; an
// empty TG_ACTUATION_ALLOWED_UNITS allowlist refuses every unit; and the spec/013 host-match gate (REQ-1219)
// passes only because ActuationHost()==target by construction and stays as defense-in-depth. A per-target build
// refusal (unset identity/key, empty target) surfaces as a GOVERNED refusal at LaneEffect.Apply, never a bypass.
func nativeSSHLaneFor(chokepoint *safety.Chokepoint, staticLeaf actuation.Actuator) regime.Lane {
	if !truthyEnv("TG_ACTUATION_SSH_PER_TARGET") {
		return regime.NewNativeSSHLane(staticLeaf)
	}
	identity := getenv("TG_ACTUATION_SSH_IDENTITY", "")
	keyRef := config.SecretRef(getenv("TG_ACTUATION_SSH_KEY", ""))
	knownHosts := getenv("TG_ACTUATION_SSH_KNOWN_HOSTS", "")
	allowedUnits := bootstrap.ParseAllowedUnits(getenv("TG_ACTUATION_ALLOWED_UNITS", ""))
	allowedContainers := bootstrap.ParseAllowedContainers(getenv("TG_ACTUATION_ALLOWED_CONTAINERS", ""))
	log.Printf("actuation regime: native-ssh lane = PER-TARGET (TG_ACTUATION_SSH_PER_TARGET on, REQ-1717) — each action's leaf binds to its own target host via actuation identity %q over the fleet known_hosts; %d allowed unit(s); mutation still gated by the mode chokepoint + allowlist + host-match gate (dormant until an operator arms mutation)", identity, len(allowedUnits))
	return regime.NewNativeSSHLaneFunc(func(ctx context.Context, target string) (actuation.Actuator, error) {
		target = strings.TrimSpace(target)
		if target == "" {
			return nil, fmt.Errorf("empty target host — refusing to build an actuation leaf")
		}
		// Fail CLEANLY on an incomplete actuation identity: BuildSSHActuator only rejects an empty HOST or
		// IDENTITY, so a set identity + empty key ref would build a leaf that fails opaquely at connect
		// (not a clean fail-closed refusal). Require both up front so a misconfig is a governed refusal.
		if strings.TrimSpace(identity) == "" || strings.TrimSpace(string(keyRef)) == "" {
			return nil, fmt.Errorf("per-target actuation requires BOTH TG_ACTUATION_SSH_IDENTITY and TG_ACTUATION_SSH_KEY — refusing to build a leaf with an empty identity or key ref")
		}
		m := bootstrap.BuildSSHActuator(chokepoint, target, identity, sshactuation.NewNativeRunner(knownHosts, keyRef), allowedUnits, allowedContainers)
		if m == nil {
			return nil, fmt.Errorf("actuation leaf could not be built for %q (incomplete actuation config)", target)
		}
		return m, nil
	})
}

func wireActuationRegime(chokepoint *safety.Chokepoint, ledger *audit.Ledger, sshLeaf actuation.Actuator, grad *policy.Ladder, pool *db.Pool, modeName string) *regime.Engine {
	// (1) Config-not-code regime rules over the shared object-model (REQ-1700/1703). A malformed rule is a
	//     boot refusal — fail closed rather than silently dropping a regime mapping.
	rules, err := parseRegimeRules(getenv("TG_REGIME_RULES", ""))
	if err != nil {
		log.Fatalf("actuation regime: %v (fail closed — a malformed regime rule never routes a target down an undefined channel)", err)
	}

	// (2) native-ssh lane: the EXISTING spec/013 SSH effect leaf re-expressed as one lane among several
	//     (REQ-1700). It is the operator-declared default lane for an unmatched target (REQ-1701) unless
	//     TG_REGIME_DEFAULT_LANE=none disables the default (then an unmatched target refuses, fail closed).
	//     With TG_ACTUATION_SSH_PER_TARGET set (REQ-1717, P3-B2) it becomes a PER-TARGET lane — each action's
	//     leaf binds to its OWN target host rather than a single configured host — else the static leaf.
	sshLane := nativeSSHLaneFor(chokepoint, sshLeaf)

	// (3) awx-job lane: its REAL effect leaf (modules/actuation/awxjob) is injected ONLY when the operator
	//     declares an AWX base URL AND a DISTINCT launch-capable token (REQ-1706/1708). ABSENT ⇒ the lane
	//     keeps its pendingActuator fail-closed default (it can only REFUSE) — never a permissive default.
	awxBase := strings.TrimSpace(getenv("TG_AWXJOB_BASE_URL", ""))
	awxTokenRef := strings.TrimSpace(getenv("TG_AWXJOB_LAUNCH_TOKEN_REF", ""))
	var (
		awxClient   *awxjob.Client
		awxState    = "pendingActuator — FAIL-CLOSED refuse (TG_AWXJOB_BASE_URL / TG_AWXJOB_LAUNCH_TOKEN_REF unset)"
		awxLaneOpts []regime.AWXJobLaneOption
	)
	if awxBase != "" && awxTokenRef != "" {
		allowlist, aerr := parseAWXJobAllowlist(getenv("TG_AWXJOB_ALLOWLIST", ""))
		if aerr != nil {
			log.Fatalf("actuation regime: %v (fail closed)", aerr)
		}
		client, cerr := awxjob.NewClient(awxjob.ClientConfig{
			BaseURL:    awxBase,
			TokenRef:   config.SecretRef(awxTokenRef),
			CACertPath: getenv("TG_AWXJOB_CA", ""),
		})
		if cerr != nil {
			log.Fatalf("actuation regime: awx-job launch client (fail closed): %v", cerr)
		}
		// The mode chokepoint is passed as the actuator's own defense-in-depth gate: ReadOnly() is true and
		// Exec refuses at Shadow BEFORE any network launch, independent of the interceptor's gate.
		actuator, xerr := awxjob.New(awxjob.Config{Client: client, Allowlist: allowlist, ModeGate: chokepoint})
		if xerr != nil {
			log.Fatalf("actuation regime: awx-job actuator (fail closed): %v", xerr)
		}
		awxClient = client
		awxLaneOpts = append(awxLaneOpts, regime.WithAWXActuator(actuator))
		awxState = fmt.Sprintf("real awxjob actuator (read_only=%v, allowlist=%d template(s)) — routes beneath the mode chokepoint (Shadow), inert until the owner-present flip", actuator.ReadOnly(), len(allowlist))
	}
	awxLane := regime.NewAWXJobLane(awxLaneOpts...)

	// (3b) proxmox lane: the PVE hypervisor guest-LIFECYCLE channel (start-guest). Its REAL effect leaf
	//      (modules/actuation/proxmox) is injected ONLY when the operator declares a PVE base URL AND an API
	//      token ref. ABSENT ⇒ the lane keeps its pendingActuator fail-closed default (refuse only). It is
	//      selected by the proxmox-lifecycle effect KIND (not the target regime). The actuator floor-clamps the
	//      lifecycle verb (reboot/shutdown/reset/destroy never auto-execute), allowlists the guest, and re-checks
	//      the mode chokepoint at its own leaf — defense in depth, inert at Shadow.
	pveBase := strings.TrimSpace(getenv("TG_PROXMOX_BASE_URL", ""))
	pveTokenRef := strings.TrimSpace(getenv("TG_PROXMOX_TOKEN_REF", ""))
	var (
		proxmoxState    = "pendingActuator — FAIL-CLOSED refuse (TG_PROXMOX_BASE_URL / TG_PROXMOX_TOKEN_REF unset)"
		proxmoxLaneOpts []regime.ProxmoxLaneOption
	)
	if pveBase != "" && pveTokenRef != "" {
		allowedGuests := splitTokens(getenv("TG_PROXMOX_ALLOWED_GUESTS", ""))
		// A DEDICATED actuation HTTP client (estateHTTPClient is scoped read-only). PVE serves a self-signed cert
		// on :8006, so certificate verification is opt-in-disabled via TG_PROXMOX_INSECURE for the internal
		// endpoint; otherwise system roots apply. A short timeout bounds a stuck launch.
		pveClient := &http.Client{Timeout: 30 * time.Second}
		if truthyEnv("TG_PROXMOX_INSECURE") {
			pveClient.Transport = &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}
		}
		pveActuator := proxmoxactuation.New(pveBase, config.SecretRef(pveTokenRef),
			proxmoxactuation.WithMutation(chokepoint, allowedGuests),
			proxmoxactuation.WithHTTPClient(pveClient))
		proxmoxLaneOpts = append(proxmoxLaneOpts, regime.WithProxmoxActuator(pveActuator))
		proxmoxState = fmt.Sprintf("real proxmox actuator (read_only=%v, allowed_guests=%d) — routes beneath the mode chokepoint (Shadow), inert until the owner-present flip", pveActuator.ReadOnly(), len(allowedGuests))
	}
	proxmoxLane := regime.NewProxmoxLane(proxmoxLaneOpts...)

	// (4) Build the resolver. Unmatched targets take the operator default lane (native-ssh) unless disabled.
	var engOpts []regime.EngineOption
	defaultLane := strings.ToLower(strings.TrimSpace(getenv("TG_REGIME_DEFAULT_LANE", "native-ssh")))
	switch defaultLane {
	case "", "native-ssh":
		engOpts = append(engOpts, regime.WithDefaultLane(sshLane))
		defaultLane = "native-ssh"
	case "none", "refuse":
		defaultLane = "none (unmatched targets refuse — fail closed)"
	default:
		log.Fatalf("actuation regime: TG_REGIME_DEFAULT_LANE=%q unsupported (use native-ssh or none)", defaultLane)
	}
	engine := regime.NewEngine(rules, []regime.Lane{sshLane, awxLane, proxmoxLane}, engOpts...)

	// Boot self-validation (fail-safe, non-fatal): every declared rule must resolve to a WIRED lane. A rule
	// that resolves to a regime with no wired lane (e.g. gitops-mr, a future lane) is logged as inert — an
	// operator config warning, not a crash (mutation is OFF regardless).
	wired, inert := 0, 0
	for _, r := range rules {
		if _, serr := engine.SelectLane(targetForSelector(r.Selector)); serr != nil {
			log.Printf("actuation regime: rule %q (regime %s) resolves to NO wired lane yet — inert (%v)", r.ID, r.Regime, serr)
			inert++
			continue
		}
		wired++
	}

	// (5) Append-only regime audit writer (REQ-1715, migration 0020): one row per resolution / launch /
	//     deferred verdict, each also chained into the governance ledger; no secret value is ever written
	//     (only a SecretRef reference). Constructed ready; the execute-path callers (RecordResolution /
	//     RecordActuation) land with the runner integration at the flip — today only the deferred-verify poll
	//     cron writes, and only after a launch, which cannot happen at Shadow.
	regimeAudit := regime.NewAudit(db.NewRegimeAuditWriteStore(pool), ledger)

	// (6) GLOBAL deferred-verify channel + poll cron (REQ-1709..1712) — armed ONLY when the awx-job launch
	//     client exists (its read-only GetJob is the poller). It OBSERVES; it launches nothing. At Shadow no
	//     launch ever reserves a pending record, so the cron polls an EMPTY queue. The pending-verification
	//     store is now the DURABLE pgx table (migration 0022, T-017-8): a launched job's deferred verify
	//     SURVIVES a worker restart — the flip-prerequisite — so a mutation whose effect was never confirmed
	//     stays a visible pending/unverified record instead of being forgotten. pool is always present here
	//     (wireActuationRegime is called only from the DB-present boot path); the in-memory fake stays the
	//     no-DB fallback so the channel is never left unwired.
	asyncState := "off (no awx-job launch client — no async lane to verify)"
	if awxClient != nil {
		bound := envDuration("TG_REGIME_VERIFY_BOUND", regime.DefaultVerificationBound)
		var pendingStore regime.PendingStore
		pendingKind := "in-memory (no DB pool)"
		if pool != nil {
			pendingStore = db.NewRegimePendingWriteStore(pool)
			pendingKind = "durable pgx (pending_verification, 0022 — survives restart)"
		} else {
			pendingStore = regime.NewMemPendingStore()
		}
		av, verr := regime.NewAsyncVerify(pendingStore, awxJobPoller{client: awxClient},
			regime.WithGraduationSink(regimeGradSink{ladder: grad}),
			regime.WithVerificationBound(bound),
			regime.WithLogger(log.Printf))
		if verr != nil {
			log.Fatalf("actuation regime: async-verify channel (fail closed): %v", verr)
		}
		interval := envDuration("TG_REGIME_VERIFY_INTERVAL", time.Minute)
		go regimeVerifyLoop(av, regimeAudit, interval)
		asyncState = fmt.Sprintf("ARMED (poll every %s, bound %s; %s pending store, empty at Shadow)", interval, bound, pendingKind)
	}

	log.Printf("actuation regime engine: WIRED (spec/017) — resolver over %d rule(s) (%d→wired lane, %d→inert), default lane=%s; lanes registered: native-ssh (effect leaf=%s, read_only=%v), awx-job (%s), proxmox (%s); async-verify=%s; audit=pgx regime_resolution/regime_actuation/deferred_verdict (0020, append-only) + governance ledger; mode=%s may_actuate=%v — every lane routes beneath the interceptor + mode chokepoint, no path actuates at Shadow",
		len(rules), wired, inert, defaultLane, sshLeaf.Capability(), sshLeaf.ReadOnly(), awxState, proxmoxState, asyncState, modeName, chokepoint.MayActuate())
	// Return the engine so the caller routes the execute activity's dispatch through it (SelectLane → LaneEffect
	// → the per-lane spec/013 interceptor). Before this wave the engine was built for its INERT side effects
	// (audit + async-verify loop) and discarded; now it is the actuation path's regime resolver (REQ-1700).
	return engine
}

// regimeVerifyLoop drives the GLOBAL deferred-verify channel on an interval (REQ-1709..1712). Each tick lists
// the pending-verification queue (empty at Shadow) and single-steps Verify per action; on the ONE transition
// to a terminal outcome it appends the append-only deferred_verdict audit row (REQ-1715). It launches nothing
// and never crashes the worker — a poll/persist error is logged and retried next tick.
func regimeVerifyLoop(av *regime.AsyncVerify, aud *regime.Audit, interval time.Duration) {
	if interval <= 0 {
		interval = time.Minute
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for range t.C {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		pending, err := av.PendingQueue(ctx)
		if err != nil {
			log.Printf("regime async-verify: list pending failed: %v (retried next tick)", err)
			cancel()
			continue
		}
		for _, rec := range pending {
			res, verr := av.Verify(ctx, rec.ActionID)
			if verr != nil {
				log.Printf("regime async-verify: verify action %s: %v (still pending)", rec.ActionID, verr)
				continue
			}
			// Persist the append-only deferred_verdict row ONCE — on the transition to a terminal (GradFed is
			// true only on that call). A timeout (StateUnverified) has no terminal AWX status, so it never
			// writes a deferred_verdict row (the 0020 status CHECK requires a terminal); the pending record +
			// graduation feed already handle it.
			if res.GradFed && res.State == regime.StateVerified && res.TerminalStatus.Valid() {
				if werr := aud.RecordDeferredVerdict(ctx, deferredVerdictRow(res)); werr != nil {
					log.Printf("regime async-verify: persist deferred verdict for %s: %v", res.ActionID, werr)
				}
			}
		}
		cancel()
	}
}

// deferredVerdictRow maps a completed deferred-verify resolution onto the append-only deferred_verdict row
// vocabulary (REQ-1715): the mechanical verdict slug and the earned-trust graduation outcome, both non-secret.
func deferredVerdictRow(res regime.DeferredResolution) regime.DeferredVerdictRow {
	var v regime.Verdict
	switch res.Verdict {
	case safety.VerdictMatch:
		v = regime.VerdictMatch
	case safety.VerdictDeviation:
		v = regime.VerdictDeviation
	default:
		v = regime.VerdictUnverified
	}
	var g regime.GraduationOutcome
	switch {
	case res.CleanRun:
		g = regime.GraduationVerifiedClean
	case res.Verified && res.Verdict == safety.VerdictDeviation:
		g = regime.GraduationDeviated
	default:
		g = regime.GraduationNoCredit
	}
	return regime.DeferredVerdictRow{
		ActionID:   res.ActionID,
		JobID:      res.JobID,
		Status:     res.TerminalStatus,
		Verdict:    v,
		Graduation: g,
	}
}

// regimeGradSink adapts the regime deferred-verify channel's decoupled GraduationSink onto the spec/015
// policy graduation ladder WITHOUT core/regime importing core/policy (REQ-1710): it maps a deferred verdict
// to the ladder's RunOutcome (OutcomeFromVerdict) and records it against the op-class. An unverified run is
// fed as not-clean, so a launch we could not confirm within the bound never earns autonomy.
type regimeGradSink struct{ ladder *policy.Ladder }

// RecordDeferredVerdict feeds one completed deferred verify to the graduation ladder.
func (s regimeGradSink) RecordDeferredVerdict(ctx context.Context, opClass string, v safety.Verdict, verified bool) error {
	if s.ladder == nil {
		return nil
	}
	_, err := s.ladder.Record(ctx, opClass, policy.OutcomeFromVerdict(v, verified))
	return err
}

// awxJobPoller adapts the AWX-job launch client's read-only GetJob into the regime deferred-verify JobPoller
// (REQ-1709). It OBSERVES only (GET /api/v2/jobs/{id}/) — it launches nothing. A non-numeric handle or an
// unreadable read leaves the deferred verify pending (the channel retries) rather than fabricating a terminal.
type awxJobPoller struct{ client *awxjob.Client }

// PollJob reads the current AWX job status by its handle.
func (p awxJobPoller) PollJob(ctx context.Context, jobID string) (regime.JobStatus, error) {
	id, err := strconv.Atoi(strings.TrimSpace(jobID))
	if err != nil || id <= 0 {
		return "", fmt.Errorf("regime poll: non-numeric AWX job handle %q", jobID)
	}
	j, err := p.client.GetJob(ctx, id)
	if err != nil {
		return "", err
	}
	return regime.JobStatus(strings.TrimSpace(j.Status)), nil
}

// regimeRuleSpec is one operator-declared regime rule as config-not-code JSON (TG_REGIME_RULES). The selector
// is a "kind:pattern" token over the SHARED estate object-model, the same grammar policy + credential key off.
type regimeRuleSpec struct {
	ID       string `json:"id"`
	Selector string `json:"selector"`
	Regime   string `json:"regime"`
}

// parseRegimeRules parses TG_REGIME_RULES (a JSON array of {id,selector,regime}) into regime rules. Empty ⇒
// no rules (every target then takes the operator default lane). A malformed rule / unknown regime / unknown
// selector kind fails closed.
func parseRegimeRules(spec string) ([]regime.Rule, error) {
	spec = strings.TrimSpace(spec)
	if spec == "" {
		return nil, nil
	}
	var raw []regimeRuleSpec
	if err := json.Unmarshal([]byte(spec), &raw); err != nil {
		return nil, fmt.Errorf("TG_REGIME_RULES is not a JSON array of {id,selector,regime}: %w", err)
	}
	out := make([]regime.Rule, 0, len(raw))
	for i, r := range raw {
		id := strings.TrimSpace(r.ID)
		if id == "" {
			return nil, fmt.Errorf("TG_REGIME_RULES[%d]: rule id is required (audit provenance)", i)
		}
		sel, err := parseSharedSelector(r.Selector)
		if err != nil {
			return nil, fmt.Errorf("TG_REGIME_RULES[%d] (%q): %w", i, id, err)
		}
		reg := regime.Regime(strings.TrimSpace(r.Regime))
		if !reg.Valid() {
			return nil, fmt.Errorf("TG_REGIME_RULES[%d] (%q): unknown regime %q (want native-ssh/awx-job/gitops-mr/k8s-declarative/api)", i, id, r.Regime)
		}
		out = append(out, regime.Rule{ID: id, Selector: sel, Regime: reg})
	}
	return out, nil
}

// parseSharedSelector parses a "kind:pattern" token into a shared-object-model Selector (mirrors the
// credential resolver's grammar; a malformed/unknown kind fails closed by construction).
func parseSharedSelector(tok string) (credential.Selector, error) {
	tok = strings.TrimSpace(tok)
	i := strings.IndexByte(tok, ':')
	if i < 0 {
		return credential.Selector{}, fmt.Errorf("malformed selector %q: want kind:pattern", tok)
	}
	kind := credential.SelectorKind(strings.TrimSpace(tok[:i]))
	pattern := strings.TrimSpace(tok[i+1:])
	if pattern == "" {
		return credential.Selector{}, fmt.Errorf("selector %q has an empty pattern", tok)
	}
	switch kind {
	case credential.KindHost, credential.KindResource, credential.KindHostGlob, credential.KindGroup, credential.KindDeviceClass:
		return credential.Selector{Kind: kind, Pattern: pattern}, nil
	default:
		return credential.Selector{}, fmt.Errorf("selector %q has unknown kind %q (use host/host-glob/group/device-class/resource)", tok, kind)
	}
}

// targetForSelector builds a representative estate Target that a selector matches, for the boot self-check
// (does each declared rule resolve to a wired lane?). It maps the selector kind to the Target field it keys.
func targetForSelector(s credential.Selector) credential.Target {
	switch s.Kind {
	case credential.KindHost, credential.KindHostGlob:
		return credential.Target{Host: s.Pattern}
	case credential.KindResource:
		return credential.Target{Resource: s.Pattern}
	case credential.KindGroup:
		return credential.Target{Groups: []string{s.Pattern}}
	case credential.KindDeviceClass:
		return credential.Target{DeviceClass: s.Pattern}
	default:
		return credential.Target{}
	}
}

// awxTemplateSpec is one allowlisted AWX job template as config-not-code JSON (TG_AWXJOB_ALLOWLIST): the AWX
// job_template id, the op-class the policy engine authorizes for it, and the CLOSED extra_vars schema its
// launch variables must conform to (REQ-1704/1705). No command string anywhere — a template is not a shell.
type awxTemplateSpec struct {
	TemplateID int               `json:"template_id"`
	OpClass    string            `json:"op_class"`
	ExtraVars  map[string]string `json:"extra_vars"` // key -> declared primitive type (string/number/bool)
}

// awxTemplateResolver builds the runner's op-class→AWX-template id resolver (temporal/runner Deps seam) from
// the SAME operator allowlist the awx-job actuator uses (TG_AWXJOB_ALLOWLIST), inverting its template_id→op-class
// mapping to op-class→template_id so the runner can stamp a LaunchSpec's template id at seal time. It is
// FAIL-CLOSED: an unparseable/empty allowlist, an op-class bound to MORE THAN ONE template (the runner cannot
// deterministically choose), or a non-positive id all yield ok=false for that op-class — so the runner cannot
// encode a launch and the awx op is refused. The awx-job effect leaf RE-validates the resolved template
// against its own allowlist + the op-class binding at Exec (authoritative), so this seam is a convenience,
// never the authority.
func awxTemplateResolver(spec string) func(opClass string) (int, bool) {
	rev := map[string]int{}
	ambiguous := map[string]bool{}
	if al, err := parseAWXJobAllowlist(spec); err == nil {
		for id, pol := range al {
			oc := strings.TrimSpace(pol.OpClass)
			if oc == "" || id <= 0 {
				continue
			}
			if _, seen := rev[oc]; seen {
				ambiguous[oc] = true // >1 template bound to this op-class ⇒ the runner cannot pick one ⇒ fail closed
				continue
			}
			rev[oc] = id
		}
	}
	return func(opClass string) (int, bool) {
		oc := strings.TrimSpace(opClass)
		if ambiguous[oc] {
			return 0, false
		}
		id, ok := rev[oc]
		return id, ok
	}
}

// parseAWXJobAllowlist parses TG_AWXJOB_ALLOWLIST (a JSON array of {template_id,op_class,extra_vars}) into the
// operator template allowlist. Empty ⇒ an empty allowlist (the actuator is then read-only — it can only
// refuse). A malformed entry / illegal var type fails closed.
func parseAWXJobAllowlist(spec string) (awxjob.TemplateAllowlist, error) {
	spec = strings.TrimSpace(spec)
	if spec == "" {
		return awxjob.TemplateAllowlist{}, nil
	}
	var raw []awxTemplateSpec
	if err := json.Unmarshal([]byte(spec), &raw); err != nil {
		return nil, fmt.Errorf("TG_AWXJOB_ALLOWLIST is not a JSON array of {template_id,op_class,extra_vars}: %w", err)
	}
	out := make(awxjob.TemplateAllowlist, len(raw))
	for i, t := range raw {
		if t.TemplateID <= 0 {
			return nil, fmt.Errorf("TG_AWXJOB_ALLOWLIST[%d]: template_id must be positive", i)
		}
		op := strings.TrimSpace(t.OpClass)
		if op == "" {
			return nil, fmt.Errorf("TG_AWXJOB_ALLOWLIST[%d] (template %d): op_class is required (the policy + graduation bucket)", i, t.TemplateID)
		}
		schema := awxjob.ExtraVarsSchema{}
		for k, typ := range t.ExtraVars {
			vt := awxjob.VarType(strings.TrimSpace(typ))
			if !vt.Valid() {
				return nil, fmt.Errorf("TG_AWXJOB_ALLOWLIST[%d] (template %d): extra_var %q declares illegal type %q (want string/number/bool)", i, t.TemplateID, k, typ)
			}
			schema[k] = vt
		}
		out[t.TemplateID] = awxjob.TemplatePolicy{OpClass: op, ExtraVarsSchema: schema}
	}
	return out, nil
}

// armAWXPlaybooksIngest arms the read-only playbooks-as-knowledge cron (spec/017 T-017-5 follow-on). Disabled
// unless TG_AWXPLAYBOOKS_* is fully configured. It ingests AWX runbooks (re-read by id) into a FileCorpus and,
// when a semantic index is configured, folds them into the vector index over the UNION of the live corpus +
// the runbooks so a partial sync never prunes a lesson. It launches NOTHING and never crashes the worker.
func armAWXPlaybooksIngest(dbPool *db.Pool, holder *knowledge.Holder) {
	base := strings.TrimSpace(getenv("TG_AWXPLAYBOOKS_BASE_URL", ""))
	tokenRef := strings.TrimSpace(getenv("TG_AWXPLAYBOOKS_SENSOR_TOKEN_REF", ""))
	corpusPath := strings.TrimSpace(getenv("TG_AWXPLAYBOOKS_CORPUS", ""))
	interval := envDuration("TG_AWXPLAYBOOKS_INTERVAL", 0) // OFF by default — opt-in
	if base == "" || tokenRef == "" || corpusPath == "" || interval <= 0 {
		log.Printf("awxplaybooks knowledge lane: disabled (needs TG_AWXPLAYBOOKS_BASE_URL + TG_AWXPLAYBOOKS_SENSOR_TOKEN_REF + TG_AWXPLAYBOOKS_CORPUS + TG_AWXPLAYBOOKS_INTERVAL>0) — read-only, launches nothing")
		return
	}
	client, err := awxplaybooks.NewClient(awxplaybooks.ClientConfig{
		BaseURL:    base,
		TokenRef:   config.SecretRef(tokenRef),
		CACertPath: getenv("TG_AWXPLAYBOOKS_CA", ""),
	})
	if err != nil {
		log.Printf("awxplaybooks knowledge lane: disabled — client build failed: %v (read-only; never fatal)", err)
		return
	}
	corpus := awxplaybooks.FileCorpus{Path: corpusPath}
	ingest, err := awxplaybooks.NewIngest(client, corpus)
	if err != nil {
		log.Printf("awxplaybooks knowledge lane: disabled — ingest build failed: %v", err)
		return
	}
	ingest.Logf = log.Printf
	var index knowledge.IndexSync
	if dbPool != nil && strings.TrimSpace(getenv("TG_EMBED_MODEL", "")) != "" {
		index = db.NewKnowledgeEmbeddingStore(dbPool)
	}
	runOnce := func() {
		rctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
		defer cancel()
		res, rerr := ingest.Run(rctx)
		if rerr != nil {
			log.Printf("awxplaybooks knowledge lane: ingest failed: %v (prior corpus intact; retried next tick)", rerr)
			return
		}
		// Make the ingested runbooks semantically retrievable WITHOUT pruning the lessons index: sync over the
		// UNION of the live retriever corpus + the freshly-ingested runbooks. SyncIndex prunes refs absent from
		// the corpus it is handed, so a subset would drop lessons — the union never does.
		if index != nil && (res.Added > 0 || res.Updated > 0) {
			runbooks, lerr := corpus.Load(rctx)
			if lerr != nil {
				log.Printf("awxplaybooks knowledge lane: reload corpus for index sync failed: %v", lerr)
				return
			}
			combined := runbooks
			if holder != nil {
				combined = knowledge.MergeCorpus(holder.Snapshot(), runbooks)
			}
			if up, pruned, serr := knowledge.SyncIndex(rctx, index, combined); serr != nil {
				log.Printf("awxplaybooks knowledge lane: index sync failed: %v (runbooks still lexically discoverable once corpus reloads)", serr)
			} else {
				log.Printf("awxplaybooks knowledge lane: index synced (%d upserted, %d pruned) — runbooks now RAG-retrievable", up, pruned)
			}
		}
	}
	runOnce() // ingest immediately at boot
	go func() {
		t := time.NewTicker(interval)
		defer t.Stop()
		for range t.C {
			runOnce()
		}
	}()
	log.Printf("awxplaybooks knowledge lane: ARMED — read-only AWX runbook ingest every %s into %s (re-read by id, launches nothing; discovery grants no authority)", interval, corpusPath)
}
