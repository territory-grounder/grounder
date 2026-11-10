package main

import (
	"net/http"
	"testing"

	"github.com/territory-grounder/grounder/core/httpfailover"
)

// TG-572: with the other cluster members configured, http.DefaultTransport becomes the failover layer over the
// previously installed transport (the egress meter in production); with a single endpoint it is untouched.
func TestInstallPVEFailoverArmsOnlyWithAGroup(t *testing.T) {
	orig := http.DefaultTransport
	t.Cleanup(func() { http.DefaultTransport = orig })

	t.Setenv("TG_PVE_URL", "https://dc1pve04.example.net:8006")
	t.Setenv("TG_PROXMOX_BASE_URL", "https://dc1pve04.example.net:8006")
	t.Setenv("TG_PVE_FAILOVER_URLS", "")
	installPVEFailover()
	if http.DefaultTransport != orig {
		t.Fatal("one distinct endpoint must leave http.DefaultTransport untouched (byte-identical default)")
	}

	t.Setenv("TG_PVE_FAILOVER_URLS", "https://dc1pve03.example.net:8006, https://dc2pve01.example.net:8006")
	installPVEFailover()
	ft, ok := http.DefaultTransport.(*httpfailover.Transport)
	if !ok {
		t.Fatalf("with failover members configured the default transport must be the failover layer, got %T", http.DefaultTransport)
	}
	if got := ft.Members(); len(got) != 3 || got[0] != "https://dc1pve04.example.net:8006" {
		t.Fatalf("the configured endpoint must lead the group, deduplicated, got %v", got)
	}
}
