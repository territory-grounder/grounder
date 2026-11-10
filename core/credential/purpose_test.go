package credential

import (
	"context"
	"errors"
	"testing"
)

// TG-687: a diagnostic READ prefers TG's guarded read-only identity over a higher-precedence source; every other
// purpose keeps operator-declared precedence. Live trigger (2026-11-08): on NL hosts with a per-host OpenBao binding
// (precedence 10) TG's diagnostic and journal reads authenticated with the estate ADMIN key instead of the native
// hostdiag read-only key (precedence 100).

// tg687DiagSource is a fakeSource that declares (or explicitly denies) the DiagReadSource capability.
type tg687DiagSource struct {
	fakeSource
	serves bool
}

func (s *tg687DiagSource) ServesDiagRead() bool { return s.serves }

func tg687Engine(t *testing.T, extra ...*tg687DiagSource) *SyncEngine {
	t.Helper()
	se := NewSyncEngine(NewEngine(nil))
	admin := &fakeSource{id: "openbao", plane: PlaneMachine, entries: []SourceEntry{
		entry(t, "a-web01", Selector{KindHost, "web01"}, "root", "env:ADMIN_KEY"),
		entry(t, "a-db01", Selector{KindHost, "db01"}, "root", "env:ADMIN_KEY"),
	}}
	readOnly := &tg687DiagSource{fakeSource: fakeSource{id: "native-hostdiag", plane: PlaneMachine, entries: []SourceEntry{
		entry(t, "r-web", Selector{KindHostGlob, "web*"}, "root", "env:READONLY_KEY"),
	}}, serves: true}
	if err := se.RegisterSource(admin, 10); err != nil {
		t.Fatal(err)
	}
	if err := se.RegisterSource(readOnly, 100); err != nil {
		t.Fatal(err)
	}
	for i, x := range extra {
		if err := se.RegisterSource(x, 100+i+1); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := se.SyncAll(context.Background()); err != nil {
		t.Fatalf("sync: %v", err)
	}
	return se
}

func tg687Key(t *testing.T, r Resolution) string {
	t.Helper()
	return string(r.Bundle.SSHKeyRef())
}

// KILLING MUTATION: drop the `if t.Purpose == PurposeDiagRead` branch in SyncEngine.Resolve — RED here.
func TestADiagnosticReadPrefersTheReadOnlyIdentityOverAnAdminBinding(t *testing.T) {
	se := tg687Engine(t)

	// Control: every non-read purpose keeps the operator's precedence — the admin binding wins.
	def, err := se.Resolve(Target{Host: "web01"})
	if err != nil || def.Source != "openbao" || tg687Key(t, def) != "env:ADMIN_KEY" {
		t.Fatalf("default purpose must keep precedence (openbao wins), got %+v err=%v", def, err)
	}

	// A diagnostic READ takes TG's guarded read-only identity and records the admin binding as shadowed.
	rd, err := se.Resolve(Target{Host: "web01", Purpose: PurposeDiagRead})
	if err != nil {
		t.Fatal(err)
	}
	if rd.Source != "native-hostdiag" || tg687Key(t, rd) != "env:READONLY_KEY" {
		t.Fatalf("a diagnostic read must resolve the read-only identity, got source=%q key=%q", rd.Source, tg687Key(t, rd))
	}
	if len(rd.Shadowed) != 1 || rd.Shadowed[0] != "openbao" {
		t.Fatalf("the admin binding must be recorded as shadowed, got %v", rd.Shadowed)
	}
}

// No read-only identity covers the host ⇒ ordinary precedence, unchanged: the purpose adds no refusal.
func TestADiagnosticReadWithNoReadOnlyCoverageKeepsPrecedence(t *testing.T) {
	se := tg687Engine(t)
	rd, err := se.Resolve(Target{Host: "db01", Purpose: PurposeDiagRead})
	if err != nil || rd.Source != "openbao" || tg687Key(t, rd) != "env:ADMIN_KEY" {
		t.Fatalf("with no read-only coverage a diagnostic read keeps today's resolution, got %+v err=%v", rd, err)
	}
}

// A source that DENIES the capability is not preferred; two read-only sources still compete by precedence.
func TestOnlyADeclaredReadOnlySourceIsPreferred(t *testing.T) {
	notRO := &tg687DiagSource{fakeSource: fakeSource{id: "awx", plane: PlaneMachine, entries: []SourceEntry{
		entry(t, "x-web01", Selector{KindHost, "web01"}, "root", "env:AWX_KEY"),
	}}, serves: false}
	se := tg687Engine(t, notRO)
	rd, err := se.Resolve(Target{Host: "web01", Purpose: PurposeDiagRead})
	if err != nil || rd.Source != "native-hostdiag" {
		t.Fatalf("a source declaring ServesDiagRead()=false must not win a diagnostic read, got %+v err=%v", rd, err)
	}

	// Two read-only sources at EQUAL precedence still fail closed (REQ-1609): the purpose narrows, never invents.
	se2 := NewSyncEngine(NewEngine(nil))
	for _, id := range []string{"ro-a", "ro-b"} {
		src := &tg687DiagSource{fakeSource: fakeSource{id: id, plane: PlaneMachine, entries: []SourceEntry{
			entry(t, id+"-web01", Selector{KindHost, "web01"}, "root", "env:"+id),
		}}, serves: true}
		if err := se2.RegisterSource(src, 50); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := se2.SyncAll(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := se2.Resolve(Target{Host: "web01", Purpose: PurposeDiagRead}); !errors.Is(err, ErrAmbiguous) {
		t.Fatalf("equal-precedence read-only sources must stay ambiguous (fail closed), got %v", err)
	}
}
