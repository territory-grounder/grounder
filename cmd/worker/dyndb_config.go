package main

import "os"

// dyndbConfigFromEnv reads the dynamic-Postgres-credential (TG-422) bootstrap DIRECTLY from the environment,
// never through the console-override resolver (getenv). It is its own function, exempt in
// boot_config_test.go's resolver-bypass guard, for the SAME circularity reason the DSN is (installBootConfig
// / planeDBDSNFromEnv): this IS the credential path TO the database. A console-stored override would let a
// row in the very database the worker is trying to obtain a login for redirect the OpenBao engine that mints
// that login — and the worker could not read the override without already holding the credential it
// describes. Keeping it in one function means the exemption covers these lines, not the whole of main().
func dyndbConfigFromEnv() (addr, mount, tokenRef, caCert, dsnTemplate string) {
	return os.Getenv("TG_DYNDB_ADDR"),
		os.Getenv("TG_DYNDB_MOUNT"),
		os.Getenv("TG_DYNDB_TOKEN_REF"),
		os.Getenv("TG_DYNDB_CA"),
		os.Getenv("TG_DYNDB_DSN_TEMPLATE")
}
