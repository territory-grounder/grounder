package knowledge

import (
	"reflect"
	"testing"
)

// geCorpus: an alerting PVE node, a precedent on a host in its blast radius (vm-a runs on pve01), and a
// precedent on an unrelated host. K-blast deliberately shares NEITHER rule nor site with the alerting-host
// query, so the base query cannot see it — only the vm-a host variant the graph adds surfaces it (host match).
func geCorpus() []Incident {
	return []Incident{
		{ExternalRef: "K-node", Host: "pve01", AlertRule: "NodeDown", Site: "nl", Resolution: "checked quorum, node returned"},
		{ExternalRef: "K-blast", Host: "vm-a", AlertRule: "DiskFull", Site: "gr", Resolution: "grew the disk"},
		{ExternalRef: "K-unrel", Host: "other99", AlertRule: "DiskFull", Site: "gr", Resolution: "grew the disk"},
	}
}

var geQuery = Query{Host: "pve01", AlertRule: "NodeDown", Site: "nl"}

func geRefs(hits []Hit) []string {
	out := make([]string, 0, len(hits))
	for _, h := range hits {
		out = append(out, h.Incident.ExternalRef)
	}
	return out
}

func geHas(hits []Hit, ref string) bool {
	for _, h := range hits {
		if h.Incident.ExternalRef == ref {
			return true
		}
	}
	return false
}

// nil BlastHosts ⇒ pure pass-through: the graph-expand retriever serves the base ranking exactly (the
// OFF-when-wrapped contract the composition-root flag rests on).
func TestGraphExpandNilBlastHostsIsPassThrough(t *testing.T) {
	base := NewLexicalRetriever(geCorpus())
	r := &GraphExpandRetriever{Base: base, BlastHosts: nil}
	if got, want := geRefs(r.Retrieve(geQuery, 5)), geRefs(base.Retrieve(geQuery, 5)); !reflect.DeepEqual(got, want) {
		t.Fatalf("nil BlastHosts must be pass-through: got %v, base %v", got, want)
	}
}

// An empty blast radius (the host has no dependents) ⇒ base ranking, unchanged.
func TestGraphExpandEmptyBlastRadiusIsPassThrough(t *testing.T) {
	base := NewLexicalRetriever(geCorpus())
	r := &GraphExpandRetriever{Base: base, BlastHosts: func(string) []string { return nil }}
	if got, want := geRefs(r.Retrieve(geQuery, 5)), geRefs(base.Retrieve(geQuery, 5)); !reflect.DeepEqual(got, want) {
		t.Fatalf("empty blast radius must be pass-through: got %v, base %v", got, want)
	}
}

// The core: a past incident on a BLAST-RADIUS host (vm-a, which fails with pve01) is surfaced by the graph
// expansion though the alerting-host query alone never sees it — and a precedent OUTSIDE the blast radius is
// not. KILLING MUTATION: delete the blast-radius variant loop (retrieve only the original) ⇒ K-blast is not
// surfaced and this reddens.
func TestGraphExpandSurfacesBlastRadiusPrecedent(t *testing.T) {
	base := NewLexicalRetriever(geCorpus())

	// VACUITY FLOOR / control: the base (alerting-host) query must NOT already surface K-blast — otherwise
	// "expansion surfaced it" would prove nothing about the graph's contribution.
	if geHas(base.Retrieve(geQuery, 5), "K-blast") {
		t.Fatal("control broken: the base query already surfaces K-blast — the fixture no longer isolates the graph's contribution")
	}

	r := &GraphExpandRetriever{Base: base, BlastHosts: func(h string) []string {
		if h == "pve01" {
			return []string{"vm-a"} // vm-a runs on pve01, so it is in pve01's blast radius
		}
		return nil
	}}
	got := r.Retrieve(geQuery, 5)
	if !geHas(got, "K-blast") {
		t.Fatalf("graph expansion must surface the blast-radius host's precedent (K-blast on vm-a); got %v", geRefs(got))
	}
	if geHas(got, "K-unrel") {
		t.Errorf("a precedent on a host OUTSIDE the blast radius (K-unrel/other99) must NOT be surfaced; got %v", geRefs(got))
	}
	// The alerting host's OWN strong precedent stays on top — the graph adds recall, it never displaces the
	// exact same-host+rule match the incident is actually about.
	if len(got) == 0 || got[0].Incident.ExternalRef != "K-node" {
		t.Fatalf("the alerting host's own precedent (K-node) must remain ranked first; got %v", geRefs(got))
	}
}
