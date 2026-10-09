package librenms

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"
)

// TG-146 S2. For this puller a TRUNCATED device/alert list is not a smaller answer but a WRONG one: a missing
// device resolves Host="" (envelopeFor allows it — not every alert is host-scoped), the close-out clear-check's
// host match then never matches, and ObserveClearedActivity returns Cleared=true for a host that is still
// alerting. That is a false auto-close, a false de-novel, and a clean run credited to the graduation ladder on
// post-state evidence nobody observed. So the reader must UNDER-CONFIRM a possibly-partial list (return an
// error → FetchActive error → ClearObserve ok=false → ObserveClearedActivity holds To Verify).
//
// The load-bearing subtlety (measured 2026-08-06 on both deployments, LibreNMS 26.8.0-dev.68, and confirmed in
// the upstream api_functions source): LibreNMS applies NO limit/offset to list_devices or list_alerts, and its
// `count` field is defined server-side as count(returned_rows) — so count EQUALS len on every genuine response.
// A truncation therefore NEVER shows as count!=len. The only signal a client gets is that the response reached
// the row cap it asked for (got >= pageLimit). These tests drive that primary signal with a shrunk pageLimit;
// the count-based cases exercise the defensive secondary guard that would catch a future version reporting a
// real total.

func truncDoer(devices, alerts string) pathDoer {
	return pathDoer{byPath: map[string]string{
		"/api/v0/rules":   `{"rules":[{"id":1,"name":"Device Down","severity":"critical"}]}`,
		"/api/v0/devices": devices,
		"/api/v0/alerts":  alerts,
	}}
}

func truncSource(t *testing.T, d pathDoer) *AlertSource {
	t.Helper()
	t.Setenv("TG_TEST_LN_TOKEN", "test-token")
	return NewAlertSource(
		[]Deployment{{Site: "nl", BaseURL: "https://ln.test", TokenRef: "env:TG_TEST_LN_TOKEN"}},
		WithAlertHTTPClient(d),
		WithAlertClock(func() time.Time { return time.Date(2026, 7, 17, 10, 5, 0, 0, time.UTC) }),
	)
}

const oneAlert = `{"status":"ok","count":1,"alerts":[{"id":7,"device_id":42,"rule_id":1,"state":1,"timestamp":"2026-07-17 10:00:00"}]}`
const oneDevice = `{"status":"ok","count":1,"devices":[{"device_id":42,"hostname":"web01.nl.example","sysName":"web01"}]}`

// PRIMARY GUARD — the load-bearing safety property. A device list whose row count REACHED the requested cap
// (got==limit) with count==len — the shape real LibreNMS emits when the estate exceeds a honoured limit — must
// be refused: we cannot prove there is not a further device beyond it, and an alert on that device would
// resolve Host="" and false-clear a still-alerting host.
//
// KILLING MUTATION: change `got >= limit` to `got > limit`, or delete the primary branch. The page is served,
// the alert on the unlisted device 43 arrives with Host="" and FetchActive returns nil error. RED.
func TestDeviceListReachingRequestedLimitUnderConfirms(t *testing.T) {
	// count==len==2 (a valid LibreNMS shape) and the requested limit is shrunk to 2, so this page fills the cap.
	// The alert is on device 43, which is NOT in the two returned rows.
	devices := `{"status":"ok","count":2,"devices":[` +
		`{"device_id":41,"hostname":"web01.nl.example","sysName":"web01"},` +
		`{"device_id":42,"hostname":"web02.nl.example","sysName":"web02"}]}`
	alerts := `{"status":"ok","count":1,"alerts":[{"id":7,"device_id":43,"rule_id":1,"state":1,"timestamp":"2026-07-17 10:00:00"}]}`
	src := truncSource(t, truncDoer(devices, alerts))
	src.pageLimit = 2
	envs, _, err := src.FetchActive(context.Background())
	if err == nil {
		host := "<no envelope>"
		if len(envs) > 0 {
			host = envs[0].Host
		}
		t.Fatalf("a device page filled to the requested limit was ACCEPTED — the alert on the unlisted device 43 "+
			"resolves Host=%q and the close-out check reads that as a clear (got %d envelope(s))", host, len(envs))
	}
	if !strings.Contains(err.Error(), "TRUNCATED") {
		t.Errorf("error should name the truncation so an operator can act on it; got: %v", err)
	}
	if !strings.Contains(err.Error(), "/api/v0/devices") {
		t.Errorf("error should name the endpoint that was short; got: %v", err)
	}
}

// PRIMARY GUARD on the active-alert page. An alert list filled to the requested cap means firing alerts may
// exist beyond it that TG cannot see — and their absence is exactly what the close-out check calls a clear.
//
// KILLING MUTATION: change `got >= limit` to `got > limit` in fetchActiveAlerts' path, or delete it. RED.
func TestAlertListReachingRequestedLimitUnderConfirms(t *testing.T) {
	alerts := `{"status":"ok","count":2,"alerts":[` +
		`{"id":7,"device_id":42,"rule_id":1,"state":1,"timestamp":"2026-07-17 10:00:00"},` +
		`{"id":8,"device_id":42,"rule_id":1,"state":1,"timestamp":"2026-07-17 10:00:00"}]}`
	src := truncSource(t, truncDoer(oneDevice, alerts))
	src.pageLimit = 2
	_, _, err := src.FetchActive(context.Background())
	if err == nil {
		t.Fatal("an alert page filled to the requested limit was ACCEPTED — firing alerts may exist beyond it, " +
			"and their absence is what the close-out check calls a clear")
	}
	if !strings.Contains(err.Error(), "/api/v0/alerts") {
		t.Errorf("error should name the endpoint that was short; got: %v", err)
	}
}

// SECONDARY (DEFENSIVE) GUARD. Today LibreNMS never sends count>len, so this shape does not occur on the wire;
// the case exists so a FUTURE version that reports a real TOTAL in `count` would expose truncation directly.
//
// KILLING MUTATION: delete the `count > got` branch in checkComplete. RED.
func TestDeviceTotalAboveReturnedIsRefused(t *testing.T) {
	// A real total of 3 with only 1 row returned. The alert is on the unlisted device 99.
	devices := `{"status":"ok","count":3,"devices":[{"device_id":42,"hostname":"web01.nl.example","sysName":"web01"}]}`
	alerts := `{"status":"ok","count":1,"alerts":[{"id":7,"device_id":99,"rule_id":1,"state":1,"timestamp":"2026-07-17 10:00:00"}]}`
	envs, _, err := truncSource(t, truncDoer(devices, alerts)).FetchActive(context.Background())
	if err == nil {
		host := "<no envelope>"
		if len(envs) > 0 {
			host = envs[0].Host
		}
		t.Fatalf("a device page of 1 row against an upstream TOTAL of 3 was ACCEPTED — the alert on the unlisted "+
			"device 99 resolves Host=%q and the close-out check reads that as a clear (got %d envelope(s))", host, len(envs))
	}
	if !strings.Contains(err.Error(), "/api/v0/devices") {
		t.Errorf("error should name the endpoint that was short; got: %v", err)
	}
}

// SECONDARY guard on the alert page.
func TestAlertTotalAboveReturnedIsRefused(t *testing.T) {
	alerts := `{"status":"ok","count":9,"alerts":[{"id":7,"device_id":42,"rule_id":1,"state":1,"timestamp":"2026-07-17 10:00:00"}]}`
	_, _, err := truncSource(t, truncDoer(oneDevice, alerts)).FetchActive(context.Background())
	if err == nil {
		t.Fatal("an alert page of 1 row against an upstream TOTAL of 9 was ACCEPTED — the 8 unseen alerts read " +
			"as absent, and absence is what the close-out check calls a clear")
	}
	if !strings.Contains(err.Error(), "/api/v0/alerts") {
		t.Errorf("error should name the endpoint that was short; got: %v", err)
	}
}

// The complete case must still work — a refusal that fires on healthy traffic takes ingestion down, which is
// strictly worse than the defect. This is the regression floor for the guards above.
func TestCompletePageIsServed(t *testing.T) {
	envs, _, err := truncSource(t, truncDoer(oneDevice, oneAlert)).FetchActive(context.Background())
	if err != nil {
		t.Fatalf("a page whose row count is below the requested limit and matches its count was refused: %v", err)
	}
	if len(envs) != 1 {
		t.Fatalf("want 1 envelope from a complete page, got %d", len(envs))
	}
	if envs[0].Host != "web01.nl.example" {
		t.Errorf("host = %q, want web01.nl.example", envs[0].Host)
	}
}

// The reason to lift the old limit=500 cap: a HEALTHY estate larger than the old cap must be served whole, not
// truncated and not refused. With the default pageLimit (100k) a 600-device response (count==len==600, the
// ordinary LibreNMS shape) resolves every host — including the 600th, which the old limit=500 would have
// dropped the day LibreNMS honoured it. This is the positive complement to the primary-guard tests and the
// regression floor that keeps pageLimit well above the estate.
func TestLargeCompleteDeviceSetResolvesEveryHost(t *testing.T) {
	var b strings.Builder
	b.WriteString(`{"status":"ok","count":600,"devices":[`)
	for i := 0; i < 600; i++ {
		if i > 0 {
			b.WriteByte(',')
		}
		fmt.Fprintf(&b, `{"device_id":%d,"hostname":"h%d.nl.example","sysName":"s%d"}`, i+1, i, i)
	}
	b.WriteString(`]}`)
	// An alert on device 600 — beyond the retired 500 cap — must resolve its real host, not "".
	alerts := `{"status":"ok","count":1,"alerts":[{"id":7,"device_id":600,"rule_id":1,"state":1,"timestamp":"2026-07-17 10:00:00"}]}`
	envs, _, err := truncSource(t, truncDoer(b.String(), alerts)).FetchActive(context.Background())
	if err != nil {
		t.Fatalf("a complete 600-device estate (below the 100k pageLimit) was refused: %v", err)
	}
	if len(envs) != 1 {
		t.Fatalf("want 1 envelope, got %d", len(envs))
	}
	if envs[0].Host != "h599.nl.example" {
		t.Errorf("host = %q, want h599.nl.example (device 600 resolves past the retired 500 cap)", envs[0].Host)
	}
}

// BACKWARD COMPATIBILITY, and the reason the secondary guard is count>got not count!=got: an upstream that
// omits `count` (or reports 0) must not have every page read as truncated. Both live deployments DO send it,
// but a completeness check that failed closed on a missing field would take ingestion down on the first
// LibreNMS that drops it — the fail-closed-on-unmigrated-config trap.
func TestAbsentCountIsNotTreatedAsTruncation(t *testing.T) {
	devices := `{"status":"ok","devices":[{"device_id":42,"hostname":"web01.nl.example","sysName":"web01"}]}`
	alerts := `{"status":"ok","alerts":[{"id":7,"device_id":42,"rule_id":1,"state":1,"timestamp":"2026-07-17 10:00:00"}]}`
	envs, _, err := truncSource(t, truncDoer(devices, alerts)).FetchActive(context.Background())
	if err != nil {
		t.Fatalf("a response with no `count` field was refused as truncated: %v", err)
	}
	if len(envs) != 1 {
		t.Fatalf("want 1 envelope, got %d", len(envs))
	}
}

// The upstream-availability denominator (TG-344) must not publish a page that reached the requested cap as "N
// available" — that is the same conflation the probe exists to end, one row short. The error belongs in errs,
// not in counts.
//
// KILLING MUTATION: delete the checkComplete call in CountActive. The cap-filled page publishes a count and
// errs is empty. RED.
func TestCountActiveReportsTruncationAsAnErrorNotACount(t *testing.T) {
	alerts := `{"status":"ok","count":2,"alerts":[` +
		`{"id":7,"device_id":42,"rule_id":1,"state":1,"timestamp":"2026-07-17 10:00:00"},` +
		`{"id":8,"device_id":42,"rule_id":1,"state":1,"timestamp":"2026-07-17 10:00:00"}]}`
	src := truncSource(t, truncDoer(oneDevice, alerts))
	src.pageLimit = 2
	counts, errs := src.CountActive(context.Background())
	if len(errs) == 0 {
		t.Fatalf("an alert page that reached the requested cap published as an availability COUNT (%v) with no "+
			"error — an unreadable upstream must never publish as a number", counts)
	}
	if _, published := counts["librenms-nl"]; published {
		t.Errorf("counts still carries librenms-nl=%d for a page known to be capped", counts["librenms-nl"])
	}
}

// Negative control for checkComplete itself: it must be capable of BOTH verdicts, and — the anti-vacuity floor
// — it must NOT read the ordinary count==len response as truncated (the inversion the old guard fell into).
func TestCheckCompleteHasBothVerdicts(t *testing.T) {
	if err := checkComplete("/p", 5, 100, 5); err != nil {
		t.Errorf("a complete page (got<limit, count==got) was refused: %v", err)
	}
	if err := checkComplete("/p", 5, 100, 0); err != nil {
		t.Errorf("a page with no declared count was refused: %v", err)
	}
	// The ordinary LibreNMS shape: count==len, comfortably below the limit. Must be SERVED. A checker that
	// refused this would be the old vacuous guard's mirror image — it would take ingestion down on every poll.
	if err := checkComplete("/p", 50, 100, 50); err != nil {
		t.Errorf("the ordinary count==len response was refused as truncated: %v", err)
	}
	// PRIMARY: the response reached the requested cap.
	if err := checkComplete("/p", 100, 100, 100); err == nil {
		t.Error("a page whose row count reached the requested limit was accepted (the real-LibreNMS truncation shape)")
	}
	// SECONDARY: a future upstream reporting a real total above the rows returned.
	if err := checkComplete("/p", 5, 100, 9); err == nil {
		t.Error("a page of 5 rows against a declared total of 9 was accepted")
	}
	// A limit of 0 disables the primary check (defensive: never turn a mis-set limit into a total outage).
	if err := checkComplete("/p", 5, 0, 5); err != nil {
		t.Errorf("a zero limit must disable the primary guard, not refuse every page: %v", err)
	}
}
