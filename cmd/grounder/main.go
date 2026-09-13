// Command grounder is the Territory Grounder control-plane entrypoint.
//
// It performs the boot preflight (P0-9): it refuses to start unless the trust boundaries are wired
// and it keeps global mutation DISABLED (read-only). Every fail path is fail-closed — an absent
// boundary or a bad config aborts startup rather than degrading open. [O] INV-01, INV-09, P0-9.
//
// Run `grounder --check` to execute the preflight and exit (no infra required) — this is the P0
// acceptance smoke test.
package main

import (
	"cmp"
	_ "time/tzdata" // embed the IANA zoneinfo DB so time.LoadLocation resolves on distroless (no OS tzdata)

	"context"
	"crypto/sha256"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"go.temporal.io/sdk/client"

	"github.com/territory-grounder/grounder/adapters/model"
	"github.com/territory-grounder/grounder/core/auth"
	"github.com/territory-grounder/grounder/core/config"
	"github.com/territory-grounder/grounder/core/cpconfig"
	"github.com/territory-grounder/grounder/core/credential/dyndb"
	"github.com/territory-grounder/grounder/core/db"
	"github.com/territory-grounder/grounder/core/egress"
	"github.com/territory-grounder/grounder/core/httpapi"
	"github.com/territory-grounder/grounder/core/metrics"
	credpreflight "github.com/territory-grounder/grounder/core/preflight"
	"github.com/territory-grounder/grounder/core/safety"
	"github.com/territory-grounder/grounder/core/seal"
	"github.com/territory-grounder/grounder/core/suppressionshadow"
	contracts "github.com/territory-grounder/grounder/docs/contracts"
	"github.com/territory-grounder/grounder/modules/bootstrap"
	"github.com/territory-grounder/grounder/modules/catalog"
	"github.com/territory-grounder/grounder/modules/credsource/openbao"
	"github.com/territory-grounder/grounder/modules/ingest/authlog"
	"github.com/territory-grounder/grounder/modules/ingest/crowdsec"
	alertmanager "github.com/territory-grounder/grounder/modules/ingest/prometheus-alertmanager"
)

type envConfig struct {
	RuntimeDSN    string // TG_RUNTIME_DSN — DML-only role (never committed; supplied by env/secret store)
	MigrationDSN  string // TG_MIGRATION_DSN — DDL-capable role, used only at startup
	LiteLLMURL    string // TG_LITELLM_URL — e.g. http://litellm:4000
	LiteLLMKeyRef config.SecretRef
	PublicAddr    string // TG_PUBLIC_ADDR
	AdminAddr     string // TG_ADMIN_ADDR
	// GateMarginEpsilon is the DEFAULT review band the gate-decision boundary-case queue uses when a caller
	// names no eps= (TG-178: ε is loadable configuration, not a compiled constant). 0/unset ⇒ the compiled
	// 0.05 default; the handler ignores an out-of-range value. Resolved through the SAME get() chain as every
	// other knob (console override → env → default), not os.Getenv.
	GateMarginEpsilon float64 // TG_GATE_MARGIN_EPSILON
	// Browser operator session (spec/006 REQ-508). All secrets arrive as references (env:/file:), never
	// literals. If either reference does not resolve, the browser path is simply not registered.
	SessionKeyRef    config.SecretRef // TG_SESSION_KEY_REF    — signing key for the session cookie (≥32 bytes)
	OperatorName     string           // TG_OPERATOR_NAME      — the single Phase-1 operator account
	OperatorTokenRef config.SecretRef // TG_OPERATOR_TOKEN_REF — the operator's login token
	SessionTTL       time.Duration    // TG_SESSION_TTL        — session lifetime (default 12h)
	// Admin operator tier (task #27 Phase B, REQ-522): the SEPARATE step-up credential. If the token
	// reference does not resolve, the admin lane (elevation + config/secret writes) is not registered.
	AdminName     string           // TG_ADMIN_NAME      — the admin account name
	AdminTokenRef config.SecretRef // TG_ADMIN_TOKEN_REF — the admin step-up token (ref, never literal)
	AdminTTL      time.Duration    // TG_ADMIN_TTL       — elevation lifetime (default 15m, short by design)
	// LDAP / FreeIPA console login (spec/006 REQ-508 extension). When Enabled AND at least one URL is set,
	// a NON-break-glass login is authenticated by binding as the user against FreeIPA (the token is the
	// user's password, never stored) and their group membership maps to a role. Transport knobs mirror the
	// worker's credsource/ldap connector. No secret here — the password arrives per login request.
	LDAPAuthEnabled   bool             // TG_LDAP_AUTH_ENABLED         — gate (true + URLs present ⇒ LDAP login on)
	LDAPURLs          string           // TG_LDAP_URLS                 — comma/space-separated replica list (ldaps://…)
	LDAPCACertRef     config.SecretRef // TG_LDAP_CA                   — PEM CA SecretRef (file:/store:); empty = system roots
	LDAPStartTLS      bool             // TG_LDAP_STARTTLS             — upgrade a plain ldap:// with StartTLS
	LDAPUserDNTmpl    string           // TG_LDAP_AUTH_USER_DN_TEMPLATE — bind DN template (one %s), default FreeIPA layout
	LDAPAdminGroup    string           // TG_LDAP_AUTH_ADMIN_GROUP     — admin-eligible group CN (default tg-admins)
	LDAPOperatorGroup string           // TG_LDAP_AUTH_OPERATOR_GROUP  — read-only operator group CN (default tg-operators)
	// Sealed secrets (task #27 Phase D, REQ-524): the envelope master key, resolved per use and
	// discarded. Unresolvable = sealing writes and store: resolution are unavailable (fail closed).
	SealKeyRef config.SecretRef // TG_SEAL_KEY_REF — master key material (≥32 bytes)
	// KnowledgeFile is the worker-MAINTAINED distilled-lessons corpus (TG_KNOWLEDGE_FILE) — the
	// grounder needs READ access to serve the wiki lessons section (REQ-521). It is unioned with the
	// read-only bootstrap SEED (KnowledgeSeedFile) so the wiki sees the full precedent set. Empty = the
	// wiki serves an honestly empty lessons section (runbooks remain, embedded in the binary).
	KnowledgeFile string
	// KnowledgeSeedFile is the read-only, tracked bootstrap precedent corpus (TG_KNOWLEDGE_SEED_FILE)
	// unioned under KnowledgeFile on every read (deploy-synced; never written by anyone at runtime).
	KnowledgeSeedFile string
	// WikiArticlesFile is the COMPILED per-host article envelope the worker's wikicompile lane writes
	// (TG_WIKI_ARTICLES_FILE). Read-only here. Empty, or not yet written, is an honestly empty articles
	// section — the same degradation the corpus gets, because "the compiler has not run yet" and "the
	// compiler is broken" must not render identically.
	WikiArticlesFile string
	// LibrenmsDeployments is the configured LibreNMS ingest list (TG_LIBRENMS_DEPLOYMENTS) — held on
	// the struct so a console override (task #27 Phase C) can be adopted at boot alongside env.
	LibrenmsDeployments string
	// LibrenmsIngestTokenRef is the per-source static bearer REFERENCE (env:/file:/store:, INV-13) the
	// LibreNMS transports present when they POST /v1/ingest/librenms (AuthIngestPush). When deployments are
	// declared AND this ref resolves non-empty, boot provisions the `librenms` sources row from it so the
	// front door bearer-authenticates LibreNMS pushes reproducibly (config-not-code). Never a literal token.
	LibrenmsIngestTokenRef config.SecretRef // TG_LIBRENMS_INGEST_TOKEN_REF
	// AMIngestTokenRef is the Alertmanager push-ingest bearer REFERENCE (TG-278, added by TG-284). The
	// credential existed on the live box as a 64-char literal in TG_AM_INGEST_TOKEN with NO reference
	// variable anywhere in the tree, so no configuration could move it to a backend and the boot gate could
	// not see it. It defaults to `env:TG_AM_INGEST_TOKEN`, which is exactly what the hand-written
	// `prometheus-alertmanager` sources row already holds, so an existing deployment is unchanged — and
	// setting it to bao: now actually repoints that row at boot. Never a literal token.
	AMIngestTokenRef config.SecretRef // TG_AM_INGEST_TOKEN_REF
	// CrowdsecIngestTokenRef is the CrowdSec push-ingest bearer REFERENCE (TG-291). The module has been
	// declared in the registry since the beginning and advertised at every boot among the estate's
	// capabilities, and it has delivered 0 of 2,999 ingest_alert rows in the table's whole history —
	// because there is no `crowdsec` row in `sources`, so AuthIngestPush fails closed with 401 on every
	// push. The other two push sources each got a boot provisioner; this one never did, so the only
	// security-telemetry ingest TG has could not be provisioned at all without hand-run SQL.
	//
	// NO DEFAULT, deliberately, unlike the two above. Both of those default to an `env:` ref that matches a
	// row already on the live box, so their defaults preserve existing behaviour. There is no crowdsec row
	// to preserve, and defaulting to a ref that resolves empty would provision nothing while making the
	// boot log claim a knob exists. Unset means unprovisioned, and the boot line says so.
	CrowdsecIngestTokenRef config.SecretRef // TG_CROWDSEC_INGEST_TOKEN_REF
	// AuthlogIngestTokenRef is the host auth/audit-log push bearer REFERENCE (TG-315, gap found by TG-291).
	// Same shape and same reason as the crowdsec ref above: declared, advertised, never provisioned.
	AuthlogIngestTokenRef config.SecretRef // TG_AUTHLOG_INGEST_TOKEN_REF
	// SecretPolicy is the boot-time secret-scheme policy (spec/024 REQ-2400): off (default, behaviour-
	// preserving) / warn / enforce. Under enforce the preflight refuses to start on any non-exempt business
	// secret that resolves through a plaintext-bearing scheme (env:/file:/literal) instead of a backend.
	SecretPolicy string // TG_SECRET_POLICY
	// EgressAllow / EgressMode are the outbound meter's two knobs (TG-160, wired here by TG-324).
	//
	// THEY ARE READ HERE, THROUGH `get`, ON PURPOSE. deploy/envparity_test.go discovers a binary's env keys
	// by reading the literal first argument of a get/getenv-family call in a REGISTERED root file, and
	// cmd/grounder/main.go is registered. Reading them with os.Getenv, or from a second file, would leave
	// compose free to never forward them — the operator sets TG_EGRESS_MODE in .env, the container never
	// sees it, and the process reports "meter mode" while looking configured. That gap has shipped three
	// times in this repo, which is why the guard exists and why these two lines live in this struct.
	EgressAllow string // TG_EGRESS_ALLOW
	EgressMode  string // TG_EGRESS_MODE
}

func loadEnv() envConfig {
	// console override → env → compiled default, the SAME precedence the worker resolves with (TG-263).
	// Keys feeding this process's own authentication, and the bootstrap keys, never reach the console
	// layer — installGrounderConfig refuses them before they are stored in the snapshot.
	get := func(k, def string) string {
		if v, ok := grounderOverride(k); ok {
			return v
		}
		if v, ok := os.LookupEnv(k); ok {
			return v
		}
		return def
	}
	ttl, err := time.ParseDuration(get("TG_SESSION_TTL", "12h"))
	if err != nil || ttl <= 0 {
		ttl = 12 * time.Hour // fail toward the bounded default, never toward "no expiry"
	}
	adminTTL, err := time.ParseDuration(get("TG_ADMIN_TTL", "15m"))
	if err != nil || adminTTL <= 0 {
		adminTTL = 15 * time.Minute // fail toward the short default, never toward "no expiry"
	}
	gateEps, err := strconv.ParseFloat(get("TG_GATE_MARGIN_EPSILON", "0"), 64)
	if err != nil || gateEps < 0 {
		gateEps = 0 // unset/malformed ⇒ the boundary-case queue keeps its compiled 0.05 default
	}
	return envConfig{
		RuntimeDSN:             os.Getenv("TG_RUNTIME_DSN"),
		MigrationDSN:           os.Getenv("TG_MIGRATION_DSN"),
		LiteLLMURL:             get("TG_LITELLM_URL", "http://litellm:4000"),
		LiteLLMKeyRef:          config.SecretRef(get("TG_LITELLM_KEY_REF", "env:LITELLM_MASTER_KEY")),
		PublicAddr:             get("TG_PUBLIC_ADDR", ":8080"),
		AdminAddr:              get("TG_ADMIN_ADDR", ":8443"),
		GateMarginEpsilon:      gateEps,
		SessionKeyRef:          config.SecretRef(get("TG_SESSION_KEY_REF", "env:TG_SESSION_KEY")),
		OperatorName:           get("TG_OPERATOR_NAME", "operator"),
		OperatorTokenRef:       config.SecretRef(get("TG_OPERATOR_TOKEN_REF", "env:TG_OPERATOR_TOKEN")),
		SessionTTL:             ttl,
		AdminName:              get("TG_ADMIN_NAME", "admin"),
		AdminTokenRef:          config.SecretRef(get("TG_ADMIN_TOKEN_REF", "env:TG_ADMIN_TOKEN")),
		AdminTTL:               adminTTL,
		LDAPAuthEnabled:        strings.EqualFold(strings.TrimSpace(get("TG_LDAP_AUTH_ENABLED", "")), "true"),
		LDAPURLs:               get("TG_LDAP_URLS", ""),
		LDAPCACertRef:          config.SecretRef(get("TG_LDAP_CA", "")),
		LDAPStartTLS:           strings.EqualFold(strings.TrimSpace(get("TG_LDAP_STARTTLS", "")), "true"),
		LDAPUserDNTmpl:         get("TG_LDAP_AUTH_USER_DN_TEMPLATE", ""), // site DN template (deploy-time); empty ⇒ the generic connector default (STONITH — no estate suffix compiled in)
		LDAPAdminGroup:         get("TG_LDAP_AUTH_ADMIN_GROUP", "tg-admins"),
		LDAPOperatorGroup:      get("TG_LDAP_AUTH_OPERATOR_GROUP", "tg-operators"),
		SealKeyRef:             config.SecretRef(get("TG_SEAL_KEY_REF", "env:TG_SEAL_KEY")),
		KnowledgeFile:          get("TG_KNOWLEDGE_FILE", ""),
		KnowledgeSeedFile:      get("TG_KNOWLEDGE_SEED_FILE", ""),
		WikiArticlesFile:       get("TG_WIKI_ARTICLES_FILE", ""),
		LibrenmsDeployments:    get("TG_LIBRENMS_DEPLOYMENTS", ""),
		LibrenmsIngestTokenRef: config.SecretRef(get("TG_LIBRENMS_INGEST_TOKEN_REF", "env:TG_LIBRENMS_INGEST_TOKEN")),
		// The default comes from the gate's own table so the consumer and the policer cannot disagree.
		AMIngestTokenRef:       config.SecretRef(get("TG_AM_INGEST_TOKEN_REF", credpreflight.DefaultRefFor("TG_AM_INGEST_TOKEN_REF"))),
		CrowdsecIngestTokenRef: config.SecretRef(get("TG_CROWDSEC_INGEST_TOKEN_REF", "")),
		AuthlogIngestTokenRef:  config.SecretRef(get("TG_AUTHLOG_INGEST_TOKEN_REF", "")),
		SecretPolicy:           get("TG_SECRET_POLICY", "off"),
		EgressAllow:            get("TG_EGRESS_ALLOW", ""),
		// METER, NOT ENFORCE, AND THAT IS THE STAGED ORDER RATHER THAN TIMIDITY. The worker took exactly
		// this path (TG-160 metered, TG-324 flipped once off-allowlist held flat at 0 against a non-zero
		// allowlist). Enforcing on first install would gate this process's OpenBao calls on an allowlist
		// nobody has yet observed against real traffic — and the grounder cannot resolve its own read
		// credential without OpenBao, so a wrong allowlist is not a degraded grounder, it is no grounder.
		// The flip is a deploy-config edit once the live series justify it.
		EgressMode: get("TG_EGRESS_MODE", grounderEgressModeDefault),
	}
}

// secretEntries enumerates the grounder's COMPLETE process secret-reference set for the boot secret-policy
// gate (spec/024 REQ-2402 — a reference the gate cannot see is a plaintext hole). It covers the envConfig
// SecretRef fields AND the three bootstrap/CA references read inline at composition (WireDelivery /
// buildSealer). Exempt marks references that are not business secrets subject to the backend requirement:
//   - public material — the LDAP + OpenBao CA certificates (certs, not secrets);
//   - the SEALING/SUBSTRATE bootstrap — the seal master key, the OpenBao Transit seal token, and the
//     substrate's OWN delivery token: each cannot come from the backend it unseals/authenticates (the
//     chicken-and-egg of REQ-2401), so they are the permanent, code-defined exemption set and are allowed
//     to remain env:/file:. The database DSNs are policed separately in preflight (required-config bootstrap,
//     not SecretRef-typed).
//
// The three bootstrap refs are CONSUMED before this gate runs (WireDelivery/buildSealer precede preflight),
// which is safe precisely because they are exempt — an exempt ref being read early is allowed; every
// NON-exempt business secret is consumed only AFTER preflight (auth/server wiring), so the gate governs it
// before use.
func (cfg envConfig) secretEntries() []credpreflight.SecretEntry {
	entries := []credpreflight.SecretEntry{
		{Name: "TG_LITELLM_KEY_REF", Ref: cfg.LiteLLMKeyRef},
		{Name: "TG_SESSION_KEY_REF", Ref: cfg.SessionKeyRef},
		{Name: "TG_OPERATOR_TOKEN_REF", Ref: cfg.OperatorTokenRef},
		{Name: "TG_ADMIN_TOKEN_REF", Ref: cfg.AdminTokenRef},
		{Name: "TG_LIBRENMS_INGEST_TOKEN_REF", Ref: cfg.LibrenmsIngestTokenRef},
		{Name: "TG_LDAP_CA", Ref: cfg.LDAPCACertRef, Exempt: true},
		{Name: "TG_SEAL_KEY_REF", Ref: cfg.SealKeyRef, Exempt: true},
		// Bootstrap/CA refs read inline at composition — enumerated for completeness (REQ-2402), all exempt.
		{Name: "TG_OPENBAO_TOKEN_REF", Ref: config.SecretRef(os.Getenv("TG_OPENBAO_TOKEN_REF")), Exempt: true},
		{Name: "TG_SEAL_TRANSIT_TOKEN_REF", Ref: config.SecretRef(os.Getenv("TG_SEAL_TRANSIT_TOKEN_REF")), Exempt: true},
		{Name: "TG_OPENBAO_CA", Ref: config.SecretRef(os.Getenv("TG_OPENBAO_CA")), Exempt: true},
		// The console writer's AppRole. Exempt for the same reason as the read credential above — it
		// authenticates TO OpenBao — but DECLARED so the boot report shows it rather than leaving a
		// credential the gate never looked at.
		{Name: "TG_OPENBAO_WRITER_ROLE_ID_REF", Ref: config.SecretRef(os.Getenv("TG_OPENBAO_WRITER_ROLE_ID_REF")), Exempt: true},
		{Name: "TG_OPENBAO_WRITER_SECRET_ID_REF", Ref: config.SecretRef(os.Getenv("TG_OPENBAO_WRITER_SECRET_ID_REF")), Exempt: true},
	}
	// The deployment-wide credentials no binary ever declared (TG-278, closed by TG-284): the Alertmanager
	// and claude-proxy bearers plus the per-site LibreNMS tokens carried inside TG_LIBRENMS_DEPLOYMENTS.
	// All business secrets.
	//
	// Read through the SAME resolution loadEnv used (console override → env → default), NOT os.Getenv.
	// TG_LIBRENMS_DEPLOYMENTS is a console-WRITABLE descriptor field (modules/ingest/librenms/descriptor.go)
	// and boot_config.go refuses only the AUTH and BOOTSTRAP key sets, so an operator really can change it
	// from the console — and cfg.LibrenmsDeployments, not the raw env, is what the front door then runs on.
	// A gate reading os.Getenv here would police a value nothing is using while the value actually in force
	// went unexamined: this ticket's own defect, one level down. The worker's half already reads through its
	// override-aware getenv, so both binaries judge what they actually run.
	deploymentEnv := func(k string) string {
		switch k {
		case "TG_LIBRENMS_DEPLOYMENTS":
			return cfg.LibrenmsDeployments
		case "TG_AM_INGEST_TOKEN_REF":
			return string(cfg.AMIngestTokenRef)
		}
		return os.Getenv(k)
	}
	return append(entries, credpreflight.DeploymentSecretEntries(deploymentEnv)...)
}

// buildSealer builds the sealed-secret Sealer from config (spec/022 REQ-2201, TG-157). When
// TG_SEAL_TRANSIT_KEY is set the master-key operation (the DEK wrap/unwrap) runs INSIDE OpenBao Transit and the
// worker holds NO master key at all; otherwise it falls back to the in-process master key (TG_SEAL_KEY_REF).
// The Transit substrate reuses the credential-delivery OpenBao endpoint by default (address/token/CA), but a
// dedicated TG_SEAL_TRANSIT_TOKEN_REF can point the seal plane at a disjoint role (the plane split, T-022-4).
// Returns nil when neither is usable — sealing then fails closed (no seal, no store: resolution, no write).
// buildSealer delegates to seal.FromEnv so the grounder and the WORKER construct the sealer identically
// (TG-275). It stays as a named function because main() reads better with it.
func buildSealer(cfg envConfig) (*seal.Sealer, string) { return seal.FromEnv(cfg.SealKeyRef) }

// librenmsPushAuthPlan is the PURE decision for whether/what to provision for LibreNMS push-ingest at
// boot. It carries only the token REFERENCE (INV-13), never a resolved literal.
type librenmsPushAuthPlan struct {
	Provision bool   // upsert the sources row?
	TokenRef  string // the ref to store (env:/file:/store:) when provisioning — never the literal token
	Reason    string // human-readable log line for either branch
}

// planLibrenmsPushAuth decides the provisioning action from already-observed inputs — a pure function so
// the provision/skip logic is oracle-testable without a DB or env. Provision ONLY when a deployment is
// declared AND the token ref resolves to a non-empty value; otherwise skip with a reason, so a
// credential-less source is never created (it would just fail closed on every push). tokenRef is the
// configured reference string (safe to log); resolved is what that ref currently resolves to (empty when
// unset/unresolvable/blank) and is NEVER stored — only tokenRef is.
func planLibrenmsPushAuth(deploymentsDeclared bool, tokenRef, resolved string) librenmsPushAuthPlan {
	switch {
	case !deploymentsDeclared:
		return librenmsPushAuthPlan{Reason: "no TG_LIBRENMS_DEPLOYMENTS declared — LibreNMS push auth not provisioned"}
	case strings.TrimSpace(tokenRef) == "":
		return librenmsPushAuthPlan{Reason: "TG_LIBRENMS_INGEST_TOKEN_REF unset — LibreNMS push auth not provisioned"}
	case resolved == "":
		return librenmsPushAuthPlan{Reason: fmt.Sprintf("token ref %q resolves empty — LibreNMS push auth not provisioned (source would fail closed)", tokenRef)}
	default:
		return librenmsPushAuthPlan{Provision: true, TokenRef: tokenRef, Reason: fmt.Sprintf("LibreNMS push auth provisioned — sources row 'librenms' bearer via %s", tokenRef)}
	}
}

// librenmsSourceUpserter is the narrow write the boot provisioner needs (satisfied by *db.SourceResolver).
type librenmsSourceUpserter interface {
	UpsertSource(ctx context.Context, sourceID, ingestTokenRef string) error
}

// planAlertmanagerPushAuth is the same pure decision for the ALERTMANAGER push source (TG-278, added by
// TG-284). It is separate from planLibrenmsPushAuth rather than a shared generic because the two differ in
// their precondition: LibreNMS is a CONFIG-DRIVEN source and provisions only where deployments are declared,
// whereas prometheus-alertmanager is config-free and always registered by the module registry — its only
// question is whether a bearer reference exists to provision with.
//
// Why this function exists at all: TG_AM_INGEST_TOKEN was a plaintext literal with no reference variable in
// the tree, and the sources row that consumes it was created by hand-run SQL. Adding a TG_AM_INGEST_TOKEN_REF
// that nothing read would have been the worse defect — an operator setting it to bao: would get a green boot
// gate and an unchanged credential. This is the read.
func planAlertmanagerPushAuth(tokenRef, resolved string) librenmsPushAuthPlan {
	switch {
	case strings.TrimSpace(tokenRef) == "":
		return librenmsPushAuthPlan{Reason: "TG_AM_INGEST_TOKEN_REF unset — Alertmanager push auth not provisioned"}
	case resolved == "":
		return librenmsPushAuthPlan{Reason: fmt.Sprintf("token ref %q resolves empty — Alertmanager push auth not provisioned (source would fail closed)", tokenRef)}
	default:
		return librenmsPushAuthPlan{Provision: true, TokenRef: tokenRef, Reason: fmt.Sprintf("Alertmanager push auth provisioned — sources row %q bearer via %s", alertmanager.SourceType, tokenRef)}
	}
}

// provisionAlertmanagerPushAuth applies that plan at boot, idempotently upserting the
// `prometheus-alertmanager` source keyed by its REF (INV-13 — the literal is resolved only to test presence,
// never stored). The source id is taken from the ingest module itself, not retyped here: a provisioner that
// spells the id differently from the front door writes a row nothing authenticates against. A DB error logs
// and continues, exactly like the LibreNMS provisioner — optional provisioning must never crash the
// read-only foundation.
func provisionAlertmanagerPushAuth(ctx context.Context, up librenmsSourceUpserter, cfg envConfig) {
	resolved, _ := cfg.AMIngestTokenRef.Resolve()
	plan := planAlertmanagerPushAuth(string(cfg.AMIngestTokenRef), resolved)
	if !plan.Provision {
		log.Printf("ingest: %s", plan.Reason)
		return
	}
	if err := up.UpsertSource(ctx, alertmanager.SourceType, plan.TokenRef); err != nil {
		log.Printf("ingest: Alertmanager push auth provisioning failed (%v) — continuing; push will 401 until the sources row exists", err)
		return
	}
	log.Printf("ingest: %s", plan.Reason)
}

// planPushSource decides whether to provision a bearer-authenticated push-ingest source (TG-291).
//
// Shared by the two sources that never had a provisioner at all — `crowdsec` and `authlog`. It deliberately
// does NOT replace planLibrenmsPushAuth / planAlertmanagerPushAuth: those sit on the path carrying every
// alert TG triages, they each carry deployment-specific preconditions and defaults tuned to rows already on
// the live box, and folding them in to add sources that have never delivered a row would trade a real risk
// for a cosmetic one.
//
// Both callers pass the source id from the ingest module itself rather than retyping it. AuthIngestPush
// looks the row up by the URL {source_type}, so a provisioner that spells the id differently writes a row
// nothing authenticates against — a green boot log and a 401 on every push, which is the state TG-291
// describes.
//
// Unset ⇒ NOT provisioned, and the boot log says so. A row provisioned against a ref that resolves empty
// authenticates nothing and is worse than no row: an operator seeing it reads the 401s as a source fault
// rather than a missing credential.
func planPushSource(sourceType, envKey, tokenRef, resolved string) librenmsPushAuthPlan {
	switch {
	case strings.TrimSpace(tokenRef) == "":
		return librenmsPushAuthPlan{Reason: fmt.Sprintf("%s unset — %s push auth NOT provisioned; POST /v1/ingest/%s "+
			"will 401 and the declared capability stays empty (TG-291)", envKey, sourceType, sourceType)}
	case resolved == "":
		return librenmsPushAuthPlan{Reason: fmt.Sprintf("token ref %q resolves empty — %s push auth not provisioned "+
			"(the source would fail closed on every push)", tokenRef, sourceType)}
	default:
		return librenmsPushAuthPlan{Provision: true, TokenRef: tokenRef, Reason: fmt.Sprintf(
			"%s push auth provisioned — sources row %q bearer via %s", sourceType, sourceType, tokenRef)}
	}
}

// provisionPushSource applies that plan at boot, idempotently upserting the source keyed by its REF
// (INV-13 — the literal is resolved only to test presence, never stored). A DB error logs and continues,
// like both provisioners below: optional provisioning must never crash the read-only foundation.
func provisionPushSource(ctx context.Context, up librenmsSourceUpserter, sourceType, envKey string, ref config.SecretRef) {
	resolved, _ := ref.Resolve()
	plan := planPushSource(sourceType, envKey, string(ref), resolved)
	if !plan.Provision {
		log.Printf("ingest: %s", plan.Reason)
		return
	}
	if err := up.UpsertSource(ctx, sourceType, plan.TokenRef); err != nil {
		log.Printf("ingest: %s push auth provisioning failed (%v) — continuing; push will 401 until the sources row exists",
			sourceType, err)
		return
	}
	log.Printf("ingest: %s", plan.Reason)
}

// provisionLibrenmsPushAuth applies planLibrenmsPushAuth at boot: it resolves the configured token ref
// (only to decide non-emptiness — the literal is discarded, never stored) and, when the plan says so,
// idempotently upserts the `librenms` source keyed by its REF. A DB error logs and continues so this
// optional provisioning never crashes the read-only foundation (fail open on optional DB).
func provisionLibrenmsPushAuth(ctx context.Context, up librenmsSourceUpserter, cfg envConfig) {
	// Resolve to test presence only; ignore the error and the value beyond emptiness (INV-13).
	resolved, _ := cfg.LibrenmsIngestTokenRef.Resolve()
	plan := planLibrenmsPushAuth(strings.TrimSpace(cfg.LibrenmsDeployments) != "", string(cfg.LibrenmsIngestTokenRef), resolved)
	if !plan.Provision {
		log.Printf("ingest: %s", plan.Reason)
		return
	}
	if err := up.UpsertSource(ctx, "librenms", plan.TokenRef); err != nil {
		log.Printf("ingest: LibreNMS push auth provisioning failed (%v) — continuing; push will 401 until the sources row exists", err)
		return
	}
	log.Printf("ingest: %s", plan.Reason)
}

// applyConfigOverrides adopts the committed console overrides (control_plane_config, task #27 Phase C)
// into the boot config, so the running components and the /v1/config report agree (INV-15: the map
// matches the territory). Legality is re-checked per key against the compiled registry — a stray row
// for a LAW or boot-only key is ignored exactly as the resolver ignores it. A malformed value logs
// and keeps the env/default (fail toward the bounded known-good, never toward a broken boot).
func applyConfigOverrides(ctx context.Context, store *db.CPConfigStore, cfg *envConfig) {
	overrides, err := store.Overrides(ctx)
	if err != nil {
		log.Printf("config overrides: read failed (%v) — booting on env/defaults", err)
		return
	}
	for key, value := range overrides {
		if _, err := cpconfig.ValidateWrite(key, value); err != nil {
			log.Printf("config overrides: ignoring illegal row %q (%v)", key, err)
			continue
		}
		switch key {
		case "gateway.litellm_url":
			cfg.LiteLLMURL = value
		case "session.ttl":
			if d, derr := time.ParseDuration(value); derr == nil && d > 0 {
				cfg.SessionTTL = d
			} else {
				log.Printf("config overrides: session.ttl %q unparsable — keeping %s", value, cfg.SessionTTL)
				continue
			}
		case "session.admin_ttl":
			if d, derr := time.ParseDuration(value); derr == nil && d > 0 {
				cfg.AdminTTL = d
			} else {
				log.Printf("config overrides: session.admin_ttl %q unparsable — keeping %s", value, cfg.AdminTTL)
				continue
			}
		case "ingest.librenms_deployments":
			cfg.LibrenmsDeployments = value
		default:
			// A writable key with no boot consumer yet: the resolver still reports it source=console.
		}
		log.Printf("config overrides: adopted %s (source=console)", key)
	}
}

// newConfigResolver builds the control-plane config resolver (task #27 Phases A+C). LAW values come
// from the authoritative components (the mutation gate + the compiled floor); env values from the
// loaded config; console overrides from the durable control_plane_config store (the worker's
// single-writer table) — honored by the resolver ONLY for console-writable non-LAW keys, and adopted
// at boot by applyConfigOverrides so the report and the runtime agree. No secret VALUE is ever
// placed here.
func newConfigResolver(gate *safety.Chokepoint, cfg envConfig, console cpconfig.ConsoleStore) cpconfig.Resolver {
	mayActuate := "false"
	if gate.MayActuate() {
		mayActuate = "true"
	}
	return cpconfig.Resolver{
		Law: map[string]string{
			"safety.never_auto_floor":    "enforced",
			"safety.may_actuate":         mayActuate,
			"safety.predict_then_verify": "required",
		},
		Env: map[string]string{
			"gateway.litellm_url":         cfg.LiteLLMURL,
			"session.ttl":                 cfg.SessionTTL.String(),
			"session.admin_ttl":           cfg.AdminTTL.String(),
			"operator.name":               cfg.OperatorName,
			"operator.admin_name":         cfg.AdminName,
			"net.public_addr":             cfg.PublicAddr,
			"net.admin_addr":              cfg.AdminAddr,
			"ingest.librenms_deployments": cfg.LibrenmsDeployments,
			"knowledge.corpus_file":       cfg.KnowledgeFile,
		},
		Console: console,
	}
}

// buildAdminSessions constructs the admin step-up authenticator (task #27 Phase B, REQ-522), or
// returns nil when the admin token reference does not resolve — the admin lane (elevation + every
// config/secret write route) then does not exist at all (fail closed). Requires the browser session
// path: an elevation is a property OF a session.
func buildAdminSessions(cfg envConfig) *auth.AdminAuthenticator {
	token, err := cfg.AdminTokenRef.Resolve()
	if err != nil {
		log.Printf("admin tier disabled: admin token %s not resolvable (%v) — no admin routes registered", cfg.AdminTokenRef, err)
		return nil
	}
	admins := auth.MemOperators{
		cfg.AdminName: {Name: cfg.AdminName, TokenSHA256: sha256.Sum256([]byte(token))},
	}
	aa, err := auth.NewAdminAuthenticator(admins, cfg.AdminTTL)
	if err != nil {
		log.Printf("admin tier disabled: %v", err)
		return nil
	}
	log.Printf("admin tier enabled for %q (step-up elevation ttl %s)", cfg.AdminName, cfg.AdminTTL)
	return aa
}

// buildBrowserSessions constructs the browser-session authenticator from config, or returns nil when
// the references do not resolve — the browser path then does not exist (fail closed), and machine
// auth is entirely unaffected either way (REQ-508).
func buildBrowserSessions(cfg envConfig, store auth.SessionStore) *auth.SessionAuthenticator {
	key, err := cfg.SessionKeyRef.Resolve()
	if err != nil {
		log.Printf("browser sessions disabled: session key %s not resolvable (%v)", cfg.SessionKeyRef, err)
		return nil
	}
	token, err := cfg.OperatorTokenRef.Resolve()
	if err != nil {
		log.Printf("browser sessions disabled: operator token %s not resolvable (%v)", cfg.OperatorTokenRef, err)
		return nil
	}
	ops := auth.MemOperators{
		cfg.OperatorName: {Name: cfg.OperatorName, TokenSHA256: sha256.Sum256([]byte(token))},
	}
	sa, err := auth.NewSessionAuthenticator([]byte(key), store, ops, cfg.SessionTTL)
	if err != nil {
		log.Printf("browser sessions disabled: %v", err)
		return nil
	}
	log.Printf("browser sessions enabled for operator %q (ttl %s, read-only)", cfg.OperatorName, cfg.SessionTTL)
	return sa
}

// buildLDAPAuth constructs the LDAP/FreeIPA console-login authenticator (spec/006 REQ-508 extension), or
// returns nil when LDAP login is off or misconfigured — the console then authenticates ONLY via the
// static break-glass operator+token (fail closed, never a weaker check). It holds no secret: a user's
// password arrives per login request and is discarded. The CA arrives by reference (INV-13).
func buildLDAPAuth(cfg envConfig) *auth.LDAPAuthenticator {
	if !cfg.LDAPAuthEnabled {
		return nil
	}
	urls := splitEnvList(cfg.LDAPURLs)
	if len(urls) == 0 {
		log.Printf("LDAP console login disabled: TG_LDAP_AUTH_ENABLED=true but TG_LDAP_URLS is empty (fail closed to break-glass)")
		return nil
	}
	l, err := auth.NewLDAPAuthenticator(auth.LDAPConfig{
		URLs:           urls,
		StartTLS:       cfg.LDAPStartTLS,
		CACertRef:      cfg.LDAPCACertRef,
		UserDNTemplate: cfg.LDAPUserDNTmpl,
		AdminGroup:     cfg.LDAPAdminGroup,
		OperatorGroup:  cfg.LDAPOperatorGroup,
	})
	if err != nil {
		log.Printf("LDAP console login disabled: %v (fail closed to break-glass)", err)
		return nil
	}
	log.Printf("LDAP console login enabled (%d replica(s); admin group %q → step-up eligible, operator group %q → read-only; break-glass operator %q keeps the static token path)",
		len(urls), cfg.LDAPAdminGroup, cfg.LDAPOperatorGroup, cfg.OperatorName)
	return l
}

// splitEnvList splits a comma/whitespace-separated env value into its non-empty tokens (the same shape
// the worker's credential bootstrap uses for TG_LDAP_URLS).
func splitEnvList(v string) []string {
	var out []string
	for _, t := range strings.FieldsFunc(v, func(r rune) bool { return r == ',' || r == ' ' || r == '\t' || r == '\n' }) {
		if t = strings.TrimSpace(t); t != "" {
			out = append(out, t)
		}
	}
	return out
}

// preflight is the fail-closed boot gate. It never enables mutation; it only proves the base is safe
// enough to start read-only. Enabling mutation is a Phase-2 gate (actuate.EnableMutation, which requires
// the interception chain to prove itself wired before the switch can flip).
func preflight(cfg envConfig, gate *safety.Chokepoint) error {
	// 1) Mutation MUST be off at boot.
	if gate.MayActuate() {
		return errors.New("the actuation gate reports OPEN at boot — refusing to start (the grounder plane is read-only by design)")
	}
	// 2) Fail-closed enum zero-values must hold (defence against a refactor flipping them).
	if safety.Band(0) != safety.BandPollPause {
		return errors.New("safety invariant violated: zero Band is not POLL_PAUSE")
	}
	if safety.FailLane(0) != safety.LaneRemediation {
		return errors.New("safety invariant violated: zero FailLane is not the fail-closed remediation lane")
	}
	// 3) Required config present (DSNs are secrets supplied out-of-band, never in an artifact).
	var missing []string
	for k, v := range map[string]string{"TG_RUNTIME_DSN": cfg.RuntimeDSN, "TG_MIGRATION_DSN": cfg.MigrationDSN} {
		if v == "" {
			missing = append(missing, k)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("missing required config: %v", missing)
	}
	// 4) Secret-scheme policy (spec/024 REQ-2400): under enforce, refuse to boot on any non-exempt business
	// secret resolving through a plaintext-bearing scheme (env:/file:/literal) instead of a backend. Default
	// off = behaviour-preserving. Classification never resolves or logs a secret value.
	// TG-284: the gate also SHAPE-scans the REAL process environment, because the enumeration can only see
	// what a caller declared — and a credential that is neither a reference nor declared was not merely
	// unpoliced, it was invisible, with the gate's green result asserting its absence.
	policy := credpreflight.ParseSecretPolicy(cfg.SecretPolicy)
	rep := credpreflight.CheckSecretPolicyWithEnv(cfg.secretEntries(), os.Environ())
	if policy != credpreflight.PolicyOff {
		// The scan's REACH, not just its verdict: "nothing found" over an unscanned environment must not
		// read like "nothing found" over a fully scanned one.
		log.Printf("secret policy=%s: %s", policy, rep.EnvScanSummary())
	}
	if policy == credpreflight.PolicyWarn {
		for _, v := range rep.Violations {
			if v.RawPlaintext {
				log.Printf("secret policy=warn: %s holds a RAW credential VALUE in the process env (not a reference) — move it to a secret backend (bao:/vault:/store:) and REMOVE the plaintext variable", v.Name)
				continue
			}
			log.Printf("secret policy=warn: %s resolves through the %s: scheme (plaintext) — move it to a secret backend (bao:/vault:/store:)", v.Name, v.Scheme)
		}
	}
	if err := credpreflight.EnforceSecretPolicy(policy, rep); err != nil {
		return err
	}
	return nil
}

// secretsMountEvidence reports what the process can actually SEE of the secrets mount, so the zero-refs
// skip above carries evidence rather than an assumption. CI's preflight-smoke has no mount; a deploy host
// does. Naming the difference is what lets a human tell a legitimate no-op from an unchecked deploy.
func secretsMountEvidence() string {
	const dir = "/secrets"
	entries, err := os.ReadDir(dir)
	switch {
	case err != nil:
		return "no " + dir + " mount visible — consistent with CI, where there is nothing to actuate over SSH"
	case len(entries) == 0:
		return dir + " is mounted but EMPTY — a deploy host with an empty secrets mount has lost its keys"
	default:
		return fmt.Sprintf("%s is mounted and holds %d entr(y/ies) — so this host HAS secrets and still "+
			"configured no key refs", dir, len(entries))
	}
}

func main() {
	// Install the module configuration keys, derived from each module's published descriptor.
	// Without this the control-plane registry holds ZERO module keys, so a console write of a
	// connector setting is rejected as unknown — and cmd/worker referenced cpconfig not at all, so
	// the write was inert in both directions. core/ does not import modules/, so the keys are
	// PUSHED in here rather than pulled from the catalog by the safety core.
	cpconfig.SetModuleKeys(catalog.ConfigKeys())
	check := flag.Bool("check", false, "run the boot preflight and exit (no infra required)")
	// TG-170: the compose HEALTHCHECK entry point. Distroless has no shell and no curl, so the binary
	// probes its own listener. Parsed and handled BEFORE any config load — a healthcheck that needs the
	// database to answer would report the app unhealthy every time the database blinked.
	healthcheck := flag.Bool("healthcheck", false, "probe this process's own HTTP listener and exit 0/1")
	flag.Parse()
	if *healthcheck {
		runHealthcheck(cmp.Or(os.Getenv("TG_PUBLIC_ADDR"), ":8080"), "/healthz")
	}
	log.SetPrefix("grounder: ")
	log.SetFlags(log.LstdFlags | log.LUTC)

	// TG-263: load the operator's saved settings BEFORE loadEnv resolves anything. The DSN is read with
	// os.Getenv directly — a database cannot supply the address of the database it lives in.
	// The config store prefers TG_DB_DSN — a STATIC login pinned for exactly this pre-dyndb read, the same
	// posture as the worker's boot_config — falling back to TG_RUNTIME_DSN for deployments that never armed
	// dyn:. This read runs BEFORE wireDynDB by design (saved overrides must apply to that wiring), so a
	// dyn: reference cannot resolve here; installGrounderConfig refuses one with a named log instead of
	// handing pgx an unparseable ref (TG-422 slice-2 follow-up: observed live 2026-08-22, the grounder's
	// console overrides silently stopped applying the moment TG_RUNTIME_DSN went dyn:).
	cfgStoreDSN := os.Getenv("TG_DB_DSN")
	if strings.TrimSpace(cfgStoreDSN) == "" {
		cfgStoreDSN = os.Getenv("TG_RUNTIME_DSN")
	}
	installGrounderConfig(context.Background(), cfgStoreDSN)
	cfg := loadEnv()

	// OUTBOUND METER (TG-324). Position is load-bearing, and it is the same ordering the worker uses:
	// AFTER installGrounderConfig + loadEnv, so the allowlist sees the operator's saved module settings
	// (a stored value beats the environment, and os.Environ() alone cannot see one) — and BEFORE the
	// OpenBao credential delivery below, so the process's VERY FIRST outbound call is already counted.
	//
	// This process was the gap. TG-160 installed the meter in the worker only, and nothing recorded the
	// grounder as out of scope; measured 2026-08-07, grounder:8080/metrics served zero tg_egress_* series
	// while the egress table's own reason field said this service dials OpenBao off-host. It is also the
	// ONLY TG process on the published tg-frontdoor, so the control was strongest where exposure was
	// lowest and absent where it is highest.
	grounderEgress = egress.Install(egress.InstallConfig{
		Environ:   grounderEffectiveEnviron(),
		Extra:     cfg.EgressAllow,
		ModeRaw:   cfg.EgressMode,
		Component: "grounder",
		Logf:      log.Printf,
	})

	// Credential delivery (spec/022 REQ-2200/REQ-2204, TG-156/TG-157): make the control plane's own SecretRefs
	// resolvable as bao: references from OpenBao, before any secret resolves. Substrate OFF by default
	// (TG_OPENBAO_ADDR unset) ⇒ behaviour-preserving no-op; fail-closed when enabled but misconfigured.
	// mTLS machine-identity bootstrap (spec/024 Amendment 2026-07-25, T-024-10): where a FreeIPA-CA client
	// cert+key are configured (TG_OPENBAO_CERT/_KEY), authenticate to OpenBao by PRESENTING that identity —
	// no bootstrap token on disk. Preferred, higher-assurance path; it takes precedence over the token, which
	// stays as a transition fallback until retired. Mirrors cmd/worker so ALL OpenBao consumers can retire the
	// shared bootstrap token together (the grounder was the second of the three still on token-only).
	baoCert, baoKey := os.Getenv("TG_OPENBAO_CERT"), os.Getenv("TG_OPENBAO_KEY")
	var delErr error
	// APPROLE, ranked between the two (TG-153): below mTLS, above the token. It is the only bootstrap that
	// gives one host two distinct OpenBao identities, which the credential-plane split depends on.
	grRoleID, grSecretID := os.Getenv("TG_OPENBAO_ROLE_ID_REF"), os.Getenv("TG_OPENBAO_SECRET_ID_REF")
	switch {
	case baoCert != "" || baoKey != "":
		delErr = openbao.WireDeliveryCert(os.Getenv("TG_OPENBAO_ADDR"), baoCert, baoKey, os.Getenv("TG_OPENBAO_CERT_ROLE"), os.Getenv("TG_OPENBAO_CA"), log.Printf, meteredBaoTransport()...)
	case grRoleID != "" || grSecretID != "":
		delErr = openbao.WireDeliveryAppRole(os.Getenv("TG_OPENBAO_ADDR"), grRoleID, grSecretID, os.Getenv("TG_OPENBAO_CA"), log.Printf, meteredBaoTransport()...)
	default:
		delErr = openbao.WireDelivery(os.Getenv("TG_OPENBAO_ADDR"), os.Getenv("TG_OPENBAO_TOKEN_REF"), os.Getenv("TG_OPENBAO_CA"), log.Printf, meteredBaoTransport()...)
	}
	if delErr != nil {
		log.Fatalf("credential delivery: %v", delErr)
	}

	// TG-422 slice 2: the dyn: dynamic-Postgres-credential scheme at the grounder root — the resolver for a
	// dyn: migration/runtime DSN below. Wired AFTER delivery so a bao: token ref can resolve; nil when OFF.
	dynProvider, dynDSNTmpl := wireDynDB()
	if dynProvider != nil {
		// Revoke every leased credential at shutdown — a dynamic credential dies with the process instead
		// of outliving it as a static password would.
		defer func() { _ = dynProvider.Close(context.Background()) }()
	}

	// The sealed-secret Sealer (spec/022 REQ-2201): OpenBao Transit (master key off the worker) when configured,
	// else the in-process master key. Built once; shared by the seal-write backend and the store: resolver.
	sealer, sealDesc := buildSealer(cfg)

	gate := safety.NewReadOnlyChokepoint() // read-only chokepoint: the control plane never actuates (no mode authority)

	// In --check mode, only the pure preflight runs (no DB), so config presence is advisory.
	if *check {
		checkCfg := cfg
		checkCfg.RuntimeDSN, checkCfg.MigrationDSN = "dryrun", "dryrun"
		if err := preflight(checkCfg, gate); err != nil {
			log.Fatalf("boot preflight FAILED (fail-closed): %v", err)
		}
		// Credential deploy-gate (TG-113): if any SSH private key is CONFIGURED, prove this process's REAL
		// runtime user (the distroless image runs as nonroot uid:gid 65532, and the box mounts the same
		// ./secrets read-only) can resolve + read + parse it — BEFORE the worker goes live. This catches a
		// re-provision that dropped /secrets/one_key (the silent-kill: the worker booted preflight-GREEN
		// while ALL native SSH was dead). It fails the pipeline with a NON-ZERO exit. When no SSH key is
		// configured (e.g. the CI preflight-smoke, which has no ./secrets), it is a documented no-op so CI
		// stays green — there is nothing to actuate/investigate over SSH. Run this on the box WITH the
		// ./secrets mount + TG_ACTUATION_SSH_KEY set (see deploy/secrets/README.md) as the deploy gate.
		if sshRep := credpreflight.CheckSSHKeys(credpreflight.SSHKeyRefsFromEnv(os.Getenv)); sshRep.Configured() == 0 {
			// SAY WHICH CASE THIS IS (TG-249 item 7 follow-on). Zero refs is legitimate in CI's
			// preflight-smoke, which has no ./secrets mount and nothing to actuate over SSH. It is a
			// MISCONFIGURATION on a deploy host, where the mount exists and an actuation identity is
			// expected — and until now both produced this identical green line.
			//
			// That is why item 7 went unnoticed: compose forwarded one of five key-ref sources to this
			// service and the gate that exists to catch exactly that reported the same thing it reports
			// when there is genuinely nothing to check. A check whose "nothing to check" and "everything
			// checked" outcomes are indistinguishable cannot catch its own omission.
			//
			// Deliberately LOG-ONLY: the pass/fail behaviour is unchanged. Turning zero refs into a
			// failure on a host that actuates is the right end state, but it is a deploy-gate posture
			// change and belongs in its own reviewed step, not smuggled in beside a log line.
			log.Printf("credential preflight: no SSH key references configured — skipping (%s). If this is a "+
				"deploy host, that is a MISCONFIGURATION, not a no-op: check that the grounder service is "+
				"forwarded every TG_* key-ref source core/preflight.SSHKeyRefsFromEnv reads.",
				secretsMountEvidence())
		} else if sshRep.Failed() {
			log.Fatalf("credential preflight FAILED (fail-closed): %s — provision the key readable by uid:gid %d:%d mode 0640 (see deploy/secrets/README.md)", sshRep.Summary(), os.Getuid(), os.Getgid())
		} else {
			log.Printf("credential preflight OK — %d SSH key ref(s) resolve+parse as uid:gid %d:%d: %s", sshRep.Configured(), os.Getuid(), os.Getgid(), strings.Join(sshRep.OK, ", "))
		}
		log.Printf("boot preflight OK — may_actuate=%v, Band(0)=%s (read-only foundation)", gate.MayActuate(), safety.Band(0))
		return
	}

	if err := preflight(cfg, gate); err != nil {
		log.Fatalf("boot preflight FAILED (fail-closed): %v", err)
	}

	ctx := context.Background()
	// TG-422 slice 2: a MIGRATION DSN of `dyn:<role>` resolves ONCE here, through the registered SecretRef
	// scheme — boot-scoped (Migrate + ApplyPlaneGrants finish in minutes, well inside the 1h lease TTL), and
	// the lease is renewed/rotated by the provider until shutdown revokes it. With the scheme OFF a dyn:
	// reference fails closed here and the boot REFUSES — never a silent fallback to a static password.
	migrationDSN := cfg.MigrationDSN
	if strings.HasPrefix(migrationDSN, dyndb.Scheme+":") {
		resolved, rerr := config.SecretRef(migrationDSN).Resolve()
		if rerr != nil {
			log.Fatalf("migrations: the migration DSN is a dyn: reference that will not resolve — refusing "+
				"to boot rather than fall back to a static credential (TG-422): %v", rerr)
		}
		migrationDSN = resolved
		log.Print("migrations: the migration DSN leases its login from OpenBao's database engine " +
			"(dyn:, boot-scoped resolution; TG-422 slice 2)")
	}
	if err := db.Migrate(ctx, migrationDSN); err != nil {
		log.Fatalf("migrations failed: %v", err)
	}
	// ★ THE DATABASE HALF OF THE CREDENTIAL-PLANE SPLIT (TG-164). Derive tg_triage's and tg_actuate's table
	// privileges from tg_runtime's, withholding each plane's off-plane writes (core/db/plane_roles.go).
	//
	// It runs HERE, on every boot, rather than as a one-shot DDL block inside migration 0059, because the two
	// plane roles carry passwords and are therefore created outside the migration lattice — possibly long
	// after 0059 applied. A one-shot grant would have run against roles that did not exist and never run
	// again: the privileges would be silently absent and the split worker would fail with a permission error
	// deep inside an activity. Re-running makes the state converge, and picks up tables added by later
	// migrations, which a frozen grant list could not.
	//
	// NOT FATAL, deliberately: a database with neither plane role is the default and returns a clean no-op, so
	// the only way to reach the error arm is a database that HAS the roles and could not be derived. Killing
	// the console over that would take the whole control plane down to protect a hardening the deployment may
	// not even be using — but it is logged as the security event it is, because a split worker whose grants
	// failed to apply is running with authority its operator believes was removed.
	if rep, gerr := db.ApplyPlaneGrants(ctx, migrationDSN); gerr != nil {
		log.Printf("credential-plane DB roles: DERIVATION FAILED (%v) — any tg_triage/tg_actuate worker is "+
			"running on whatever privileges it already had, NOT the ones this build declares (TG-164)", gerr)
	} else {
		// One line in both postures. rep.String() distinguishes them in words ("NOT in force" vs the per-role
		// privilege counts) rather than leaving the reader to infer a split from the absence of a complaint.
		log.Printf("credential-plane DB roles: %s", rep)
	}
	// TG-422 slice 2: a RUNTIME DSN of `dyn:<role>` leases its login PER CONNECTION (db.ConnectDynamic) —
	// the pool outlives any single lease, so it must not freeze one resolved string. Fail closed: a dyn:
	// runtime DSN with the engine OFF refuses the boot, never falls back to a static credential.
	var pool *db.Pool
	var err error
	if role, isDyn := strings.CutPrefix(cfg.RuntimeDSN, dyndb.Scheme+":"); isDyn {
		if dynProvider == nil {
			log.Fatalf("db connect: the runtime DSN is a dyn: reference but dynamic credentials are OFF " +
				"(TG_DYNDB_ADDR unset) — refusing to boot rather than fall back to a static credential (TG-422)")
		}
		pool, err = db.ConnectDynamic(ctx, dynDSNTmpl, dynProvider.Credentials(role))
		if err == nil {
			log.Printf("db: runtime pool leases role %q per connection from OpenBao's database engine — "+
				"rotated at max_ttl, revoked at shutdown (TG-422 slice 2)", role)
			dyndb.ArmRotationEviction(dynProvider, role, pool.Reset, log.Printf) // TG-553: evict pooled conns on lease rotation
		}
	} else {
		pool, err = db.Connect(ctx, cfg.RuntimeDSN)
	}
	if err != nil {
		log.Fatalf("db connect failed: %v", err)
	}
	defer pool.Close()

	// SCHEMA DRIFT AGAINST THE RUNNING DATABASE (TG-383). Deliberately here, next to the plane-roles line
	// above, because they answer adjacent halves of one question and only one of them was ever asked.
	//
	// Every schema guard in this tree runs against a fixture built from the migrations in this repo, so
	// they all answer "is the schema we DECLARE self-consistent?". Nothing answered "does the schema that
	// EXISTS match it?" — and production carries a table no migration creates
	// (policy_ruleset_bak_handsoff, a hand-made backup) which already aborted the entire plane-grant
	// derivation once. The guard that should have caught it is green and structurally cannot see it.
	//
	// NOT FATAL. This is a report, not a gate: refusing to boot over a table someone made by hand would
	// take the control plane down to protect a property the deployment may be fine with, and the drift is
	// most likely to be discovered on a database that is otherwise serving. It is logged at the same
	// weight as the plane-roles line and published as a gauge, because the failure mode here is silence.
	if drift, derr := db.DetectSchemaDrift(ctx, pool); derr != nil {
		log.Printf("schema drift: CHECK FAILED (%v) — the running schema is unverified against this "+
			"build's migrations; an undeclared table would be invisible exactly as before (TG-383)", derr)
	} else {
		schemaDrift.Store(&drift)
		log.Printf("schema drift: %s", drift)
	}

	// Adopt committed console overrides (task #27 Phase C) BEFORE any component reads its config, so
	// the /v1/config report and the running components agree (INV-15). Legality re-checked per key.
	cpStore := db.NewCPConfigStore(pool)
	applyConfigOverrides(ctx, cpStore, &cfg)

	// The WORKER publishes its live mutation posture to runtime_posture (spec/012 REQ-1107); the grounder
	// reports THAT on /v1/whoami + /v1/governance, never its own gate — which is read-only by construction
	// (preflight refuses to boot with mutation on). postureStaleAfter is the freshness bound (default 90s =
	// 3x the worker's 30s heartbeat): a row older than this reads as posture-unknown, so a worker/DB hiccup
	// can never make the console under-report a live-ON worker. Config-not-code (TG_POSTURE_STALE_AFTER);
	// blank/invalid/non-positive keeps the default.
	postureRead := db.NewPostureReadStore(pool)
	postureStaleAfter := 90 * time.Second
	if s := strings.TrimSpace(os.Getenv("TG_POSTURE_STALE_AFTER")); s != "" {
		if d, derr := time.ParseDuration(s); derr == nil && d > 0 {
			postureStaleAfter = d
		}
	}

	verifier, err := auth.NewVerifier(db.NewSourceResolver(pool), db.NewNonceStore(pool), 5*time.Minute)
	if err != nil {
		log.Fatalf("auth verifier misconfigured (fail-closed): %v", err)
	}
	gw := model.NewGateway(cfg.LiteLLMURL, cfg.LiteLLMKeyRef)
	_ = gw // consumed by the native agent loop in P1-4

	// The module registry backs the alert front door: /v1/ingest/{source_type} accepts a payload only from a
	// declared, enabled ingest capability (INV-17). Config-free ingest sources (crowdsec, prometheus-
	// alertmanager) declare here; config-driven ingest joins as the grounder gains its config surface.
	moduleReg, err := bootstrap.NewRegistry()
	if err != nil {
		log.Fatalf("module registry bootstrap failed (fail-closed): %v", err)
	}
	// LibreNMS is a config-driven ingest source (site|url|tokenref list), so the front door accepts a
	// /v1/ingest/librenms POST only where it is declared — matching the worker's estate/ingest config.
	// The list may carry a console override adopted above (task #27 Phase C).
	if err := bootstrap.RegisterConfiguredIngest(moduleReg, bootstrap.ParseLibrenmsDeployments(cfg.LibrenmsDeployments)); err != nil {
		log.Fatalf("ingest registration failed (fail-closed): %v", err)
	}
	// Provision the LibreNMS push-ingest bearer REPRODUCIBLY (config-not-code): when deployments are declared
	// AND the configured token ref resolves to a non-empty value, idempotently upsert the `librenms` sources
	// row so the front door bearer-authenticates LibreNMS pushes (AuthIngestPush) exactly like Alertmanager —
	// no hand-run SQL. Only the REF is stored (INV-13), never the literal. The decision is a PURE function so
	// the provision/skip logic is oracle-testable. If the ref is unset/empty or resolves empty, we do NOT
	// provision (a credential-less source would just fail closed on every push). A DB error logs and continues
	// — this optional provisioning must never crash the read-only foundation (fail open on optional DB).
	provisionLibrenmsPushAuth(ctx, db.NewSourceResolver(pool), cfg)
	// Same treatment for the Alertmanager push bearer (TG-278/TG-284). Its sources row was created by
	// hand-run SQL against a plaintext literal that no *_REF variable pointed at; this is the read that makes
	// TG_AM_INGEST_TOKEN_REF a real knob, so the credential can be migrated to bao: like every other one.
	provisionAlertmanagerPushAuth(ctx, db.NewSourceResolver(pool), cfg)
	// And CrowdSec (TG-291) — the only security-telemetry ingest TG declares, which has never delivered a
	// row because it is the one push source that never got a provisioner. Unset ref ⇒ not provisioned, said
	// plainly in the boot log rather than left to be inferred from 401s.
	provisionPushSource(ctx, db.NewSourceResolver(pool), crowdsec.SourceType, "TG_CROWDSEC_INGEST_TOKEN_REF", cfg.CrowdsecIngestTokenRef)
	// And authlog (TG-315), which has the SAME hole: catalog.go records it as a "push-only receiver" — the
	// syslog-ng collector POSTs folded events to /v1/ingest/authlog — and it too has no sources row, so it
	// has never delivered either. Found while fixing crowdsec; shipping one and leaving the identical hole
	// in the connector built last week would be the same defect with a different name.
	provisionPushSource(ctx, db.NewSourceResolver(pool), authlog.SourceType, "TG_AUTHLOG_INGEST_TOKEN_REF", cfg.AuthlogIngestTokenRef)

	// The native credential-rule read backend (TG-109): a pure pool read, deliberately set BEFORE the
	// optional Temporal block so GET /v1/credentials/native serves even when the write lane (worker-side)
	// is unreachable — reads must not depend on Temporal.
	grounderNativeRules = nativeRulesReadStore{s: db.NewCredentialNativeRuleStore(pool)}
	// TG-481: the object-group read surface, same pool + read-serves-without-Temporal rationale.
	grounderObjectGroups = objectGroupsReadStore{s: db.NewEstateObjectGroupStore(pool)}

	// Optional Temporal client for the front door's triage trigger. Best-effort by design: the read-only API
	// (stats/ledger/whoami) must not depend on Temporal, so a dial failure logs and degrades ingest to
	// validate-only (accept + normalize, no session minted) rather than refusing to start.
	var triage httpapi.TriageStarter
	var votes httpapi.VoteSignaler
	var skillsWrite httpapi.SkillsWriter
	var configWrite httpapi.ConfigWriter
	var secretsWrite httpapi.SealedSecretWriter
	var sealRewrap httpapi.SealRewrapper
	var modeTransition httpapi.ModeTransitioner
	var engineToggle httpapi.EngineToggler
	// The active-ruleset write lane (spec/015 REQ-1503, TG-104): replacing the rules-as-data policy
	// document executes in the worker (single ledger writer, validated + ledgered). Without a Temporal
	// client this stays nil ⇒ POST /v1/policy/ruleset is 503 and the console renders its editor disabled.
	var rulesetWrite httpapi.RulesetWriter
	// The world-model review lane (spec/027 REQ-2703): adopt/reject/retire execute in the worker on the
	// single ledger writer. Without a Temporal client this stays nil => the write routes are 503 and the
	// console renders its controls disabled — an honest "no write path", never a silent no-op.
	var manifestWrite httpapi.ManifestWriter
	// The earned op-class ratify lane (spec/028 REQ-2813). Same gating, same reason, and it matters more
	// here: nil means the ratify route is 503 and the console renders the form disabled. A grant that
	// appeared to succeed with no worker behind it would be an operator believing they had authorized a
	// capability that does not exist.
	var opClassWrite httpapi.OpClassWriter
	// The module TEST lane. Same gating and the same reason: the probe runs in the WORKER, so without a
	// Temporal client there is no way to reach a module and the route must be 503 rather than reporting a
	// result nobody produced.
	var moduleTest httpapi.ModuleTester
	// The policy packet-tracer (TG-105). Same gating and the same reason: the trace evaluates on the worker's
	// REAL engine, so without a Temporal client there is nothing to ask and POST /v1/policy/trace must be 503
	// rather than answer from a grounder-side engine that could disagree with the live decision.
	var policyTrace httpapi.PolicyTracer
	if tport := os.Getenv("TG_TEMPORAL_HOSTPORT"); tport != "" {
		if tc, terr := client.Dial(client.Options{HostPort: tport}); terr != nil {
			log.Printf("temporal dial %s failed — ingest degraded to validate-only: %v", tport, terr)
		} else {
			defer tc.Close()
			triage = temporalTriage{c: tc}
			votes = temporalVotes{c: tc}
			skillsWriteStore := db.NewSkillStore(pool)
			// TG-489: initialize/verify the distillate tamper chain so console-originated drafts can
			// append (idempotent; advisory-locked against the worker's boot doing the same).
			if rep, cerr := skillsWriteStore.EnsureChain(context.Background()); cerr != nil {
				log.Printf("skills: distillate chain: %v", cerr)
			} else {
				log.Printf("skills: %s", rep)
			}
			skillsWrite = skillsWriteBackend{store: skillsWriteStore, tc: tc}
			// Config writes execute in the worker (single ledger writer, task #27 Phase C).
			configWrite = configWriteBackend{tc: tc}
			// Sealed-secret writes: seal HERE (the material never transits Temporal), commit in the
			// worker. Requires the master key reference to resolve — probed per write, fail closed.
			secretsWrite = secretsWriteBackend{tc: tc, sealer: sealer}
			// DEK rewrap (TG-163): re-key the sealed store onto the current master-key version so the
			// previous OpenBao Transit key version can actually be retired. Operator-driven only —
			// there is no cron behind it. Without a Temporal client it stays nil ⇒ the route is 503.
			sealRewrap = sealRewrapBackend{tc: tc}
			// Autonomy-mode transitions (spec/015 REQ-1502) — the LAST gate before the mutation flip —
			// execute in the worker on the single chokepoint-bound ModeController (authority + preflight
			// gated, audited). Without a Temporal client the surface stays nil ⇒ POST /v1/mode is 503.
			modeTransition = modeTransitionBackend{tc: tc}
			// Policy-engine enable/disable (spec/015 REQ-1519) executes in the worker on the single live
			// EngineToggle (the ledger's single writer). Without a Temporal client the surface stays nil ⇒
			// POST /v1/policy/engine-toggle is 503.
			engineToggle = engineToggleBackend{tc: tc}
			// Active-ruleset replacement (TG-104) executes in the worker on the single ledger writer: the
			// document is validated (ParseRuleSet, fail-closed), ledgered, and persisted. Without a Temporal
			// client it stays nil ⇒ POST /v1/policy/ruleset is 503.
			rulesetWrite = rulesetWriteBackend{tc: tc}
			manifestWrite = manifestWriteBackend{tc: tc}
			opClassWrite = opClassWriteBackend{tc: tc}
			// The policy packet-tracer (TG-105) runs the worker's REAL engine over the SAME Temporal client;
			// without it the surface stays nil ⇒ POST /v1/policy/trace is 503.
			policyTrace = policyTraceBackend{tc: tc}
			moduleTest = temporalModuleTest{c: tc}
			// Operator-facing MANUAL ROLLBACK (TG-462): the pre-check reads the forward execution + manifest
			// stores; the governed inverse is sealed + actuated in the WORKER. Without a Temporal client this
			// stays nil ⇒ POST /v1/actions/{action_id}/rollback is 503. Set as a package var (not a
			// buildPublicAPI param) to avoid growing that signature — the documented positional-rebind hazard.
			grounderRollback = rollbackBackend{tc: tc, execs: db.NewActionExecutionStore(pool), manifests: db.NewManifestStore(pool)}
			// Credential "Sync now" (TG-109): same package-var wiring, same reason.
			grounderCredentialSync = temporalCredentialSync{c: tc}
			// Native credential-rule writes (TG-109) execute in the worker (validated + ledgered, single
			// writer). Without a Temporal client this stays nil ⇒ the native-rule write routes are 503,
			// while the read above still serves.
			grounderNativeRuleWrite = nativeRuleWriteBackend{tc: tc}
			// TG-481: the object-group write lane, same worker-write / read-still-serves rationale.
			grounderObjectGroupWrite = objectGroupWriteBackend{tc: tc}
			// Benchmark-axis scoreboard (TG-480): package-var wiring, same positional-rebind rationale.
			grounderAxes = &axesReadStore{store: db.NewAxisReadStore(pool)}
			log.Printf("alert front door: triage trigger wired to temporal at %s", tport)
		}
	}

	// The sealed-secret store (task #27 Phase D): the store: SecretRef scheme resolves against it
	// when the master key reference resolves; otherwise the scheme stays fail-closed-unwired.
	sealedStore := db.NewSealedSecretStore(pool)
	if sealer != nil {
		config.RegisterStoreResolver(seal.StoreResolver(sealer, sealedStore))
		log.Printf("sealed secrets: store: references enabled (%s)", sealDesc)
	} else {
		log.Printf("sealed secrets: disabled — no usable seal backend (set TG_SEAL_TRANSIT_KEY or a usable TG_SEAL_KEY_REF); store: refs and secret writes fail closed")
	}

	// Public API: EVERY route authenticated (INV-01). Read-only in Phase 0/1. Built by buildPublicAPI so
	// the exact mounted route set is oracle-testable without a live listener. The governance ledger is
	// served from its durable, hash-chained store so the console renders the real audit spine.
	sessions := buildBrowserSessions(cfg, db.NewSessionStore(pool)) // durable: survives restarts (REQ-508)
	var adminSessions *auth.AdminAuthenticator
	if sessions != nil {
		// LDAP / FreeIPA console login (REQ-508 extension): a FreeIPA user logs in with their own directory
		// username + password, composed WITH the static break-glass operator (cfg.OperatorName). Enabled only
		// when configured; a nil result leaves the static-only path untouched (fail closed to break-glass).
		ldapEnabled := false
		if ldapAuth := buildLDAPAuth(cfg); ldapAuth != nil {
			sessions.EnableLDAP(ldapAuth, cfg.OperatorName)
			ldapEnabled = true
		}
		verifier.EnableBrowserSessions(sessions)
		// The admin tier exists only ON TOP of the browser path (an elevation is a property of a session):
		// registered when the static admin credential resolves (REQ-522) OR LDAP login is on (a tg-admins
		// LDAP session is admin-eligible and steps up WITHOUT the static credential).
		adminSessions = buildAdminSessions(cfg)
		if adminSessions == nil && ldapEnabled {
			// No static admin credential, but LDAP is on: register an admin tier whose only path is the LDAP
			// tg-admins step-up (empty static resolver ⇒ the static-credential Elevate always fails closed).
			if aa, err := auth.NewAdminAuthenticator(auth.MemOperators{}, cfg.AdminTTL); err == nil {
				adminSessions = aa
				log.Printf("admin tier enabled for LDAP tg-admins step-up only (no static admin credential; elevation ttl %s)", cfg.AdminTTL)
			}
		}
		if adminSessions != nil {
			verifier.EnableAdminSessions(adminSessions)
		}
	}
	// ONE fleet view for every surface that describes the fleet (TG-268).
	fleet := fleetView{reg: moduleReg, projection: db.NewCapabilityProjectionStore(pool).Load,
		staleAfter: capabilityStaleWindow()}
	// spec/029 T-029-5 (REQ-2906): the console's armed-revert chip read — wired via the package-level
	// seam (the ingestRefusalCounter precedent: buildPublicAPI's signature is parse-tested and already
	// the longest in the tree). Adapts the store row to the httpapi-local DTO (layering: httpapi does
	// not import core/db).
	commitConfirmChipRead = func(ctx context.Context, externalRef string) (*httpapi.CommitConfirmChipDTO, error) {
		row, found, err := db.NewCommitConfirmStore(pool).LatestForRef(ctx, externalRef)
		if err != nil || !found {
			return nil, err
		}
		chip := &httpapi.CommitConfirmChipDTO{
			State: row.State, OpClass: row.OpClass,
			ArmedAt: row.ArmedAt.UTC().Format(time.RFC3339), DeadlineAt: row.DeadlineAt.UTC().Format(time.RFC3339),
			Detail: row.ResolutionDetail, InverseActionID: row.InverseActionID,
		}
		return chip, nil
	}
	api := buildPublicAPI(verifier, gate, ledgerReadStore{s: db.NewLedgerStore(pool)}, registryIngesters{reg: moduleReg}, triage, fleet, sessions, sessionsReadStore{s: db.NewSessionReadStore(pool)}, db.NewAlertLogStore(pool), db.NewTransitionLogStore(pool), governanceReader{gate: gate, sessions: db.NewSessionReadStore(pool), ledger: db.NewLedgerStore(pool), posture: postureRead, staleAfter: postureStaleAfter}, configSecrets{cfg: cfg}, newLitellmModels(cfg.LiteLLMURL, cfg.LiteLLMKeyRef), contracts.OpenAPI, estateReadStore{s: db.NewEstateReadStore(pool)}, groundingReadStore{s: db.NewGroundingReadStore(pool)}, votes, db.NewPendingStore(pool), newConfigResolver(gate, cfg, cpStore), db.NewSkillStore(pool), skillsWrite, newFileWiki(cfg.KnowledgeFile, cfg.KnowledgeSeedFile, cfg.WikiArticlesFile), adminSessions, configWrite, secretsWrite, sealRewrap, sealedReadStore{s: sealedStore}, credentialsReadStore{s: db.NewCredentialReadStore(pool)}, policyReadStore{s: db.NewPolicyReadStore(pool)}, regimeReadStore{s: db.NewRegimeReadStore(pool)}, modeTransition, engineToggle, postureRead, postureStaleAfter, sessionDetailReadStore{s: db.NewTraceSpineStore(pool)}, db.NewAgentStepEvidenceStore(pool), db.NewTriageStore(pool), credentialOnboardingStore{s: db.NewCredentialBindingProjectionStore(pool), staleAfter: 3 * time.Minute}, db.NewActionManifestReadStore(pool), suppressionshadow.New(db.NewAlertHistoryStore(pool), log.Printf), proposalsReadStore{s: db.NewTriageStore(pool)}, proposalsReadStore{s: db.NewTriageStore(pool)}, manifestReadStore{s: db.NewWorldManifestStore(pool)}, manifestWrite, opClassReadBackend{store: db.NewOpClassCandidateStore(pool)}, opClassWrite,
		catalogSchema{fleet: fleet}, moduleTest,
		baoSecretWriter(os.Getenv("TG_OPENBAO_ADDR"), os.Getenv("TG_OPENBAO_WRITER_ROLE_ID_REF"), os.Getenv("TG_OPENBAO_WRITER_SECRET_ID_REF"), os.Getenv("TG_OPENBAO_CA"), log.Printf),
		db.NewGateVerdictStore(pool), cfg.GateMarginEpsilon, policyTrace, rulesetWrite)

	// Root mux: the liveness/readiness probes are the ONLY unauthenticated paths — they expose nothing and
	// have no side effects; every other path goes through the authenticated router. /healthz + /readyz match
	// the Helm chart probes (spec/009) and the compose healthcheck; /livez is kept as an alias.
	root := http.NewServeMux()
	probeOK := func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	}
	root.HandleFunc("/livez", probeOK)
	root.HandleFunc("/healthz", probeOK) // liveness (chart livenessProbe)
	root.HandleFunc("/readyz", probeOK)  // readiness (chart readinessProbe)
	// Read-only /metrics (Phase-2 readiness review §2: "Gather() never served → /metrics 404"). Served on
	// the SAME unauthenticated-internal footing as the probes — it emits no secret and has no side effect —
	// matching how the predecessor exported metrics to Prometheus. It publishes the gate posture so the
	// "unexpected mutation while OFF" alert can fire on the grounder too; the mutation breaker lives in the
	// worker, which serves circuit_breaker_state on its own /metrics.
	root.Handle("/metrics", metrics.Handler(func() []metrics.Sample {
		enabled := 0.0
		if gate.MayActuate() {
			enabled = 1
		}
		// TG-371: front-door REFUSALS ride alongside the posture gauges. Appended rather than inlined so
		// the per-(source, reason) series stay a variable-length set — nothing is emitted until a
		// delivery is actually turned away, and the presence of any series is itself the signal.
		return append([]metrics.Sample{
			// tg_may_actuate is THE name (TG-112). The deprecated `mutation_enabled` alias this process
			// emitted beside it is retired: every consumer (alert.rules.yml, safety.json, the console,
			// shadowbench) now joins on tg_may_actuate / tg_policy_mode, so a second name for the same
			// read would only invite the two to drift.
			{Name: "tg_may_actuate", Kind: metrics.Gauge, Value: enabled, Labels: map[string]string{"component": "grounder"},
				Help: "1 when this process may actuate. The grounder is read-only BY CONSTRUCTION (it holds " +
					"no mode authority and refuses to boot able to actuate), so this is 0 and a 1 here is a " +
					"defect, not a posture change."},
			{Name: "tg_up", Kind: metrics.Gauge, Help: "process liveness marker (1 while serving)", Value: 1, Labels: map[string]string{"component": "grounder"}},
			// TG-324: the outbound meter's lane, appended for the same reason the refusal counter is —
			// it is a variable-length set. It emits NOTHING when no meter is installed, so "not wired"
			// stays distinguishable from "wired and quiet" instead of both reading as a row of zeros.
		}, append(append(append(ingestRefusalCounter.samples(), ingestPredropCounter.samples()...), grounderEgressSamples()...), schemaDriftSamples()...)...)
	}))
	root.Handle("/", api.Mux())

	// Separate elevated admin listener (mTLS in deploy; AuthMTLS routes fail closed without a client cert).
	admin := auth.NewRouter(verifier)
	admin.Handle("/admin/status", auth.AuthMTLS, func(w http.ResponseWriter, r *http.Request, p auth.Principal) {
		fmt.Fprintf(w, `{"admin":true,"may_actuate":%v}`, gate.MayActuate())
	})

	log.Printf("read-only foundation up — public=%s admin=%s may_actuate=%v", cfg.PublicAddr, cfg.AdminAddr, gate.MayActuate())
	go func() { log.Printf("admin listener exited: %v", http.ListenAndServe(cfg.AdminAddr, admin.Mux())) }()
	log.Fatalf("public listener exited: %v", http.ListenAndServe(cfg.PublicAddr, root))
}
