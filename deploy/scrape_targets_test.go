package deploy

// TG-413/TG-324 — A SCRAPE TARGET THAT STAYS GREEN WHILE POINTING AT THE WRONG MACHINE.
//
// When the model sidecar was migrated onto this box, prometheus.yml kept scraping the old address. The
// target did not go down — the old proxy is still running as a cold spare — so:
//
//	prometheus target opus-sidecar  http://dc1claude01:8094/metrics  health=up
//	  cold spare, same instant:  sidecar_completions_total{outcome="ok"} 2179
//	  the sidecar actually serving every production completion: NOT SCRAPED AT ALL
//
// Every dashboard and every alert about "the brain" was reading a machine that had stopped being the
// brain. Nothing was red. This is the wrong-subject class rather than the missing-signal class, and it is
// strictly harder to notice: a broken scrape announces itself, a correct scrape of the wrong host does
// not.
//
// `lint-forbidden` 6/7 (STONITH) refuses estate literals in shipped artifacts, but its scan set is Go
// sources plus docker-compose.yml — prometheus.yml is not in it, which is why the literal survived there.
// Rather than widen the law-surface lint script, this pins the same property where it actually failed.

import (
	"os"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// scrapeTargetsByJob keeps the JOB NAME, which deploy/admin_surfaces_scraped_test.go's scrapeTargets
// deliberately flattens away (it only asks "is this address scraped at all"). The distinct name is not
// cosmetic: an identically-named helper in the same package is a compile error at best, and at worst two
// tests silently share one that answers a subtly different question.
func scrapeTargetsByJob(t *testing.T) map[string][]string {
	t.Helper()
	b, err := os.ReadFile("monitoring/prometheus.yml")
	if err != nil {
		t.Fatalf("read monitoring/prometheus.yml: %v", err)
	}
	var doc struct {
		ScrapeConfigs []struct {
			JobName       string `yaml:"job_name"`
			StaticConfigs []struct {
				Targets []string `yaml:"targets"`
			} `yaml:"static_configs"`
		} `yaml:"scrape_configs"`
	}
	if err := yaml.Unmarshal(b, &doc); err != nil {
		t.Fatalf("parse prometheus.yml: %v", err)
	}
	out := map[string][]string{}
	for _, sc := range doc.ScrapeConfigs {
		for _, st := range sc.StaticConfigs {
			out[sc.JobName] = append(out[sc.JobName], st.Targets...)
		}
	}
	return out
}

// NO SCRAPE TARGET MAY NAME AN ESTATE HOST.
//
// Not a style rule. A hostname compiled into a shipped monitoring config is site configuration in the
// image: it cannot be repointed by a deploy, it survives the machine it names being decommissioned, and —
// as measured here — it keeps reporting healthy about the wrong subject.
//
// KILLING MUTATION: restore `dc1claude01:8094` as the opus-sidecar target. RED.
func TestNoScrapeTargetNamesAnEstateHost(t *testing.T) {
	targets := scrapeTargetsByJob(t)
	if len(targets) == 0 {
		t.Fatal("parsed no scrape jobs from prometheus.yml — every assertion below would be vacuous. " +
			"If the file moved, repoint this guard; do not let it go quiet.")
	}

	var checked int
	for job, addrs := range targets {
		if len(addrs) == 0 {
			t.Errorf("job %q declares no targets — a job that scrapes nothing produces no series, and its "+
				"alerts then evaluate against absent data rather than failing", job)
			continue
		}
		for _, a := range addrs {
			checked++
			for _, estate := range []string{"dc1", "dc2"} {
				if strings.Contains(a, estate) {
					t.Errorf("job %q scrapes %q — an estate hostname baked into a shipped config. It cannot "+
						"be repointed at deploy time, and if that machine is decommissioned while still "+
						"running (a cold spare, a staged migration) the target stays UP and the series "+
						"describe the wrong subject. Use a compose service name, or make it deploy-set.",
						job, a)
				}
			}
		}
	}
	if checked == 0 {
		t.Fatal("no target was actually examined — the loop asserted nothing and passed")
	}
}

// THE BRAIN MUST STILL BE SCRAPED. The anti-vacuity half: the cheapest way to satisfy the guard above is
// to delete the job, which would restore the exact condition TG-231 filed — "Prometheus scraped four
// targets and none was the brain" — while turning this file green.
func TestTheModelSidecarIsStillScraped(t *testing.T) {
	targets := scrapeTargetsByJob(t)
	addrs, ok := targets["opus-sidecar"]
	if !ok {
		t.Fatal("no `opus-sidecar` scrape job. TG runs SINGLE-BRAIN with no fallback, so this container is " +
			"the estate's most important one; TG-231 exists because it once had zero monitoring. Deleting " +
			"the job is not a way to satisfy TestNoScrapeTargetNamesAnEstateHost.")
	}
	var onBox bool
	for _, a := range addrs {
		if strings.HasPrefix(a, "sidecar:") {
			onBox = true
		}
	}
	if !onBox {
		t.Errorf("opus-sidecar scrapes %v, which does not include the on-box compose service. Production "+
			"dials the sidecar over tg-backplane by service name (TG-413); scraping anything else measures "+
			"a container that is not serving the traffic.", addrs)
	}
}

// PROMETHEUS MUST NOT HOLD EGRESS WHILE EVERY TARGET IS INTERNAL.
//
// The two halves have to agree or the guard is decorative: if a future off-host target is added, the
// network must come back with it (TestEveryOffHostScrapeTargetHasAPathToIt enforces that direction). This
// enforces the other one — the grant does not outlive its reason.
//
// KILLING MUTATION: add tg-egress back to the prometheus service. RED.
func TestPrometheusHoldsNoEgressWhileEveryTargetIsInternal(t *testing.T) {
	targets := scrapeTargetsByJob(t)
	var offHost []string
	for job, addrs := range targets {
		for _, a := range addrs {
			host, _, _ := strings.Cut(a, ":")
			// A compose service name has no dots and is not localhost; anything else may be off-box.
			if strings.Contains(host, ".") && host != "localhost" {
				offHost = append(offHost, job+" -> "+a)
			}
		}
	}

	svcs, _ := sidecarComposeDoc(t)["services"].(map[string]any)
	prom, ok := svcs["prometheus"].(map[string]any)
	if !ok {
		t.Fatal("no `prometheus` service in docker-compose.yml — this guard's subject is absent")
	}
	var nets []string
	for _, n := range prom["networks"].([]any) {
		nets = append(nets, n.(string))
	}
	hasEgress := strings.Contains(strings.Join(nets, ","), "tg-egress")

	switch {
	case len(offHost) == 0 && hasEgress:
		t.Errorf("prometheus holds tg-egress (networks=%v) while every scrape target is inside this compose "+
			"project. deploy/egress_parity_test.go requires each grant to carry an enumerated reason, and a "+
			"grant whose reason has become false is worse than an undocumented one — it reads as reviewed.",
			nets)
	case len(offHost) > 0 && !hasEgress:
		t.Errorf("prometheus scrapes off-host target(s) %v but is not on tg-egress — the scrape will time "+
			"out and the alert built on it will fire against a healthy target, which is how an operator "+
			"learns to ignore criticals", offHost)
	}
}
