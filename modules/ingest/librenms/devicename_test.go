package librenms

import (
	"context"
	"testing"
	"time"
)

func TestDeviceNamePrefersSysNameOverABareIP(t *testing.T) {
	cases := []struct{ hostname, sysName, want string }{
		{"192.168.2.44", "dc2librespeed01", "dc2librespeed01"},             // TG-628: the live G2 drill device
		{"192.168.2.44", "dc2librespeed01.example.net", "dc2librespeed01"}, // FQDN sysName ⇒ short
		{"2001:db8::44", "dc2host01", "dc2host01"},                         // IPv6 too
		{"dc1linkwarden02", "dc1linkwarden02", "dc1linkwarden02"},      // a real hostname is kept
		{"dc1nms01.example.net", "other", "dc1nms01.example.net"},          // ...even an FQDN one
		{"192.168.2.44", "", "192.168.2.44"},                                       // nothing better to offer
		{"192.168.2.44", "192.0.2.1", "192.168.2.44"},                               // an IP sysName adds nothing
		{"", "dc2myspeed01", "dc2myspeed01"},                               // the old empty fallback
	}
	for _, c := range cases {
		if got := deviceName(c.hostname, c.sysName); got != c.want {
			t.Errorf("deviceName(%q, %q) = %q, want %q", c.hostname, c.sysName, got, c.want)
		}
	}
}

// The push path end to end: an IP-registered device's alert becomes an incident on the device NAME.
func TestNormalizeKeysAnIPRegisteredDeviceByItsName(t *testing.T) {
	fixed := time.Date(2026, 11, 6, 0, 0, 0, 0, time.UTC)
	m := New([]Deployment{{Site: "GR"}}, WithClock(func() time.Time { return fixed }))
	body := `{"site":"GR","alert_id":162017,"rule":"Service up/down","severity":"critical","state":1,` +
		`"hostname":"192.168.2.44","sysName":"dc2librespeed01","timestamp":"2026-11-06 01:50:03"}`
	env, err := m.Normalize(context.Background(), []byte(body))
	if err != nil {
		t.Fatalf("Normalize: %v", err)
	}
	if env.Host != "dc2librespeed01" {
		t.Fatalf("TG-628: an IP-registered device must be keyed by its sysName, got host %q", env.Host)
	}
}
