package crowdsec

import (
	"testing"

	"github.com/territory-grounder/grounder/core/fuzzcorpus"
)

// FuzzCrowdSecIngest fuzzes the CrowdSec ingress — an UNTRUSTED SECURITY-TELEMETRY boundary (TG-5, "fuzz
// every ingress"). CrowdSec notification bodies arrive over the network from a security engine; the module
// decodes them (a JSON ARRAY of alerts OR a single bare object) and maps each alert to a canonical envelope
// through coreingest.Normalize. The property: the full decode→normalize path NEVER PANICS on any bytes — a
// panic here crashes the security-signal intake, a DoS delivered by one malformed notification — and an
// ACCEPTED envelope carries its required fields. A REJECT is always a safe outcome (reject-before-enqueue).
//
// This is new coverage over the CrowdSec-specific transforms (array/single decode, slugify, severity mapping,
// IP-scope validation); the shared Normalize tail is covered separately by FuzzNormalize.
func FuzzCrowdSecIngest(f *testing.F) {
	for _, s := range [][]byte{
		[]byte(`[{"scenario":"crowdsecurity/ssh-bf","source":{"scope":"Ip","value":"192.0.2.9","ip":"192.0.2.9"},"start_at":"2026-08-16T00:00:00Z","decisions":[{"type":"ban"}]}]`),
		[]byte(`{"scenario":"x","source":{"scope":"Range","value":"10.0.0.0/8"}}`), // single object, non-IP scope
		[]byte(``), []byte(`[]`), []byte(`{`), []byte(`[{}]`), []byte(`null`), []byte(`[null]`), // empty / malformed / edge
		[]byte(`{"scenario":"s","source":{"value":"v"},"start_at":"not-a-time"}`),             // bad timestamp
		[]byte(`{"scenario":"s","source":{"scope":"Ip","value":"not-an-ip","ip":"garbage"}}`), // non-address IP candidates
		[]byte("\xff\xfe\x00\x01garbage"),                                                     // non-UTF8 bytes
	} {
		f.Add(s)
	}
	for _, hostile := range fuzzcorpus.Strings() {
		f.Add([]byte(hostile)) // the shared §3.2 battery as raw notification bodies
	}
	m := New()
	f.Fuzz(func(t *testing.T, raw []byte) {
		alerts, err := decodeAlerts(raw) // MUST NOT PANIC on any body
		if err != nil {
			return
		}
		for _, a := range alerts {
			env, err := m.normalizeOne(a) // MUST NOT PANIC on any decoded alert
			if err != nil {
				continue // a reject is safe
			}
			// Accepted ⇒ the security signal carries its required fields (built from scenario+source, which
			// normalizeOne requires non-empty and Normalize re-validates).
			if env.ExternalRef == "" || env.AlertRule == "" {
				t.Fatalf("crowdsec accepted an envelope with an empty required field: %+v (alert=%+v)", env, a)
			}
		}
	})
}
