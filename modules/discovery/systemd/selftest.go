package systemd

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"sync"

	"github.com/territory-grounder/grounder/core/selftest"
)

// compile-time proof this source can answer the console's TEST button. The capability is OPTIONAL and
// detected by assertion (core/selftest.Of), so without this line the dialog would degrade to "no test is
// implemented" — honest, but it would leave the declared verb unperformable.
var _ selftest.Tester = (*Source)(nil)

const (
	// selfTestFanout bounds how many hosts are read CONCURRENTLY.
	//
	// moduletest allows 30 seconds and ONE attempt, and the transport gives each host its own timeout (15s
	// by default), so a serial probe over a dozen hosts could not finish and would report a timeout instead
	// of an estate. Concurrency is what lets the probe honour the verb — "the declared hosts", not "the
	// first two" — while the cap keeps it from opening sixty-four SSH sessions at once against machines
	// other people are also using. A host that does not get its turn before the budget expires is REPORTED
	// as not attempted, never counted as reachable.
	selfTestFanout = 8
	// selfTestHostSample bounds how many hosts the Summary and the Detail name each. The totals stay exact;
	// only the enumeration is trimmed, because a Result is rendered in a dialog and pasted into tickets and
	// must not grow with the estate.
	selfTestHostSample = 6
)

// hostProbe is one host's outcome. `attempted` distinguishes "this host failed" from "the budget ran out
// before this host was tried" — collapsing those two would let an unprobed host read as a healthy one.
type hostProbe struct {
	host      string
	units     int
	sample    string
	err       error
	attempted bool
}

// SelfTest runs `systemctl list-units --type=service` on the declared hosts and counts the units each
// returns — the descriptor's verb, performed with the source's OWN fixed argv over its OWN injected runner.
//
// WHY THIS IS THE REAL PATH AND NOT A REHEARSAL OF IT. The runner behind s.run is the production discovery
// transport, and every layer of it can refuse for a different reason an operator would have to fix
// differently: the host allowlist (read once at boot, so a host added since is refused), the audited
// credential engine (no read-only identity for that host ⇒ fail closed, nothing sent), host-key
// verification against known_hosts (no file, unknown key, or a CHANGED key ⇒ refuse rather than read from a
// machine TG cannot authenticate), then the SSH authentication itself. None of those is visible to a check
// that the host list and the known_hosts path are non-empty, and all of them are exactly what TEST exists
// to rule out. The argv is the package CONSTANT the source always issues; the probe builds no command,
// because a discovery source that could be handed one would be an execution path wearing a reader's name.
//
// WHY IT COUNTS RATHER THAN DRAFTS. Nothing here emits an edge, writes a draft, or adopts anything —
// adoption is a governed transition an operator performs deliberately, and a settings dialog must never be
// a way to cause one. The probe reads, counts, and keeps one unit name per host as evidence.
//
// WHAT A GREEN RESULT PROVES: for each host named, TG holds a usable read-only credential, the host's key
// verified, the session opened, and systemctl ran. WHAT IT DOES NOT PROVE: that these are the hosts you
// meant — the Summary lists them with their unit counts so a human can see that for themselves — nor
// anything about a host absent from the list, which the transport refuses and this probe cannot see, just
// as discovery cannot.
//
// operator is ignored: the probe leaves no trace in anyone's console, so there is no event needing an
// author.
func (s *Source) SelfTest(ctx context.Context, _ string) (selftest.Result, error) {
	if s.run == nil {
		return selftest.Result{
			Summary: "no read-only command runner is wired for systemd discovery",
			Detail: "this is a TG-side fault, not a host one: nothing was contacted. The transport is built " +
				"only when the host allowlist and the credential engine are both present, so a module with " +
				"no runner is one whose configuration never produced a transport — check the declared hosts " +
				"and the known_hosts path, then restart the worker (both are read once at boot).",
		}, fmt.Errorf("systemd discovery: no runner injected")
	}
	if len(s.hosts) == 0 {
		return selftest.Result{
			Summary: "no hosts are declared, so there is nothing to enumerate",
			Detail: "with an empty host list this source is never built and no unit is ever drafted for " +
				"adoption. Add the hosts to enumerate and restart the worker.",
		}, fmt.Errorf("systemd discovery: no hosts declared")
	}

	results := make([]hostProbe, len(s.hosts))
	var wg sync.WaitGroup
	gate := make(chan struct{}, min(selfTestFanout, len(s.hosts)))
	for i, host := range s.hosts {
		wg.Add(1)
		go func(i int, host string) {
			defer wg.Done()
			gate <- struct{}{}
			defer func() { <-gate }()
			results[i].host = host
			// Checked HERE, after waiting for a slot: a host whose turn comes after the operator's spinner
			// has already expired must not open a session nobody is waiting for.
			if ctx.Err() != nil {
				return
			}
			results[i].attempted = true
			out, err := s.run.Run(ctx, host, listUnitsArgv)
			if err != nil {
				results[i].err = err
				return
			}
			// The REAL parser, not a line count: what the probe reports must be what discovery would
			// actually draft, so a host whose output TG cannot parse reads as zero units here too rather
			// than as a healthy host that mysteriously contributes nothing later.
			units := ParseUnits(string(out))
			results[i].units = len(units)
			if len(units) > 0 {
				results[i].sample = units[0]
			}
		}(i, host)
	}
	wg.Wait()

	var reached, units int
	var perHost, problems, quiet []string
	var example string
	for _, r := range results {
		switch {
		case !r.attempted:
			problems = append(problems, r.host+": not attempted — the test budget expired before its turn")
		case r.err != nil:
			problems = append(problems, r.host+": "+classifySelfTestFailure(r.err))
		default:
			reached++
			units += r.units
			perHost = append(perHost, fmt.Sprintf("%s (%d)", r.host, r.units))
			if r.units == 0 {
				quiet = append(quiet, r.host)
			}
			if example == "" && r.sample != "" {
				example = r.sample + " on " + r.host
			}
		}
	}

	detail := strings.Join(bound(problems, selfTestHostSample), "; ")
	if reached == 0 {
		return selftest.Result{
			Summary: fmt.Sprintf("could not run `systemctl list-units` on any of the %s declared",
				plural(len(s.hosts), "host")),
			Detail: detail,
		}, fmt.Errorf("systemd discovery: selftest reached none of the %d declared host(s)", len(s.hosts))
	}

	summary := fmt.Sprintf("ran `systemctl list-units --type=service` on %d of %d declared hosts: %s",
		reached, len(s.hosts), plural(units, "loaded service"))
	if len(perHost) > 0 {
		summary += " — " + strings.Join(bound(perHost, selfTestHostSample), ", ")
	}
	if example != "" {
		summary += "; e.g. " + example
	}

	if len(problems) > 0 {
		// Partial is LOUD, and deliberately so: it is the rule Edges already follows, because one
		// unreachable machine must never be smoothed into a pass that reads as "this host runs nothing".
		return selftest.Result{Summary: summary, Detail: detail},
			fmt.Errorf("systemd discovery: selftest failed on %d of %d host(s)", len(problems), len(s.hosts))
	}
	if len(quiet) > 0 {
		// Zero units is NOT the same finding here as an empty `docker ps`. A running Linux host always has
		// loaded services, so an empty read means the session worked and the answer was unusable — a shell
		// that mangled the command, a container without systemd, or output in a shape ParseUnits does not
		// recognise. It is still not a failure of the credential or the host key, which is why it is a
		// qualified pass and not a red test.
		return selftest.Result{
			Summary: summary,
			Detail: "no .service units came back from " + strings.Join(bound(quiet, selfTestHostSample), ", ") +
				". The session opened and the command ran, so this is not a credential or host-key problem — " +
				"but a running Linux host normally has loaded services, so check that machine actually uses " +
				"systemd (a container may not) and that the read-only account may run systemctl there. " +
				"Until it returns units, nothing from it will be offered for adoption.",
		}, nil
	}
	return selftest.Result{Summary: summary}, nil
}

// classifySelfTestFailure turns one host's failure into something an operator can act on. "error" tells them
// nothing; "this host's key is not in known_hosts" tells them exactly what to fix, and "the key CHANGED"
// tells them not to "fix" it at all until they know why.
//
// It classifies on the SHAPE of the failure — which layer of the transport refused, in the order they are
// tried — and falls through to the raw error rather than inventing a diagnosis. Ordering matters: an SSH
// library reports a host-key rejection AS a handshake failure, so the host-key arms come before the
// authentication arm; otherwise every changed key would be reported as a bad account, which is the one
// diagnosis that makes an operator "fix" a security event by overwriting the evidence.
func classifySelfTestFailure(err error) string {
	s := strings.ToLower(err.Error())
	switch {
	case strings.Contains(s, "not in the operator discovery allowlist"):
		return "the transport refused this host because it is not on the discovery allowlist (fail closed). " +
			"The list is read once at boot, so a host added since then is refused until the worker restarts."
	case strings.Contains(s, "not a valid host label"):
		return "this is not a usable host label — it must be a plain hostname (letters, digits, dot, dash, " +
			"underscore), never a path, a URL or a user@host."
	case strings.Contains(s, "no resolvable read-only ssh credential") ||
		strings.Contains(s, "did not resolve") || strings.Contains(s, "resolved empty"):
		return "no read-only SSH credential could be resolved for this host, so the read was REFUSED rather " +
			"than attempted unverified. The per-host identity comes from the audited credential engine, not " +
			"from this dialog — a host it cannot answer for is fail-closed by design."
	case strings.Contains(s, "did not parse as a private key"):
		return "the SSH key resolved for this host is not a usable private key (wrong format, or a public " +
			"key stored by mistake). Nothing was sent."
	case strings.Contains(s, "key mismatch"):
		return "THE HOST KEY HAS CHANGED since it was recorded in known_hosts. TG refuses to read from a " +
			"host it cannot verify. If this machine was genuinely rebuilt, update the known_hosts entry; if " +
			"it was not, this is a security event and not a configuration nuisance."
	case strings.Contains(s, "knownhosts") || strings.Contains(s, "known_hosts"):
		return "host-key verification failed: this host's key is missing from the known_hosts file, or the " +
			"file itself is unset or unreadable. Every read is refused rather than performed unverified — " +
			"the failure direction is a missing observation, never an unverified host."
	case strings.Contains(s, "unable to authenticate") || strings.Contains(s, "handshake failed") ||
		strings.Contains(s, "permission denied"):
		return "the host answered but rejected TG's key. Check the read-only account exists there and that " +
			"the public half of TG's discovery key is in its authorized_keys."
	case strings.Contains(s, "no such host") || strings.Contains(s, "connection refused") ||
		strings.Contains(s, "no route to host") || strings.Contains(s, "network is unreachable"):
		return "the host could not be reached — check the name resolves, that the machine is up, and that " +
			"the worker may reach it on port 22."
	case strings.Contains(s, "timeout") || strings.Contains(s, "deadline") || strings.Contains(s, "context canceled"):
		return "the read timed out. The host is either down, unreachable through a firewall, or slower than " +
			"the per-host read timeout; the other hosts still contributed."
	default:
		return err.Error()
	}
}

// bound trims a list for display and says how many were dropped. The COUNTS in the Summary are always
// complete — only the enumeration is trimmed — so an operator is never shown a number that quietly means
// "of the ones we printed".
func bound(items []string, limit int) []string {
	if len(items) <= limit {
		return items
	}
	return append(append([]string{}, items[:limit]...), fmt.Sprintf("+%d more", len(items)-limit))
}

// plural renders a count with its noun so the Summary reads as a sentence rather than a log line: an
// operator who reads "1 loaded services" wonders whether the probe counted correctly.
func plural(n int, noun string) string {
	if n == 1 {
		return "1 " + noun
	}
	return strconv.Itoa(n) + " " + noun + "s"
}
