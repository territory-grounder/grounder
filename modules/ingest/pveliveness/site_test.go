package pveliveness

import (
	"context"
	"testing"
)

// TG-631: one cluster spans both sites. A guest-down must carry the site of the guest's OWNING NODE, not the
// one configured site — a GR guest recorded as NL is scored, diagnosed and joined at the wrong site.
func TestGuestDownCarriesItsNodesSite(t *testing.T) {
	f := &fakeDoer{}
	src := mkFollowSource(t, f, nil)
	run := func(node, name, status string) []string {
		f.body = `{"data":[{"type":"lxc","node":"` + node + `","name":"` + name + `","vmid":201,"status":"` + status + `","template":0}]}`
		envs, err := src.FetchActive(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		var sites []string
		for _, e := range envs {
			sites = append(sites, e.Site)
		}
		return sites
	}
	run("dc2pve01", "dc2app01", "running")
	if got := run("dc2pve01", "dc2app01", "stopped"); len(got) != 1 || got[0] != "dc2" {
		t.Fatalf("a guest on a GR node must be stamped dc2, got %v", got)
	}
	run("dc1pve04", "dc1app01", "running")
	if got := run("dc1pve04", "dc1app01", "stopped"); len(got) != 1 || got[0] != "dc1" {
		t.Fatalf("a guest on an NL node must be stamped dc1, got %v", got)
	}
}

// A node whose name carries no declared site keeps the configured site ("NL" folds to dc1).
func TestGuestDownOnAnUnnamedSiteNodeKeepsTheConfiguredSite(t *testing.T) {
	f := &fakeDoer{}
	src := mkFollowSource(t, f, nil)
	for _, st := range []string{"running", "stopped"} {
		f.body = `{"data":[{"type":"qemu","node":"pve-a","name":"guest-x","vmid":7,"status":"` + st + `","template":0}]}`
		envs, err := src.FetchActive(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if st == "stopped" && (len(envs) != 1 || envs[0].Site != "dc1") {
			t.Fatalf("want the configured site (dc1) for an unrecognised node, got %+v", envs)
		}
	}
}
