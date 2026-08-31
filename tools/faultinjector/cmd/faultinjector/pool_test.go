package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/territory-grounder/grounder/tools/faultinjector"
)

// The pool file is the ONLY place an estate identity may appear — no container name and no unit name is
// compiled in, because a literal estate identity in a shipped artifact is the class of defect the
// forbidden-pattern gate exists to catch. That makes the parser load-bearing: what it mis-reads becomes what
// the injector breaks.

func writePool(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "pool.txt")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// TestPoolOptionalFieldsAreIndependent is the positional-slot oracle. The optional fields are read by
// POSITION, so a guest with a unit but no container has no natural way to be written — and the obvious
// workaround, putting the unit in the 4th slot, silently declares it as a CONTAINER. The injector would then
// run `docker stop nginx` on a host running no such container: a fault that never fires, recorded as one that
// did, against an obligation whose undo is `docker start nginx`. "-" is the explicit empty marker.
func TestPoolOptionalFieldsAreIndependent(t *testing.T) {
	guests, err := faultinjector.LoadPool(writePool(t, `
100 guest-both   node-a  some-container  some.service
101 guest-unit   node-a  -               some.service
102 guest-cont   node-a  some-container
103 guest-cont2  node-a  some-container  -
104 guest-plain  node-a
`))
	if err != nil {
		t.Fatal(err)
	}
	want := []struct{ name, container, unit string }{
		{"guest-both", "some-container", "some.service"},
		{"guest-unit", "", "some.service"},
		{"guest-cont", "some-container", ""},
		// the trailing "-" must read as ABSENT, not as a unit literally named "-"
		{"guest-cont2", "some-container", ""},
		{"guest-plain", "", ""},
	}
	if len(guests) != len(want) {
		t.Fatalf("got %d guests, want %d", len(guests), len(want))
	}
	for i, w := range want {
		g := guests[i]
		if g.Name != w.name || g.Container != w.container || g.Unit != w.unit {
			t.Errorf("line %d: got name=%q container=%q unit=%q, want %q/%q/%q",
				i+1, g.Name, g.Container, g.Unit, w.name, w.container, w.unit)
		}
	}
}

// TestThePlaceholderNeverBecomesATarget — "-" must mean ABSENT, never a literal name. If it leaked through,
// the injector would run `systemctl stop -` / `docker stop -`, and the guest would be eligible for a class
// that has nothing to act on.
func TestThePlaceholderNeverBecomesATarget(t *testing.T) {
	guests, err := faultinjector.LoadPool(writePool(t, "100 guest-a node-a - -\n"))
	if err != nil {
		t.Fatal(err)
	}
	if guests[0].Container == "-" || guests[0].Unit == "-" {
		t.Errorf("the empty marker leaked through as a literal target: container=%q unit=%q",
			guests[0].Container, guests[0].Unit)
	}
}

// A line with more fields than the format allows must be fatal, not silently truncated — a 6th field would be
// a declaration the operator believes is in effect and that nothing reads.
func TestTooManyFieldsIsFatal(t *testing.T) {
	if _, err := faultinjector.LoadPool(writePool(t, "100 guest-a node-a cont unit extra\n")); err == nil {
		t.Fatal("a 6-field pool line must be refused, not truncated")
	}
}

// TestTheHealthProbeAbsorbsTheRestOfTheLine (TG-226). The probe is the only field that is a COMMAND, and a
// real one — `curl -sf http://127.0.0.1:3000/api/chats` — cannot fit in a single whitespace-delimited
// token. If the parser took only f[6], the operator's declaration would silently become `curl`, which
// exits 2 with a usage message and would fail every restore on a perfectly healthy guest.
func TestTheHealthProbeAbsorbsTheRestOfTheLine(t *testing.T) {
	guests, err := faultinjector.LoadPool(writePool(t, "100 guest-a node-a - - - curl -sf http://127.0.0.1:3000/api/chats\n"))
	if err != nil {
		t.Fatal(err)
	}
	const want = "curl -sf http://127.0.0.1:3000/api/chats"
	if guests[0].HealthProbe != want {
		t.Fatalf("probe parsed as %q, want %q — a truncated probe runs a DIFFERENT command than the one "+
			"declared, and its failure would be blamed on the guest", guests[0].HealthProbe, want)
	}
	// The earlier fields must not have been swallowed by the absorbing tail.
	if guests[0].Container != "" || guests[0].Unit != "" || guests[0].LogPath != "" {
		t.Errorf("absorbing the probe corrupted the earlier fields: container=%q unit=%q logpath=%q",
			guests[0].Container, guests[0].Unit, guests[0].LogPath)
	}
}

// TestAnAbsentHealthProbeStaysAbsent. Every existing pool line has 3-6 fields, so the common case is no
// probe at all — and "-" must read as absent here exactly as it does for the other optional fields, or the
// injector would try to run a command literally named "-".
func TestAnAbsentHealthProbeStaysAbsent(t *testing.T) {
	guests, err := faultinjector.LoadPool(writePool(t, "100 guest-a node-a\n101 guest-b node-a - - - -\n"))
	if err != nil {
		t.Fatal(err)
	}
	for _, g := range guests {
		if g.HealthProbe != "" {
			t.Errorf("%s parsed a probe %q where none was declared", g.Name, g.HealthProbe)
		}
	}
}

// TestAMalformedHealthProbeIsFatal. An absent probe is a legitimate declaration meaning "no app-level
// check"; a malformed one is an operator who BELIEVES they declared a check and did not — the same false
// assurance TG-226 exists to remove. It must not be treated as absent.
func TestAMalformedHealthProbeIsFatal(t *testing.T) {
	if _, err := faultinjector.LoadPool(writePool(t, "100 guest-a node-a - - - curl -sf localhost/api | grep ok\n")); err == nil {
		t.Fatal("a probe containing a shell pipeline was accepted. It would run as fixed argv, handing curl " +
			"the literal arguments \"|\" and \"grep\" — the declaration and the behaviour would diverge " +
			"silently, and the operator would believe the guest had a data-path check.")
	}
}
