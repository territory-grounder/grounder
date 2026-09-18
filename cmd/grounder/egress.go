package main

// egress.go — the grounder's half of the outbound meter (TG-324).
//
// TG-160 built core/egress and installed it in cmd/worker. Nothing installed it here, and nothing
// recorded that as a decision: docs/THREAT-MODEL.md §5.3's "Where it is enforced" list names
// cmd/worker/egress.go, cmd/worker/main.go and cmd/worker/admin.go, and the grounder is simply absent
// from it rather than excluded by it.
//
// Measured on the running estate, 2026-08-07:
//
//	grounder:8080/metrics  ->  200, 9 lines, tg_egress_* series: 0
//	grep installEgressMeter cmd/grounder/  ->  nothing
//
// This is not an "empty column is not a broken feature" case, which is the mistake to rule out first.
// The egress posture table's own reason field for this service says it egresses:
//
//	"grounder": {frontdoor: true, egress: true, why: "OpenBao for its own read credential and the console
//	 WRITER AppRole, plus LDAPS to FreeIPA for browser operator login."}
//
// OpenBao is HTTPS over http.DefaultTransport — exactly what the meter wraps. (LDAPS is not HTTP and the
// meter cannot see it either way; that residual is stated in §5.3 and is unchanged by this file.)
//
// AND IT IS THE WORST PLANE TO LEAVE OPEN. The grounder is the only TG process attached to
// `tg-frontdoor`, the published surface. An attacker with code execution in the worker meets an enforcing
// allowlist; the same attacker in the internet-facing process met nothing at all.

import (
	"os"
	"sync/atomic"

	"github.com/territory-grounder/grounder/core/db"
	"github.com/territory-grounder/grounder/core/egress"
	"github.com/territory-grounder/grounder/core/metrics"
	"github.com/territory-grounder/grounder/modules/credsource/openbao"
)

// grounderEgressModeDefault is the posture this binary COMPILES IN when compose/.env say nothing.
//
// It is a named constant rather than a literal at the call site so the guard can assert the real default
// instead of a copy of it — a test that hardcodes "meter" passes forever after someone changes the call
// site to "enforce", which is the mutation that matters here.
const grounderEgressModeDefault = "meter"

// grounderEgress is the installed meter, held for the /metrics exposition. nil until main() installs it,
// so tests that never boot the process see no meter and no behaviour change.
var grounderEgress *egress.Meter

// grounderEffectiveEnviron is os.Environ() with the operator's console-saved settings folded ON TOP, so
// the allowlist reflects what this process will actually dial rather than what its .env said at build
// time. Without the fold, a connector configured entirely through the console (TG-260) is invisible to
// the destination scan and its legitimate traffic is reported as off-allowlist — the false positive that
// gets a security meter muted in week one, and the reason the worker folds the same way.
//
// It reads the SAME snapshot `get` resolves overrides from, so the allowlist and the configuration can
// not disagree about what this deployment declared.
func grounderEffectiveEnviron() []string {
	out := append([]string(nil), os.Environ()...)
	if m := grounderOverrides.Load(); m != nil {
		for k, v := range *m {
			out = append(out, k+"="+v)
		}
	}
	return out
}

// grounderEgressSamples is the tg_egress_* lane for this process's /metrics.
//
// IT RETURNS NOTHING WHEN THE METER IS ABSENT, deliberately, rather than a row of zeros. A fabricated
// `tg_egress_requests_total 0` would make "the meter is not installed" and "the meter is installed and
// this process made no outbound calls" the same observation — which is the precise defect this ticket
// exists to close, reproduced one layer up. Absent is honest; zero would not be.
//
// The help strings are the worker's, verbatim, because two processes publishing the same series name
// with different explanations is how an operator learns to distrust both.
func grounderEgressSamples() []metrics.Sample {
	if grounderEgress == nil {
		return nil
	}
	es := grounderEgress.Snapshot()
	enforcing := 0.0
	if es.Enforcing {
		enforcing = 1
	}
	lbl := map[string]string{"component": "grounder"}
	return []metrics.Sample{
		{Name: "tg_egress_requests_total", Kind: metrics.Counter, Labels: lbl, Value: float64(es.Requests),
			Help: "outbound HTTP requests metered by the TG-160 egress meter, all destinations."},
		{Name: "tg_egress_bytes_out_total", Kind: metrics.Counter, Labels: lbl, Value: float64(es.BytesOut),
			Help: "outbound request-body bytes metered by the TG-160 egress meter. VOLUME is the exfil dimension a destination count cannot see."},
		{Name: "tg_egress_bytes_in_total", Kind: metrics.Counter, Labels: lbl, Value: float64(es.BytesIn),
			Help: "inbound response-body bytes read back from outbound calls."},
		{Name: "tg_egress_offallowlist_requests_total", Kind: metrics.Counter, Labels: lbl, Value: float64(es.OffRequests),
			Help: "outbound requests to a destination this deployment never DECLARED. Non-zero is the covert-channel signal — read it against tg_egress_allowlist_rules before treating it as an intrusion."},
		{Name: "tg_egress_offallowlist_bytes_out_total", Kind: metrics.Counter, Labels: lbl, Value: float64(es.OffBytesOut),
			Help: "request-body bytes sent to UNDECLARED destinations. This is the exfil volume."},
		{Name: "tg_egress_offallowlist_destinations", Kind: metrics.Gauge, Labels: lbl, Value: float64(len(es.OffAllowlist)),
			Help: "distinct undeclared destination hosts seen (bounded; overflow folds into the 'other' bucket)."},
		{Name: "tg_egress_refused_total", Kind: metrics.Counter, Labels: lbl, Value: float64(es.Refusals),
			Help: "outbound requests REFUSED for being off-allowlist. Always 0 unless TG_EGRESS_MODE=enforce."},
		{Name: "tg_egress_allowlist_rules", Kind: metrics.Gauge, Labels: lbl, Value: float64(es.AllowlistRules),
			Help: "declared outbound destinations the meter compares traffic against. A flat 0 means the meter is measuring against nothing (the vacuity condition), NOT that egress is clean."},
		{Name: "tg_egress_enforcing", Kind: metrics.Gauge, Labels: lbl, Value: enforcing,
			Help: "1 = off-allowlist destinations are BLOCKED; 0 = metered only (the default posture)."},
	}
}

// meteredBaoTransport hands this process's meter to the OpenBao delivery client (TG-415).
//
// WHY IT IS NEEDED AT ALL. vault.New must build its own http.Transport to carry the CA / mTLS config, and
// a client with its own Transport never touches http.DefaultTransport — where the meter installs. So the
// grounder's OpenBao traffic, the destination its whole egress grant exists for, was uncounted,
// unnamed and unblockable. Measured before the fix: tg_egress_enforcing 1, tg_egress_allowlist_rules 15,
// tg_egress_requests_total 0, in the same second the boot log resolved four bao: references.
//
// A meter enforcing over ZERO observed requests reads on every dashboard as a clean estate. That is the
// failure mode, not the missing count.
//
// Returns EMPTY when no meter is installed rather than a wrap around nil: the delivery client must still
// be built, because refusing to resolve secrets in order to measure them would trade a blind spot for an
// outage. Ordering is what makes this work — egress.Install runs earlier in main() than the OpenBao
// wiring, so grounderEgress is already set by the time this is called.
func meteredBaoTransport() []openbao.WireOption {
	if grounderEgress == nil {
		return nil
	}
	return []openbao.WireOption{openbao.WithTransportWrap(grounderEgress.Wrap)}
}

// schemaDrift holds the boot-time result of comparing the RUNNING database against the migrations
// embedded in this build (TG-383). nil until main() runs the check, which is load-bearing: the samples
// below emit NOTHING while it is nil, so "the check never ran" stays distinguishable from "the check ran
// and found nothing". A fabricated `tg_schema_undeclared_tables 0` would read on every dashboard as a
// verified-clean schema, which is the exact confusion this ticket exists to end.
var schemaDrift atomic.Pointer[db.SchemaDrift]

// schemaDriftSamples publishes the drift lane.
//
// tg_schema_tables_total is the DENOMINATOR and it is not decoration: a zero undeclared-count means
// nothing unless you know how many tables were examined. That pairing is the whole lesson of this
// finding — the original guard reported clean over a population that excluded the only interesting
// member.
func schemaDriftSamples() []metrics.Sample {
	d := schemaDrift.Load()
	if d == nil {
		return nil
	}
	lbl := map[string]string{"component": "grounder"}
	return []metrics.Sample{
		{Name: "tg_schema_tables_total", Kind: metrics.Gauge, Labels: lbl, Value: float64(d.Total),
			Help: "ordinary tables in `public` on the RUNNING database. The denominator: an undeclared count of 0 against an unknown total is the vacuous reading TG-383 was filed about."},
		{Name: "tg_schema_undeclared_tables", Kind: metrics.Gauge, Labels: lbl, Value: float64(len(d.Undeclared)),
			Help: "tables in the running database that NO migration in this build creates. Non-zero means an object nothing in this repo knows about — no schema guard, grant rule or review has considered it, and one such table has already aborted the credential-plane grant derivation."},
		{Name: "tg_schema_unplaned_tables", Kind: metrics.Gauge, Labels: lbl, Value: float64(len(d.UnplaneD)),
			Help: "tables carrying no `plane:` declaration. An undeclared table is granted to BOTH credential planes by default, so a compromised triage worker could forge any actuation record it holds."},
	}
}
