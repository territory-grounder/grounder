package main

// TG-215 — the RECORDED live disclosure delta + the live disclosure floor.
//
// The per-class byte-identity goldens live in agent/ over a small fixture; THIS control renders the
// class-keyed catalog over the REAL registered tool families (the same inert-double constructions the
// ACI adoption control uses) and records the number the ticket asks for: how much smaller the
// FAST_AGENT preamble's tool catalog actually is than the flat catalog every class used to carry. It
// also pins the two live floors the reduction must never sink through:
//
//   - EVERY live tool stays LISTED in the FAST_AGENT render (an index line is a disclosure reduction,
//     never a removal — the loop's dispatch reads the registered set either way);
//   - the four fast-disclosed point reads keep their FULL schema entries.
//
// KILLING MUTATIONS: drop an index entry from the fast render — the listing floor reddens; move a
// point read out of the full form — the schema floor reddens; render the flat catalog for FAST_AGENT —
// the reduction assertion reddens.

import (
	"strings"
	"testing"

	"github.com/territory-grounder/grounder/agent"
	"github.com/territory-grounder/grounder/core/execclass"
)

// deltaSourceForFamily mirrors the source namespaces main() declares at each RegisterFrom site. A label
// drifting from main.go moves a heading in the rendered grouping, nothing more — dispatch, names and
// the disclosure selection are all keyed elsewhere.
var deltaSourceForFamily = map[string]string{
	"librenms.NewTools":             "librenms",
	"netbox.NewTools":               "netbox",
	"syslogng.NewTools":             "host",
	"hostdiag.NewTools":             "host",
	"estatetools.New":               "estate",
	"openobserve.NewCorrelateTools": "estate",
	"incidenthistory.New":           "history",
	"trackerhistory.New":            "history",
	"actorevidencetool.New":         "host",
}

func TestFastAgentDisclosureDeltaOverLiveTools(t *testing.T) {
	ts := agent.NewReadOnlyToolSet()
	total := 0
	for fam, tools := range aciBuiltFamilies() {
		src, ok := deltaSourceForFamily[fam]
		if !ok {
			t.Fatalf("family %s has no source namespace in deltaSourceForFamily — add it (mirroring its "+
				"RegisterFrom label in main.go) so this measurement keeps covering the whole live set", fam)
		}
		for _, tl := range tools {
			if err := ts.RegisterFrom(src, tl); err != nil {
				t.Fatalf("register %s: %v", tl.Name(), err)
			}
			total++
		}
	}
	if total == 0 {
		t.Fatal("no live tools built — this measurement would record a delta over nothing")
	}

	full := ts.Catalog()
	fast := ts.CatalogFor(execclass.FastAgent)

	// The live listing floor: reduction never removes a tool from the model's view.
	for _, name := range ts.Names() {
		if !strings.Contains(fast, "- "+name) {
			t.Errorf("live tool %q is missing from the FAST_AGENT catalog — that is a capability the "+
				"preamble stopped mentioning, not a disclosure reduction", name)
		}
	}
	// The live schema floor: the fast point-read set (agent.fastDisclosed; spelled out here because the
	// set is deliberately unexported) keeps full entries — "name: description" rather than "name — head".
	for _, name := range []string{"get-device-status", "get-device-eventlog", "get-active-alerts", "get-estate-context"} {
		if !strings.Contains(fast, "- "+name+": ") {
			t.Errorf("fast-disclosed point read %q must keep its FULL catalog entry in the FAST_AGENT render", name)
		}
	}
	if len(fast) >= len(full) {
		t.Fatalf("FAST_AGENT catalog (%d bytes) is not smaller than the full catalog (%d bytes) over the "+
			"live tool set — the reduction this ticket ships is not happening where it matters", len(fast), len(full))
	}
	// THE RECORDED DELTA (TG-215). ~tokens is the chars/4 proxy — approximate by construction, recorded
	// alongside the exact byte counts.
	t.Logf("TG-215 live disclosure delta: full catalog %d bytes (~%d tokens) → FAST_AGENT %d bytes (~%d tokens): %.1f%% catalog reduction over %d live tools",
		len(full), len(full)/4, len(fast), len(fast)/4, 100*(1-float64(len(fast))/float64(len(full))), total)
}
