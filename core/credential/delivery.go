// Delivery wires the PROCESS's own secrets to an external secret substrate (OpenBao KV v2) so that every
// config.SecretRef the process resolves — the model-gateway key, operator/admin tokens, the seal master key,
// ingest tokens, per-target credentials — can be a `bao:` reference resolved at runtime instead of a plaintext
// value handed to the container through the environment. This is the boot half of spec/022 REQ-2200/REQ-2204
// (env→OpenBao, fail-closed): the resolution mechanism already exists (config.SecretRef.Resolve dispatches the
// bao: scheme to a registered resolver — proven for per-target bundles in spec/016 REQ-1613); what was missing
// is registering that resolver for the PROCESS at composition, and doing so under a fail-closed policy.
//
// Layering: core/ must not import modules/, so the OpenBao CLIENT is built by the composition root (cmd/worker,
// cmd/grounder) and its resolver is INJECTED here. DeliveryConfig owns the policy (when the substrate is on, the
// bootstrap-token invariant, the fail-closed decisions) which is unit-testable without a live substrate.
//
// Fail-closed contract (REQ-2204): a `bao:` reference NEVER degrades to a plaintext or empty fallback. When the
// substrate is OFF, bao: references fail closed in config.SecretRef.Resolve (unregistered scheme → error); when
// it is ON but the resolver errors (substrate unreachable, path/field absent, permission denied), that error
// propagates — the operation refuses. env:/file: references are unaffected either way (behaviour-preserving for
// a deployment that has not yet moved any secret to the substrate).
//
// Provenance: [O] INV-13 (secrets are references), spec/022 (REQ-2200, REQ-2204), TG-156/TG-157.
package credential

import (
	"fmt"
	"strings"

	"github.com/territory-grounder/grounder/core/config"
)

// DeliveryConfig declares how the process reaches its external secret substrate. It is parsed from the
// environment by the composition root (TG_OPENBAO_ADDR, TG_OPENBAO_TOKEN_REF, TG_OPENBAO_CACERT). An empty
// Addr means the substrate is OFF (the default): the process keeps resolving env:/file: references exactly as
// before, and any bao: reference fails closed.
type DeliveryConfig struct {
	// Addr is the substrate base URL (e.g. https://openbao.example:8200). Empty ⇒ substrate OFF.
	Addr string
	// TokenRef is the ONE bootstrap secret that authenticates the process to the substrate. It must itself be
	// an env: or file: reference — the substrate cannot authenticate itself from the substrate.
	TokenRef config.SecretRef
	// CACert is an optional filesystem path to the substrate's private-CA certificate.
	CACert string
	// CertPath / KeyPath configure the mTLS MACHINE-IDENTITY bootstrap (spec/024 Amendment 2026-07-25,
	// REQ-2407): a FreeIPA-CA-signed client certificate (CertPath, public) + its private key (KeyPath, a
	// root-only file). When both are set the process authenticates to the substrate by PRESENTING this identity
	// (an identity it IS — CA-anchored, revocable), so NO bootstrap token is required or stored. CertRole names
	// the substrate's cert auth role (optional). This is the highest-assurance bootstrap available without
	// platform/hardware attestation and is preferred over the token where a site CA exists.
	CertPath string
	KeyPath  string
	CertRole string
	// RoleIDRef / SecretIDRef configure the APPROLE bootstrap. Like the mTLS pair they satisfy the
	// "not from the substrate" invariant: both are operator-delivered files, and neither can be fetched
	// from the store they authenticate to.
	//
	// ★ WHY THEY ARE HERE (TG-153). modules/bootstrap/credential.go:316 has implemented approle since it
	// was written, but THIS validator only ever recognised mTLS or a token — so a process configured with
	// TG_OPENBAO_AUTH_METHOD=approle and both refs was rejected at boot with "no bootstrap credential
	// (neither an mTLS cert/key nor TG_OPENBAO_TOKEN_REF)". Two halves of the same feature disagreeing
	// about what a valid bootstrap is.
	//
	// It surfaced deploying the TG-153 credential-plane split, and it BLOCKED it: the split's whole
	// mechanism is two workers holding two DISTINCT OpenBao identities, and approle is how you give one
	// host two identities. mTLS cannot — both containers present the same host certificate.
	RoleIDRef   config.SecretRef
	SecretIDRef config.SecretRef
}

// approleBootstrap reports whether an AppRole bootstrap is configured. BOTH refs are required: a lone
// role-id (a public identifier) authenticates nothing, and a lone secret-id has no role to present it for.
// Half-configured is a misconfiguration, and it fails closed below rather than silently degrading.
func (c DeliveryConfig) approleBootstrap() bool {
	return strings.TrimSpace(string(c.RoleIDRef)) != "" && strings.TrimSpace(string(c.SecretIDRef)) != ""
}

// certBootstrap reports whether an mTLS machine-identity bootstrap is configured (both cert and key paths set).
func (c DeliveryConfig) certBootstrap() bool {
	return strings.TrimSpace(c.CertPath) != "" && strings.TrimSpace(c.KeyPath) != ""
}

// Enabled reports whether an external substrate is configured.
func (c DeliveryConfig) Enabled() bool { return strings.TrimSpace(c.Addr) != "" }

// Validate enforces the fail-closed configuration invariants for an ENABLED substrate. A disabled substrate is
// always valid (it is the behaviour-preserving default). An enabled substrate REQUIRES a bootstrap token, and
// that token must be an env:/file: reference (never bao:/store:/vault:) so the substrate is not asked to
// bootstrap its own credential.
func (c DeliveryConfig) Validate() error {
	if !c.Enabled() {
		return nil
	}
	// mTLS machine-identity bootstrap (spec/024): a FreeIPA-CA cert+key satisfies the "not from the substrate"
	// invariant WITHOUT a token — the credential is an identity the host IS, not a secret the substrate issued.
	// It requires BOTH paths (a lone cert or lone key is a misconfiguration, fail closed). A token, if also set,
	// is an unused fallback. This branch takes precedence: cert bootstrap is the preferred, higher-assurance path.
	if c.certBootstrap() {
		return nil
	}
	if strings.TrimSpace(c.CertPath) != "" || strings.TrimSpace(c.KeyPath) != "" {
		return fmt.Errorf("credential delivery: mTLS cert bootstrap requires BOTH a client cert and a key path (fail closed)")
	}
	// AppRole bootstrap (TG-153). Ranked below mTLS — a certificate is an identity the host IS, an AppRole
	// secret-id is a secret it HOLDS — but it is the only bootstrap that gives ONE host TWO identities,
	// which is precisely what the credential-plane split needs.
	if c.approleBootstrap() {
		return nil
	}
	if strings.TrimSpace(string(c.RoleIDRef)) != "" || strings.TrimSpace(string(c.SecretIDRef)) != "" {
		return fmt.Errorf("credential delivery: approle bootstrap requires BOTH TG_OPENBAO_ROLE_ID_REF and " +
			"TG_OPENBAO_SECRET_ID_REF (fail closed) — a lone role-id authenticates nothing and a lone " +
			"secret-id has no role to present it for")
	}
	tr := strings.TrimSpace(string(c.TokenRef))
	if tr == "" {
		return fmt.Errorf("credential delivery: substrate address is set but no bootstrap credential (no mTLS cert/key, no approle role-id/secret-id, no TG_OPENBAO_TOKEN_REF) — fail closed")
	}
	scheme, _, ok := strings.Cut(tr, ":")
	if !ok || (scheme != "env" && scheme != "file") {
		// Name the SCHEME only — never the ref value: a misconfigured bare-literal token (the actual
		// OpenBao token) would otherwise leak into this error and any log that records it (INV-13: the
		// bootstrap token is a reference, never an inline literal).
		got := "a bare value with no scheme prefix"
		if ok && scheme != "" {
			got = fmt.Sprintf("scheme %q", scheme)
		}
		return fmt.Errorf("credential delivery: bootstrap token reference must use env: or file: (got %s) — the substrate credential cannot itself come from the substrate", got)
	}
	return nil
}

// SchemeResolverFunc resolves a full SecretRef (scheme included), matching config.RegisterSchemeResolver's
// contract. The composition root passes the OpenBao client's ResolveRef here.
type SchemeResolverFunc = func(ref string) (string, error)

// Register validates the configuration and, when the substrate is enabled, wires the injected resolver for
// scheme (e.g. "bao") into core/config so that process SecretRefs using that scheme resolve at runtime. It is
// fail-closed: an invalid config, or an enabled substrate with no injected resolver, returns an error (the
// caller must refuse to start). A disabled substrate is a logged no-op — bao: references then fail closed in
// SecretRef.Resolve and env:/file: references keep working. Call once at composition; safe for concurrent
// Resolve afterwards. logf may be nil.
func (c DeliveryConfig) Register(scheme string, resolve SchemeResolverFunc, logf func(string, ...any)) error {
	if logf == nil {
		logf = func(string, ...any) {}
	}
	if !c.Enabled() {
		logf("credential delivery: external secret substrate OFF (address unset); %q references fail closed, env:/file: references unaffected", scheme)
		return nil
	}
	if err := c.Validate(); err != nil {
		return err
	}
	if resolve == nil {
		return fmt.Errorf("credential delivery: substrate enabled (%s) but no %q resolver was injected (fail closed)", c.Addr, scheme)
	}
	config.RegisterSchemeResolver(scheme, resolve)
	logf("credential delivery: external secret substrate ON (%s); %q references now resolve from the external key store", c.Addr, scheme)
	return nil
}
