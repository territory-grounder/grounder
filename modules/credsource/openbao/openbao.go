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
	// Source is the KV v2 CredentialSource.
	Source = vault.Source
	// SourceConfig configures a Source.
	SourceConfig = vault.SourceConfig
)

// New builds an OpenBao client (identical to vault.New — the wire protocol is the same).
func New(cfg Config) (*Client, error) { return vault.New(cfg) }

// NewSource builds an OpenBao KV v2 CredentialSource. The RefScheme defaults to bao: so emitted bundles
// carry bao: SecretRefs that this package's resolver dereferences.
func NewSource(cfg SourceConfig) (*Source, error) {
	if cfg.RefScheme == "" {
		cfg.RefScheme = Scheme
	}
	return vault.NewSource(cfg)
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
func WireDelivery(addr, tokenRef, caCertPath string, logf func(string, ...any)) error {
	dc := credential.DeliveryConfig{Addr: addr, TokenRef: config.SecretRef(tokenRef), CACert: caCertPath}
	if !dc.Enabled() {
		return dc.Register(Scheme, nil, logf)
	}
	if err := dc.Validate(); err != nil {
		return err
	}
	client, err := New(Config{BaseURL: addr, Auth: Token{TokenRef: config.SecretRef(tokenRef)}, CACertPath: caCertPath})
	if err != nil {
		return fmt.Errorf("openbao delivery: build client: %w", err)
	}
	return dc.Register(Scheme, client.ResolveRef, logf)
}
