package main

// Drills for the cisco-show operator surface (TG-85 read-tool slice): the parse is fail-closed on every
// direction, and the dark default parses to exactly nothing.

import (
	"strings"
	"testing"
)

func TestParseCiscoReadDevicesDarkDefaultIsNothing(t *testing.T) {
	for _, raw := range []string{"", "   ", "\n"} {
		devs, err := parseCiscoReadDevices(raw)
		if err != nil || devs != nil {
			t.Fatalf("the unset default must parse to exactly nothing: devs=%v err=%v", devs, err)
		}
	}
}

func TestParseCiscoReadDevicesFailClosed(t *testing.T) {
	cases := []struct {
		name, raw, want string
	}{
		{"not json", "nonsense", "not a JSON array"},
		{"missing known_hosts", `[{"device_id":"fw01","host":"h","identity":"tg","key_ref":"bao:x#k"}]`, "known_hosts"},
		{"missing key_ref", `[{"device_id":"fw01","host":"h","identity":"tg","known_hosts":"/kh"}]`, "key_ref"},
		{"unknown platform", `[{"device_id":"fw01","host":"h","identity":"tg","key_ref":"bao:x#k","known_hosts":"/kh","platform":"nexus"}]`, "unknown platform"},
	}
	for _, c := range cases {
		if _, err := parseCiscoReadDevices(c.raw); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Fatalf("%s: want refusal containing %q, got %v", c.name, c.want, err)
		}
	}
	// One bad entry refuses the WHOLE list (a half-parsed set = a silently narrower tool enum).
	mixed := `[{"device_id":"ok","host":"h","identity":"tg","key_ref":"bao:x#k","known_hosts":"/kh","platform":"asa"},{"device_id":"bad","host":"h","identity":"tg"}]`
	if _, err := parseCiscoReadDevices(mixed); err == nil || !strings.Contains(err.Error(), "[1]") {
		t.Fatalf("a bad entry must refuse the whole list naming its index, got %v", err)
	}
}

func TestParseCiscoReadDevicesGoodEntry(t *testing.T) {
	devs, err := parseCiscoReadDevices(`[{"device_id":"fw01","host":"192.0.2.1","identity":"tg","key_ref":"bao:secret/tg/cisco#key","known_hosts":"/etc/tg/cisco_known_hosts","platform":"asa","legacy_crypto":true,"pager_off_cmd":"terminal pager 0"}]`)
	if err != nil || len(devs) != 1 {
		t.Fatalf("good entry must parse: %v %v", devs, err)
	}
	d := devs[0]
	if d.ID != "fw01" || d.Dev.Host != "192.0.2.1" || !d.Dev.LegacyCrypto || d.Dev.PagerOffCmd != "terminal pager 0" {
		t.Fatalf("fields dropped in translation: %+v", d)
	}
	if string(d.Dev.KeyRef) != "bao:secret/tg/cisco#key" {
		t.Fatalf("key ref must survive as a REFERENCE, got %q", d.Dev.KeyRef)
	}
}
