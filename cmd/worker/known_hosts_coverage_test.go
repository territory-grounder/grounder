package main

import (
	"os"
	"strings"
	"testing"

	"github.com/territory-grounder/grounder/core/metrics"
)

func covSample(ss []metrics.Sample, name string) (metrics.Sample, bool) {
	for _, s := range ss {
		if s.Name == name {
			return s, true
		}
	}
	return metrics.Sample{}, false
}

// ★ THE SPLIT THAT KEEPS THIS GAUGE HONEST (TG-271).
//
// Alertmanager's `host` label carries Kubernetes component names. Measured live 2026-08-06: of 86 alert
// host labels, 52 were uncovered and exactly HALF of those — cilium-agent, coredns, kube-etcd,
// node-exporter, tetragon, seaweedfs-master — are not hosts TG should ever hold a key for. A naive
// covered/alerted gauge reads 34/86 and is red forever, and a permanently red alarm is how the real one
// gets ignored.
func TestOnlyResolvableHostsCountAsUncovered(t *testing.T) {
	hosts := []string{"dc1pve01", "dc1rtr01", "cilium-agent", "coredns"}
	known := func(h string) bool { return h == "dc1pve01" }
	resolvable := func(h string) bool { return strings.HasPrefix(h, "dc1") }

	c := measureCoverage(hosts, known, resolvable, 42)

	if c.Alerted != 4 {
		t.Errorf("Alerted = %d, want 4 — the denominator is every host TG was asked about", c.Alerted)
	}
	if c.Covered != 1 {
		t.Errorf("Covered = %d, want 1", c.Covered)
	}
	if c.UncoveredResolvable != 1 {
		t.Fatalf("UncoveredResolvable = %d, want 1. Only dc1rtr01 is a real host with no key; counting "+
			"cilium-agent and coredns makes this gauge permanently red and it stops being read.", c.UncoveredResolvable)
	}
}

// A covered host must never also count as uncovered, or the two numbers stop summing to anything.
func TestACoveredHostIsNeverAlsoUncovered(t *testing.T) {
	c := measureCoverage([]string{"h1"}, func(string) bool { return true }, func(string) bool { return true }, 1)
	if c.Covered != 1 || c.UncoveredResolvable != 0 {
		t.Errorf("covered=%d uncoveredResolvable=%d for a host that is both known and resolvable; "+
			"want 1 and 0 — known must win", c.Covered, c.UncoveredResolvable)
	}
}

// THE VACUITY FLOOR. Every series is emitted even at zero: absent and zero are different claims, and
// "0 uncovered" from a worker that measured nothing is the inverse of the truth.
func TestEverySeriesIsEmittedEvenAtZero(t *testing.T) {
	ss := coverageSamples(knownHostsCoverage{})
	for _, want := range []string{
		"tg_hostdiag_hosts_alerted",
		"tg_hostdiag_hosts_covered",
		"tg_hostdiag_hosts_uncovered_resolvable",
		"tg_hostdiag_known_hosts_entries",
	} {
		if _, ok := covSample(ss, want); !ok {
			t.Errorf("%s was not emitted for an empty pass — its absence is indistinguishable from a healthy "+
				"zero, which is the exact conflation this gate exists to end", want)
		}
	}
}

// A worker with no verifier must publish NOTHING rather than zeros. Zeros would read as full coverage on a
// process that cannot verify a single host.
func TestNoVerifierPublishesNothing(t *testing.T) {
	read := startKnownHostsCoverageJob(t.Context(), nil, nil, 0, 0, 0, nil)
	if got := read(); got != nil {
		t.Errorf("a worker with no verifier published %d sample(s); want none. Publishing 0-uncovered "+
			"without a verifier reports perfect coverage for a process that can diagnose nothing.", len(got))
	}
}

// THE COMPOSITION ROOT. Everything above exercises the job directly; none of it notices if main() never
// calls it, which is how the original defect survived — the tools were registered and the boot log was
// cheerful while every read failed.
func TestMainWiresTheKnownHostsCoverageJob(t *testing.T) {
	src := stripCovComments(readCovMain(t))
	if !strings.Contains(src, "withKnownHostsCoverage(") {
		t.Fatal("main.go never wires withKnownHostsCoverage — the coverage job exists, is fully tested, and " +
			"publishes nothing, so host-diagnostic reach stays exactly as unmeasured as before")
	}
	// It must read the SAME env var the diagnostic tools use, or the gauge measures a different file from
	// the one the tools dial with and can report coverage the agent does not have.
	if !strings.Contains(src, `knownHostsCoverageInputs(getenv("TG_HOSTDIAG_KNOWN_HOSTS"`) {
		t.Error("the coverage job does not build from TG_HOSTDIAG_KNOWN_HOSTS. Measuring a different file " +
			"from the one the tools verify against would report reach the agent does not have.")
	}
}

func TestTheCoverageWiringGuardIgnoresProse(t *testing.T) {
	prose := "// withKnownHostsCoverage(startKnownHostsCoverageJob(ctx))\nfunc main() {}\n"
	if got := stripCovComments(prose); strings.Contains(got, "withKnownHostsCoverage(") {
		t.Fatalf("stripCovComments left commented-out code in place; got %q", got)
	}
}

func readCovMain(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatalf("read main.go: %v", err)
	}
	if len(b) == 0 {
		t.Fatal("main.go is empty — the assertions would be vacuous")
	}
	return string(b)
}

func stripCovComments(src string) string {
	var b strings.Builder
	for _, l := range strings.Split(src, "\n") {
		if strings.HasPrefix(strings.TrimSpace(l), "//") {
			continue
		}
		b.WriteString(l)
		b.WriteString("\n")
	}
	return b.String()
}
