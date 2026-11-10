package main

import (
	"crypto/tls"
	"log"
	"os"

	"go.temporal.io/sdk/client"

	"github.com/territory-grounder/grounder/core/config"
	"github.com/territory-grounder/grounder/temporal/clienttls"
)

// temporalTLSFromEnv builds the client identity the grounder presents to the Temporal frontend (TG-660 part a):
// TG_TEMPORAL_TLS_CA_REF / _CERT_REF / _KEY_REF resolved in-process (bao: in production), server name
// TG_TEMPORAL_TLS_SERVER_NAME (default "temporal"). Unset ⇒ a plaintext dial, said so at boot. ANY ref set ⇒ all
// three must resolve and parse or the grounder refuses to boot — the surface that signs every admin write must
// never reach the control plane without the identity it was configured to present.
func temporalTLSFromEnv() *tls.Config {
	refs := clienttls.Refs{
		CA:         config.SecretRef(os.Getenv("TG_TEMPORAL_TLS_CA_REF")),
		Cert:       config.SecretRef(os.Getenv("TG_TEMPORAL_TLS_CERT_REF")),
		Key:        config.SecretRef(os.Getenv("TG_TEMPORAL_TLS_KEY_REF")),
		ServerName: os.Getenv("TG_TEMPORAL_TLS_SERVER_NAME"),
	}.Normalized()
	cfg, err := refs.Build(func(r config.SecretRef) (string, error) { return r.Resolve() })
	if err != nil {
		log.Fatalf("temporal: %v — refusing to boot rather than dial the frontend without the configured client identity (TG-660)", err)
	}
	log.Printf("temporal: %s", clienttls.Summary(cfg))
	return cfg
}

// temporalClientOptions is the one place the grounder's Temporal dial options are assembled; the patient boot
// dial and the degraded-mode redial both use the same options.
func temporalClientOptions(hostPort string) client.Options {
	return client.Options{HostPort: hostPort, ConnectionOptions: client.ConnectionOptions{TLS: temporalTLSFromEnv()}}
}
