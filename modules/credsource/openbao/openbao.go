// Package openbao is the OpenBao credential connector — the bao: SecretRef scheme (spec/016 T-016-9,
// REQ-1613, TG-90). OpenBao is API-compatible with HashiCorp Vault (same KV v2 read/list and AppRole/JWT/
// token auth endpoints), so this package is a thin wrapper over modules/credsource/vault: it reuses the
// exact same native-Go, distroless, read-only, fail-closed client and CredentialSource, and only differs in
// the SecretRef scheme it registers (bao: instead of vault:).
//
// The estate's OpenBao is live at https://dc1k8s-openbao01.example.net:8200 (GET /v1/sys/health
// → 200) behind a private CA; TG's worker (not a GitLab CI job) authenticates via AppRole or a static token.
//
// Provenance: [O] INV-13/INV-05/INV-02, spec/016 (REQ-1613), TG-90.
package openbao

import (
	"fmt"
	"net/http"
	"strings"

	"github.com/territory-grounder/grounder/core/config"
	"github.com/territory-grounder/grounder/core/credential"
	"github.com/territory-grounder/grounder/modules/credsource/vault"
)

// SourceType is the vendor slug this connector serves.
const SourceType = "openbao"

// Scheme is the SecretRef scheme this connector resolves.
const Scheme = "bao"

// Re-export the shared client + auth types so callers configure OpenBao without importing the vault package
// directly. OpenBao and Vault are wire-identical here (KV v2 + AppRole/JWT/token).
type (
	// Client is the shared OpenBao/Vault KV v2 client.
	Client = vault.Client
	// Config constructs a Client.
	Config = vault.Config
	// AppRole is role_id + secret_id auth.
	AppRole = vault.AppRole
	// WrappedAppRole is the secret-zero-free AppRole variant (spec/024 REQ-2407): a response-wrapped,
	// single-use SecretID delivered as a wrapping token on tmpfs, unwrapped at boot — no durable secret on disk.
	WrappedAppRole = vault.WrappedAppRole
	// JWT is jwt + role auth.
	JWT = vault.JWT
	// Token is static-token auth.
	Token = vault.Token
	// Cert is TLS client-certificate (mTLS) auth — the FreeIPA machine-identity path (spec/024 Amendment
	// 2026-07-25): the host presents a CA-signed identity cert and needs NO bootstrap token on disk.
	Cert = vault.Cert
	// Source is the KV v2 CredentialSource.
	Source = vault.Source
	// SourceConfig configures a Source.
	SourceConfig = vault.SourceConfig
)

// New builds an OpenBao client (identical to vault.New — the wire protocol is the same).
func New(cfg Config) (*Client, error) { return vault.New(cfg) }

// WireOption tunes how the delivery client is built. Variadic on purpose (TG-415): this threads through
// three security-path functions with existing callers in two composition roots, and a variadic option
// leaves every one of them compiling unchanged rather than forcing a mechanical edit across a path where
// a mechanical edit is exactly what nobody reviews carefully.
type WireOption func(*Config)

// WithTransportWrap decorates the transport the delivery client builds for itself.
//
// It exists because that transport was INVISIBLE TO THE EGRESS METER. vault.New must build its own
// http.Transport to carry the CA / mTLS config, and a client with its own Transport never touches
// http.DefaultTransport — which is where the TG-160 meter installs. Measured on the grounder 2026-08-07:
// tg_egress_enforcing 1, allowlist_rules 15, requests_total 0, in the same second four bao: refs resolved.
//
// The composition root passes its meter's Wrap here. The module stays free of core/egress.
func WithTransportWrap(f func(http.RoundTripper) http.RoundTripper) WireOption {
	return func(c *Config) { c.TransportWrap = f }
}

// applyWire folds the options over a Config the Wire* functions have already populated.
func applyWire(c Config, opts []WireOption) Config {
	for _, o := range opts {
		if o != nil {
			o(&c)
		}
	}
	return c
}

// NewSource builds an OpenBao KV v2 CredentialSource. The RefScheme defaults to bao: so emitted bundles
// carry bao: SecretRefs that this package's resolver dereferences.
//
// It returns a *Module — the vault Source embedded in the small wrapper declared in selftest.go — so the
// value handed to the composition root carries the console's TEST capability alongside the CredentialSource
// interface it already satisfied. The sync behaviour is the vault source's own and is untouched; see the
// Module doc for why the probe cannot simply be a method on Source (it is an alias for a type declared in
// another package).
func NewSource(cfg SourceConfig) (*Module, error) {
	if cfg.RefScheme == "" {
		cfg.RefScheme = Scheme
	}
	src, err := vault.NewSource(cfg)
	if err != nil {
		return nil, err
	}
	// The mount and prefix are normalised EXACTLY as vault.NewSource normalises them, so the path the probe
	// lists and the path the sync lists cannot drift apart.
	return &Module{
		Source: src,
		client: cfg.Client,
		mount:  strings.Trim(cfg.Mount, "/"),
		prefix: strings.Trim(cfg.Prefix, "/"),
	}, nil
}

// RegisterResolver wires this client's bao: scheme into core/config so a Bundle SecretRef like
// "bao:secret/data/hosts/hostA#ssh_key" resolves through it at use time (REQ-1613). Read-only, fail-closed.
// Pass a nil client to unregister (fail closed). Composition-time only.
func RegisterResolver(c *Client) {
	if c == nil {
		config.RegisterSchemeResolver(Scheme, nil)
		return
	}
	config.RegisterSchemeResolver(Scheme, c.ResolveRef)
}

// WireDelivery is the composition-root entry point for spec/022 REQ-2200/REQ-2204: it makes the PROCESS's own
// SecretRefs (model-gateway key, operator/admin tokens, seal master key, ingest tokens, per-target bundles)
// resolvable as bao: references from OpenBao. Given the substrate address, its bootstrap token reference (an
// env:/file: ref), and an optional CA path, it builds the read-only client and registers the bao: scheme
// under credential.DeliveryConfig's fail-closed policy. An empty address is a logged no-op (substrate OFF:
// bao: refs fail closed, env:/file: refs unaffected). Any error (bad config or unbuildable client) is returned
// so the caller can refuse to start — a declared bao: secret must never degrade to a plaintext fallback.
func WireDelivery(addr, tokenRef, caCertPath string, logf func(string, ...any), opts ...WireOption) error {
	dc := credential.DeliveryConfig{Addr: addr, TokenRef: config.SecretRef(tokenRef), CACert: caCertPath}
	if !dc.Enabled() {
		return dc.Register(Scheme, nil, logf)
	}
	if err := dc.Validate(); err != nil {
		return err
	}
	client, err := New(applyWire(Config{BaseURL: addr, Auth: Token{TokenRef: config.SecretRef(tokenRef)}, CACertPath: caCertPath}, opts))
	if err != nil {
		return fmt.Errorf("openbao delivery: build client: %w", err)
	}
	return dc.Register(Scheme, client.ResolveRef, logf)
}

// WireDeliveryAppRole is the APPROLE variant of WireDelivery (TG-153): the process authenticates with a
// role-id + secret-id pair delivered as files, and OpenBao returns a token scoped to that role's policy.
//
// ★ WHY IT EXISTS. It is the only bootstrap that gives ONE HOST TWO IDENTITIES. mTLS cannot: both worker
// containers on a box present the same host certificate, so they get the same policy — which collapses the
// credential-plane split back into the single blast radius it exists to break. Approle is ranked BELOW mTLS
// (a certificate is an identity the host IS; a secret-id is a secret it HOLDS) and is the right tool
// exactly here.
//
// The refs must be env:/file: — a credential that authenticates TO the substrate cannot come FROM it.
// An empty address is a logged no-op. Any config/client error is returned so the caller refuses to start.
func WireDeliveryAppRole(addr, roleIDRef, secretIDRef, caCertPath string, logf func(string, ...any), opts ...WireOption) error {
	dc := credential.DeliveryConfig{
		Addr: addr, CACert: caCertPath,
		RoleIDRef:   config.SecretRef(roleIDRef),
		SecretIDRef: config.SecretRef(secretIDRef),
	}
	if !dc.Enabled() {
		return dc.Register(Scheme, nil, logf)
	}
	if err := dc.Validate(); err != nil {
		return err
	}
	client, err := New(applyWire(Config{
		BaseURL:    addr,
		Auth:       AppRole{RoleIDRef: config.SecretRef(roleIDRef), SecretIDRef: config.SecretRef(secretIDRef)},
		CACertPath: caCertPath,
	}, opts))
	if err != nil {
		return fmt.Errorf("openbao delivery: build approle client: %w", err)
	}
	return dc.Register(Scheme, client.ResolveRef, logf)
}

// WireDeliveryCert is the mTLS machine-identity variant of WireDelivery (spec/024 Amendment 2026-07-25,
// REQ-2407): the process authenticates to OpenBao by PRESENTING a FreeIPA-CA-signed client certificate
// (certPath, public) + its private key (keyPath, a root-only file) — an identity it IS, not a bootstrap token
// it holds — so nothing bootstrap-secret sits on disk. role names the OpenBao cert auth role (optional). An
// empty address is a logged no-op (substrate OFF). Any config/client error is returned so the caller can
// refuse to start (a declared bao: secret must never degrade to a plaintext fallback). This is the preferred,
// higher-assurance bootstrap where a site CA exists; the token-based WireDelivery remains for other deploys.
func WireDeliveryCert(addr, certPath, keyPath, role, caCertPath string, logf func(string, ...any), opts ...WireOption) error {
	dc := credential.DeliveryConfig{Addr: addr, CertPath: certPath, KeyPath: keyPath, CertRole: role, CACert: caCertPath}
	if !dc.Enabled() {
		return dc.Register(Scheme, nil, logf)
	}
	if err := dc.Validate(); err != nil {
		return err
	}
	client, err := New(applyWire(Config{
		BaseURL:    addr,
		Auth:       Cert{CertPath: certPath, KeyPath: keyPath, Name: role},
		CACertPath: caCertPath,
	}, opts))
	if err != nil {
		return fmt.Errorf("openbao delivery (mTLS): build client: %w", err)
	}
	return dc.Register(Scheme, client.ResolveRef, logf)
}
