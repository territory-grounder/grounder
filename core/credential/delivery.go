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
	tr := strings.TrimSpace(string(c.TokenRef))
	if tr == "" {
		return fmt.Errorf("credential delivery: substrate address is set but TG_OPENBAO_TOKEN_REF is empty (fail closed — refusing to enable the substrate without a bootstrap token)")
	}
	scheme, _, ok := strings.Cut(tr, ":")
	if !ok || (scheme != "env" && scheme != "file") {
		return fmt.Errorf("credential delivery: bootstrap token reference must use env: or file: (got %q) — the substrate credential cannot itself come from the substrate", tr)
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
