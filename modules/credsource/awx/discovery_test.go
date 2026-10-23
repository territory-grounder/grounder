package awx

import "testing"

// ORACLES FOR THE CREDENTIAL-ONBOARDING FIRST SCREEN (TG-274).
//
// The connector already knew every fact on this screen and discarded it: the job-template walk resolved the
// credential name, its inventory and whether the operator had mapped it, then recorded only the FAILURES
// into Skipped() — which `grep -rn '\.Skipped()'` over non-test code showed nobody ever called. Measured on
// this estate 2026-08-04: AWX holds 11 Machine credentials, TG_AWX_CRED_REF_MAP maps ONE.

// KILLING MUTATION: record only mapped bindings (move the note() below the `if !ok` guard). RED — the
// source would then answer "everything I can see works" while blind to the ten credentials it cannot use,
// which is the precise shape of a surface that looks configured and is not.
func TestUnmappedCredentialsAreReportedNotOmitted(t *testing.T) {
	d := newDiscovery()
	d.note(CredentialBinding{CredentialName: "SSH ED25519 (one_key)", Inventory: "Territory Grounder", Hosts: 1, Mapped: true, Ref: "file:/secrets/one_key"})
	d.note(CredentialBinding{CredentialName: "Edge Infrastructure SSH", Inventory: "Edge Infrastructure", Hosts: 12, Mapped: false})

	all := d.all()
	if len(all) != 2 {
		t.Fatalf("got %d bindings, want 2 — an unmapped credential must still be REPORTED; omitting it is "+
			"how an operator comes to believe TG covers a fleet it cannot reach", len(all))
	}
	if all[0].Mapped {
		t.Fatal("a mapped binding sorted ahead of an unmapped one — the rows needing action must lead, or a " +
			"long tail of working credentials pushes them off the screen")
	}
}

// KILLING MUTATION: treat Mapped alone as usable. RED — a mapping to an empty ref is a control that reads
// as configured and resolves to nothing.
func TestAMappingWithNoRefIsNotUsable(t *testing.T) {
	if (CredentialBinding{Mapped: true, Ref: "  "}).Usable() {
		t.Fatal("a credential mapped to a blank SecretRef reported usable")
	}
	if !(CredentialBinding{Mapped: true, Ref: "bao:secret/data/tg/onekey#key"}).Usable() {
		t.Fatal("a properly mapped credential reported unusable")
	}
}

// KILLING MUTATION: drop reset() from Sync. RED — a credential deleted in AWX would keep appearing, and a
// surface that shows things that no longer exist stops being read.
func TestASyncReportsWhatAwxSaysNowNotWhatItOnceSaid(t *testing.T) {
	d := newDiscovery()
	d.note(CredentialBinding{CredentialName: "deleted-in-awx", Inventory: "old"})
	d.reset()
	d.note(CredentialBinding{CredentialName: "still-there", Inventory: "current"})
	all := d.all()
	if len(all) != 1 || all[0].CredentialName != "still-there" {
		t.Fatalf("stale bindings survived a resync: %+v", all)
	}
}

// One credential governing several inventories yields a row EACH: supplying one key can unlock more than
// one fleet, and the operator needs to see all of it before deciding.
func TestOneCredentialAcrossSeveralInventoriesYieldsARowEach(t *testing.T) {
	d := newDiscovery()
	d.note(CredentialBinding{CredentialName: "shared", Inventory: "A", Hosts: 3})
	d.note(CredentialBinding{CredentialName: "shared", Inventory: "B", Hosts: 9})
	all := d.all()
	if len(all) != 2 {
		t.Fatalf("got %d rows, want 2 — the second inventory was collapsed away", len(all))
	}
	if all[0].Hosts != 9 {
		t.Fatalf("rows not ordered by blast radius: %+v", all)
	}
}

// A nil source must not panic — Discovered() is called from an HTTP handler.
func TestDiscoveredOnANilSourceIsEmptyNotAPanic(t *testing.T) {
	var s *Source
	if got := s.Discovered(); got != nil {
		t.Fatalf("nil source returned %v", got)
	}
}
