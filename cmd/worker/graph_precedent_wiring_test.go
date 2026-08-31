package main

import (
	"testing"

	"github.com/territory-grounder/grounder/core/estate"
	"github.com/territory-grounder/grounder/core/knowledge"
)

// TG-53 wiring: estateBlastHosts must turn a runs_on topology into the guest host names in a PVE node's blast
// radius, and — fed to GraphExpandRetriever — surface a guest's precedent for an alert on its node. This is
// the estate↔knowledge JOIN the composition root arms; the GraphExpandRetriever unit tests fake the blast-host
// func, so this is the only test that exercises the real Graph.BlastRadius adapter.
func TestEstateBlastHostsSurfacesGuestPrecedentForNodeAlert(t *testing.T) {
	g := estate.NewGraph()
	// vm-a runs on pve01 ⇒ vm-a is in pve01's blast radius (it fails if pve01 fails). Edge convention is
	// SOURCE depends-on TARGET, so the runs_on edge points guest -> node and BlastRadius(node) walks it inward.
	g.Upsert(estate.Edge{
		From: estate.Entity{Type: estate.TypeLXC, Name: "vm-a"},
		To:   estate.Entity{Type: estate.TypePVENode, Name: "pve01"},
		Rel:  estate.RelRunsOn, Confidence: 0.95, Source: estate.SourcePVE,
	})
	holder := estate.NewHolder(g)

	bh := estateBlastHosts(holder, 2)

	// The adapter itself: the node's blast radius names the guest.
	got := bh("pve01")
	found := false
	for _, h := range got {
		if h == "vm-a" {
			found = true
		}
	}
	if !found {
		t.Fatalf("estateBlastHosts(pve01) must include the guest vm-a that runs on it; got %v", got)
	}
	// An unknown host has an empty blast radius ⇒ nil (a pass-through case).
	if hosts := bh("ghost-host-404"); len(hosts) != 0 {
		t.Errorf("an unknown host must have an empty blast radius, got %v", hosts)
	}
	// A nil holder is safe (returns nil, never panics).
	if hosts := estateBlastHosts(nil, 2)("pve01"); hosts != nil {
		t.Errorf("a nil holder must yield nil, got %v", hosts)
	}

	// End-to-end through the wired retriever: a precedent on the guest (invisible to the node's own query —
	// different host, rule, and site) is surfaced via the blast-radius vm-a variant.
	corpus := []knowledge.Incident{
		{ExternalRef: "N-node", Host: "pve01", AlertRule: "NodeDown", Site: "nl", Resolution: "quorum recovered"},
		{ExternalRef: "N-guest", Host: "vm-a", AlertRule: "GuestStopped", Site: "gr", Resolution: "started the guest"},
	}
	base := knowledge.NewLexicalRetriever(corpus)
	nodeQuery := knowledge.Query{Host: "pve01", AlertRule: "NodeDown", Site: "nl"}

	// Control: the base query alone must NOT see the guest precedent — else the graph proves nothing.
	for _, h := range base.Retrieve(nodeQuery, 5) {
		if h.Incident.ExternalRef == "N-guest" {
			t.Fatal("control broken: the base node query already surfaces the guest precedent")
		}
	}

	r := &knowledge.GraphExpandRetriever{Base: base, BlastHosts: bh}
	sawGuest := false
	var refs []string
	for _, h := range r.Retrieve(nodeQuery, 5) {
		refs = append(refs, h.Incident.ExternalRef)
		if h.Incident.ExternalRef == "N-guest" {
			sawGuest = true
		}
	}
	if !sawGuest {
		t.Fatalf("a node alert must surface its blast-radius guest's precedent (N-guest on vm-a) via the estate graph; got %v", refs)
	}
}
