package main

// vsphere_wiring.go — the vCenter estate source, DARK BY DEFAULT (TG-91).
//
// vSphere joins the estate-source family (pve/netbox/librenms/slurpit/declared/tunnel/learned) as a
// read-only live-hypervisor topology reader: it emits VM→physical_host `runs_on` edges. Like every other
// source it arms ONLY when configured — TG_VSPHERE_URL unset means the loop never constructs it and a
// deployment with no VMware estate ships byte-identical. Read-only (Phase-1-safe): it lists inventory and
// never actuates.
//
// The construction is extracted into this ONE testable function so an aliveness oracle
// (vsphere_wiring_test.go) can prove the source is wired AND dark-gated without a live vCenter — the
// present-not-reaching guard (a source built, tested, and linked into no binary) TG-91's family exists to
// satisfy. main() calls vsphereEstateSource; that call is what links the source into the worker binary.

import (
	"strings"

	"github.com/territory-grounder/grounder/core/config"
	"github.com/territory-grounder/grounder/core/estate"
	"github.com/territory-grounder/grounder/modules/cmdb/vsphere"
)

// vsphereEstateSource builds the vCenter estate source from the environment, or (nil, false) when
// TG_VSPHERE_URL is unset or blank — DARK by default. Username is plain config (TG_VSPHERE_USER); only the
// password is sealed (TG_VSPHERE_TOKEN_REF, default env:VSPHERE_PASSWORD). TG_VSPHERE_INSECURE opts INTO
// skipping TLS verification for a self-signed vCenter.
func vsphereEstateSource(getenv func(k, def string) string) (estate.EdgeSource, bool) {
	vsURL := strings.TrimSpace(getenv("TG_VSPHERE_URL", ""))
	if vsURL == "" {
		return nil, false
	}
	var vopts []vsphere.Option
	if truthyValue(getenv("TG_VSPHERE_INSECURE", "")) {
		vopts = append(vopts, vsphere.WithInsecureTLS(true))
	}
	return vsphere.New(vsURL, getenv("TG_VSPHERE_USER", ""),
		config.SecretRef(getenv("TG_VSPHERE_TOKEN_REF", "env:VSPHERE_PASSWORD")), vopts...), true
}
