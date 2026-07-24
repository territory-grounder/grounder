package credential

import (
	"strings"
	"testing"

	"github.com/territory-grounder/grounder/core/config"
)

func TestPlaneSetDisjointOK(t *testing.T) {
	p := PlaneSet{
		ReadTriage: []config.SecretRef{"file:/secrets/tg-syslog-ro", "env:NETBOX_TOKEN", "file:/secrets/tg-openbao-token"},
		Actuation:  []config.SecretRef{"file:/secrets/tg-actuator", "file:/secrets/pve-actuate-token", "env:AWX_TOKEN"},
	}
	if err := p.Validate(); err != nil {
		t.Fatalf("disjoint planes must validate: %v", err)
	}
	if !strings.Contains(p.Summary(), "3 read-triage ref(s) disjoint from 3 actuation ref(s)") {
		t.Fatalf("summary: %s", p.Summary())
	}
}

func TestPlaneSetCrossFailsClosed(t *testing.T) {
	// the SAME key used for read AND actuation — exactly the REQ-2203 violation.
	p := PlaneSet{
		ReadTriage: []config.SecretRef{"file:/secrets/tg-syslog-ro", "file:/secrets/tg-actuator"},
		Actuation:  []config.SecretRef{"file:/secrets/tg-actuator"},
	}
	err := p.Validate()
	if err == nil {
		t.Fatal("a reference in both planes must fail closed")
	}
	if !strings.Contains(err.Error(), "tg-actuator") || !strings.Contains(err.Error(), "REQ-2203") {
		t.Fatalf("error must name the crossing ref + the requirement: %v", err)
	}
}

func TestPlaneSetEmptyRefsIgnored(t *testing.T) {
	// unconfigured ("") references never cross, even if both planes have them.
	p := PlaneSet{ReadTriage: []config.SecretRef{"", "env:R"}, Actuation: []config.SecretRef{"", "env:A"}}
	if err := p.Validate(); err != nil {
		t.Fatalf("empty refs must be ignored: %v", err)
	}
}

func TestCredentialClassString(t *testing.T) {
	if ClassActuation.String() != "actuation" || ClassReadTriage.String() != "read-triage" {
		t.Fatal("class strings wrong")
	}
}
