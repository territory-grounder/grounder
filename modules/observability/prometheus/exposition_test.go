package prometheus

import (
	"context"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	observability "github.com/territory-grounder/grounder/adapters/observability"
)

// renderExposition renders Gather output into the Prometheus text exposition format a real /metrics scrape
// reads back — `name{k="v",...} value`, labels in sorted key order, one line per series, lines sorted for a
// stable golden. This is the module's vendor "response": Prometheus pulls this text, so it is what must be
// vendor-correct and unique per series.
func renderExposition(samples []observability.Sample) string {
	lines := make([]string, 0, len(samples))
	for _, s := range samples {
		var b strings.Builder
		b.WriteString(s.Name)
		if len(s.Labels) > 0 {
			keys := make([]string, 0, len(s.Labels))
			for k := range s.Labels {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			b.WriteByte('{')
			for i, k := range keys {
				if i > 0 {
					b.WriteByte(',')
				}
				b.WriteString(k)
				b.WriteString(`="`)
				b.WriteString(s.Labels[k])
				b.WriteByte('"')
			}
			b.WriteByte('}')
		}
		b.WriteByte(' ')
		b.WriteString(strconv.FormatFloat(s.Value, 'g', -1, 64))
		lines = append(lines, b.String())
	}
	sort.Strings(lines)
	return strings.Join(lines, "\n")
}

var expositionLine = regexp.MustCompile(`^[a-zA-Z_][a-zA-Z0-9_]*(\{[a-zA-Z_][a-zA-Z0-9_]*="[^"]*"(,[a-zA-Z_][a-zA-Z0-9_]*="[^"]*")*\})? -?[0-9]`)

// TestGatherRendersRealPromExposition is the "test against reality" for this scrape-target module: it
// renders a scrape whose fixture mixes a control-plane series with two per-connector series (one dead) and
// asserts the bytes equal testdata/scrape.txt — a checked-in golden in the ACTUAL Prometheus text
// exposition format. Because staleness is now per identity, the golden contains a DISTINCT
// tg_connector_up_stale{connector="snmp",...} 1 line for the dead writer alongside the healthy
// tg_connector_up_stale{connector="netbox",...} 0 line; the name-only bug could not produce that pair. The
// golden also proves every emitted line is a syntactically valid, UNIQUE exposition series (a duplicate
// name+labels line would be an invalid scrape).
func TestGatherRendersRealPromExposition(t *testing.T) {
	want, err := os.ReadFile("testdata/scrape.txt")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 7, 15, 12, 5, 0, 0, time.UTC)
	m := New(fixedClock(now), WithStalenessWindow(2*time.Minute))
	if err := m.Export(context.Background(), []observability.Sample{
		{Name: "tg_control_plane_up", Value: 1, Labels: map[string]string{"component": "grounder"}},
		{Name: "tg_connector_up", Value: 1, Labels: map[string]string{"connector": "netbox"}},
		{Name: "tg_connector_up", Value: 1, Stamped: now.Add(-10 * time.Minute), Labels: map[string]string{"connector": "snmp"}},
	}); err != nil {
		t.Fatalf("Export must succeed: %v", err)
	}

	got := renderExposition(m.Gather())

	if got != strings.TrimRight(string(want), "\n") {
		t.Errorf("exposition mismatch\n--- got ---\n%s\n--- want (testdata/scrape.txt) ---\n%s", got, strings.TrimRight(string(want), "\n"))
	}

	// Validity: every line is a well-formed exposition series and no series (name+labels) repeats.
	seen := map[string]bool{}
	for _, line := range strings.Split(got, "\n") {
		if !expositionLine.MatchString(line) {
			t.Errorf("line is not valid Prometheus exposition: %q", line)
		}
		series := line[:strings.LastIndexByte(line, ' ')]
		if seen[series] {
			t.Errorf("duplicate series in exposition (invalid scrape): %q", series)
		}
		seen[series] = true
	}
}
