package systemd

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/territory-grounder/grounder/core/estate"
)

// realListUnitsOutput is REAL `systemctl list-units --no-legend --plain` output shape, including the
// degraded-unit bullet and a non-service unit — the two cases a paraphrased fixture would quietly omit.
const realListUnitsOutput = `nginx.service                loaded active   running A high performance web server
docker.service               loaded active   running Docker Application Container Engine
● mealie.service             loaded failed   failed  Mealie recipe manager
systemd-journald.service     loaded active   running Journal Service
dbus.socket                  loaded active   running D-Bus System Message Bus Socket
`

type fakeRunner struct {
	out  map[string][]byte
	err  map[string]error
	seen [][]string
}

func (f *fakeRunner) Run(_ context.Context, host string, argv []string) ([]byte, error) {
	f.seen = append(f.seen, argv)
	if e, ok := f.err[host]; ok {
		return nil, e
	}
	return f.out[host], nil
}

// TestDiscoveryDraftsEntriesWithSourceProvenanceAndTableConfidence is O-2701: every discovered edge carries
// its source provenance and the FIXED table confidence, so an adopted entry can never claim more authority
// than its source is entitled to.
func TestDiscoveryDraftsEntriesWithSourceProvenanceAndTableConfidence(t *testing.T) {
	f := &fakeRunner{out: map[string][]byte{"dc1mealie01": []byte(realListUnitsOutput)}}
	edges, err := New([]string{"dc1mealie01"}, WithRunner(f)).Edges(context.Background())
	if err != nil {
		t.Fatalf("edges: %v", err)
	}
	if len(edges) != 4 {
		t.Fatalf("want the 4 .service units (the .socket is not a restart-service target), got %d: %+v", len(edges), edges)
	}
	for _, e := range edges {
		if e.Source != estate.SourceDeclared {
			t.Fatalf("edge %q must carry its source provenance, got %q", e.From.Name, e.Source)
		}
		if want := estate.SourceConfidence[estate.SourceDeclared]; e.Confidence != want {
			t.Fatalf("edge %q must carry the FIXED table confidence %v, got %v", e.From.Name, want, e.Confidence)
		}
		if e.Rel != estate.RelRunsOn || e.To.Name != "dc1mealie01" {
			t.Fatalf("edge must be service runs_on host, got %+v", e)
		}
		if e.Confidence >= estate.SourceConfidence[estate.SourcePVE] {
			t.Fatalf("discovery must never outrank the hypervisor's own placement record: %v", e.Confidence)
		}
	}

	// The enumeration is a CONSTANT: a discovery source that could be handed an arbitrary command would be
	// an execution path wearing a reader's name.
	if len(f.seen) != 1 || f.seen[0][0] != "systemctl" || f.seen[0][1] != "list-units" {
		t.Fatalf("exactly one non-mutating systemctl list-units expected, got %+v", f.seen)
	}
	for _, argv := range f.seen {
		for _, a := range argv {
			if strings.Contains(a, "restart") || strings.Contains(a, "start") || strings.Contains(a, "stop") {
				t.Fatalf("discovery issued a MUTATING verb: %+v", argv)
			}
		}
	}
}

// TestTheBulletAndNonServiceUnitsAreParsedHonestly pins the parse against real output: a degraded unit's
// leading bullet must not be drafted as a target named "●", and non-service units must not be drafted at
// all (the leaf only ever restarts services).
func TestTheBulletAndNonServiceUnitsAreParsedHonestly(t *testing.T) {
	units := ParseUnits(realListUnitsOutput)
	want := map[string]bool{"nginx.service": true, "docker.service": true, "mealie.service": true, "systemd-journald.service": true}
	if len(units) != len(want) {
		t.Fatalf("want %d services, got %d: %v", len(want), len(units), units)
	}
	for _, u := range units {
		if !want[u] {
			t.Fatalf("parsed a unit that is not a real service target: %q (from %v)", u, units)
		}
	}
}

// TestAFailingHostIsReportedLoudlyAndTheOthersStillContribute is O-2705's per-host half: one unreachable
// machine must degrade to "its units are missing", never to a silent empty estate that reads as
// "this host runs nothing" — the difference between a gap and a lie.
func TestAFailingHostIsReportedLoudlyAndTheOthersStillContribute(t *testing.T) {
	f := &fakeRunner{
		out: map[string][]byte{"good01": []byte("nginx.service loaded active running x\n")},
		err: map[string]error{"bad01": errors.New("dial tcp: connection refused")},
	}
	edges, err := New([]string{"good01", "bad01"}, WithRunner(f)).Edges(context.Background())
	if err == nil {
		t.Fatal("a failing host must be reported LOUDLY, not swallowed")
	}
	if !strings.Contains(err.Error(), "bad01") {
		t.Fatalf("the error must name the failing host, got %v", err)
	}
	if len(edges) != 1 || edges[0].To.Name != "good01" {
		t.Fatalf("the healthy host must still contribute, got %+v", edges)
	}
}

// TestNoRunnerIsRefusedRatherThanReportingAnEmptyEstate — an unconfigured source must error, never return
// zero edges that a drift pass would read as "everything disappeared".
func TestNoRunnerIsRefusedRatherThanReportingAnEmptyEstate(t *testing.T) {
	if _, err := New([]string{"h1"}).Edges(context.Background()); err == nil {
		t.Fatal("an unconfigured source must error, not report an empty estate")
	}
}
