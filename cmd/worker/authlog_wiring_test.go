package main

import (
	"context"
	"strings"
	"testing"
	"time"

	coreingest "github.com/territory-grounder/grounder/core/ingest"
	"github.com/territory-grounder/grounder/modules/observability/syslogng"
)

// TG-315 — the collector must be DARK BY DEFAULT and must publish its register EITHER WAY.
//
// The second property is the load-bearing one. A yield register that appears only once the loop is armed
// makes "not configured" and "configured but dead" the same observation — which is precisely the defect
// this source was built to stop being an example of. Every refusal path below is therefore asserted to
// still return a register.

func authlogTestEnv(kv map[string]string) func(string, string) string {
	return func(k, def string) string {
		if v, ok := kv[k]; ok {
			return v
		}
		return def
	}
}

func authlogNoopMint() authlogMintFunc {
	return func(context.Context, coreingest.IncidentEnvelope) error { return nil }
}

func authlogOneServer() []syslogng.Server {
	return []syslogng.Server{{Site: "nl", SSHHost: "dc1syslogng01", SSHUser: "root"}}
}

// DARK BY DEFAULT: no interval, no loop — and still a register.
func TestTheAuthlogCollectorIsDarkWithoutAnInterval(t *testing.T) {
	reg := startAuthlogCollector(
		authlogTestEnv(map[string]string{"TG_AUTHLOG_HOSTS": "web01"}),
		authlogOneServer(), &fakeAuthlogRunner{}, authlogNoopMint())

	if reg == nil {
		t.Fatal("no yield register returned when the collector is dark — 'not configured' and 'configured " +
			"but dead' would then be the same observation, which is the defect this source exists to avoid")
	}
	if got := len(reg.samples(time.Now())); got != 7 {
		t.Errorf("a dark collector published %d series, want all 7 — a register that appears only once "+
			"armed cannot distinguish dark from dead", got)
	}
}

// Every refusal path must behave the same way: no loop, but a register.
func TestEveryAuthlogRefusalPathStillPublishesItsRegister(t *testing.T) {
	cases := map[string]struct {
		env     map[string]string
		servers []syslogng.Server
		runner  syslogng.Runner
		mint    authlogMintFunc
	}{
		"malformed interval": {
			env:     map[string]string{"TG_AUTHLOG_POLL_INTERVAL": "not-a-duration", "TG_AUTHLOG_HOSTS": "web01"},
			servers: authlogOneServer(), runner: &fakeAuthlogRunner{}, mint: authlogNoopMint(),
		},
		"non-positive interval": {
			env:     map[string]string{"TG_AUTHLOG_POLL_INTERVAL": "0s", "TG_AUTHLOG_HOSTS": "web01"},
			servers: authlogOneServer(), runner: &fakeAuthlogRunner{}, mint: authlogNoopMint(),
		},
		"no syslog server": {
			env:     map[string]string{"TG_AUTHLOG_POLL_INTERVAL": "5m", "TG_AUTHLOG_HOSTS": "web01"},
			servers: nil, runner: &fakeAuthlogRunner{}, mint: authlogNoopMint(),
		},
		"no hosts": {
			env:     map[string]string{"TG_AUTHLOG_POLL_INTERVAL": "5m"},
			servers: authlogOneServer(), runner: &fakeAuthlogRunner{}, mint: authlogNoopMint(),
		},
		"no admission function": {
			env:     map[string]string{"TG_AUTHLOG_POLL_INTERVAL": "5m", "TG_AUTHLOG_HOSTS": "web01"},
			servers: authlogOneServer(), runner: &fakeAuthlogRunner{}, mint: nil,
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			reg := startAuthlogCollector(authlogTestEnv(tc.env), tc.servers, tc.runner, tc.mint)
			if reg == nil {
				t.Fatalf("%s returned no register", name)
			}
			if got := len(reg.samples(time.Now())); got != 7 {
				t.Errorf("%s published %d series, want 7", name, got)
			}
			// A refusal must not have polled.
			var polls float64
			for _, s := range reg.samples(time.Now()) {
				if s.Name == "tg_authlog_polls_total" {
					polls = s.Value
				}
			}
			if polls != 0 {
				t.Errorf("%s recorded %v poll(s) — a refused configuration must not run the loop", name, polls)
			}
		})
	}
}

// THE HOST SET IS NEVER DEFAULTED. A guessed host is an SSH session against a machine nobody asked to be
// read, so an empty spec must yield nothing rather than a built-in list.
func TestAnEmptyHostSpecYieldsNoHostsRatherThanADefault(t *testing.T) {
	if got := authlogHosts(""); len(got) != 0 {
		t.Errorf("an empty TG_AUTHLOG_HOSTS produced %v — a defaulted host set opens SSH sessions against "+
			"machines nobody declared", got)
	}
	if got := authlogHosts("   ,  , "); len(got) != 0 {
		t.Errorf("a whitespace/comma-only spec produced %v", got)
	}
}

// The parser accepts what an operator will actually write, and collapses duplicates so a copy-paste does
// not double every poll's SSH sessions against one host.
func TestTheHostSpecAcceptsCommasSpacesAndCollapsesDuplicates(t *testing.T) {
	got := authlogHosts("dc1fw01, dc1rtr01 dc1sw01,NLLEI01FW01")
	want := []string{"dc1fw01", "dc1rtr01", "dc1sw01"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v (duplicates must collapse, case-insensitively)", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("host %d = %q, want %q", i, got[i], want[i])
		}
	}
}

// The knobs must be PLANE-SCOPED to triage: the collector mints triage sessions from untrusted device-log
// content, which the actuation worker must never do. Without this the actuation plane could arm it.
func TestTheAuthlogKnobsArePlaneScopedToTriage(t *testing.T) {
	joined := strings.Join(triagePlaneEnvKeys, ",")
	for _, k := range []string{"TG_AUTHLOG_POLL_INTERVAL", "TG_AUTHLOG_HOSTS"} {
		if !strings.Contains(joined, k) {
			t.Errorf("%s is not on triagePlaneEnvKeys — the actuation plane could arm a lane that MINTS "+
				"triage sessions from untrusted device-log content (TG-153)", k)
		}
	}
}
