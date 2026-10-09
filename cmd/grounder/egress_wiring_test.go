package main

// TG-324 — THE GROUNDER MUST ACTUALLY INSTALL THE METER.
//
// core/egress was fully built, fully unit-tested, and installed in exactly one of the two TG binaries.
// The other one — this one — is on `tg-egress`, dials OpenBao off-host, and is the ONLY TG process on the
// published `tg-frontdoor`. Measured before this change: grounder:8080/metrics served 200 with zero
// tg_egress_* series.
//
// That is this repo's signature defect shape, not a missing feature: a control that EXISTS and does not
// run. So the load-bearing assertion here is not "the meter works" (core/egress owns that) — it is
// "http.DefaultTransport was replaced", because every outbound client in this process that does not set
// its own Transport resolves to it at call time.

import (
	"net/http"
	"testing"

	"github.com/territory-grounder/grounder/core/egress"
)

// THE LOAD-BEARING ORACLE.
//
// KILLING MUTATION: delete `http.DefaultTransport = m` in core/egress/install.go. RED here.
func TestTheGrounderInstallReplacesDefaultTransport(t *testing.T) {
	orig := http.DefaultTransport
	t.Cleanup(func() { http.DefaultTransport = orig })

	m := egress.Install(egress.InstallConfig{
		Environ:   []string{"TG_OPENBAO_ADDR=https://vault.example.test:8200"},
		Component: "grounder",
		Logf:      func(string, ...any) {},
	})
	if m == nil {
		t.Fatal("Install returned no meter")
	}
	if http.DefaultTransport == orig {
		t.Fatal("http.DefaultTransport was NOT replaced. Every client in this process built as " +
			"http.DefaultClient or &http.Client{Timeout: …} without a Transport would egress unmetered — " +
			"which is precisely the state the grounder was in before TG-324.")
	}
}

// THE SERIES MUST BE ABSENT WHEN NO METER IS INSTALLED, not a row of zeros.
//
// A fabricated `tg_egress_requests_total 0` makes "this process has no meter" and "this process has a
// meter and made no outbound calls" the same observation — reproducing the very defect one layer up, and
// this time inside the thing meant to reveal it.
func TestNoMeterPublishesNoSeriesRatherThanZeros(t *testing.T) {
	prev := grounderEgress
	t.Cleanup(func() { grounderEgress = prev })

	grounderEgress = nil
	if got := grounderEgressSamples(); len(got) != 0 {
		t.Errorf("an uninstalled meter published %d series; want 0. A zero row would read as a clean, "+
			"working meter to every dashboard and alert.", len(got))
	}
}

// ...AND PRESENT ONCE IT IS. The anti-vacuity half: without this, deleting the whole lane would satisfy
// the test above forever.
func TestAnInstalledMeterPublishesTheWholeLane(t *testing.T) {
	orig := http.DefaultTransport
	prev := grounderEgress
	t.Cleanup(func() { http.DefaultTransport = orig; grounderEgress = prev })

	grounderEgress = egress.Install(egress.InstallConfig{
		Environ:   []string{"TG_OPENBAO_ADDR=https://vault.example.test:8200"},
		Component: "grounder",
		Logf:      func(string, ...any) {},
	})

	got := grounderEgressSamples()
	if len(got) != 9 {
		t.Fatalf("published %d series, want the full 9-series lane — a partial lane means an alert that "+
			"exists for the worker has no counterpart here", len(got))
	}
	// The vacuity gauge specifically: an operator reading a flat 0 off-allowlist MUST be able to tell
	// whether the meter was comparing against anything at all.
	var sawRules, sawEnforcing bool
	for _, s := range got {
		if s.Name == "tg_egress_allowlist_rules" {
			sawRules = true
			if s.Value == 0 {
				t.Error("tg_egress_allowlist_rules is 0 against a declared OpenBao address — the destination " +
					"scan found nothing, so every call would be reported off-allowlist and a later enforce " +
					"flip would block everything")
			}
		}
		if s.Name == "tg_egress_enforcing" {
			sawEnforcing = true
		}
		if s.Labels["component"] != "grounder" {
			t.Errorf("series %q carries component=%q — the worker publishes the same NAMES, so without a "+
				"distinguishing label the two processes' lanes are indistinguishable in Prometheus",
				s.Name, s.Labels["component"])
		}
	}
	if !sawRules || !sawEnforcing {
		t.Error("the lane is missing tg_egress_allowlist_rules and/or tg_egress_enforcing — the two series " +
			"that say whether the reading means anything")
	}
}

// The default posture must be METER. Enforcing on first install would gate this process's OpenBao calls
// on an allowlist nobody has observed against real traffic — and the grounder cannot resolve its own read
// credential without OpenBao, so a wrong allowlist here is not a degraded grounder, it is no grounder.
func TestTheGrounderShipsInMeterModeNotEnforce(t *testing.T) {
	orig := http.DefaultTransport
	t.Cleanup(func() { http.DefaultTransport = orig })

	m := egress.Install(egress.InstallConfig{
		Environ:   []string{"TG_OPENBAO_ADDR=https://vault.example.test:8200"},
		ModeRaw:   grounderEgressModeDefault,
		Component: "grounder",
		Logf:      func(string, ...any) {},
	})
	if m.Snapshot().Enforcing {
		t.Errorf("the grounder's compiled default resolves to ENFORCE (%q). It has never been observed "+
			"against real traffic; the workers earned enforce by running metered until off-allowlist held "+
			"flat at 0 against a non-zero allowlist, and this plane must be flipped the same way.",
			grounderEgressModeDefault)
	}
}

// TG-415 — THE METER MUST REACH OPENBAO, the destination this process's egress grant exists for.
//
// deploy/egress_parity_test.go records the grounder's reason as "OpenBao for its own read credential and
// the console WRITER AppRole, plus LDAPS to FreeIPA". LDAPS is not HTTP and was always a stated residual.
// OpenBao was the covered half — and it was not covered, because vault.New builds its own transport.
//
// KILLING MUTATION: make meteredBaoTransport return nil unconditionally. RED.
func TestTheOpenBaoClientIsHandedThisProcessMeter(t *testing.T) {
	orig := http.DefaultTransport
	prev := grounderEgress
	t.Cleanup(func() { http.DefaultTransport = orig; grounderEgress = prev })

	grounderEgress = egress.Install(egress.InstallConfig{
		Environ:   []string{"TG_OPENBAO_ADDR=https://vault.example.test:8200"},
		Component: "grounder",
		Logf:      func(string, ...any) {},
	})
	if got := meteredBaoTransport(); len(got) == 0 {
		t.Fatal("no wire option produced while a meter IS installed — the OpenBao client would build its " +
			"own unmetered transport, and this process's meter would keep reporting enforcing=1 over a " +
			"request count of zero, which reads as a clean estate")
	}
}

// ...AND IT MUST DEGRADE TO BUILDING THE CLIENT ANYWAY. Refusing to resolve secrets in order to measure
// them trades a blind spot for an outage: with no meter installed the delivery client must still be built.
func TestNoMeterStillLetsTheOpenBaoClientBeBuilt(t *testing.T) {
	prev := grounderEgress
	t.Cleanup(func() { grounderEgress = prev })

	grounderEgress = nil
	if got := meteredBaoTransport(); len(got) != 0 {
		t.Errorf("produced %d wire option(s) with no meter installed — a wrap around a nil meter would "+
			"panic on the first secret resolution, at boot, before anything is serving", len(got))
	}
}
