package bootstrap

// THE COMPOSITION-ROOT GUARD for TG's second witness (TG-315).
//
// The parser being correct proves nothing about whether the front door will accept a payload for it.
// /v1/ingest/{source_type} admits only a DECLARED, ENABLED ingest capability (INV-17), so an unregistered
// authlog module is a parser nobody can post to — present, fully tested, CI-green, and unreachable. That
// is this repo's standing failure shape, and it is the one a unit test in the module's own package cannot
// see.

import (
	"testing"

	"github.com/territory-grounder/grounder/modules"
	"github.com/territory-grounder/grounder/modules/ingest/authlog"
)

func TestAuthlogIsADeclaredEnabledIngestCapability(t *testing.T) {
	reg, err := NewRegistry()
	if err != nil {
		t.Fatalf("registry bootstrap: %v", err)
	}
	caps := reg.Capabilities()
	if len(caps) == 0 {
		t.Fatal("the registry declares NOTHING, so every assertion below is vacuous")
	}

	var found, enabled bool
	var ingestSources []string
	for _, c := range caps {
		if c.Surface != modules.SurfaceIngest {
			continue
		}
		ingestSources = append(ingestSources, c.SourceType)
		if c.SourceType == authlog.SourceType {
			found = true
			enabled = c.Enabled
		}
	}
	if len(ingestSources) < 2 {
		t.Fatalf("only %d ingest source(s) declared (%v) — too few for the cross-source correlation rule "+
			"to have anything to compare, so this guard would pass against a registry that lost most of them",
			len(ingestSources), ingestSources)
	}
	if !found {
		t.Fatalf("authlog is NOT a declared ingest capability. Declared: %v.\n"+
			"/v1/ingest/authlog then rejects every post (INV-17), the collector has nowhere to deliver, and "+
			"the correlator's cross-source rule keeps the zero second-witness state TG-315 exists to end.",
			ingestSources)
	}
	if !enabled {
		t.Error("authlog is declared but DISABLED — the front door refuses it just as surely, and " +
			"declaredIngestSources() would then also report it as never-delivered forever")
	}
}

// The witness must be DISTINCT. The cross-source rule keys on differing source_type, so a collision with an
// availability source adds a witness the correlator cannot tell from the one it already had.
func TestAuthlogDoesNotCollideWithAnExistingIngestSource(t *testing.T) {
	reg, err := NewRegistry()
	if err != nil {
		t.Fatalf("registry bootstrap: %v", err)
	}
	seen := map[string]int{}
	for _, c := range reg.Capabilities() {
		if c.Surface == modules.SurfaceIngest {
			seen[c.SourceType]++
		}
	}
	if seen[authlog.SourceType] != 1 {
		t.Errorf("authlog declared %d times, want exactly 1", seen[authlog.SourceType])
	}
	for src, n := range seen {
		if n > 1 {
			t.Errorf("ingest source %q is declared %d times — a duplicate declaration means one of them is "+
				"shadowed and nobody can tell which parser the front door actually uses", src, n)
		}
	}
}
