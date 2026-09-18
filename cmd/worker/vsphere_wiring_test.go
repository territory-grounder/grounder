package main

import (
	"testing"

	"github.com/territory-grounder/grounder/core/estate"
)

// vsphereTestEnv builds a getenv(k, def) closure over a fixed map — a present key wins, an absent key returns
// the caller's default. Distinctively named (not a bare fakeEnv) to avoid a helper collision in package main.
func vsphereTestEnv(kv map[string]string) func(k, def string) string {
	return func(k, def string) string {
		if v, ok := kv[k]; ok {
			return v
		}
		return def
	}
}

// TestVsphereSourceIsDarkWithoutAURL is the ALIVENESS ORACLE for the vCenter estate source (TG-91): the exact
// construction path main() runs must return (nil, false) when TG_VSPHERE_URL is unset — so a deployment with
// no VMware estate ships byte-identical — and (src, true) carrying the vsphere provenance when it is set, so
// the source is genuinely WIRED rather than built-and-linked-into-nothing (the present-not-reaching defect
// this family guards against). main() calls vsphereEstateSource; this pins both of its outcomes.
//
// KILLING MUTATION: make vsphereEstateSource return (src, true) unconditionally → RED (the dark cases). Point
// its Source() at estate.SourcePVE → RED (the provenance case).
func TestVsphereSourceIsDarkWithoutAURL(t *testing.T) {
	if src, ok := vsphereEstateSource(vsphereTestEnv(nil)); ok || src != nil {
		t.Fatalf("vSphere source must be DARK when TG_VSPHERE_URL is unset: got ok=%v src=%v", ok, src)
	}
	// A blank/whitespace URL is still dark — a stray empty value must not arm a SOAP login loop.
	if src, ok := vsphereEstateSource(vsphereTestEnv(map[string]string{"TG_VSPHERE_URL": "   "})); ok || src != nil {
		t.Fatalf("a whitespace TG_VSPHERE_URL must stay dark: got ok=%v src=%v", ok, src)
	}

	src, ok := vsphereEstateSource(vsphereTestEnv(map[string]string{
		"TG_VSPHERE_URL":       "https://vcenter.example.com",
		"TG_VSPHERE_USER":      "svc-tg@vsphere.local",
		"TG_VSPHERE_TOKEN_REF": "env:VSPHERE_PASSWORD",
	}))
	if !ok || src == nil {
		t.Fatalf("vSphere source must be ENABLED when TG_VSPHERE_URL is set: got ok=%v src=%v", ok, src)
	}
	if src.Source() != estate.SourceVsphere {
		t.Fatalf("wired source has provenance %q, want %q — the estate graph would mislabel every vSphere edge",
			src.Source(), estate.SourceVsphere)
	}
}
