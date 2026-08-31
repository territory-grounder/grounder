package config

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// TG-367. The defect these oracles exist for: TG_PVE_INSECURE=true was carried for years against an
// endpoint that verifies fine under its FQDN, and the boot log ASSERTED it was self-signed. The
// question "is this skip necessary?" has an answer that can be measured, and nothing measured it.

func TestAnUnnecessarySkipIsReportedAsUnnecessary(t *testing.T) {
	// The real case: dc1pve01.example.net:8006 verifies against a Let's Encrypt wildcard.
	v := SkipIsNecessary(context.Background(), "https://dc1pve01.example.net:8006",
		func(context.Context, string, string) error { return nil })

	if !v.Probed {
		t.Fatalf("probe did not run: %s", v.Reason)
	}
	if v.Necessary {
		t.Fatalf("a verifying handshake SUCCEEDED and the verdict still says the skip is necessary: %+v", v)
	}
	if !strings.Contains(v.String(), "UNNECESSARY") {
		t.Fatalf("the operator-facing line must say UNNECESSARY, got: %s", v.String())
	}
	// The line has to say what is at stake, or it reads as a style note and nobody acts on it.
	if !strings.Contains(v.String(), "receives the credential") {
		t.Fatalf("the verdict must state the consequence (the endpoint receives TG's credential), got: %s", v.String())
	}
}

func TestAGenuinelySelfSignedEndpointKeepsItsSkip(t *testing.T) {
	// The control, and it is a real one: BOTH of TG's LibreNMS deployments are FQDN-addressed AND
	// genuinely self-signed (verified live 2026-08-06). A rule that assumed "FQDN implies verifiable"
	// would strip a load-bearing flag from them and take ingest down.
	v := SkipIsNecessary(context.Background(), "https://dc1nms01.example.net",
		func(context.Context, string, string) error { return errors.New("x509: certificate signed by unknown authority") })

	if !v.Necessary {
		t.Fatalf("a verifying handshake FAILED and the verdict says the skip is unnecessary: %+v", v)
	}
	if strings.Contains(v.String(), "UNNECESSARY") {
		t.Fatalf("a self-signed endpoint must not be reported as an unnecessary skip: %s", v.String())
	}
	if !strings.Contains(v.Reason, "unknown authority") {
		t.Fatalf("the verdict must carry the underlying error so an operator can tell a hostname mismatch "+
			"from an untrusted issuer — they need opposite fixes. got: %s", v.Reason)
	}
}

// TestAnUnprobableEndpointIsNotBlessed is the vacuity floor. The dangerous default is to treat "I could
// not check" as "the skip is fine", which is how a control goes quiet without anyone noticing.
func TestAnUnprobableEndpointIsNotBlessed(t *testing.T) {
	for _, bad := range []string{"", "   ", "://"} {
		v := SkipIsNecessary(context.Background(), bad, func(context.Context, string, string) error { return nil })
		if v.Probed {
			t.Fatalf("endpoint %q is not probable but the verdict claims it was probed: %+v", bad, v)
		}
		if !strings.Contains(v.String(), "NOT PROBED") {
			t.Fatalf("an unprobable endpoint must say NOT PROBED rather than render a verdict, got: %s", v.String())
		}
	}
}

// TestTheProbeTargetsTheConfiguredHostAndPort pins the bug that CAUSED TG-367: the distinction between
// the short name and the FQDN is the entire finding, so a probe that silently normalised, defaulted or
// dropped the host would report on an endpoint nobody configured.
func TestTheProbeTargetsTheConfiguredHostAndPort(t *testing.T) {
	cases := []struct {
		in           string
		wantHostPort string
		wantSNI      string
	}{
		{"https://dc1pve01:8006", "dc1pve01:8006", "dc1pve01"},
		{"https://dc1pve01.example.net:8006", "dc1pve01.example.net:8006", "dc1pve01.example.net"},
		{"https://netbox.example.net", "netbox.example.net:443", "netbox.example.net"},
		{"dc1nms01.example.net", "dc1nms01.example.net:443", "dc1nms01.example.net"},
	}
	for _, c := range cases {
		var gotHostPort, gotSNI string
		v := SkipIsNecessary(context.Background(), c.in, func(_ context.Context, hp, sni string) error {
			gotHostPort, gotSNI = hp, sni
			return nil
		})
		if !v.Probed {
			t.Fatalf("%q: probe did not run: %s", c.in, v.Reason)
		}
		if gotHostPort != c.wantHostPort {
			t.Errorf("%q: dialled %q, want %q", c.in, gotHostPort, c.wantHostPort)
		}
		if gotSNI != c.wantSNI {
			t.Errorf("%q: SNI %q, want %q — SNI must be the configured hostname, because a hostname MISMATCH "+
				"is exactly the failure this ticket was about", c.in, gotSNI, c.wantSNI)
		}
		if v.Endpoint != c.wantHostPort {
			t.Errorf("%q: verdict names %q, want %q — the operator must be told which endpoint was probed",
				c.in, v.Endpoint, c.wantHostPort)
		}
	}
}
