package estatedoc

import (
	"strings"
	"testing"
)

func TestNewIdentifierRedactor(t *testing.T) {
	// Fake, distinctive names — never real estate identifiers (the git mirror denylist scans committed files).
	hosts := []string{"hostzz01alpha", "hostzz01alphalong"}

	// Empty identifier set ⇒ passthrough.
	if s, n := NewIdentifierRedactor(nil)("hostzz01alpha down"); s != "hostzz01alpha down" || n != 0 {
		t.Errorf("empty identifiers must passthrough, got %q/%d", s, n)
	}

	red := NewIdentifierRedactor(hosts)

	// No match ⇒ byte-identical, count 0 (probe-first).
	if s, n := red("nothing sensitive here"); s != "nothing sensitive here" || n != 0 {
		t.Errorf("no-match must be byte-identical / 0, got %q / %d", s, n)
	}

	// A host name ⇒ redacted, counted, and the value never survives.
	if s, n := red("the hostzz01alpha host is down"); !strings.Contains(s, EstateIDMarker) || strings.Contains(s, "hostzz01alpha") || n != 1 {
		t.Errorf("must redact the host name (marker in, value out), got %q / %d", s, n)
	}

	// Word boundary: a host name INSIDE a larger token must NOT be redacted.
	if s, _ := red("xhostzz01alphax rebooted"); strings.Contains(s, EstateIDMarker) {
		t.Errorf("must not redact a substring inside a larger token, got %q", s)
	}

	// Case-insensitive.
	if s, n := red("HOSTZZ01ALPHA rebooted"); !strings.Contains(s, EstateIDMarker) || n != 1 {
		t.Errorf("must redact case-insensitively, got %q / %d", s, n)
	}

	// Longest-first: the full longer name is redacted as one unit (its shorter prefix does not leak).
	if s, n := red("see hostzz01alphalong now"); !strings.Contains(s, EstateIDMarker) || strings.Contains(s, "hostzz01alpha") || n != 1 {
		t.Errorf("longest-first must redact the full longer name, got %q / %d", s, n)
	}

	// Every occurrence is counted.
	if _, n := red("hostzz01alpha and hostzz01alpha again"); n != 2 {
		t.Errorf("must count each occurrence, got %d", n)
	}
}
