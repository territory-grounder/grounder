package main

// authlog_wiring.go — STARTING THE COLLECTOR, PLANE-SCOPED AND DARK BY DEFAULT (TG-315).
//
// PLANE-SCOPED TO TRIAGE. The collector MINTS TRIAGE SESSIONS from untrusted device-log content, which is
// exactly the intake the actuation worker must never perform — the same reasoning that puts
// TG_PVE_LIVENESS_POLL_INTERVAL on triagePlaneEnvKeys (TG-153). Its knobs are read through planeEnv, so
// the actuation plane cannot start it even if the value is present in the shared environment.
//
// DARK BY DEFAULT, DELIBERATELY. `TG_AUTHLOG_POLL_INTERVAL` ships EMPTY. Unset means the loop does not
// run — an operator arms it, exactly like the liveness poller and the discovery probes. Reading auth logs
// opens an SSH session per host per tick against a shipping estate, and that is a posture change an
// operator makes, not one a deploy makes for them.
//
// THE HOST SET IS CONFIGURED, NOT DISCOVERED, and the default is MEASURED rather than guessed. Probed
// 2026-08-06 through the production read guard across all 126 monitored dc1 hosts, no sampling:
// exactly 15 ship logs. Enumerating the syslog-ng tree instead would read every directory it has ever
// created — including the malformed hostnames it occasionally mints (`ankh`, `DPAA`, `I2C` are live
// examples) — and mint triage sessions keyed on them.

import (
	"context"
	"log"
	"strings"
	"time"

	coreingest "github.com/territory-grounder/grounder/core/ingest"
	"github.com/territory-grounder/grounder/modules/observability/syslogng"
)

// authlogMintFunc admits ONE envelope: mint the triage workflow, record the accepted alert, make the
// suppression observation. Injected rather than reached for, so this file carries no Temporal client, no
// database handle and no knowledge of how admission works — and so the collector's own oracles never need
// any of them either.
type authlogMintFunc func(ctx context.Context, env coreingest.IncidentEnvelope) error

// startAuthlogCollector arms the loop if — and only if — an interval and a host set are both configured.
//
// Returns the yield register in every case, INCLUDING when the loop does not start. A register that
// appears only once the collector is armed makes "not configured" and "configured but dead" the same
// observation, which is the whole defect this source exists to avoid being an example of.
func startAuthlogCollector(
	planeEnv func(key, def string) string,
	servers []syslogng.Server,
	runner syslogng.Runner,
	mint authlogMintFunc,
) *authlogYield {
	reg := &authlogYield{}

	iv := strings.TrimSpace(planeEnv("TG_AUTHLOG_POLL_INTERVAL", ""))
	hosts := authlogHosts(planeEnv("TG_AUTHLOG_HOSTS", ""))
	if iv == "" {
		log.Printf("authlog collector: DARK — TG_AUTHLOG_POLL_INTERVAL is unset, so the estate's auth logs " +
			"are not read and ingest_alert gains no security-incident rows. This is the shipped default; an " +
			"operator arms it.")
		return reg
	}
	d, err := time.ParseDuration(iv)
	if err != nil || d <= 0 {
		log.Printf("authlog collector: NOT STARTED — TG_AUTHLOG_POLL_INTERVAL=%q is not a positive duration "+
			"(%v). Refusing to guess an interval that opens SSH sessions across the estate.", iv, err)
		return reg
	}
	if len(servers) == 0 || runner == nil {
		log.Printf("authlog collector: NOT STARTED — an interval is configured but no syslog-ng server is " +
			"(TG_SYSLOGNG_DEPLOYMENTS). The collector reads the same trees the investigation tools do; " +
			"without a server there is nothing to read.")
		return reg
	}
	if len(hosts) == 0 {
		log.Printf("authlog collector: NOT STARTED — an interval is configured but TG_AUTHLOG_HOSTS is empty. " +
			"The host set is deliberately explicit: enumerating the syslog-ng tree would read every " +
			"directory it ever created, including malformed hostnames, and mint triage keyed on them.")
		return reg
	}
	if mint == nil {
		log.Printf("authlog collector: NOT STARTED — no admission function wired (no Temporal client). The " +
			"loop would read logs and drop every event, which is worse than not reading them.")
		return reg
	}

	c := newAuthlogCollector(servers, hosts, runner, time.Now)
	c.yield = reg
	log.Printf("authlog collector: ARMED every %s over %d host(s) across %d syslog server(s) — the "+
		"correlator's first non-availability witness. ingest_alert holds 0 security-incident rows today.",
		d, len(hosts), len(servers))

	go func() {
		t := time.NewTicker(d)
		defer t.Stop()
		for range t.C {
			ctx, cancel := context.WithTimeout(context.Background(), d)
			got := c.collectOnce(ctx)
			reg.record(time.Now(), got)
			for _, env := range got.Envelopes {
				if err := mint(ctx, env); err != nil {
					log.Printf("authlog collector: admit %s failed: %v", env.ExternalRef, err)
				}
			}
			// Failures are logged per poll, not per host, so a wholly-unreachable server does not write one
			// line per host per tick forever. The COUNT is in the register either way.
			if n := len(got.Failures); n > 0 {
				log.Printf("authlog collector: %d of %d read(s) failed this poll (first: %s)",
					n, got.Offered, got.Failures[0])
			}
			// A tripped enumeration cap is a security signal, not routine noise — a username spray was
			// bounded so it could not mint one investigation per attempted username. The count also rides
			// tg_authlog_enumeration_suppressed_total for alerting.
			if got.Suppressed > 0 {
				log.Printf("authlog collector: enumeration cap suppressed %d distinct-principal event(s) "+
					"this poll (ceiling %d per host+kind) — a username spray bounded before it could flood "+
					"the single-brain triage queue", got.Suppressed, authlogMaxPrincipalsPerHostKind)
			}
			cancel()
		}
	}()
	return reg
}

// authlogHosts parses the operator-declared host list: comma or space separated, order irrelevant,
// duplicates collapsed. Empty yields nothing — never a default host set, because a guessed host is an SSH
// session against a machine nobody asked to be read.
func authlogHosts(spec string) []string {
	seen := map[string]bool{}
	var out []string
	for _, f := range strings.FieldsFunc(spec, func(r rune) bool { return r == ',' || r == ' ' || r == '\t' || r == '\n' }) {
		h := strings.ToLower(strings.TrimSpace(f))
		if h == "" || seen[h] {
			continue
		}
		seen[h] = true
		out = append(out, h)
	}
	return out
}
