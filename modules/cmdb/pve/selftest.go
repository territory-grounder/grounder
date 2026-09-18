package pve

import (
	"context"
	"fmt"
	"net/url"
	"sort"
	"strconv"
	"strings"

	"github.com/territory-grounder/grounder/core/estate"
	"github.com/territory-grounder/grounder/core/selftest"
)

// compile-time proof this reader can answer the console's TEST button. The capability is OPTIONAL and
// detected by assertion (core/selftest.Of), so without this line the dialog degrades to "no test is
// implemented" — honest, but it would leave the declared verb unperformable.
var _ selftest.Tester = (*EstateSource)(nil)

// selfTestNodeSample bounds how many hypervisor nodes the Summary names.
//
// /cluster/resources answers for the WHOLE cluster and there is no server-side page size to ask for, so the
// bounding has to happen on the reporting side: a 30-node cluster must not produce a Summary no one can
// read, and a Result is rendered in a dialog and pasted into tickets. Four is enough to recognise a cluster
// (or to see immediately that it is the wrong one); the rest are counted, not listed.
const selfTestNodeSample = 4

// SelfTest lists the cluster's guests and the node each one runs on — the descriptor's verb, performed over
// the reader's OWN read path against the real cluster: s.get for the request and s.edgesFrom for the parse,
// the same two halves Edges is made of.
//
// WHY THIS READ AND NOT A LIGHTER CALL. /api2/json/version would prove the token is valid in one small
// response, and it would prove nothing about the thing this module exists to do. The read that matters is
// GET /api2/json/cluster/resources?type=vm (the package's clusterResourcesPath, shared with Edges so the two
// cannot drift): it is the only endpoint that answers "what runs where", it is the one this source issues on
// every estate refresh, and PVE authorises it PER OBJECT — a token that authenticates fine can still see an
// empty cluster. Going through get() means the probe exercises the token resolution (INV-13, resolved inside
// get() on this very call), the Authorization scheme, the injected transport with its TLS policy, and the
// base URL — the entire path, not a rehearsal of it.
//
// WHY IT CALLS get() RATHER THAN Edges(). Edges hands back edges and drops the body, and the body is where
// the difference between "an empty cluster" and "not Proxmox at all" lives: an endpoint answering 2xx with
// JSON that has no `data` envelope yields zero edges, exactly as a cluster whose guests this token may not
// see does. Naming that as an authorised-but-empty cluster would send an operator to grant PVEAuditor on a
// machine that is not a hypervisor. The PARSE is still the reader's own (edgesFrom), so what the probe
// counts is what the refresh loop would draft.
//
// WHAT IT COSTS AND HOW THAT IS BOUNDED. edgesFrom materialises one edge per guest, exactly as the refresh
// loop does; the probe reduces that to counters immediately and drops the slice, so nothing cluster-sized is
// held for the report or shown to the operator. ctx is the caller's (moduletest allows 30 seconds, one
// attempt) and is threaded through the request, so a hung cluster ends the probe rather than the activity.
//
// WHAT A GREEN RESULT PROVES: the endpoint answered, its certificate satisfied the configured TLS policy,
// the token resolved and was accepted, and the account behind it can see the guests named in the Summary.
// WHAT IT DOES NOT PROVE: that this is the RIGHT cluster — which is why the Summary names the host and the
// nodes rather than saying "ok" — and nothing whatever about the separate ACTUATION credential, which is a
// different token on a different plane and is never touched here.
//
// operator is ignored: nothing leaves a trace in anyone's console, so there is no event needing an author.
func (s *EstateSource) SelfTest(ctx context.Context, _ string) (selftest.Result, error) {
	body, err := s.get(ctx, clusterResourcesPath)
	if err != nil {
		return selftest.Result{
			Summary: "could not list the cluster's guests from " + s.instanceLabel(),
			Detail:  classifySelfTestFailure(err),
		}, err
	}

	// A 2xx from something that is not the PVE API is NOT an empty cluster, and must never be reported as
	// one. Both shapes it takes are caught here: a body that is not JSON at all (a login page, the web UI),
	// and a body that is JSON without the `data` envelope every PVE answer carries (a gateway, an SSO
	// redirect, another product). Both mean the base URL is wrong, which is a fault the operator fixes in
	// this very dialog — quite unlike the permission diagnosis an empty cluster earns below.
	edges, err := s.edgesFrom(body)
	if err != nil {
		return selftest.Result{
			Summary: "the endpoint at " + s.instanceLabel() + " answered, but not as the PVE API",
			Detail: "the request succeeded (2xx) yet the body is not a PVE cluster-resources page. The base " +
				"URL most likely points at a proxy, a login page or another product rather than at the " +
				"Proxmox API — check it is scheme+host+port of the API itself (typically " +
				"https://<node>:8006) with no path.",
		}, err
	}

	var lxc, qemu int
	perNode := map[string]int{}
	for _, e := range edges {
		switch e.From.Type {
		case estate.TypeLXC:
			lxc++
		case estate.TypeVM:
			qemu++
		}
		perNode[e.To.Name]++
	}
	guests := len(edges)
	edges = nil // the tallies are all the report needs; do not keep a cluster-sized slice alive for a string

	if guests == 0 {
		// A PASS, loudly qualified. The credential and the endpoint are proven — that is real information —
		// but PVE authorises /cluster/resources by FILTERING it, so a token with no role on /vms gets 200
		// and an empty data array rather than 403. A token created with privilege separation left on (the
		// default) behaves exactly this way even though its user is an administrator. This module would
		// then contribute no placement at all, and the highest-confidence edges TG has would silently be
		// absent from every blast radius; reporting it as an unqualified success is how that goes unnoticed.
		return selftest.Result{
			Summary: "reached the PVE cluster at " + s.instanceLabel() + ", but it reports NO guests visible to this token",
			Detail: "the token authenticated and the cluster answered — it simply returned nothing. Either " +
				"the cluster genuinely runs no guests, or this token cannot see them: PVE filters " +
				"/cluster/resources by permission and returns an empty list rather than a refusal. Check the " +
				"token has the PVEAuditor role on / (propagating), and that privilege separation is either " +
				"off or the same role is granted to the TOKEN as well as to its user. Until then this module " +
				"contributes no guest→node edges at all.",
		}, nil
	}

	nodes := make([]string, 0, len(perNode))
	for n := range perNode {
		nodes = append(nodes, n)
	}
	// Sorted because Go randomises map iteration and a Summary that reorders itself between presses looks
	// like the cluster changed. The same reason topology order is sorted at boot.
	sort.Strings(nodes)

	var placement []string
	for i, n := range nodes {
		if i == selfTestNodeSample {
			placement = append(placement, fmt.Sprintf("+%d more", len(nodes)-selfTestNodeSample))
			break
		}
		placement = append(placement, fmt.Sprintf("%s (%d)", n, perNode[n]))
	}

	summary := fmt.Sprintf("read the PVE cluster at %s: %s (%d lxc, %d qemu) placed across %s — %s",
		s.instanceLabel(), plural(guests, "guest"), lxc, qemu, plural(len(nodes), "node"),
		strings.Join(placement, ", "))
	return selftest.Result{Summary: summary}, nil
}

// instanceLabel renders the configured endpoint for display, and it renders the HOST AND PORT ONLY.
//
// Naming the cluster is the whole point of the Summary: "reached PVE" cannot tell production from the lab
// cluster, "reached PVE at dc1pve01:8006" can. Printing the raw base URL would be simpler and wrong — a
// URL may legally carry userinfo (https://user:token@pve01:8006), and Result is rendered in a dialog and
// pasted into tickets, so this is precisely the string that must never carry credential material. url.Host
// drops userinfo by construction, and a URL too malformed to parse degrades to a phrase rather than to its
// own raw text.
func (s *EstateSource) instanceLabel() string {
	if u, err := url.Parse(s.baseURL); err == nil && u.Host != "" {
		return u.Host
	}
	return "the configured PVE URL"
}

// classifySelfTestFailure turns a failed read into something an operator can act on. "error" tells them
// nothing; "the token authenticated but has no role on /vms" tells them exactly what to grant.
//
// It classifies on the SHAPE of the failure — HTTP status first, then transport class — never on PVE's
// prose, which differs by version and locale. Anything it cannot place falls through to the raw error
// rather than to an invented diagnosis: sending an operator to re-issue a token that was never the problem
// costs more than saying "I do not know".
func classifySelfTestFailure(err error) string {
	switch code := statusFromGetError(err); {
	case code == 401:
		return "the API token was rejected — check the reference resolves to the FULL " +
			"`user@realm!tokenid=secret` value (all three parts, no spaces), and that the token has not been " +
			"deleted or expired in Datacenter → Permissions → API Tokens."
	case code == 403:
		return "the token authenticated but is not permitted to read cluster resources. Grant it the " +
			"PVEAuditor role on / (propagating). Grant nothing more: this credential sits on the read-triage " +
			"plane and must never be able to start, stop or reboot a guest."
	case code == 404:
		return "there is no PVE API at that URL: /api2/json/cluster/resources does not exist there. The base " +
			"URL must be scheme+host+port of the Proxmox API (typically https://<node>:8006) with no path."
	case code == 501:
		return "that endpoint is not implemented at this URL — a single PVE node answers /cluster/resources, " +
			"so this is most likely not a Proxmox endpoint at all."
	case code >= 500:
		return fmt.Sprintf("PVE answered with a server error (status %d). The URL and the token are reaching "+
			"it, so this is a cluster-side fault rather than a TG configuration one — check the node's "+
			"pveproxy and cluster quorum.", code)
	case code != 0:
		return fmt.Sprintf("PVE refused the read with status %d.", code)
	}

	s := strings.ToLower(err.Error())
	switch {
	case strings.Contains(s, "resolve token"):
		return "the API token could not be READ from the secret backend — the token reference is wrong, or " +
			"the backend is unreachable. This is a TG-side problem, not a Proxmox one: nothing was sent."
	// NOTE: a 2xx body that is not a cluster-resources page never reaches here — SelfTest reports that
	// itself, from the parse, because it is the one failure whose diagnosis needs the body rather than the
	// transport.
	case strings.Contains(s, "x509") || strings.Contains(s, "certificate") || strings.Contains(s, "tls"):
		return "the endpoint's TLS certificate could not be verified. PVE serves a SELF-SIGNED certificate on " +
			":8006, so this is expected on a stock cluster: either add that certificate to the worker's trust " +
			"store (preferred), or turn on Skip TLS verification — which is a trust-boundary change, because " +
			"it means TG will accept whatever answers on that address as the hypervisor."
	case strings.Contains(s, "timeout") || strings.Contains(s, "deadline") ||
		strings.Contains(s, "no such host") || strings.Contains(s, "connection refused") ||
		strings.Contains(s, "connection reset") || strings.Contains(s, "eof"):
		return "the cluster could not be reached — check the host resolves, that it is up, and that the " +
			"worker may reach it on the API port (8006 by default; a missing port is the usual cause)."
	default:
		return err.Error()
	}
}

// statusFromGetError recovers the HTTP status from the error get() formats:
//
//	pve: GET <path>: status 403: <server body>
//
// It reads OUR OWN frame — the first ": status " in the string, written before the server's body is
// appended — rather than searching the whole error for a three-digit number. That distinction keeps a PVE
// error body that happens to mention 403 from being reported as a permission fault when the real status was
// 500. A transport failure carries no status and yields 0, which routes classification to the transport arm.
func statusFromGetError(err error) int {
	const marker = ": status "
	s := err.Error()
	i := strings.Index(s, marker)
	if i < 0 {
		return 0
	}
	digits := s[i+len(marker):]
	end := 0
	for end < len(digits) && digits[end] >= '0' && digits[end] <= '9' {
		end++
	}
	code, convErr := strconv.Atoi(digits[:end])
	if convErr != nil {
		return 0
	}
	return code
}

// plural renders a count with its noun so the Summary reads as a sentence rather than a log line: an
// operator who reads "1 guests" wonders whether the probe counted correctly.
func plural(n int, noun string) string {
	if n == 1 {
		return "1 " + noun
	}
	return strconv.Itoa(n) + " " + noun + "s"
}
