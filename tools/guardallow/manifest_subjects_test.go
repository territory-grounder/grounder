package main

import (
	"strings"
	"testing"
)

// The spec/027 REQ-2704 host-guard half: the adopt click that grants TG a target also authorises it at
// the host — and nothing else does.

const manifestJSON = `{"entries":[
 {"name":"mealie.service","status":"approved","allowlist_kind":"unit"},
 {"name":"vanished.service","status":"retired_candidate_stale","allowlist_kind":"unit"},
 {"name":"actualbudget-actual_server-1","status":"approved","allowlist_kind":"container"},
 {"name":"discovered-but-unreviewed.service","status":"draft","allowlist_kind":"unit"},
 {"name":"refused.service","status":"rejected","allowlist_kind":"unit"},
 {"name":"revoked.service","status":"retired","allowlist_kind":"unit"},
 {"name":"dc1mealie01","status":"approved","allowlist_kind":"guest"},
 {"name":"dc1","status":"approved","allowlist_kind":""}
]}`

func has(ss []string, want string) bool {
	for _, s := range ss {
		if s == want {
			return true
		}
	}
	return false
}

// TestOnlyAdoptedEntriesBecomeHostGuardSubjects is the load-bearing one: a DRAFT contributes nothing.
// If discovery finding a unit authorised it at the host, discovery would be authorising itself and the
// operator's review would be decorative.
func TestOnlyAdoptedEntriesBecomeHostGuardSubjects(t *testing.T) {
	units, containers, err := SubjectsFromManifest([]byte(manifestJSON))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if !has(units, "mealie.service") {
		t.Fatalf("an ADOPTED unit must become a subject, got %v", units)
	}
	if !has(containers, "actualbudget-actual_server-1") {
		t.Fatalf("an ADOPTED container must become a subject, got %v", containers)
	}
	// A stale entry is still granted in the control plane; the host guard must not disagree.
	if !has(units, "vanished.service") {
		t.Fatalf("a STALE (still-granted) unit must stay a subject — discovery losing sight of it must not silently narrow the host guard, got %v", units)
	}
	for _, forbidden := range []string{"discovered-but-unreviewed.service", "refused.service", "revoked.service"} {
		if has(units, forbidden) {
			t.Fatalf("%q must NOT be a host-guard subject: only an operator's adoption authorises, never discovery itself (got %v)", forbidden, units)
		}
	}
	// A guest is not a host-guard subject: the guard authorises systemctl and docker, not hypervisor calls.
	if has(units, "dc1mealie01") || has(containers, "dc1mealie01") {
		t.Fatalf("a guest must not become a systemctl/docker subject, got units=%v containers=%v", units, containers)
	}
	if has(units, "dc1") || has(containers, "dc1") {
		t.Fatalf("an entry that materialises into nothing must contribute no subject, got units=%v containers=%v", units, containers)
	}
}

// TestManifestSubjectsRenderThroughTheRealRegistry proves the end-to-end shape: an adopted unit produces
// the exact SSH_ORIGINAL_COMMAND lines the guard compares byte-for-byte, forward AND rollback.
func TestManifestSubjectsRenderThroughTheRealRegistry(t *testing.T) {
	units, containers, err := SubjectsFromManifest([]byte(manifestJSON))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	lines, err := Render(units, containers)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	joined := strings.Join(lines, "\n")
	if !strings.Contains(joined, "mealie.service") {
		t.Fatalf("the adopted unit must appear in the rendered allowlist:\n%s", joined)
	}
	if strings.Contains(joined, "discovered-but-unreviewed.service") {
		t.Fatalf("an unreviewed draft reached the host allowlist:\n%s", joined)
	}
	// The rollback verb must be authorised too — an op-class that can act but cannot be undone is worse
	// than one refused outright (this tool's founding defect).
	var sawStop bool
	for _, l := range lines {
		if strings.Contains(l, "stop") && strings.Contains(l, "mealie.service") {
			sawStop = true
		}
	}
	if !sawStop {
		t.Fatalf("the compensating verb for an adopted unit must be authorised:\n%s", joined)
	}
}

// TestMalformedManifestIsRefusedNotSilentlyEmpty — a parse failure that returned an empty subject list
// would render a SHORTER allowlist and silently revoke the host's existing grants.
func TestMalformedManifestIsRefusedNotSilentlyEmpty(t *testing.T) {
	if _, _, err := SubjectsFromManifest([]byte(`{"entries":[{`)); err == nil {
		t.Fatal("a malformed manifest must be refused loudly, not treated as an empty grant set")
	}
}
