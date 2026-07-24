package risk

import (
	"os"
	"path/filepath"
	"testing"
)

func TestCanaryPinsMatch(t *testing.T) {
	pins := &CanaryPins{pins: []CanaryPin{
		{HostPattern: "canary-*", OpClass: "restart-service", Reason: "canary-1"},
		{HostPattern: "web01", OpClass: "*", Reason: "web01-any"},
		{HostPattern: "", OpClass: "reload-nginx", Reason: "reload-anywhere"},
	}}
	cases := []struct {
		host, op   string
		wantMatch  bool
		wantReason string
	}{
		{"canary-a", "restart-service", true, "canary-1"},  // glob host + exact op
		{"canary-a", "reboot", false, ""},                  // host glob but op mismatch (falls through)
		{"web01", "anything", true, "web01-any"},           // exact host + wildcard op
		{"web01", "reload-nginx", true, "web01-any"},       // first-match-wins (web01 rule before reload rule)
		{"db07", "reload-nginx", true, "reload-anywhere"},  // empty host pattern = any host
		{"db07", "restart-service", false, ""},             // no rule matches
		{"", "", false, ""},                                // empty inputs, no any-any rule
	}
	for _, c := range cases {
		got, reason := pins.Match(c.host, c.op)
		if got != c.wantMatch || reason != c.wantReason {
			t.Errorf("Match(%q,%q) = (%v,%q); want (%v,%q)", c.host, c.op, got, reason, c.wantMatch, c.wantReason)
		}
	}
}

func TestCanaryPinsNilAndEmptyMatchNothing(t *testing.T) {
	var nilPins *CanaryPins
	if ok, _ := nilPins.Match("web01", "restart-service"); ok {
		t.Error("nil CanaryPins must match nothing")
	}
	if nilPins.Len() != 0 {
		t.Error("nil CanaryPins Len must be 0")
	}
	empty := &CanaryPins{}
	if ok, _ := empty.Match("web01", "restart-service"); ok {
		t.Error("empty CanaryPins must match nothing")
	}
	if empty.Len() != 0 {
		t.Error("empty CanaryPins Len must be 0")
	}
}

func TestCanaryPinsWildcardOnlyMatchesAny(t *testing.T) {
	pins := &CanaryPins{pins: []CanaryPin{{HostPattern: "*", OpClass: "*", Reason: "all"}}}
	if ok, r := pins.Match("anything", "whatever"); !ok || r != "all" {
		t.Errorf("wildcard pin must match any (host,op); got (%v,%q)", ok, r)
	}
}

func TestLoadCanaryPinsEmptyPathIsInert(t *testing.T) {
	pins, err := LoadCanaryPins("")
	if err != nil {
		t.Fatalf("empty path must not error: %v", err)
	}
	if pins.Len() != 0 {
		t.Fatalf("empty path must yield an inert (0-pin) set, got %d", pins.Len())
	}
	if ok, _ := pins.Match("web01", "restart-service"); ok {
		t.Error("inert set must match nothing")
	}
}

func TestLoadCanaryPinsMissingFileErrors(t *testing.T) {
	if _, err := LoadCanaryPins(filepath.Join(t.TempDir(), "does-not-exist.json")); err == nil {
		t.Fatal("a present-but-unreadable path must be a hard error, never silently unpinned")
	}
}

func TestLoadCanaryPinsMalformedErrors(t *testing.T) {
	p := filepath.Join(t.TempDir(), "bad.json")
	if err := os.WriteFile(p, []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadCanaryPins(p); err == nil {
		t.Fatal("a malformed policy file must be a hard error")
	}
}

func TestLoadCanaryPinsMalformedGlobErrors(t *testing.T) {
	// A valid-JSON pin whose glob is malformed (unclosed class) must fail the BOOT, not silently
	// never-match at classify time — a typo'd pin that never fires would let a staged mutation reach AUTO.
	p := filepath.Join(t.TempDir(), "badglob.json")
	body := `[{"host_pattern":"canary-[","op_class":"*","reason":"typo"}]`
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadCanaryPins(p); err == nil {
		t.Fatal("a malformed glob pattern must be a hard load error (fail-closed), never silently unpinned")
	}
}

func TestLoadCanaryPinsValidRoundTrip(t *testing.T) {
	p := filepath.Join(t.TempDir(), "pins.json")
	body := `[{"host_pattern":"canary-*","op_class":"restart-service","reason":"canary-1"}]`
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	pins, err := LoadCanaryPins(p)
	if err != nil {
		t.Fatalf("valid file must load: %v", err)
	}
	if pins.Len() != 1 {
		t.Fatalf("want 1 pin, got %d", pins.Len())
	}
	if ok, r := pins.Match("canary-x", "restart-service"); !ok || r != "canary-1" {
		t.Errorf("loaded pin must match; got (%v,%q)", ok, r)
	}
}
