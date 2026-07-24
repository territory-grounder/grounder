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
	_ "time/tzdata" // embed the IANA zoneinfo DB so time.LoadLocation resolves on distroless (no OS tzdata)

	"context"
	"crypto/sha256"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	"go.temporal.io/sdk/client"

	"github.com/territory-grounder/grounder/adapters/model"
	"github.com/territory-grounder/grounder/core/auth"
	"github.com/territory-grounder/grounder/core/config"
	"github.com/territory-grounder/grounder/core/cpconfig"
	"github.com/territory-grounder/grounder/core/db"
	"github.com/territory-grounder/grounder/core/httpapi"
	"github.com/territory-grounder/grounder/core/metrics"
	credpreflight "github.com/territory-grounder/grounder/core/preflight"
	"github.com/territory-grounder/grounder/core/safety"
	"github.com/territory-grounder/grounder/core/seal"
	contracts "github.com/territory-grounder/grounder/docs/contracts"
	"github.com/territory-grounder/grounder/modules/bootstrap"
	"github.com/territory-grounder/grounder/modules/credsource/openbao"
)

type envConfig struct {
	RuntimeDSN    string // TG_RUNTIME_DSN — DML-only role (never committed; supplied by env/secret store)
	MigrationDSN  string // TG_MIGRATION_DSN — DDL-capable role, used only at startup
	LiteLLMURL    string // TG_LITELLM_URL — e.g. http://litellm:4000
	LiteLLMKeyRef config.SecretRef
	PublicAddr    string // TG_PUBLIC_ADDR
	AdminAddr     string // TG_ADMIN_ADDR
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
	// LibrenmsDeployments is the configured LibreNMS ingest list (TG_LIBRENMS_DEPLOYMENTS) — held on
	// the struct so a console override (task #27 Phase C) can be adopted at boot alongside env.
	LibrenmsDeployments string
	// LibrenmsIngestTokenRef is the per-source static bearer REFERENCE (env:/file:/store:, INV-13) the
	// LibreNMS transports present when they POST /v1/ingest/librenms (AuthIngestPush). When deployments are
	// declared AND this ref resolves non-empty, boot provisions the `librenms` sources row from it so the
	// front door bearer-authenticates LibreNMS pushes reproducibly (config-not-code). Never a literal token.
	LibrenmsIngestTokenRef config.SecretRef // TG_LIBRENMS_INGEST_TOKEN_REF
	// SecretPolicy is the boot-time secret-scheme policy (spec/024 REQ-2400): off (default, behaviour-
	// preserving) / warn / enforce. Under enforce the preflight refuses to start on any non-exempt business
	// secret that resolves through a plaintext-bearing scheme (env:/file:/literal) instead of a backend.
	SecretPolicy string // TG_SECRET_POLICY
}

func loadEnv() envConfig {
	get := func(k, def string) string {
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
	return envConfig{
		RuntimeDSN:             os.Getenv("TG_RUNTIME_DSN"),
		MigrationDSN:           os.Getenv("TG_MIGRATION_DSN"),
		LiteLLMURL:             get("TG_LITELLM_URL", "http://litellm:4000"),
		LiteLLMKeyRef:          config.SecretRef(get("TG_LITELLM_KEY_REF", "env:LITELLM_MASTER_KEY")),
		PublicAddr:             get("TG_PUBLIC_ADDR", ":8080"),
		AdminAddr:              get("TG_ADMIN_ADDR", ":8443"),
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
		KnowledgeFile:          os.Getenv("TG_KNOWLEDGE_FILE"),
		KnowledgeSeedFile:      os.Getenv("TG_KNOWLEDGE_SEED_FILE"),
		LibrenmsDeployments:    os.Getenv("TG_LIBRENMS_DEPLOYMENTS"),
		LibrenmsIngestTokenRef: config.SecretRef(get("TG_LIBRENMS_INGEST_TOKEN_REF", "env:TG_LIBRENMS_INGEST_TOKEN")),
		SecretPolicy:           get("TG_SECRET_POLICY", "off"),
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
	return []credpreflight.SecretEntry{
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
	}
}

// buildSealer builds the sealed-secret Sealer from config (spec/022 REQ-2201, TG-157). When
// TG_SEAL_TRANSIT_KEY is set the master-key operation (the DEK wrap/unwrap) runs INSIDE OpenBao Transit and the
// worker holds NO master key at all; otherwise it falls back to the in-process master key (TG_SEAL_KEY_REF).
// The Transit substrate reuses the credential-delivery OpenBao endpoint by default (address/token/CA), but a
// dedicated TG_SEAL_TRANSIT_TOKEN_REF can point the seal plane at a disjoint role (the plane split, T-022-4).
// Returns nil when neither is usable — sealing then fails closed (no seal, no store: resolution, no write).
func buildSealer(cfg envConfig) (*seal.Sealer, string) {
	if key := strings.TrimSpace(os.Getenv("TG_SEAL_TRANSIT_KEY")); key != "" {
		addr := os.Getenv("TG_SEAL_TRANSIT_ADDR")
		if addr == "" {
			addr = os.Getenv("TG_OPENBAO_ADDR")
		}
		tokRef := os.Getenv("TG_SEAL_TRANSIT_TOKEN_REF")
		if tokRef == "" {
			tokRef = os.Getenv("TG_OPENBAO_TOKEN_REF")
		}
		w, err := seal.NewTransitWrapper(seal.TransitConfig{
			BaseURL: addr, KeyName: key, TokenRef: config.SecretRef(tokRef),
			Mount: os.Getenv("TG_SEAL_TRANSIT_MOUNT"), CACert: os.Getenv("TG_OPENBAO_CA"),
		})
		if err != nil {
			log.Printf("sealed secrets: OpenBao Transit wrapper unusable (%v); sealing fails closed", err)
			return nil, ""
		}
		s, err := seal.NewSealer(w)
		if err != nil {
			return nil, ""
		}
		return s, "OpenBao Transit key " + key + " (master key never on the worker)"
	}
	master, err := seal.MasterKeyFromRef(cfg.SealKeyRef)
	if err != nil {
		return nil, ""
	}
	w, err := seal.NewLocalWrapper(master)
	if err != nil {
		return nil, ""
	}
	s, err := seal.NewSealer(w)
	if err != nil {
		return nil, ""
	}
	return s, "in-process master key " + string(cfg.SealKeyRef)
}

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
	mutation := "off"
	if gate.MayActuate() {
		mutation = "on"
	}
	return cpconfig.Resolver{
		Law: map[string]string{
			"safety.never_auto_floor":    "enforced",
			"safety.mutation_enabled":    mutation,
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
		return errors.New("mutation_enabled is true at boot — refusing to start (Phase 0/1 is read-only)")
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
	policy := credpreflight.ParseSecretPolicy(cfg.SecretPolicy)
	rep := credpreflight.CheckSecretPolicy(cfg.secretEntries())
	if policy == credpreflight.PolicyWarn {
		for _, v := range rep.Violations {
			log.Printf("secret policy=warn: %s resolves through the %s: scheme (plaintext) — move it to a secret backend (bao:/vault:/store:)", v.Name, v.Scheme)
		}
	}
	if err := credpreflight.EnforceSecretPolicy(policy, rep); err != nil {
		return err
	}
	return nil
}

func main() {
	check := flag.Bool("check", false, "run the boot preflight and exit (no infra required)")
	flag.Parse()
	log.SetPrefix("grounder: ")
	log.SetFlags(log.LstdFlags | log.LUTC)

	cfg := loadEnv()

	// Credential delivery (spec/022 REQ-2200/REQ-2204, TG-156/TG-157): make the control plane's own SecretRefs
	// resolvable as bao: references from OpenBao, before any secret resolves. Substrate OFF by default
	// (TG_OPENBAO_ADDR unset) ⇒ behaviour-preserving no-op; fail-closed when enabled but misconfigured.
	if err := openbao.WireDelivery(os.Getenv("TG_OPENBAO_ADDR"), os.Getenv("TG_OPENBAO_TOKEN_REF"), os.Getenv("TG_OPENBAO_CA"), log.Printf); err != nil {
		log.Fatalf("credential delivery: %v", err)
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
			log.Printf("credential preflight: no SSH key references configured — skipping (nothing to actuate/investigate over SSH)")
		} else if sshRep.Failed() {
			log.Fatalf("credential preflight FAILED (fail-closed): %s — provision the key readable by uid:gid %d:%d mode 0640 (see deploy/secrets/README.md)", sshRep.Summary(), os.Getuid(), os.Getgid())
		} else {
			log.Printf("credential preflight OK — %d SSH key ref(s) resolve+parse as uid:gid %d:%d: %s", sshRep.Configured(), os.Getuid(), os.Getgid(), strings.Join(sshRep.OK, ", "))
		}
		log.Printf("boot preflight OK — mutation_enabled=%v, Band(0)=%s (read-only foundation)", gate.MayActuate(), safety.Band(0))
		return
	}

	if err := preflight(cfg, gate); err != nil {
		log.Fatalf("boot preflight FAILED (fail-closed): %v", err)
	}

	ctx := context.Background()
	if err := db.Migrate(ctx, cfg.MigrationDSN); err != nil {
		log.Fatalf("migrations failed: %v", err)
	}
	pool, err := db.Connect(ctx, cfg.RuntimeDSN)
	if err != nil {
		log.Fatalf("db connect failed: %v", err)
	}
	defer pool.Close()

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

	// Optional Temporal client for the front door's triage trigger. Best-effort by design: the read-only API
	// (stats/ledger/whoami) must not depend on Temporal, so a dial failure logs and degrades ingest to
	// validate-only (accept + normalize, no session minted) rather than refusing to start.
	var triage httpapi.TriageStarter
	var votes httpapi.VoteSignaler
	var skillsWrite httpapi.SkillsWriter
	var configWrite httpapi.ConfigWriter
	var secretsWrite httpapi.SealedSecretWriter
	var modeTransition httpapi.ModeTransitioner
	if tport := os.Getenv("TG_TEMPORAL_HOSTPORT"); tport != "" {
		if tc, terr := client.Dial(client.Options{HostPort: tport}); terr != nil {
			log.Printf("temporal dial %s failed — ingest degraded to validate-only: %v", tport, terr)
		} else {
			defer tc.Close()
			triage = temporalTriage{c: tc}
			votes = temporalVotes{c: tc}
			skillsWrite = skillsWriteBackend{store: db.NewSkillStore(pool), tc: tc}
			// Config writes execute in the worker (single ledger writer, task #27 Phase C).
			configWrite = configWriteBackend{tc: tc}
			// Sealed-secret writes: seal HERE (the material never transits Temporal), commit in the
			// worker. Requires the master key reference to resolve — probed per write, fail closed.
			secretsWrite = secretsWriteBackend{tc: tc, sealer: sealer}
			// Autonomy-mode transitions (spec/015 REQ-1502) — the LAST gate before the mutation flip —
			// execute in the worker on the single chokepoint-bound ModeController (authority + preflight
			// gated, audited). Without a Temporal client the surface stays nil ⇒ POST /v1/mode is 503.
			modeTransition = modeTransitionBackend{tc: tc}
			log.Printf("alert front door: triage trigger wired to temporal at %s", tport)
		}
	}

	// The sealed-secret store (task #27 Phase D): the store: SecretRef scheme resolves against it
	// when the master key reference resolves; otherwise the scheme stays fail-closed-unwired.
	sealedStore := db.NewSealedSecretStore(pool)
	if sealer != nil {
		config.RegisterStoreResolver(func(name string) (string, error) {
			blob, found, gerr := sealedStore.Get(context.Background(), name)
			if gerr != nil {
				return "", gerr
			}
			if !found {
				return "", fmt.Errorf("sealed secret %q not found", name)
			}
			value, oerr := sealer.Open(name, blob) // DEK unwrap runs local or in OpenBao Transit per config
			if oerr != nil {
				return "", oerr
			}
			return string(value), nil
		})
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
	api := buildPublicAPI(verifier, gate, ledgerReadStore{s: db.NewLedgerStore(pool)}, registryIngesters{reg: moduleReg}, triage, moduleReg, sessions, sessionsReadStore{s: db.NewSessionReadStore(pool)}, db.NewAlertLogStore(pool), db.NewTransitionLogStore(pool), governanceReader{gate: gate, sessions: db.NewSessionReadStore(pool), ledger: db.NewLedgerStore(pool), posture: postureRead, staleAfter: postureStaleAfter}, configSecrets{cfg: cfg}, newLitellmModels(cfg.LiteLLMURL, cfg.LiteLLMKeyRef), contracts.OpenAPI, estateReadStore{s: db.NewEstateReadStore(pool)}, groundingReadStore{s: db.NewGroundingReadStore(pool)}, votes, db.NewPendingStore(pool), newConfigResolver(gate, cfg, cpStore), db.NewSkillStore(pool), skillsWrite, newFileWiki(cfg.KnowledgeFile, cfg.KnowledgeSeedFile), adminSessions, configWrite, secretsWrite, sealedReadStore{s: sealedStore}, credentialsReadStore{s: db.NewCredentialReadStore(pool)}, policyReadStore{s: db.NewPolicyReadStore(pool)}, regimeReadStore{s: db.NewRegimeReadStore(pool)}, modeTransition, postureRead, postureStaleAfter, sessionDetailReadStore{s: db.NewTraceSpineStore(pool)})

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
		return []metrics.Sample{
			{Name: "mutation_enabled", Kind: metrics.Gauge, Help: "mutation gate: 1 on / 0 off (read-only foundation holds 0)", Value: enabled, Labels: map[string]string{"component": "grounder"}},
			{Name: "tg_up", Kind: metrics.Gauge, Help: "process liveness marker (1 while serving)", Value: 1, Labels: map[string]string{"component": "grounder"}},
		}
	}))
	root.Handle("/", api.Mux())

	// Separate elevated admin listener (mTLS in deploy; AuthMTLS routes fail closed without a client cert).
	admin := auth.NewRouter(verifier)
	admin.Handle("/admin/status", auth.AuthMTLS, func(w http.ResponseWriter, r *http.Request, p auth.Principal) {
		fmt.Fprintf(w, `{"admin":true,"mutation_enabled":%v}`, gate.MayActuate())
	})

	log.Printf("read-only foundation up — public=%s admin=%s mutation_enabled=%v", cfg.PublicAddr, cfg.AdminAddr, gate.MayActuate())
	go func() { log.Printf("admin listener exited: %v", http.ListenAndServe(cfg.AdminAddr, admin.Mux())) }()
	log.Fatalf("public listener exited: %v", http.ListenAndServe(cfg.PublicAddr, root))
}
