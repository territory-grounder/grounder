package ingest

import (
	"strings"
	"testing"
	"time"

	"github.com/territory-grounder/grounder/core/fuzzcorpus"
)

// FuzzNormalize hammers the ingest front door — the untrusted-input boundary (TG-5, "fuzz every ingress").
// The property is twofold, and both halves matter at an ingress:
//
//  1. Normalize NEVER PANICS, whatever bytes a provider sends. A panic on the intake path is a crash of the
//     front door — a denial of service delivered by one malformed webhook.
//  2. An ACCEPTED envelope is WELL-FORMED: required fields present, lengths within their bounds, timestamps
//     stamped. A malformed ACCEPT is worse than a reject — it enqueues an incident the rest of the pipeline
//     (dedup, correlate, the model, the ledger) then trusts.
//
// A REJECT is always a valid, safe outcome — reject-before-enqueue is the design (INV-04). The fuzzer fails
// ONLY on a panic or a bad accept, so it can run in CI over the seed corpus and be driven wide with
// `go test -fuzz=FuzzNormalize ./core/ingest`.
func FuzzNormalize(f *testing.F) {
	seeds := []struct{ src, ext, rule, sev, host, ip, sum, site string }{
		{"librenms", "ext-ref-1", "HostDown", "warning", "dc1demo-web01", "192.0.2.5", "host is down", "dc1"},
		{"", "", "", "", "", "", "", ""},                                                   // all empty → missing-field rejects
		{"s", "ext ref spaces", "r", "nonsense", "h", "not-an-ip", "sum", ""},              // bad slug, bad severity, bad ip
		{"s", strings.Repeat("a", 300), "r", "warning", "", "", "", ""},                    // over-long external_ref
		{"s", "r", "r", "warning", "", "", strings.Repeat("x", 5000), ""},                  // over-long summary
		{"s", "r\x00ef", "ru\nle", "CRITICAL", "h\tost", "::1", "sum\x00\xff\xfe", "SITE"}, // control bytes, ipv6, invalid utf8
		{"s", "réf-ünïcode-💥", "rule", "info", "host", "203.0.113.9", "unicode ✓", "gr"},   // multibyte
	}
	for _, s := range seeds {
		f.Add(s.src, s.ext, s.rule, s.sev, s.host, s.ip, s.sum, s.site)
	}
	for _, hostile := range fuzzcorpus.Strings() {
		// map the shared §3.2 battery onto the provider-controlled free-text fields (rule/host/summary)
		f.Add("crowdsec", "http-probe", hostile, "warning", hostile, "192.0.2.1", hostile, "nl")
	}
	now := time.Date(2026, 8, 16, 12, 0, 0, 0, time.UTC)

	f.Fuzz(func(t *testing.T, src, ext, rule, sev, host, ip, sum, site string) {
		raw := RawEvent{
			SourceID:    src,
			ExternalRef: ext,
			AlertRule:   rule,
			Severity:    sev,
			Host:        host,
			IP:          ip,
			Summary:     sum,
			Site:        site,
		}
		env, err := Normalize(raw, now) // MUST NOT PANIC — the front-door robustness claim.
		if err != nil {
			return // a rejection is a valid, safe outcome
		}
		// Accepted ⇒ well-formed.
		if env.SourceID == "" || env.ExternalRef == "" || env.AlertRule == "" {
			t.Fatalf("accepted an envelope with an empty required field: %+v (raw=%+v)", env, raw)
		}
		if len(env.ExternalRef) > maxExternalRefLen || len(env.AlertRule) > maxAlertRuleLen || len(env.Summary) > maxSummaryLen {
			t.Fatalf("accepted an envelope exceeding a length bound: extRef=%d rule=%d sum=%d",
				len(env.ExternalRef), len(env.AlertRule), len(env.Summary))
		}
		if env.ObservedAt.IsZero() || env.ReceivedAt.IsZero() {
			t.Fatalf("accepted an envelope with a zero timestamp: %+v (raw=%+v)", env, raw)
		}
	})
}
