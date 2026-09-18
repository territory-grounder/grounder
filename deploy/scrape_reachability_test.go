package deploy

// A SCRAPE TARGET THE SCRAPER CANNOT REACH (TG-370).
//
// deploy/monitoring/prometheus.yml names what to scrape. deploy/docker-compose.yml decides what the
// prometheus container can reach. Nothing compared them, so on 2026-08-06 the two disagreed in the worst
// available direction:
//
//	- job_name: opus-sidecar
//	  static_configs:
//	    - targets: ["dc1claude01:8094"]        # off-host
//
//	prometheus:
//	  networks: [tg-backplane, tg-frontdoor]        # internal:true + masquerade off = no route off the box
//
// Measured from inside the running container, by NAME and by RAW IP, both `wget: download timed out` after
// 10.0s — while the identical GET from the TG host returned HTTP 200 in 2ms and the sidecar was serving
// 0.0.0.0:8094. So `SidecarDown`, severity CRITICAL and the only such alert TG raises about its single model
// brain, fired continuously against a healthy service. An always-firing critical is worse than no alert: it
// is the one that teaches an operator to ignore criticals, and it made the WRONG conclusion the easy one —
// earlier the same day I read it as the sidecar being down when it was up.
//
// egress_parity_test.go could not catch this. It compares the compose file against a table declared in Go,
// and never reads prometheus.yml — so a TARGET added without the network it needs is outside its population
// entirely. Its header even predicts the scenario ("prometheus, when someone adds an off-host scrape
// target") and the compose file carries the instruction that was not followed ("this service needs
// tg-egress — add it here"). Both were right and neither was a check.
//
// This is that check: every scrape target must either be a compose service (reachable east-west on the
// backplane), be localhost, or prometheus must hold tg-egress.

import (
	"fmt"
	"os"
	"sort"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// networkNames decodes a compose `networks:` key in either legal shape — a sequence of names, or a
// mapping of name → options (the form a service takes the moment it needs `ipv4_address`, as the workers
// did in TG-381). Struct-decoding it as []string rejects the mapping form file-wide: one pinned service
// and every guard in this package that parses compose dies of a parse error, which reads like the guard
// working. Any test here that only needs the NAMES should use this type.
type networkNames []string

func (n *networkNames) UnmarshalYAML(node *yaml.Node) error {
	switch node.Kind {
	case yaml.SequenceNode:
		var s []string
		if err := node.Decode(&s); err != nil {
			return err
		}
		*n = s
	case yaml.MappingNode:
		var m map[string]any
		if err := node.Decode(&m); err != nil {
			return err
		}
		for name := range m {
			*n = append(*n, name)
		}
		sort.Strings(*n)
	default:
		return fmt.Errorf("networks: unsupported yaml node kind %v", node.Kind)
	}
	return nil
}

// scrapeTargetHostsByJob returns every static target host in prometheus.yml, paired with its job, so a failure names
// the job an operator would go and look at.
func scrapeTargetHostsByJob(t *testing.T) map[string]string {
	t.Helper()
	raw, err := os.ReadFile("monitoring/prometheus.yml")
	if err != nil {
		t.Fatalf("read prometheus.yml: %v", err)
	}
	var doc struct {
		ScrapeConfigs []struct {
			JobName       string `yaml:"job_name"`
			StaticConfigs []struct {
				Targets []string `yaml:"targets"`
			} `yaml:"static_configs"`
		} `yaml:"scrape_configs"`
	}
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("parse prometheus.yml: %v", err)
	}
	out := map[string]string{}
	for _, j := range doc.ScrapeConfigs {
		for _, sc := range j.StaticConfigs {
			for _, tgt := range sc.Targets {
				host := tgt
				if i := strings.LastIndex(tgt, ":"); i > 0 {
					host = tgt[:i]
				}
				out[host] = j.JobName
			}
		}
	}
	return out
}

// composeServiceNames returns the compose service names — the hosts prometheus can reach east-west on the
// backplane without any route off the box.
func composeServiceNames(t *testing.T) map[string]bool {
	t.Helper()
	raw, err := os.ReadFile("docker-compose.yml")
	if err != nil {
		t.Fatalf("read compose: %v", err)
	}
	var doc struct {
		Services map[string]struct {
			Networks networkNames `yaml:"networks"`
		} `yaml:"services"`
	}
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("parse compose: %v", err)
	}
	out := make(map[string]bool, len(doc.Services))
	for n := range doc.Services {
		out[n] = true
	}
	return out
}

func prometheusNetworks(t *testing.T) []string {
	t.Helper()
	raw, err := os.ReadFile("docker-compose.yml")
	if err != nil {
		t.Fatalf("read compose: %v", err)
	}
	var doc struct {
		Services map[string]struct {
			Networks networkNames `yaml:"networks"`
		} `yaml:"services"`
	}
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("parse compose: %v", err)
	}
	p, ok := doc.Services["prometheus"]
	if !ok {
		t.Fatal("no prometheus service in docker-compose.yml — this guard is scanning a file that no longer " +
			"describes the deployment")
	}
	return p.Networks
}

// KILLING MUTATION: remove tg-egress from the prometheus service (the state this defect shipped in). RED,
// naming opus-sidecar. The guard is written against the RELATIONSHIP, not against that one target, so the
// next off-host job someone adds fails the same way whether or not anyone remembers this incident.
func TestEveryOffHostScrapeTargetHasAPathToIt(t *testing.T) {
	targets := scrapeTargetHostsByJob(t)
	services := composeServiceNames(t)

	// VACUITY FLOOR. A parser that silently stops matching returns an empty map and this test passes while
	// checking nothing — which is the exact defect class it was written for.
	if len(targets) == 0 {
		t.Fatal("parsed ZERO scrape targets from prometheus.yml — this guard is examining nothing and would " +
			"pass on any network configuration")
	}
	if len(services) < 5 {
		t.Fatalf("parsed only %d compose service(s) — the compose reader has stopped matching, so every "+
			"target would look off-host (or none would)", len(services))
	}

	hasEgress := false
	for _, n := range prometheusNetworks(t) {
		if n == netEgress {
			hasEgress = true
		}
	}

	var offHost []string
	for host, job := range targets {
		if services[host] || host == "localhost" || host == "127.0.0.1" {
			continue // east-west on the backplane, or prometheus itself
		}
		offHost = append(offHost, job+" -> "+host)
	}
	if len(offHost) > 0 && !hasEgress {
		t.Errorf("prometheus.yml scrapes %v, which is not a compose service, but the prometheus container is "+
			"on %v — tg-backplane is internal:true (no gateway) and tg-frontdoor runs with masquerade "+
			"DISABLED, so the packet's source stays 172.22.x and the destination has no route back. The "+
			"scrape times out, `up` reads 0, and any alert built on it fires forever against a healthy "+
			"service. Add tg-egress to prometheus AND record it in egress_parity_test.go.",
			offHost, prometheusNetworks(t))
	}
}

// THE OTHER DIRECTION. Egress granted with no off-host target left behind is a route off the box that
// nothing needs — the same doctrine egress_parity_test.go applies, extended to the reason rather than the
// declaration. Without this, deleting the opus-sidecar job would leave prometheus with permanent outbound
// and every test still green.
//
// KILLING MUTATION: delete the opus-sidecar job from prometheus.yml while leaving tg-egress in place. RED.
func TestPrometheusDoesNotHoldEgressWithNothingOffHostToScrape(t *testing.T) {
	targets := scrapeTargetHostsByJob(t)
	services := composeServiceNames(t)
	hasEgress := false
	for _, n := range prometheusNetworks(t) {
		if n == netEgress {
			hasEgress = true
		}
	}
	if !hasEgress {
		return // nothing to justify
	}
	for host := range targets {
		if !services[host] && host != "localhost" && host != "127.0.0.1" {
			return // justified
		}
	}
	t.Error("prometheus holds tg-egress but every scrape target is a compose service or localhost — a route " +
		"off the box that nothing uses. Remove it here and in egress_parity_test.go, or the grant stops " +
		"being a decision and becomes a leftover.")
}
