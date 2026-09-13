package native

// ORACLE for the native hostdiag-backed credential source (spec/016 REQ-1600/1607/1610): it parses the SAME
// `site|hostglob|sshuser|keyref` allowlist grammar as hostdiag (one parser, no fork), maps each row to a
// machine-plane SourceEntry carrying a SecretRef reference only, SKIPS a row it cannot build a valid sealed
// bundle from (fail closed — never a blank identity), and — driven through a real SyncEngine — resolves a
// covered host to that bundle and FAILS CLOSED (ErrUnresolved) for an uncovered host, with NO default fallback.

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/territory-grounder/grounder/core/credential"
)

func TestNewFromSpec_ParsesRowsToEntries(t *testing.T) {
	src, err := NewFromSpec("", "nl|dc1*|root|file:/secrets/hostdiag_key;gr|dc2*|svc|env:GR_KEY")
	if err != nil {
		t.Fatalf("NewFromSpec: %v", err)
	}
	if src == nil {
		t.Fatal("expected a source for a well-formed spec")
	}
	if src.ID() != DefaultID {
		t.Errorf("default id = %q, want %q", src.ID(), DefaultID)
	}
	if src.Plane() != credential.PlaneMachine {
		t.Errorf("plane = %q, want machine", src.Plane())
	}
	entries, err := src.Sync(context.Background())
	if err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("got %d entries, want 2", len(entries))
	}
	e := entries[0]
	if e.Selector.Kind != credential.KindHostGlob || e.Selector.Pattern != "dc1*" {
		t.Errorf("entry selector = %+v, want host-glob dc1*", e.Selector)
	}
	if e.Bundle.User() != "root" || e.Bundle.Scheme() != credential.SchemeSSH {
		t.Errorf("entry bundle = user %q scheme %q, want root/ssh", e.Bundle.User(), e.Bundle.Scheme())
	}
	if e.Bundle.Port() != sshPort {
		t.Errorf("entry port = %d, want %d", e.Bundle.Port(), sshPort)
	}
	if string(e.Bundle.SSHKeyRef()) != "file:/secrets/hostdiag_key" {
		t.Errorf("entry key ref = %q (must be the REFERENCE, never material)", e.Bundle.SSHKeyRef())
	}
	if len(src.Skipped()) != 0 {
		t.Errorf("no row should have been skipped, got %+v", src.Skipped())
	}
}

// A row whose key field is a LITERAL (not a sealed env:/file:/store: reference) cannot build a valid bundle —
// NewBundle rejects it — so the source SKIPS it with a record and injects NO blank/partial identity.
func TestSync_MalformedRowFailsClosed(t *testing.T) {
	// The first row's key is a bare literal (no scheme) → non-sealed → rejected. The second is well-formed.
	src, err := NewFromSpec("", "nl|dc1*|root|literalkeymaterial;gr|dc2*|svc|env:GR_KEY")
	if err != nil {
		t.Fatalf("NewFromSpec: %v", err)
	}
	entries, err := src.Sync(context.Background())
	if err != nil {
		t.Fatalf("Sync must not fail the whole pull for one bad row: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("got %d entries, want 1 (the malformed row must be skipped, never injected blank)", len(entries))
	}
	if entries[0].Selector.Pattern != "dc2*" {
		t.Errorf("surviving entry = %q, want the well-formed dc2* row", entries[0].Selector.Pattern)
	}
	skipped := src.Skipped()
	if len(skipped) != 1 || skipped[0].HostGlob != "dc1*" {
		t.Fatalf("expected the dc1* row skipped-with-record, got %+v", skipped)
	}
	// The skip reason must not echo the suspected literal secret.
	if strings.Contains(skipped[0].Reason, "literalkeymaterial") {
		t.Errorf("skip reason leaked the literal secret: %q", skipped[0].Reason)
	}
}

func TestNewFromSpec_EmptyYieldsNoSource(t *testing.T) {
	for _, spec := range []string{"", "   ", ";;", "tooshort|onlytwo"} {
		src, err := NewFromSpec("", spec)
		if err != nil {
			t.Fatalf("NewFromSpec(%q): %v", spec, err)
		}
		if src != nil {
			t.Errorf("spec %q has no well-formed rows — expected no source, got one", spec)
		}
	}
}

// End-to-end through a real SyncEngine: the SAME host hostdiag reaches today resolves to the SAME (user,
// keyref) THROUGH the engine, and an uncovered host fails closed with NO default identity.
func TestResolveThroughSyncEngine(t *testing.T) {
	src, err := NewFromSpec("", "nl|dc1*|root|file:/secrets/hostdiag_key")
	if err != nil || src == nil {
		t.Fatalf("NewFromSpec: %v (src=%v)", err, src)
	}
	se := credential.NewSyncEngine(nil) // no native Engine fallback — only this registered source resolves
	if err := se.RegisterSource(src, 100); err != nil {
		t.Fatalf("register: %v", err)
	}
	if _, err := se.SyncAll(context.Background()); err != nil {
		t.Fatalf("SyncAll: %v", err)
	}

	// Covered host → the exact identity the allowlist row declares.
	res, err := se.Resolve(credential.Target{Host: "dc1librespeed01"})
	if err != nil {
		t.Fatalf("Resolve(covered): %v", err)
	}
	if res.Bundle.User() != "root" || string(res.Bundle.SSHKeyRef()) != "file:/secrets/hostdiag_key" {
		t.Errorf("resolved bundle = user %q keyref %q, want root/file:/secrets/hostdiag_key",
			res.Bundle.User(), res.Bundle.SSHKeyRef())
	}
	if res.Source != DefaultID {
		t.Errorf("winning source = %q, want %q", res.Source, DefaultID)
	}

	// Uncovered host → fail closed, NO default identity.
	if _, err := se.Resolve(credential.Target{Host: "dc2unknown"}); !errors.Is(err, credential.ErrUnresolved) {
		t.Errorf("uncovered host must fail closed with ErrUnresolved, got %v", err)
	}
}
