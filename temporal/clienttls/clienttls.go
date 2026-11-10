// Package clienttls builds the TLS configuration a TG process presents to the Temporal frontend (TG-660 part a,
// deep-review F1 / pack 12): mutual TLS, so that the frontend admits ONLY a dialer holding a client certificate
// issued by the deployment's Temporal CA — the grounder, the two workers and the UI — and refuses the plaintext
// `temporal workflow start`, the rogue worker on the task queue and the forged VoteSignal from anything else
// that can reach :7233 on the backplane.
//
// The material arrives as SecretRefs (TG_TEMPORAL_TLS_CA_REF / _CERT_REF / _KEY_REF), resolved IN-PROCESS: the
// PEM never touches a disk the Go process shares with anything. The server name (TG_TEMPORAL_TLS_SERVER_NAME,
// default "temporal", the compose service name the server certificate carries as a SAN) is what the dialer
// verifies the frontend against — host verification stays ON.
//
// FAIL CLOSED AT BOOT. Three refs unset ⇒ a plaintext dial, said so in the boot line (the pre-TG-660 shape, for a
// deployment that has not turned mTLS on). ANY ref set ⇒ all three must resolve and parse, or the caller refuses
// to boot: a process that was configured for mTLS and silently fell back to plaintext would be the exact gap
// this closes, wearing a green health check.
package clienttls

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"strings"

	"github.com/territory-grounder/grounder/core/config"
)

// Env key names, shared by the worker, the grounder and the compose file (deploy.TestComposeEnvParity holds
// them in step).
const (
	EnvCARef      = "TG_TEMPORAL_TLS_CA_REF"
	EnvCertRef    = "TG_TEMPORAL_TLS_CERT_REF"
	EnvKeyRef     = "TG_TEMPORAL_TLS_KEY_REF"
	EnvServerName = "TG_TEMPORAL_TLS_SERVER_NAME"
	// DefaultServerName is the compose service name of the Temporal frontend — the SAN its certificate carries.
	DefaultServerName = "temporal"
)

// Refs is the configured material, unresolved.
type Refs struct {
	CA         config.SecretRef
	Cert       config.SecretRef
	Key        config.SecretRef
	ServerName string
}

// Normalized trims every field and applies the default server name — the shape Build expects. The binaries
// construct Refs from the literal env keys (so deploy.TestComposeEnvParity sees them) and normalise here.
func (r Refs) Normalized() Refs {
	r.CA = config.SecretRef(strings.TrimSpace(string(r.CA)))
	r.Cert = config.SecretRef(strings.TrimSpace(string(r.Cert)))
	r.Key = config.SecretRef(strings.TrimSpace(string(r.Key)))
	r.ServerName = strings.TrimSpace(r.ServerName)
	if r.ServerName == "" {
		r.ServerName = DefaultServerName
	}
	return r
}

// Enabled reports whether ANY of the three refs is set — the signal that the deployment intends mTLS, after
// which a missing or unreadable half is a boot refusal, never a fallback.
func (r Refs) Enabled() bool {
	return r.CA != "" || r.Cert != "" || r.Key != ""
}

// ErrPartial names the fail-closed rule: one ref set, not all three.
var ErrPartial = errors.New("temporal tls: TG_TEMPORAL_TLS_CA_REF, _CERT_REF and _KEY_REF must all be set together")

// Build resolves the refs through resolve and returns the client TLS configuration, or nil when mTLS is not
// configured. It names which half failed (never its value).
func (r Refs) Build(resolve func(config.SecretRef) (string, error)) (*tls.Config, error) {
	if !r.Enabled() {
		return nil, nil
	}
	if r.CA == "" || r.Cert == "" || r.Key == "" {
		return nil, ErrPartial
	}
	caPEM, err := resolve(r.CA)
	if err != nil {
		return nil, fmt.Errorf("temporal tls: %s did not resolve (%s backend)", EnvCARef, config.SchemeOf(r.CA))
	}
	certPEM, err := resolve(r.Cert)
	if err != nil {
		return nil, fmt.Errorf("temporal tls: %s did not resolve (%s backend)", EnvCertRef, config.SchemeOf(r.Cert))
	}
	keyPEM, err := resolve(r.Key)
	if err != nil {
		return nil, fmt.Errorf("temporal tls: %s did not resolve (%s backend)", EnvKeyRef, config.SchemeOf(r.Key))
	}
	return Assemble(caPEM, certPEM, keyPEM, r.ServerName)
}

// Assemble builds the configuration from PEM text: the CA pool the frontend must chain to, the client
// certificate the frontend requires, and the server name verified against the frontend's certificate.
func Assemble(caPEM, certPEM, keyPEM, serverName string) (*tls.Config, error) {
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM([]byte(caPEM)) {
		return nil, errors.New("temporal tls: the CA material holds no PEM certificate")
	}
	cert, err := tls.X509KeyPair([]byte(certPEM), []byte(keyPEM))
	if err != nil {
		// tls.X509KeyPair's error names the defect class (no PEM, key mismatch), never the key bytes.
		return nil, fmt.Errorf("temporal tls: client certificate/key: %w", err)
	}
	if strings.TrimSpace(serverName) == "" {
		return nil, errors.New("temporal tls: server name is empty (host verification stays on)")
	}
	return &tls.Config{
		RootCAs:      pool,
		Certificates: []tls.Certificate{cert},
		ServerName:   serverName,
		MinVersion:   tls.VersionTLS12,
	}, nil
}

// Summary is the boot line's description of a built configuration — never the material.
func Summary(cfg *tls.Config) string {
	if cfg == nil {
		return "PLAINTEXT dial (TG_TEMPORAL_TLS_*_REF unset) — the frontend admits any dialer on the backplane"
	}
	leaf := "client certificate loaded"
	if len(cfg.Certificates) == 1 && len(cfg.Certificates[0].Certificate) > 0 {
		if c, err := x509.ParseCertificate(cfg.Certificates[0].Certificate[0]); err == nil {
			leaf = fmt.Sprintf("client certificate CN=%q valid until %s", c.Subject.CommonName, c.NotAfter.UTC().Format("2006-01-02"))
		}
	}
	return fmt.Sprintf("mTLS dial, server name %q, %s, TLS>=1.2", cfg.ServerName, leaf)
}
