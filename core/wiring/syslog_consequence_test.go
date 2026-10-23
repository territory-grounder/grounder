package wiring

import (
	"strings"
	"testing"
)

// TG-363. The register's authored consequence for the syslog seam was BACKWARDS, and had never been
// measured.
//
// It read: "the agent has no device-log window: every firewall, switch and router incident is triaged
// without the device's own syslog". Probed 2026-08-06 against dc1syslogng01 through the production read
// guard — all 126 monitored dc1 hosts, no sampling, probed count asserted against the enumeration:
//
//	ships logs today  15 / 126
//	  ankh dc1actualbudget01 dc1ap01 dc1ap02 dc1ap03 dc1atlantis01
//	  dc1fw01 dc1haproxy01 dc1haproxy02 dc1k8s-node01 dc1k8s-node04
//	  dc1pve01 dc1pve02 dc1rtr01 dc1sw01
//
// The firewall, the router and the switch are all COVERED. Network gear is the best-covered class; the 111
// uncovered are overwhelmingly application guests.
//
// This register exists so a dark seam names what its darkness costs. A consequence that names the WRONG
// cost is worse than a missing one: it sends the reader to a repair that is already working. That is the
// defect this register catches, applied to itself.

func syslogSeam(t *testing.T) SeamSpec {
	t.Helper()
	for _, s := range All() {
		if s.ID == SeamSyslogRead {
			return s
		}
	}
	t.Fatalf("the register declares no %s seam", SeamSyslogRead)
	return SeamSpec{}
}

// KILLING MUTATION: restore the "every firewall, switch and router incident is triaged without the device's
// own syslog" wording. RED.
func TestTheSyslogConsequenceDoesNotClaimTheNetworkGearIsBlind(t *testing.T) {
	c := syslogSeam(t).Consequence
	if c == "" {
		t.Fatal("the syslog seam declares no consequence — a dark seam that cannot say what it costs is " +
			"the state this register exists to end")
	}
	if strings.Contains(c, "every firewall, switch and router") {
		t.Errorf("the consequence still claims every firewall, switch and router incident is triaged "+
			"without syslog. Measured 2026-08-06, fw01/rtr01/sw01 are all in the COVERED set — this sends "+
			"the reader to the devices that already work:\n%s", c)
	}
}

// A consequence is only better than a guess if it says what was measured and when. "Some hosts are
// covered" would pass the test above and be just as unactionable.
//
// KILLING MUTATION: replace the numbers with a vague phrase. RED.
func TestTheSyslogConsequenceCarriesItsMeasurement(t *testing.T) {
	c := syslogSeam(t).Consequence
	for _, want := range []string{"2026-08-06", "15 of 126"} {
		if !strings.Contains(c, want) {
			t.Errorf("the consequence does not carry %q, so a reader cannot tell a measurement from an "+
				"assertion or judge how stale it is:\n%s", want, c)
		}
	}
	// And it must say which side is covered, since that is the half that was wrong.
	if !strings.Contains(c, "COVERED") {
		t.Errorf("the consequence does not say which class IS covered — the exact fact the old wording got "+
			"backwards:\n%s", c)
	}
}

// The rest of the seam's contract is unchanged: it still declares its unit pair, and Produced still means
// reads that REACHED a log rather than reads that returned. Rewriting prose must not have disturbed the
// thing that makes the seam measurable at all.
func TestTheSyslogSeamStillDeclaresItsUnitPair(t *testing.T) {
	s := syslogSeam(t)
	if s.Unit.Offered == "" || s.Unit.Produced == "" {
		t.Fatalf("the syslog seam lost its offered/produced pair (%+v) — without it the register cannot "+
			"distinguish a lane that is idle from one that is running and yielding nothing", s.Unit)
	}
	if !strings.Contains(s.Unit.Produced, "reached") {
		t.Errorf("Produced no longer means reads that REACHED the log (%q); counting returns instead would "+
			"make this lane incapable of reading starved, which is the defect it was built for", s.Unit.Produced)
	}
}

// EVERY seam must carry a consequence, not just this one — a register with a blank cost note anywhere is a
// register that will grow more of them.
func TestEverySeamStillNamesItsCost(t *testing.T) {
	for _, s := range All() {
		if strings.TrimSpace(s.Consequence) == "" {
			t.Errorf("seam %s declares no consequence", s.ID)
		}
	}
}
