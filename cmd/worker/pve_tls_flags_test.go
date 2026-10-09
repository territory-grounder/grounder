package main

import (
	"strings"
	"testing"
)

// TWO ENV VARS GOVERN ONE DECISION, AND A HALF-SET PAIR FAILS IN A WAY THAT DOES NOT LOOK LIKE TLS.
//
// TG_PVE_INSECURE controls the estate reader and the PVE actor-evidence reader; TG_PROXMOX_INSECURE controls
// actuation. The pve-liveness detector follows whichever credential pair it resolved (TG-350), so its
// attribution is a PARAMETER — a fixed one would name the wrong flag on a split deployment.
// Same process, same Proxmox endpoint. Set one and half of TG's
// conversations skip verification while the other half enforce it — and the enforcing half fails silently,
// because an actor-evidence reader that cannot reach its backend contributes NO evidence, which reads
// downstream as `unattributable` rather than as an error.
func TestPveTLSFlagDisagreementIsReportedWithBothPathSets(t *testing.T) {
	for _, c := range []struct {
		name         string
		pve, proxmox bool
		liveness     string
		wantDisagree bool
		mustName     []string
	}{
		{name: "both unset — the default, nothing to say", pve: false, proxmox: false, liveness: "TG_PROXMOX_INSECURE", wantDisagree: false},
		{name: "both set — consistent, nothing to say", pve: true, proxmox: true, liveness: "TG_PROXMOX_INSECURE", wantDisagree: false},
		{
			name: "only TG_PVE_INSECURE set, detector on the actuation flag", pve: true, proxmox: false,
			liveness: "TG_PROXMOX_INSECURE", wantDisagree: true,
			// The message must name the flag that is ENFORCING and the paths it breaks — that is the half an
			// operator is not thinking about, and naming only the flag they set would send them to re-read a
			// setting that is already doing what they asked.
			mustName: []string{"TG_PROXMOX_INSECURE", "pve-liveness", "actuation", "unattributable"},
		},
		{
			name: "only TG_PROXMOX_INSECURE set, detector on the actuation flag", pve: false, proxmox: true,
			liveness: "TG_PROXMOX_INSECURE", wantDisagree: true,
			mustName: []string{"TG_PVE_INSECURE", "estate reader", "actor-evidence", "unattributable"},
		},
		{
			// TG-350's live shape: the detector reads with the estate pair, so it is on the SKIPPING side and
			// the only path still enforcing is actuation. Naming pve-liveness as broken here would send an
			// operator to debug a detector that is working.
			name: "detector on the estate-read flag, which is the one that is set", pve: true, proxmox: false,
			liveness: "TG_PVE_INSECURE", wantDisagree: true,
			mustName: []string{"TG_PVE_INSECURE is TRUE", "pve-liveness detector) while TG_PROXMOX_INSECURE"},
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			disagree, detail := pveTLSFlagDisagreement(c.pve, c.proxmox, c.liveness)
			if disagree != c.wantDisagree {
				t.Fatalf("disagree = %v, want %v (pve=%v proxmox=%v)", disagree, c.wantDisagree, c.pve, c.proxmox)
			}
			if !c.wantDisagree {
				if detail != "" {
					t.Errorf("agreeing flags produced a message: %q — a warning that fires on a correct "+
						"config trains operators to ignore the line", detail)
				}
				return
			}
			for _, want := range c.mustName {
				if !strings.Contains(detail, want) {
					t.Errorf("the disagreement message does not mention %q, so an operator cannot tell WHICH "+
						"half of TG is still enforcing.\nGot: %s", want, detail)
				}
			}
		})
	}
}

// The two directions must not produce the SAME message — that would mean the report names a fixed half
// regardless of which flag was actually set, and would point at the wrong paths half the time.
func TestTheTwoDirectionsAreDistinguished(t *testing.T) {
	_, a := pveTLSFlagDisagreement(true, false, "TG_PROXMOX_INSECURE")
	_, b := pveTLSFlagDisagreement(false, true, "TG_PROXMOX_INSECURE")
	if a == b {
		t.Fatalf("both disagreement directions produced an identical message — it cannot be naming the "+
			"actually-enforcing paths:\n%s", a)
	}
	// Each direction must state its OWN flag as the true one, in that order.
	if !strings.HasPrefix(a, "TG_PVE_INSECURE is TRUE") {
		t.Errorf("pve=true direction does not lead with TG_PVE_INSECURE: %s", a)
	}
	if !strings.HasPrefix(b, "TG_PROXMOX_INSECURE is TRUE") {
		t.Errorf("proxmox=true direction does not lead with TG_PROXMOX_INSECURE: %s", b)
	}
}
