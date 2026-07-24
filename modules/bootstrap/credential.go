package bootstrap

// Credential-engine composition root (spec/016 credential engine, wired LIVE in the worker). Until this
// runs the SyncEngine is fully built on main but never instantiated in the running binary — so no external
// system-of-record (OpenBao/Vault, AWX, Semaphore, LDAP/FreeIPA) is ever synced and the console has no
// credential coverage to show. This file reads the operator's per-source config (endpoints + SecretRef
// references, never literals — INV-13) and builds a *credential.SyncEngine:
//
//   - a native fallback resolver from an optional operator-declared rule spec (TG_CREDENTIAL_NATIVE_RULES);
//   - for EACH connector whose config is present, a read-only credential.CredentialSource registered at a
//     declared precedence (OpenBao > AWX > Semaphore on the machine plane; LDAP on the human plane).
//
// FAIL-CLOSED (matching the fleet's ethos): a source whose trigger config (its address / URL list) is ABSENT
// is simply not registered — no error, that capability is just off. A source whose trigger IS present but
// whose required credentials/auth are PARTIAL or invalid FAILS THE BOOT closed — a misconfigured credential
// source must never silently drop and let actuation resolve to the wrong (or a blank) identity.
//
// READ-ONLY: every source's Sync is a read-only pull and every emitted Bundle carries SecretRef references
// only. This wiring resolves identities; it never mutates the estate — Phase-1-safe, mutation stays OFF.
//
// Provenance: [O] INV-13/INV-05/INV-02, spec/016 (REQ-1607/1611/1612/1613/1614/1619).

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/territory-grounder/grounder/core/config"
	"github.com/territory-grounder/grounder/core/credential"
	"github.com/territory-grounder/grounder/modules/credsource/ansible"
	"github.com/territory-grounder/grounder/modules/credsource/awx"
	"github.com/territory-grounder/grounder/modules/credsource/ldap"
	nativesrc "github.com/territory-grounder/grounder/modules/credsource/native"
	"github.com/territory-grounder/grounder/modules/credsource/oidctoken"
	"github.com/territory-grounder/grounder/modules/credsource/openbao"
	"github.com/territory-grounder/grounder/modules/credsource/semaphore"
)

// The declared cross-source precedence (lower value = higher precedence; RegisterSource, REQ-1609). Distinct
// per source type so two machine-plane sources covering the same target disambiguate deterministically (a
// tie fails closed). The order is OpenBao > AWX > Semaphore; LDAP is the lone human-plane source.
const (
	precedenceOpenBao   = 10
	precedenceAWX       = 20
	precedenceSemaphore = 30
	// precedenceAnsible is the un-controlled Ansible file/vault source (machine plane) — a real
	// system-of-record ranked below the API-backed controllers (AWX/Semaphore) but above the native
	// hostdiag fallback, so a controller's binding shadows a stale on-disk inventory when both cover a host.
	precedenceAnsible = 35
	precedenceLDAP    = 40
	// precedenceNativeHostDiag is the LOWEST machine-plane precedence (highest value): the native hostdiag
	// allowlist store is the standalone fallback (design step 2 — "WHEN nothing is synced, fall back to the
	// native store"), so any real synced system-of-record (OpenBao/AWX/Semaphore) shadows it.
	precedenceNativeHostDiag = 100
)

// RegisteredCredentialSource is the non-secret summary of one source the bootstrap registered — the worker
// logs it at boot and it is what the oracle asserts on.
type RegisteredCredentialSource struct {
	ID         string
	Plane      credential.Plane
	Precedence int
}

// CredentialConfig is the operator-declared credential-engine config, read from env in cmd/worker/main.go
// (config-not-code). Every secret is a SecretRef reference (env:/file:/store:/vault:/bao:), never a literal.
// A source's TRIGGER field (OpenBao/AWX/Semaphore address, LDAP URL list) gates whether it is built at all.
type CredentialConfig struct {
	// NativeRules is an optional inline ParseRules spec (TG_CREDENTIAL_NATIVE_RULES) for the standalone
	// native fallback resolver Engine. Empty ⇒ no native Engine fallback (only synced sources / the native
	// hostdiag source resolve; still fail-closed).
	NativeRules string

	// HostDiagDeployments is the operator's read-only hostdiag SSH allowlist (TG_HOSTDIAG_DEPLOYMENTS) —
	// the SAME `site|hostglob|sshuser|keyref` spec the hostdiag investigation tools parse. Present ⇒ a
	// lowest-precedence native machine-plane CredentialSource is registered so the SSH investigation path
	// resolves per-host identity THROUGH the engine (fail-closed) instead of reading it off the allowlist.
	// Absent ⇒ no native hostdiag source (the capability is simply off).
	HostDiagDeployments string

	// OpenBao / HashiCorp Vault (machine plane, KV v2). Addr present ⇒ built; partial auth ⇒ boot fails closed.
	OpenBaoAddr         string // trigger (TG_OPENBAO_ADDR)
	OpenBaoSourceID     string // default "openbao"
	OpenBaoAuthMethod   string // token|approle|jwt (default token)
	OpenBaoTokenRef     string // token auth
	OpenBaoRoleIDRef    string // approle auth
	OpenBaoSecretIDRef  string // approle auth
	OpenBaoWrapTokenRef string // wrapped-approle auth: the single-use response-wrapping token (file: on tmpfs, no secret-zero)
	OpenBaoJWTRef       string // jwt auth
	OpenBaoJWTRole      string // jwt auth
	OpenBaoCACertPath   string // optional private-CA PEM path
	OpenBaoKVMount      string // KV v2 mount (default "secret")
	OpenBaoKVPrefix     string // KV v2 path prefix under the mount

	// AWX (machine plane, inventory). Addr present ⇒ built; missing token ref ⇒ boot fails closed.
	AWXAddr        string // trigger (TG_AWX_ADDR)
	AWXSourceID    string // default "awx"
	AWXTokenRef    string // required Bearer OAuth2 token SecretRef
	AWXCACertPath  string // optional private-CA PEM path
	AWXInventoryID string // optional int; scope to one inventory
	AWXRefScheme   string // emitted SecretRef scheme (default "store")
	AWXRefPrefix   string
	AWXRefField    string
	AWXCredRefMap  string // job-template mode: ';'-separated "AWX cred name=SecretRef" pairs (TG_AWX_CRED_REF_MAP)
	AWXDefaultUser string // job-template mode: login user when the AWX credential carries no username (default "root")

	// Semaphore (machine plane, inventory). Addr present ⇒ built; missing token ref ⇒ boot fails closed.
	SemaphoreAddr       string // trigger (TG_SEMAPHORE_ADDR)
	SemaphoreSourceID   string // default "semaphore"
	SemaphoreTokenRef   string // required Bearer API token SecretRef
	SemaphoreCACertPath string
	SemaphoreProjectID  string // optional int; scope to one project
	SemaphoreRefScheme  string
	SemaphoreRefPrefix  string
	SemaphoreRefField   string

	// UN-CONTROLLED Ansible (machine plane, parsed inventory + ansible-vault). Root present ⇒ built; a
	// present root with no vault-password ref ⇒ boot fails closed (the ansible-vault: scheme could never
	// decrypt). This is the plain-files case (NO AWX/Semaphore controller): TG parses inventory +
	// group_vars/host_vars and natively decrypts inline !vault secrets at use time. Every credential is a
	// SecretRef reference (the ssh key a file:, the sudo/become secret an ansible-vault: locator), never a
	// literal (INV-13).
	AnsibleRoot          string // trigger (TG_ANSIBLE_ROOT): the un-controlled Ansible tree directory
	AnsibleSourceID      string // default "ansible"
	AnsibleInventoryPath string // optional; pins the inventory file (else the conventional names are searched)
	AnsibleVaultPassRef  string // REQUIRED when Root is set: the vault password SecretRef (file:/store:/env:)
	AnsibleDefaultUser   string // login user when a host declares no ansible_user (default "root")

	// LDAP / FreeIPA (human plane, approvers). URL list present ⇒ built; missing bind refs ⇒ boot fails closed.
	LDAPURLs      string // trigger (TG_LDAP_URLS); comma/space-separated replica list
	LDAPSourceID  string // default "ldap"
	LDAPBindDNRef string // required service bind DN SecretRef
	LDAPBindPWRef string // required service bind password SecretRef
	LDAPCACertRef string // optional CA PEM SecretRef (file:/store:)
	LDAPStartTLS  string // bool; upgrade a plain ldap:// with StartTLS before binding
	LDAPUserBase  string // site user base DN (TG_LDAP_USER_BASE); empty ⇒ the generic connector default
	LDAPGroupBase string // site group base DN (TG_LDAP_GROUP_BASE); empty ⇒ the generic connector default

	// OIDC client-credentials token minter (machine plane, oidc: SecretRef scheme, REQ-1619). Token URL
	// present ⇒ the oidc: resolver is wired; absent client refs ⇒ boot fails closed. Resolver-ONLY: it mints a
	// short-lived Bearer token at use time and registers no Sync source (unlike the KV v2 / inventory sources).
	OIDCTokenURL        string // trigger (TG_OIDC_TOKEN_URL); the provider's client-credentials token endpoint
	OIDCClientIDRef     string // required client_id SecretRef
	OIDCClientSecretRef string // required client_secret SecretRef
	OIDCScope           string // optional default space-separated scope requested at mint
	OIDCAudience        string // optional audience parameter
	OIDCCACertPath      string // optional private-CA PEM path (verified TLS)
	OIDCAuthStyle       string // client auth: post (default, client_secret_post) | basic (client_secret_basic)
}

// BuildSyncEngine constructs the SyncEngine from config: the native fallback (if any) plus every configured
// source, each registered at its declared precedence. It returns the engine and the non-secret summary of
// what it registered (for the boot log + oracles). It FAILS CLOSED on any partial/invalid source config —
// an absent source is silently skipped, a half-configured one aborts the boot.
func BuildSyncEngine(cfg CredentialConfig) (*credential.SyncEngine, []RegisteredCredentialSource, error) {
	var native *credential.Engine
	if spec := strings.TrimSpace(cfg.NativeRules); spec != "" {
		rules, err := credential.ParseRules(spec)
		if err != nil {
			return nil, nil, fmt.Errorf("credential bootstrap: native rules: %w", err)
		}
		native = credential.NewEngine(rules)
	}
	se := credential.NewSyncEngine(native)

	var registered []RegisteredCredentialSource
	register := func(src credential.CredentialSource, precedence int) error {
		if err := se.RegisterSource(src, precedence); err != nil {
			return err
		}
		registered = append(registered, RegisteredCredentialSource{ID: src.ID(), Plane: src.Plane(), Precedence: precedence})
		return nil
	}

	if strings.TrimSpace(cfg.OpenBaoAddr) != "" {
		src, err := buildOpenBaoSource(cfg)
		if err != nil {
			return nil, nil, err
		}
		if err := register(src, precedenceOpenBao); err != nil {
			return nil, nil, fmt.Errorf("credential bootstrap: register openbao: %w", err)
		}
	}
	if strings.TrimSpace(cfg.AWXAddr) != "" {
		src, err := buildAWXSource(cfg)
		if err != nil {
			return nil, nil, err
		}
		if err := register(src, precedenceAWX); err != nil {
			return nil, nil, fmt.Errorf("credential bootstrap: register awx: %w", err)
		}
	}
	if strings.TrimSpace(cfg.SemaphoreAddr) != "" {
		src, err := buildSemaphoreSource(cfg)
		if err != nil {
			return nil, nil, err
		}
		if err := register(src, precedenceSemaphore); err != nil {
			return nil, nil, fmt.Errorf("credential bootstrap: register semaphore: %w", err)
		}
	}
	if strings.TrimSpace(cfg.AnsibleRoot) != "" {
		src, err := buildAnsibleSource(cfg)
		if err != nil {
			return nil, nil, err
		}
		if err := register(src, precedenceAnsible); err != nil {
			return nil, nil, fmt.Errorf("credential bootstrap: register ansible: %w", err)
		}
	}
	if strings.TrimSpace(cfg.LDAPURLs) != "" {
		src, err := buildLDAPSource(cfg)
		if err != nil {
			return nil, nil, err
		}
		if err := register(src, precedenceLDAP); err != nil {
			return nil, nil, fmt.Errorf("credential bootstrap: register ldap: %w", err)
		}
	}
	// OIDC client-credentials token minter (machine plane, oidc: scheme, REQ-1619). Resolver-ONLY: it
	// registers no CredentialSource; it makes oidc: SecretRefs MINT a short-lived Bearer token at use time
	// (read-only, fail-closed, verified TLS). A present token URL with an absent client ref fails the boot.
	if strings.TrimSpace(cfg.OIDCTokenURL) != "" {
		if err := wireOIDCTokenMinter(cfg); err != nil {
			return nil, nil, err
		}
	}
	// The native hostdiag allowlist source (machine plane, lowest precedence) — the standalone fallback that
	// makes the read-only SSH investigation path resolve identity through the engine. NewFromSpec reuses
	// hostdiag.ParseAccess (ONE grammar); a spec with no well-formed rows yields no source (nil), so an absent
	// allowlist simply registers nothing (fail-closed: an uncovered host is unresolvable).
	if strings.TrimSpace(cfg.HostDiagDeployments) != "" {
		src, err := nativesrc.NewFromSpec(nativesrc.DefaultID, cfg.HostDiagDeployments)
		if err != nil {
			return nil, nil, fmt.Errorf("credential bootstrap: native hostdiag source: %w", err)
		}
		if src != nil {
			if err := register(src, precedenceNativeHostDiag); err != nil {
				return nil, nil, fmt.Errorf("credential bootstrap: register native hostdiag: %w", err)
			}
		}
	}
	return se, registered, nil
}

// buildOpenBaoSource builds the OpenBao/Vault KV v2 machine-plane source and registers its bao: SecretRef
// scheme so a synced Bundle's bao: reference dereferences at use time (read-only, fail-closed). Partial auth
// (a method whose required SecretRefs are absent) fails closed.
func buildOpenBaoSource(cfg CredentialConfig) (credential.CredentialSource, error) {
	boCfg := openbao.Config{
		BaseURL:    strings.TrimSpace(cfg.OpenBaoAddr),
		CACertPath: strings.TrimSpace(cfg.OpenBaoCACertPath),
	}
	method := strings.ToLower(strings.TrimSpace(cfg.OpenBaoAuthMethod))
	if method == "" {
		method = "token"
	}
	switch method {
	case "token":
		ref := strings.TrimSpace(cfg.OpenBaoTokenRef)
		if ref == "" {
			return nil, fmt.Errorf("credential bootstrap: openbao auth=token requires TG_OPENBAO_TOKEN_REF (a SecretRef)")
		}
		boCfg.Auth = openbao.Token{TokenRef: config.SecretRef(ref)}
	case "approle":
		role, secret := strings.TrimSpace(cfg.OpenBaoRoleIDRef), strings.TrimSpace(cfg.OpenBaoSecretIDRef)
		if role == "" || secret == "" {
			return nil, fmt.Errorf("credential bootstrap: openbao auth=approle requires TG_OPENBAO_ROLE_ID_REF and TG_OPENBAO_SECRET_ID_REF (SecretRefs)")
		}
		boCfg.Auth = openbao.AppRole{RoleIDRef: config.SecretRef(role), SecretIDRef: config.SecretRef(secret)}
	case "wrapped-approle":
		// Secret-zero-free bootstrap (spec/024 REQ-2407): the role_id is a non-secret env ref; the SecretID
		// arrives only as a single-use response-wrapping token on tmpfs (no durable secret on disk).
		role, wrap := strings.TrimSpace(cfg.OpenBaoRoleIDRef), strings.TrimSpace(cfg.OpenBaoWrapTokenRef)
		if role == "" || wrap == "" {
			return nil, fmt.Errorf("credential bootstrap: openbao auth=wrapped-approle requires TG_OPENBAO_ROLE_ID_REF and TG_OPENBAO_WRAP_TOKEN_REF (SecretRefs)")
		}
		boCfg.Auth = openbao.WrappedAppRole{RoleIDRef: config.SecretRef(role), WrapTokenRef: config.SecretRef(wrap)}
	case "jwt":
		jwt, role := strings.TrimSpace(cfg.OpenBaoJWTRef), strings.TrimSpace(cfg.OpenBaoJWTRole)
		if jwt == "" || role == "" {
			return nil, fmt.Errorf("credential bootstrap: openbao auth=jwt requires TG_OPENBAO_JWT_REF (a SecretRef) and TG_OPENBAO_JWT_ROLE")
		}
		boCfg.Auth = openbao.JWT{JWTRef: config.SecretRef(jwt), Role: role}
	default:
		return nil, fmt.Errorf("credential bootstrap: openbao auth method %q unknown (use token|approle|wrapped-approle|jwt)", method)
	}
	client, err := openbao.New(boCfg)
	if err != nil {
		return nil, fmt.Errorf("credential bootstrap: openbao client: %w", err)
	}
	// Make bao: SecretRefs resolvable at use time (read-only, fail-closed). Composition-time side effect.
	openbao.RegisterResolver(client)
	src, err := openbao.NewSource(openbao.SourceConfig{
		ID:     orDefaultStr(cfg.OpenBaoSourceID, "openbao"),
		Client: client,
		Mount:  orDefaultStr(cfg.OpenBaoKVMount, "secret"),
		Prefix: strings.TrimSpace(cfg.OpenBaoKVPrefix),
	})
	if err != nil {
		return nil, fmt.Errorf("credential bootstrap: openbao source: %w", err)
	}
	return src, nil
}

// buildAWXSource builds the AWX inventory machine-plane source. A present address with no token ref fails closed.
func buildAWXSource(cfg CredentialConfig) (credential.CredentialSource, error) {
	tokenRef := strings.TrimSpace(cfg.AWXTokenRef)
	if tokenRef == "" {
		return nil, fmt.Errorf("credential bootstrap: awx requires TG_AWX_TOKEN_REF (a SecretRef) when TG_AWX_ADDR is set")
	}
	client, err := awx.New(awx.Config{
		BaseURL:    strings.TrimSpace(cfg.AWXAddr),
		TokenRef:   config.SecretRef(tokenRef),
		CACertPath: strings.TrimSpace(cfg.AWXCACertPath),
	})
	if err != nil {
		return nil, fmt.Errorf("credential bootstrap: awx client: %w", err)
	}
	invID, err := optionalInt(cfg.AWXInventoryID)
	if err != nil {
		return nil, fmt.Errorf("credential bootstrap: awx inventory id: %w", err)
	}
	credRefMap, err := parseAWXCredRefMap(cfg.AWXCredRefMap)
	if err != nil {
		return nil, fmt.Errorf("credential bootstrap: awx cred ref map: %w", err)
	}
	src, err := awx.NewSource(awx.SourceConfig{
		ID:          orDefaultStr(cfg.AWXSourceID, "awx"),
		Client:      client,
		InventoryID: invID,
		RefScheme:   strings.TrimSpace(cfg.AWXRefScheme),
		RefPrefix:   cfg.AWXRefPrefix,
		RefField:    strings.TrimSpace(cfg.AWXRefField),
		CredRefMap:  credRefMap,
		DefaultUser: strings.TrimSpace(cfg.AWXDefaultUser),
	})
	if err != nil {
		return nil, fmt.Errorf("credential bootstrap: awx source: %w", err)
	}
	return src, nil
}

// buildSemaphoreSource builds the Semaphore inventory machine-plane source. A present address with no token
// ref fails closed.
func buildSemaphoreSource(cfg CredentialConfig) (credential.CredentialSource, error) {
	tokenRef := strings.TrimSpace(cfg.SemaphoreTokenRef)
	if tokenRef == "" {
		return nil, fmt.Errorf("credential bootstrap: semaphore requires TG_SEMAPHORE_TOKEN_REF (a SecretRef) when TG_SEMAPHORE_ADDR is set")
	}
	client, err := semaphore.New(semaphore.Config{
		BaseURL:    strings.TrimSpace(cfg.SemaphoreAddr),
		TokenRef:   config.SecretRef(tokenRef),
		CACertPath: strings.TrimSpace(cfg.SemaphoreCACertPath),
	})
	if err != nil {
		return nil, fmt.Errorf("credential bootstrap: semaphore client: %w", err)
	}
	projID, err := optionalInt(cfg.SemaphoreProjectID)
	if err != nil {
		return nil, fmt.Errorf("credential bootstrap: semaphore project id: %w", err)
	}
	src, err := semaphore.NewSource(semaphore.SourceConfig{
		ID:        orDefaultStr(cfg.SemaphoreSourceID, "semaphore"),
		Client:    client,
		ProjectID: projID,
		RefScheme: strings.TrimSpace(cfg.SemaphoreRefScheme),
		RefPrefix: cfg.SemaphoreRefPrefix,
		RefField:  strings.TrimSpace(cfg.SemaphoreRefField),
	})
	if err != nil {
		return nil, fmt.Errorf("credential bootstrap: semaphore source: %w", err)
	}
	return src, nil
}

// buildAnsibleSource builds the un-controlled Ansible file/vault machine-plane source and registers its
// ansible-vault: SecretRef scheme so a synced Bundle's ansible-vault: reference natively decrypts at use time
// (read-only, fail-closed). A present root with no vault-password ref fails closed. The Source and the
// resolver share ONE Tree over the same directory (both re-read on demand — INV-05).
func buildAnsibleSource(cfg CredentialConfig) (credential.CredentialSource, error) {
	passRef := strings.TrimSpace(cfg.AnsibleVaultPassRef)
	if passRef == "" {
		return nil, fmt.Errorf("credential bootstrap: ansible requires TG_ANSIBLE_VAULT_PASS_REF (a SecretRef) when TG_ANSIBLE_ROOT is set")
	}
	tree, err := ansible.NewTree(strings.TrimSpace(cfg.AnsibleRoot), strings.TrimSpace(cfg.AnsibleInventoryPath))
	if err != nil {
		return nil, fmt.Errorf("credential bootstrap: ansible tree: %w", err)
	}
	resolver, err := ansible.NewResolver(ansible.ResolverConfig{Tree: tree, PasswordRef: config.SecretRef(passRef)})
	if err != nil {
		return nil, fmt.Errorf("credential bootstrap: ansible resolver: %w", err)
	}
	// Make ansible-vault: SecretRefs resolvable (and natively decryptable) at use time. Composition-time side effect.
	ansible.RegisterResolver(resolver)
	src, err := ansible.NewSource(ansible.SourceConfig{
		ID:          orDefaultStr(cfg.AnsibleSourceID, "ansible"),
		Tree:        tree,
		DefaultUser: strings.TrimSpace(cfg.AnsibleDefaultUser),
	})
	if err != nil {
		return nil, fmt.Errorf("credential bootstrap: ansible source: %w", err)
	}
	return src, nil
}

// buildLDAPSource builds the LDAP/FreeIPA human-plane approver source. ldap.New already fails closed on
// missing bind DN / bind password SecretRefs, so a present URL list with absent bind refs aborts the boot.
func buildLDAPSource(cfg CredentialConfig) (credential.CredentialSource, error) {
	src, err := ldap.New(ldap.Config{
		ID:              orDefaultStr(cfg.LDAPSourceID, "ldap"),
		URLs:            splitList(cfg.LDAPURLs),
		StartTLS:        truthy(cfg.LDAPStartTLS),
		CACertRef:       config.SecretRef(strings.TrimSpace(cfg.LDAPCACertRef)),
		BindDNRef:       config.SecretRef(strings.TrimSpace(cfg.LDAPBindDNRef)),
		BindPasswordRef: config.SecretRef(strings.TrimSpace(cfg.LDAPBindPWRef)),
		UserBaseDN:      strings.TrimSpace(cfg.LDAPUserBase), // site DN suffix, deploy-time (never compiled in)
		GroupBaseDN:     strings.TrimSpace(cfg.LDAPGroupBase),
	})
	if err != nil {
		return nil, fmt.Errorf("credential bootstrap: ldap source: %w", err)
	}
	return src, nil
}

// wireOIDCTokenMinter builds the machine-plane OIDC client-credentials token minter and registers its oidc:
// SecretRef scheme so an oidc: reference mints a short-lived Bearer token at use time (REQ-1619) — read-only,
// fail-closed, over verified TLS. A present token URL with a missing client_id/client_secret ref fails the
// boot closed. Composition-time side effect: no Sync source is registered (the minter has no inventory).
func wireOIDCTokenMinter(cfg CredentialConfig) error {
	idRef := strings.TrimSpace(cfg.OIDCClientIDRef)
	secRef := strings.TrimSpace(cfg.OIDCClientSecretRef)
	if idRef == "" || secRef == "" {
		return fmt.Errorf("credential bootstrap: oidc requires TG_OIDC_CLIENT_ID_REF and TG_OIDC_CLIENT_SECRET_REF (SecretRefs) when TG_OIDC_TOKEN_URL is set")
	}
	var style oidctoken.AuthStyle
	switch strings.ToLower(strings.TrimSpace(cfg.OIDCAuthStyle)) {
	case "", "post", "client_secret_post":
		style = oidctoken.AuthStylePost
	case "basic", "client_secret_basic":
		style = oidctoken.AuthStyleBasic
	default:
		return fmt.Errorf("credential bootstrap: oidc auth style %q unknown (use post|basic)", cfg.OIDCAuthStyle)
	}
	m, err := oidctoken.New(oidctoken.Config{
		TokenURL:        strings.TrimSpace(cfg.OIDCTokenURL),
		ClientIDRef:     config.SecretRef(idRef),
		ClientSecretRef: config.SecretRef(secRef),
		Scope:           strings.TrimSpace(cfg.OIDCScope),
		Audience:        strings.TrimSpace(cfg.OIDCAudience),
		AuthStyle:       style,
		CACertPath:      strings.TrimSpace(cfg.OIDCCACertPath),
	})
	if err != nil {
		return fmt.Errorf("credential bootstrap: oidc minter: %w", err)
	}
	oidctoken.RegisterResolver(m)
	return nil
}

// ---- small helpers ----------------------------------------------------------------------------------

func orDefaultStr(v, def string) string {
	if strings.TrimSpace(v) == "" {
		return def
	}
	return strings.TrimSpace(v)
}

// optionalInt parses an optional positive int id. Empty ⇒ 0 (unscoped). A non-integer is an error (fail closed).
func optionalInt(v string) (int, error) {
	s := strings.TrimSpace(v)
	if s == "" {
		return 0, nil
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return 0, fmt.Errorf("invalid integer %q", s)
	}
	return n, nil
}

// parseAWXCredRefMap parses TG_AWX_CRED_REF_MAP — ';'-separated "AWX cred name=SecretRef" pairs (e.g.
// "SSH ED25519 (one_key)=file:/secrets/one_key;SSH Lab Common=file:/secrets/lab-common") — into the
// name→SecretRef map that drives the AWX job-template bundle mode. Empty ⇒ nil (JT mode disabled). A pair
// with no '=', a blank name, or a blank ref fails closed (a malformed map must not silently drop a binding).
// The SecretRef value is NOT validated here (NewBundle enforces the sealed-scheme rule at bundle build); a
// non-sealed ref surfaces as a skip-with-record, never a blank identity.
func parseAWXCredRefMap(s string) (map[string]string, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, nil
	}
	out := map[string]string{}
	for _, pair := range strings.Split(s, ";") {
		pair = strings.TrimSpace(pair)
		if pair == "" {
			continue
		}
		i := strings.IndexByte(pair, '=')
		if i < 0 {
			return nil, fmt.Errorf("entry %q is not a NAME=SecretRef pair", pair)
		}
		name := strings.TrimSpace(pair[:i])
		ref := strings.TrimSpace(pair[i+1:])
		if name == "" || ref == "" {
			return nil, fmt.Errorf("entry %q has a blank name or ref", pair)
		}
		out[name] = ref
	}
	if len(out) == 0 {
		return nil, nil
	}
	return out, nil
}

// splitList splits a comma/whitespace-separated list into its non-empty tokens.
func splitList(csv string) []string {
	var out []string
	for _, t := range strings.FieldsFunc(csv, func(r rune) bool { return r == ',' || r == ' ' || r == '\t' || r == '\n' }) {
		if t != "" {
			out = append(out, t)
		}
	}
	return out
}

func truthy(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}
