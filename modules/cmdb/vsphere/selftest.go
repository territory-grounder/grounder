package vsphere

import (
	"context"
	"fmt"
	"net/url"
	"sort"
	"strconv"
	"strings"

	"github.com/territory-grounder/grounder/core/selftest"
)

// compile-time proof this reader can answer the console's TEST button. The capability is OPTIONAL and
// detected by assertion, so without this line the dialog would degrade to "no test is implemented" — honest,
// but it would leave the descriptor's declared verb unperformable.
var _ selftest.Tester = (*EstateSource)(nil)

// selfTestHostSample bounds how many ESXi hosts the Summary names — a Result is rendered in a dialog and
// pasted into tickets, so a large vCenter must not produce a line no one can read.
const selfTestHostSample = 4

// SelfTest logs in to vCenter and lists its VMs and the ESXi host each runs on — the descriptor's verb,
// performed over the reader's OWN path: it calls Edges, the exact connect→login→view→retrieve→map the estate
// refresh runs, so what the probe counts is what the refresh loop would draft.
//
// WHY Edges IS ENOUGH HERE (unlike pve, which keeps the raw body). pve must inspect the body to tell "an
// empty cluster" from "not Proxmox at all", because a gateway answers 2xx with the wrong JSON. vCenter has no
// such ambiguity: govmomi.NewClient performs a SOAP handshake and a SessionManager login, so anything that is
// NOT a vCenter fails to CONNECT — a login error IS the wrong-URL/credentials diagnosis, and a successful
// login returning zero VMs is a genuine empty/permission-scoped inventory, never a wrong endpoint.
//
// operator is ignored: nothing here leaves a trace in anyone's console, so there is no event needing an author.
func (s *EstateSource) SelfTest(ctx context.Context, _ string) (selftest.Result, error) {
	edges, err := s.Edges(ctx)
	if err != nil {
		return selftest.Result{
			Summary: "could not log in to vCenter at " + s.instanceLabel() + " and list its VMs",
			Detail:  classifySelfTestFailure(err),
		}, err
	}

	perHost := map[string]int{}
	for _, e := range edges {
		perHost[e.To.Name]++
	}
	vms := len(edges)
	edges = nil // the tallies are all the report needs; do not hold a vCenter-sized slice alive for a string

	if vms == 0 {
		// A PASS, loudly qualified. The login succeeded — real information — but this account may lack the read
		// privilege on the VM inventory, in which case vCenter returns an empty view rather than a refusal. This
		// module would then contribute no placement, and a live-hypervisor tier of edges would silently be absent
		// from every blast radius; reporting it as an unqualified success is how that goes unnoticed.
		return selftest.Result{
			Summary: "logged in to vCenter at " + s.instanceLabel() + ", but it reports NO VMs visible to this account",
			Detail: "the login succeeded and the endpoint is a real vCenter — it simply returned no VM " +
				"placements. Either it genuinely runs no VMs, or this account cannot see them: give it a " +
				"READ-ONLY role with System.View plus VirtualMachine read on the inventory. Until then this " +
				"module contributes no VM→host edges at all.",
		}, nil
	}

	hosts := make([]string, 0, len(perHost))
	for h := range perHost {
		hosts = append(hosts, h)
	}
	// Sorted because Go randomises map iteration and a Summary that reorders itself between presses looks like
	// the inventory changed.
	sort.Strings(hosts)

	var placement []string
	for i, h := range hosts {
		if i == selfTestHostSample {
			placement = append(placement, fmt.Sprintf("+%d more", len(hosts)-selfTestHostSample))
			break
		}
		placement = append(placement, fmt.Sprintf("%s (%d)", h, perHost[h]))
	}

	summary := fmt.Sprintf("read vCenter at %s: %s placed across %s — %s",
		s.instanceLabel(), plural(vms, "VM"), plural(len(hosts), "ESXi host"),
		strings.Join(placement, ", "))
	return selftest.Result{Summary: summary}, nil
}

// instanceLabel renders the configured endpoint HOST ONLY — a URL may legally carry userinfo and a Result is
// pasted into tickets, so this string must never carry credential material. url.Host drops userinfo by
// construction, and a URL too malformed to parse degrades to a phrase rather than to its own raw text.
func (s *EstateSource) instanceLabel() string {
	if u, err := url.Parse(s.baseURL); err == nil && u.Host != "" {
		return u.Host
	}
	return "the configured vCenter URL"
}

// classifySelfTestFailure turns a failed login/list into something an operator can act on. govmomi surfaces
// SOAP faults and transport errors (no HTTP status to key on), so this classifies on the error prose's shape
// and falls through to the raw error rather than inventing a diagnosis.
func classifySelfTestFailure(err error) string {
	s := strings.ToLower(err.Error())
	switch {
	case strings.Contains(s, "resolve password"):
		return "the vCenter password could not be READ from the secret backend — the token reference is wrong, " +
			"or the backend is unreachable. This is a TG-side problem, not a vCenter one: nothing was sent."
	case strings.Contains(s, "incorrect user name or password") || strings.Contains(s, "cannot complete login") ||
		strings.Contains(s, "invalid login") || strings.Contains(s, "permission to perform this operation"):
		return "vCenter rejected the login — check TG_VSPHERE_USER (e.g. svc-tg@vsphere.local, including the SSO " +
			"domain) and the password below. Give the account a READ-ONLY role scoped to the VM inventory."
	case strings.Contains(s, "x509") || strings.Contains(s, "certificate") || strings.Contains(s, "tls"):
		return "the endpoint's TLS certificate could not be verified. Many vCenters serve a self-signed " +
			"certificate: either add its CA to the worker's trust store (preferred), or turn on Skip TLS " +
			"verification — a trust-boundary change, because TG will then accept whatever answers on that address."
	case strings.Contains(s, "no such host") || strings.Contains(s, "connection refused") ||
		strings.Contains(s, "timeout") || strings.Contains(s, "deadline") ||
		strings.Contains(s, "connection reset") || strings.Contains(s, "eof"):
		return "the vCenter could not be reached — check the host resolves, that it is up, and that the worker " +
			"may reach it on 443 (the base URL is scheme+host, e.g. https://vcenter.example.com; govmomi adds /sdk)."
	default:
		return err.Error()
	}
}

// plural renders a count with its noun so the Summary reads as a sentence rather than a log line: an operator
// who reads "1 VMs" wonders whether the probe counted correctly.
func plural(n int, noun string) string {
	if n == 1 {
		return "1 " + noun
	}
	return strconv.Itoa(n) + " " + noun + "s"
}
