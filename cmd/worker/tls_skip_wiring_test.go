package main

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// TG-367. A TLS-skip flag is honoured silently and looks identical whether it is load-bearing or a
// leftover. `config.SkipIsNecessary` answers that question — but only where it is CALLED. This project
// has shipped a correct resolver that no composition root invoked more than once (TG-344 logged "no
// prober wired" on every boot for days), so the resolver's own unit tests are not the guard.
//
// The property: EVERY place main() disables TLS verification must also report whether the skip is
// necessary. Comments are stripped first (stripGoComments, shared with upstream_available_test.go, which
// carries its own negative control) because a guard of mine has passed on its own comment before.

// disableTLSSites finds each block where main() constructs an insecure transport. estateHTTPClient(true)
// is the single constructor for "do not verify" in this file, so it is the anchor.
var disableTLSSite = regexp.MustCompile(`estateHTTPClient\(true\)`)

func TestEveryTLSDisableSiteReportsWhetherTheSkipIsNecessary(t *testing.T) {
	raw, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatalf("read main.go: %v", err)
	}
	src := stripGoComments(string(raw))

	sites := disableTLSSite.FindAllStringIndex(src, -1)
	if len(sites) == 0 {
		t.Fatal("VACUITY FLOOR: found no estateHTTPClient(true) call in main.go. Either the insecure " +
			"transport is constructed some other way now — in which case this guard is watching nothing " +
			"and must be re-anchored — or the anchor was renamed.")
	}

	for _, site := range sites {
		// Scope to the enclosing block, not the whole file: a file-wide Contains is satisfied by an
		// occurrence thousands of lines away, which is how an earlier guard of mine survived gutting
		// its own call site.
		start := site[0]
		end := site[1] + 400
		if end > len(src) {
			end = len(src)
		}
		window := src[start:end]
		if !strings.Contains(window, "reportTLSSkip(") {
			t.Errorf("a TLS-verification skip is installed at offset %d with no reportTLSSkip() beside it.\n"+
				"Every skip must state whether it is NECESSARY, measured against the endpoint — TG carried "+
				"TG_PVE_INSECURE for the lifetime of the deployment against an endpoint that verifies fine "+
				"under its FQDN, and the boot log asserted it was self-signed.\nwindow:\n%s",
				start, window)
		}
	}
}

// TestTheSkipReportIsNotGatedOnTheThingItAudits guards the subtler regression: moving the report inside a
// condition that is only true when the skip is ALREADY believed necessary would make it self-confirming.
func TestTheSkipReportIsNotGatedOnTheThingItAudits(t *testing.T) {
	raw, err := os.ReadFile("pve_liveness_config.go")
	if err != nil {
		t.Fatalf("read pve_liveness_config.go: %v", err)
	}
	src := stripGoComments(string(raw))
	i := strings.Index(src, "func reportTLSSkip(")
	if i < 0 {
		t.Fatal("reportTLSSkip is gone — the boot log no longer says whether a TLS skip is earned")
	}
	body := src[i:]
	if j := strings.Index(body[1:], "\nfunc "); j >= 0 {
		body = body[:j]
	}
	if !strings.Contains(body, "SkipIsNecessary(") {
		t.Fatal("reportTLSSkip no longer calls config.SkipIsNecessary — it reports a verdict nobody measured")
	}
	// It must log unconditionally. An `if v.Necessary` around the log would suppress the ONE case that
	// matters: the skip that is not needed.
	if strings.Contains(body, "if v.Necessary") || strings.Contains(body, "if !v.Necessary") {
		t.Fatal("reportTLSSkip logs conditionally on the verdict. The UNNECESSARY case is the finding; " +
			"suppressing either branch turns this into a check that cannot report its own subject.")
	}
}
