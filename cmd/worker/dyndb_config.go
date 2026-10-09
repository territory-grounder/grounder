package main

import (
	"os"
	"strings"

	"github.com/territory-grounder/grounder/core/config"
	"github.com/territory-grounder/grounder/core/credential/dyndb"
)

// dyndbConfigFromEnv reads the dynamic-Postgres-credential (TG-422) bootstrap DIRECTLY from the environment,
// never through the console-override resolver (getenv). It is its own function, exempt in
// boot_config_test.go's resolver-bypass guard, for the SAME circularity reason the DSN is (installBootConfig
// / planeDBDSNFromEnv): this IS the credential path TO the database. A console-stored override would let a
// row in the very database the worker is trying to obtain a login for redirect the OpenBao engine that mints
// that login — and the worker could not read the override without already holding the credential it
// describes. Keeping it in one function means the exemption covers these lines, not the whole of main().
//
// TG-565: TG_DYNDB_CERT_ROLE selects CERT mode — the engine logs in with the host certificate
// (TG_OPENBAO_CERT/TG_OPENBAO_KEY, the same machine identity the delivery path uses) instead of presenting a
// stored token, so an outage longer than the token's period can no longer strand the boot.
func dyndbConfigFromEnv() (cfg dyndb.Config, dsnTemplate string) {
	return dyndb.Config{
		BaseURL:  strings.TrimSpace(os.Getenv("TG_DYNDB_ADDR")),
		Mount:    os.Getenv("TG_DYNDB_MOUNT"),
		TokenRef: config.SecretRef(os.Getenv("TG_DYNDB_TOKEN_REF")),
		CACert:   os.Getenv("TG_DYNDB_CA"),
		CertRole: os.Getenv("TG_DYNDB_CERT_ROLE"),
		CertPath: os.Getenv("TG_OPENBAO_CERT"),
		KeyPath:  os.Getenv("TG_OPENBAO_KEY"),
	}, os.Getenv("TG_DYNDB_DSN_TEMPLATE")
}
