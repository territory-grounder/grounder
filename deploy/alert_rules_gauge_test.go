package deploy

// increase() AND rate() ON A GAUGE MAKE A RULE THAT CANNOT FIRE (TG-336).
//
// `IngestConnectorDeafToItsUpstream` is the one honest connector-broken signal TG has, and it was written
// carefully: the upstream must be READABLE, and holding alerts, and nothing arriving. Its third leg was
//
//	increase(tg_ingest_source_recent_total[1h]) == 0
//
// and tg_ingest_source_recent_total is a GAUGE — "alerts this source delivered in the baseline window", its
// own HELP text. increase() is defined for counters. A gauge that drifts DOWN as old alerts age out reads as
// a run of counter resets, so increase() extrapolates a large positive value rather than 0. Measured on the
// live series at the moment TG had received nothing for 6.8 hours:
//
//	increase(tg_ingest_source_recent_total{source_id="librenms-dc1"}[1h]) = 4276.64
//	tg_ingest_source_recent_total{source_id="librenms-dc1"}               =  524
//
// The leg never held. The rule had never been able to fire — while 83 alerts sat unread upstream and both
// LibreNMS connectors were silent for most of a day. A rule that cannot fire is worse than a missing one:
// the missing one is an obvious gap, and this one occupied the space where the gap would have been noticed.
//
// This test does not check that one rule. It scans EVERY alert expression for a counter-only function
// applied to a metric TG's own Go code declares as a Gauge, so the next one fails at CI instead of in
// production silence.

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// counterOnlyFnRe finds increase(X[..]), rate(X[..]), irate(X[..]) and delta-free counter helpers, capturing
// the metric name. delta() and idelta() are the GAUGE equivalents and are deliberately NOT matched.
var counterOnlyFnRe = regexp.MustCompile(`\b(increase|rate|irate|resets)\s*\(\s*([a-zA-Z_][a-zA-Z0-9_]*)`)

// gaugeMetricNames scans the Go tree for `Name: "x", Kind: …Gauge` / `Kind: Gauge` sample declarations and
// returns the metric names TG publishes as gauges. Comment lines are stripped — this file names
// tg_ingest_source_recent_total in prose, and a comment is not a declaration.
func gaugeMetricNames(t *testing.T) map[string]bool {
	t.Helper()
	// Name: "<metric>" … Kind: [metrics.]Gauge — on one line, which is how every sample in this tree is
	// written. A declaration split across lines would be missed; the floor below is what catches that.
	decl := regexp.MustCompile(`Name:\s*"([a-z_][a-z0-9_]*)"[^\n]*?Kind:\s*(?:metrics\.)?Gauge`)
	out := map[string]bool{}
	roots := []string{filepath.Join("..", "cmd"), filepath.Join("..", "core")}
	files := 0
	for _, root := range roots {
		err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
			if err != nil || info.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			b, rerr := os.ReadFile(path)
			if rerr != nil {
				return nil
			}
			files++
			var code []string
			for _, ln := range strings.Split(string(b), "\n") {
				if strings.HasPrefix(strings.TrimSpace(ln), "//") {
					continue
				}
				code = append(code, ln)
			}
			for _, m := range decl.FindAllStringSubmatch(strings.Join(code, "\n"), -1) {
				out[m[1]] = true
			}
			return nil
		})
		if err != nil {
			t.Fatalf("walk %s: %v", root, err)
		}
	}
	if files == 0 {
		t.Fatal("walked ZERO Go files — this guard is examining nothing")
	}
	return out
}

// alertExprs returns every alert's expression, keyed by alert name.
func alertExprs(t *testing.T) map[string]string {
	t.Helper()
	raw, err := os.ReadFile("monitoring/alert.rules.yml")
	if err != nil {
		t.Fatalf("read alert.rules.yml: %v", err)
	}
	var doc struct {
		Groups []struct {
			Rules []struct {
				Alert string `yaml:"alert"`
				Expr  string `yaml:"expr"`
			} `yaml:"rules"`
		} `yaml:"groups"`
	}
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("parse alert.rules.yml: %v", err)
	}
	out := map[string]string{}
	for _, g := range doc.Groups {
		for _, r := range g.Rules {
			if r.Alert != "" {
				out[r.Alert] = r.Expr
			}
		}
	}
	return out
}

// KILLING MUTATION: put `increase(tg_ingest_source_recent_total[1h]) == 0` back into
// IngestConnectorDeafToItsUpstream (the state this shipped in). RED, naming both the alert and the gauge.
func TestNoAlertAppliesACounterFunctionToAGauge(t *testing.T) {
	gauges := gaugeMetricNames(t)
	exprs := alertExprs(t)

	// VACUITY FLOORS, both sides. Either scanner silently returning nothing makes this test pass over any
	// configuration — which is precisely the defect it exists to catch, one level up.
	if len(gauges) < 20 {
		t.Fatalf("found only %d gauge metric name(s) — the Go scanner has stopped matching, so a counter "+
			"function on a gauge would no longer be detected", len(gauges))
	}
	if len(exprs) < 10 {
		t.Fatalf("parsed only %d alert(s) from alert.rules.yml — the rules reader has stopped matching", len(exprs))
	}
	// And prove the two scanners actually intersect: a gauge this repo demonstrably publishes must be found.
	if !gauges["tg_ingest_source_recent_total"] {
		t.Fatal("the Go scanner did not find tg_ingest_source_recent_total, which is declared as a Gauge — " +
			"the extraction is wrong, so a real violation would also be missed")
	}

	var bad []string
	for alert, expr := range exprs {
		for _, m := range counterOnlyFnRe.FindAllStringSubmatch(expr, -1) {
			fn, metric := m[1], m[2]
			if gauges[metric] {
				bad = append(bad, alert+": "+fn+"("+metric+") — "+metric+" is a GAUGE")
			}
		}
	}
	sort.Strings(bad)
	if len(bad) > 0 {
		t.Errorf("alert expressions apply a counter-only function to a gauge:\n  %s\n\n"+
			"increase()/rate()/irate() assume monotonic growth. A gauge that moves DOWN reads as a counter "+
			"reset, so the function extrapolates a large positive value instead of the 0 the rule is testing "+
			"for — and the rule can never fire. That is worse than a missing rule: it occupies the space "+
			"where the gap would have been noticed. Use the gauge directly, or a freshness gauge "+
			"(tg_ingest_source_last_seen_seconds), or delta() if a gauge DIFFERENCE is really what is meant.",
			strings.Join(bad, "\n  "))
	}
}

// The specific rule, pinned by the shape of its answer rather than by its text.
//
// It must key on the upstream set GROWING, not on its LEVEL. LibreNMS intake here is PUSH — the worker logs
// "alert intake is PUSH … active-alert pull OFF" every boot — and an edge-triggered transport fires on a
// STATE TRANSITION. A stable set of long-firing alerts produces no pushes, and that is correct. Measured
// over 10h on the live series: available sat flat at 83 from 03:59 to 10:59 with a healthy connector. A
// level-based rule (`available > 0`) fires continuously through that — the SidecarDown pathology, an
// always-on alert that teaches an operator to ignore the signal.
//
// KILLING MUTATIONS: (a) drop the delta() leg entirely; (b) replace it with the LEVEL form
// `tg_ingest_upstream_available > 0`, which is the shape that reads correct and fires on a healthy estate.
// Both RED.
func TestTheConnectorBrokenRuleStillDistinguishesAQuietEstate(t *testing.T) {
	expr, ok := alertExprs(t)["IngestConnectorDeafToItsUpstream"]
	if !ok {
		t.Fatal("IngestConnectorDeafToItsUpstream is gone — TG's only connector-broken signal")
	}
	flat := strings.Join(strings.Fields(expr), " ")
	for _, want := range []string{
		"tg_ingest_upstream_readable == 1",             // an unreadable upstream's count means nothing
		"delta(tg_ingest_upstream_available[1h]) > 0",  // the upstream set GREW — not merely that it is non-empty
		"tg_ingest_source_last_seen_seconds",           // freshness, not a counter function over a gauge
	} {
		if !strings.Contains(flat, want) {
			t.Errorf("the rule no longer requires %q.\nGot: %s", want, flat)
		}
	}
	if strings.Contains(flat, "increase(") {
		t.Errorf("the rule is back to a counter function; see TestNoAlertAppliesACounterFunctionToAGauge.\nGot: %s", flat)
	}
	// THE LEVEL FORM IS THE TRAP. It reads correct — "the upstream has alerts and we got none" — and fires
	// continuously against a healthy push connector whose estate simply is not changing. Forbid it by shape:
	// a bare comparison of the gauge with no delta() around it.
	if regexp.MustCompile(`[^)]tg_ingest_upstream_available\s*[<>=]`).MatchString(flat) {
		t.Errorf("the rule compares tg_ingest_upstream_available by LEVEL. On a PUSH source a stable set of "+
			"long-firing alerts generates no transports, so this fires continuously through a healthy "+
			"estate (measured: flat at 83 for 7h). Key it on delta().\nGot: %s", flat)
	}
}
