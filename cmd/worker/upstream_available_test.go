package main

// Guards for the upstream denominator (TG-344).
//
// The defect: every ingest gauge counted what ARRIVED and none counted what was AVAILABLE, so a quiet
// estate and a broken connector published identically. Settling which one it was, on 2026-08-06, took a
// hand-run API call. Each test below aims at a way this gauge could go back to hiding that difference.

import (
	"context"
	"errors"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/territory-grounder/grounder/core/metrics"
	"github.com/territory-grounder/grounder/modules/ingest/librenms"
)

type fakeProber struct {
	counts map[string]int
	errs   map[string]error
	calls  int
}

func (f *fakeProber) CountActive(context.Context) (map[string]int, map[string]error) {
	f.calls++
	return f.counts, f.errs
}

func upSample(ss []metrics.Sample, name, src string) (metrics.Sample, bool) {
	for _, s := range ss {
		if s.Name == name && (src == "" || s.Labels["source_id"] == src) {
			return s, true
		}
	}
	return metrics.Sample{}, false
}

// THE DISTINCTION THIS EXISTS FOR. Both scenarios have zero arriving; only one is a defect.
func TestAQuietUpstreamAndABrokenConnectorPublishDifferently(t *testing.T) {
	quiet := upstreamSamples(map[string]int{"librenms-dc1": 0}, nil)
	broken := upstreamSamples(map[string]int{"librenms-dc1": 50}, nil)

	q, ok := upSample(quiet, "tg_ingest_upstream_available", "librenms-dc1")
	if !ok {
		t.Fatal("a quiet upstream published no available count at all — absent is not zero")
	}
	b, _ := upSample(broken, "tg_ingest_upstream_available", "librenms-dc1")
	if q.Value == b.Value {
		t.Fatalf("a quiet estate (%v) and an upstream holding 50 alerts (%v) publish the same value. "+
			"That is the exact conflation this gauge exists to end: with nothing arriving, one of those "+
			"is healthy and the other is a deaf connector.", q.Value, b.Value)
	}
}

// AN UNREADABLE UPSTREAM MUST NOT PUBLISH "0 AVAILABLE". Zero means "the upstream has nothing", which is
// precisely what is not known when the probe failed.
func TestAnUnreadableUpstreamPublishesNoAvailableCount(t *testing.T) {
	ss := upstreamSamples(nil, map[string]error{"librenms-dc2": errors.New("403")})

	if s, ok := upSample(ss, "tg_ingest_upstream_available", "librenms-dc2"); ok {
		t.Errorf("an unreadable source published available=%v. Reporting an unknown as zero recreates the "+
			"conflation one level down: 'the upstream has nothing' and 'I could not ask' become the same "+
			"reading.", s.Value)
	}
	r, ok := upSample(ss, "tg_ingest_upstream_readable", "librenms-dc2")
	if !ok {
		t.Fatal("an unreadable source published NOTHING — its absence from the metrics is indistinguishable " +
			"from a source that was never configured")
	}
	if r.Value != 0 {
		t.Errorf("readable = %v for a source whose probe failed, want 0", r.Value)
	}
}

// A readable source publishes readable=1 beside its count, so a rule can require both.
func TestAReadableSourcePublishesBothHalves(t *testing.T) {
	ss := upstreamSamples(map[string]int{"librenms-dc1": 3}, nil)
	if a, ok := upSample(ss, "tg_ingest_upstream_available", "librenms-dc1"); !ok || a.Value != 3 {
		t.Errorf("available = %v (present=%v), want 3", a.Value, ok)
	}
	if r, ok := upSample(ss, "tg_ingest_upstream_readable", "librenms-dc1"); !ok || r.Value != 1 {
		t.Errorf("readable = %v (present=%v), want 1", r.Value, ok)
	}
}

// THE VACUITY FLOOR. With nothing probed every per-source series is absent and a rule over them goes
// quiet — silence that reads as health.
func TestTheProbedCountIsAlwaysEmitted(t *testing.T) {
	ss := upstreamSamples(nil, nil)
	p, ok := upSample(ss, "tg_ingest_upstream_probed", "")
	if !ok {
		t.Fatal("tg_ingest_upstream_probed was not emitted for an empty probe. It is the ONLY series that " +
			"survives probing nothing, so without it 'the prober is dead' and 'no sources configured' " +
			"publish identically — and both look like health.")
	}
	if p.Value != 0 {
		t.Errorf("probed = %v with no sources, want 0", p.Value)
	}
	// And it must count BOTH readable and unreadable sources, or a fully-broken estate reports 0 probed.
	mixed := upstreamSamples(map[string]int{"a": 1}, map[string]error{"b": errors.New("x")})
	if m, _ := upSample(mixed, "tg_ingest_upstream_probed", ""); m.Value != 2 {
		t.Errorf("probed = %v for one readable + one unreadable source, want 2 — counting only the "+
			"readable ones would make a totally-unreachable estate report zero attempts", m.Value)
	}
}

// A nil prober degrades to silence, not a panic — and says so, because with no probe the distinction is
// gone again.
func TestANilProberEmitsNothing(t *testing.T) {
	if got := startUpstreamProbeJob(context.Background(), nil, time.Hour)(); got != nil {
		t.Errorf("a nil prober published %d samples, want none", len(got))
	}
}

// The job must publish on its FIRST refresh, not only after the first tick — otherwise there is a window
// where the gauge is absent and absent reads as healthy.
func TestTheJobPublishesImmediately(t *testing.T) {
	f := &fakeProber{counts: map[string]int{"librenms-dc1": 7}}
	read := startUpstreamProbeJob(context.Background(), f, time.Hour)
	if f.calls == 0 {
		t.Fatal("the job did not probe on construction — the gauges would not exist until the first tick")
	}
	if s, ok := upSample(read(), "tg_ingest_upstream_available", "librenms-dc1"); !ok || s.Value != 7 {
		t.Errorf("first reading = %v (present=%v), want 7", s.Value, ok)
	}
}

// THE COMPOSITION ROOT. Every test above exercises the job in isolation; none notices if the admin
// surface never calls it.
func TestTheAdminSurfaceEmitsTheUpstreamGauges(t *testing.T) {
	adm := &workerAdmin{}
	baseline := len(adm.samples())
	if baseline == 0 {
		t.Fatal("the bare admin surface emitted nothing, so the comparison below is meaningless")
	}
	adm = adm.withUpstreamProbe(func() []metrics.Sample {
		return upstreamSamples(map[string]int{"librenms-dc1": 2}, nil)
	})
	names := map[string]bool{}
	for _, s := range adm.samples() {
		names[s.Name] = true
	}
	if len(adm.samples()) == baseline {
		t.Fatal("wiring the upstream reader changed NOTHING on /metrics — samples() does not call it")
	}
	for _, want := range []string{"tg_ingest_upstream_available", "tg_ingest_upstream_readable", "tg_ingest_upstream_probed"} {
		if !names[want] {
			t.Errorf("%s is computed by the job and never reaches /metrics — the denominator stays "+
				"invisible while every unit test above passes", want)
		}
	}
}

// THE REGRESSION THIS FILE EXISTS FOR, second time around.
//
// TG-344 shipped a probe that could never construct on the deployment it was written for. It required the
// LibreNMS *alert poller* to be configured, and production runs push-only, so `upstreamProbeSource` was nil
// on every boot and the worker logged "no prober wired" while the merge, the guards and the deploy all
// reported success. A gauge that only exists where an independent read already exists is not a denominator.
//
// The rule these hold: a deployment that INGESTS from a LibreNMS gets a denominator for it, whether or not
// it polls.
func TestAPushOnlyDeploymentStillGetsADenominator(t *testing.T) {
	deps := []librenms.Deployment{{Site: "dc1", BaseURL: "https://nms.example", TokenRef: "env:T"}}

	// polled == nil is the push-only case: no alert poller, deployments configured.
	got := upstreamProbeSourceFor(true, nil, deps, http.DefaultClient)
	if got == nil {
		t.Fatal("a push-only deployment with configured LibreNMS deployments got NO upstream prober. " +
			"Silence is its only signal, so it is the deployment that most needs a denominator — this is " +
			"the exact state TG-344 shipped in and ran in production with.")
	}
}

func TestNoDeploymentsMeansNoProber(t *testing.T) {
	// The honest nil. Nothing to count against must stay nil so startUpstreamProbeJob says so at boot,
	// rather than constructing a client that probes nothing and publishes a confident zero.
	if got := upstreamProbeSourceFor(true, nil, nil, http.DefaultClient); got != nil {
		t.Fatal("no configured deployments produced a prober; it must be nil so the boot log says the " +
			"denominator is absent")
	}
}

func TestAPolledSourceIsReusedRatherThanDuplicated(t *testing.T) {
	// A pull deployment already holds a source; opening a second client against the same upstream would
	// double the read load and could report two different counts for one LibreNMS.
	polled := librenms.NewAlertSource(
		[]librenms.Deployment{{Site: "dc1", BaseURL: "https://nms.example", TokenRef: "env:T"}},
	)
	if got := upstreamProbeSourceFor(true, polled, []librenms.Deployment{{Site: "other", BaseURL: "https://b", TokenRef: "env:T"}}, http.DefaultClient); got != polled {
		t.Fatal("the already-constructed polled source was not reused")
	}
}

// THE CALL SITE, not just the resolver.
//
// The three tests above all passed while the wiring in main.go still read
// `TG_LIBRENMS_DEPLOYMENTS_AGENT_TOOLS` — a key production does not set. A resolver that behaves correctly
// and is handed an empty list is exactly as blind as the bug it replaced, and unit tests on the resolver
// cannot see that. This reads the composition root itself.
//
// Comment lines are stripped before matching: an earlier guard in this repo passed by matching its own
// explanatory prose, and TestTheCallSiteGuardIgnoresProse holds that stripping honest.
func TestTheUpstreamProbeIsWiredToTheKeyIngestActuallyUses(t *testing.T) {
	src, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatalf("read main.go: %v", err)
	}
	block := upstreamProbeWiringBlock(t, stripGoComments(string(src)))

	if !strings.Contains(block, `"TG_LIBRENMS_DEPLOYMENTS"`) {
		t.Errorf("the upstream probe wiring does not read TG_LIBRENMS_DEPLOYMENTS — the list ingest, the "+
			"estate graph and the observability path all resolve from. Wiring block:\n%s", block)
	}
	// AND the plane gate must be applied AT THE CALL SITE. TestTheActuationPlaneDoesNotProbeTheUpstream
	// proves the function refuses; only this proves main.go asks. A literal `true` here re-arms the false
	// page on the actuation plane while every other test in this file stays green — that mutation was
	// executed and survived until this assertion existed.
	if !strings.Contains(block, "credentialPlane.HoldsTriage()") {
		t.Errorf("the upstream probe wiring does not gate on credentialPlane.HoldsTriage(). The actuation "+
			"plane's LibreNMS token 403s on /alerts by design (TG-337), so it would publish readable=0 and "+
			"UpstreamProbeUnreadable would page on a control working as intended. Wiring block:\n%s", block)
	}
	if strings.Contains(block, "TG_LIBRENMS_DEPLOYMENTS_AGENT_TOOLS") {
		t.Errorf("the upstream probe wiring reads TG_LIBRENMS_DEPLOYMENTS_AGENT_TOOLS. Production does not "+
			"set that key, so the probe resolves to nil and the denominator is absent on the running "+
			"system — the state TG-344 shipped in. Wiring block:\n%s", block)
	}
}

func TestTheCallSiteGuardIgnoresProse(t *testing.T) {
	// A file whose ONLY mention of the key is a comment must not satisfy the guard above; otherwise the
	// guard is inert and would pass on a codebase that merely talks about the right key.
	prose := "// withUpstreamProbe(startUpstreamProbeJob(ctx, TG_LIBRENMS_DEPLOYMENTS))\nfunc x() {}\n"
	if got := stripGoComments(prose); strings.Contains(got, "TG_LIBRENMS_DEPLOYMENTS") {
		t.Fatalf("stripGoComments left commented-out code in place, so the wiring guard would pass on "+
			"prose alone; got %q", got)
	}
}

// upstreamProbeWiringBlock returns the composition-root lines that wire the probe. It FAILS rather than
// returning empty when the call is absent: a vacuous "" would satisfy the not-contains assertion above and
// report health for a worker that no longer wires the probe at all.
func upstreamProbeWiringBlock(t *testing.T, src string) string {
	t.Helper()
	i := strings.Index(src, "withUpstreamProbe(")
	if i < 0 {
		t.Fatal("main.go no longer wires withUpstreamProbe at all — the probe cannot publish anything")
	}
	lines := strings.Split(src[i:], "\n")
	if len(lines) > 10 {
		lines = lines[:10]
	}
	return strings.Join(lines, "\n")
}

func stripGoComments(src string) string {
	var b strings.Builder
	for _, l := range strings.Split(src, "\n") {
		if strings.HasPrefix(strings.TrimSpace(l), "//") {
			continue
		}
		b.WriteString(l)
		b.WriteString("\n")
	}
	return b.String()
}

// THE FALSE PAGE THIS ALMOST CAUSED, caught on the box minutes after deploy.
//
// The actuation worker published tg_ingest_upstream_readable=0 for both sites, and UpstreamProbeUnreadable
// fires on readable==0 after 30m. That zero was CORRECT: TG-337 scoped the actuation plane's LibreNMS token
// to device reads, so it 403s on /alerts by design. A deliberate security posture would have paged as a
// fault, every 2 minutes spending a 403 against every LibreNMS for a number that plane cannot use.
func TestTheActuationPlaneDoesNotProbeTheUpstream(t *testing.T) {
	deps := []librenms.Deployment{{Site: "dc1", BaseURL: "https://nms.example", TokenRef: "env:T"}}
	if got := upstreamProbeSourceFor(false, nil, deps, http.DefaultClient); got != nil {
		t.Fatal("a plane that does not hold triage still built an upstream prober. Its LibreNMS token 403s " +
			"on /alerts by design (TG-337), so it publishes readable=0 and UpstreamProbeUnreadable pages on " +
			"a control working exactly as intended.")
	}
	// And it must stay nil even when a polled source was somehow constructed — the plane gate comes first.
	polled := librenms.NewAlertSource(deps)
	if got := upstreamProbeSourceFor(false, polled, deps, http.DefaultClient); got != nil {
		t.Fatal("the plane gate was bypassed by an already-constructed polled source")
	}
}
