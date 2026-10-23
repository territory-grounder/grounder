package netbox

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strconv"
	"strings"

	"github.com/territory-grounder/grounder/core/selftest"
)

// compile-time proof the module can answer the console's TEST button. The capability is OPTIONAL and
// detected by assertion (core/selftest.Of), so without this line the module would silently degrade to "no
// test is implemented" — honest, but a dialog that promises a read and performs none.
var _ selftest.Tester = (*Module)(nil)

// selfTestPath is the read the probe performs: ONE bounded page of the virtual-machine list.
//
// WHY THIS ENDPOINT. It is the FIRST PAGE OF THE MODULE'S OWN ESTATE READ. EstateSource.Edges (topology.go)
// pages `/api/virtualization/virtual-machines/?limit=200` following `next` on every estate refresh, so this
// probe walks the identical path with the identical token and the identical permission
// (`virtualization.view_virtualmachine`) — it just stops after one small page. Resolve(), the module's other
// read, cannot do this job: it needs an entity id nobody has typed into a settings dialog, and a 404 for an
// id that does not exist here is indistinguishable from a 404 for a base URL that is not NetBox at all.
//
// WHY limit=5 AND NOT brief. `limit` bounds the page at the SERVER, so the probe cannot pull an estate of
// forty thousand VMs through a 30-second activity no matter how large the instance is, and the envelope
// still reports the instance's TOTAL count — which is what lets a green Test reveal a module pointed at a
// staging clone. `brief` is deliberately NOT sent, for two reasons: its accepted spelling changed between
// NetBox majors (`brief=1` vs `brief=true`) and a probe that fails on a version difference would report a
// credential fault that does not exist, and a brief page omits `device`/`cluster` — the two fields whose
// absence is the difference between "the module reads NetBox" and "the module contributes edges".
const selfTestPath = "/api/virtualization/virtual-machines/?limit=5"

// SelfTest reads one bounded page of the virtual-machine list over the REAL path: m.do resolves the token
// from its secret reference on this call (INV-13), sends it as NetBox's `Token <token>` scheme through the
// module's own injected transport, and against the module's own base URL. Nothing here inspects configured
// values — a check that the URL and the token reference are non-empty passes against a revoked token, a
// permission never granted, and a host that has been down for a week, which are precisely the three faults
// an operator presses TEST to rule out.
//
// WHAT A GREEN RESULT PROVES: the endpoint is reachable, TLS verified, the token resolved from the secret
// backend, NetBox accepted it, and the account behind it may list virtual machines. WHAT IT DOES NOT PROVE:
// that this is the RIGHT NetBox (the Summary names the host and the object count so a human can see that
// for themselves), nor that the token can read devices, IP addresses or the changelog — those are separate
// object permissions in NetBox, and a probe must not certify a permission it never exercised.
//
// operator is ignored: this probe has no outward side effect, so there is no event in anyone's console that
// would need a named author.
func (m *Module) SelfTest(ctx context.Context, _ string) (selftest.Result, error) {
	body, err := m.do(ctx, selfTestPath)
	if err != nil {
		return selftest.Result{
			Summary: "could not read the virtual-machine list from " + m.instanceLabel(),
			Detail:  classifySelfTestFailure(err),
		}, err
	}

	// The page is decoded through topology.go's OWN vmPage shape (embedded, so its fields promote), plus the
	// envelope count. Reusing that type is deliberate: the probe then parses exactly what the estate reader
	// parses, and a change to the fields the reader depends on cannot leave the probe passing on a payload
	// the reader can no longer use.
	//
	// Count is a POINTER on purpose. Every NetBox list response carries a `count`; a body that parses as
	// JSON but has no count is not a NetBox list page, and that is a DIFFERENT fault from an empty one — it
	// is the shape a reverse proxy's JSON error, an SSO redirect rendered as JSON, or an entirely different
	// product returns. Decoding into a plain int would silently report that as "0 virtual machines".
	var page struct {
		Count *int `json:"count"`
		vmPage
	}
	if err := json.Unmarshal(body, &page); err != nil || page.Count == nil {
		return selftest.Result{
				Summary: "the endpoint at " + m.instanceLabel() + " answered, but not as NetBox",
				Detail: "the request succeeded (2xx) yet the body is not a NetBox list page. The base URL " +
					"most likely points at a proxy, a login page, or another product rather than at the NetBox " +
					"API root — check it is the scheme+host NetBox is served on, with no /api suffix and no " +
					"path prefix.",
			}, fmt.Errorf("netbox: selftest: %s did not return a NetBox list page (%d bytes)",
				selfTestPath, len(body))
	}

	// Two tallies, and the second one is the one that matters. `names` is the evidence a human recognises;
	// `placed` counts how many of the sampled VMs carry a placement the estate reader can turn into an
	// edge, by topology.go's own rule (a specific `device` wins, the virtualization `cluster` is the
	// fallback, a VM with neither is skipped rather than guessed). A NetBox that answers 200 with hundreds
	// of VMs and no placement on any of them is a module that is bound, authorised, reachable — and
	// contributing an empty topology, which is invisible in every downstream signal except a vacuous blast
	// radius nobody notices until an incident.
	//
	// An UNNAMED VM is skipped for BOTH tallies, because topology.go skips it outright (`name == "" =>
	// continue`) before it ever looks at the placement. Counting its cluster as placed would report an edge
	// yield the estate reader will not deliver — and would print "3 of the 1 read", which is a number an
	// operator cannot act on and, worse, is optimistic in exactly the direction this counter exists to
	// catch.
	names := make([]string, 0, len(page.Results))
	placed := 0
	for _, vm := range page.Results {
		name := strings.TrimSpace(vm.Name)
		if name == "" {
			continue
		}
		names = append(names, name)
		switch {
		case vm.Device != nil && strings.TrimSpace(vm.Device.Name) != "":
			placed++
		case vm.Cluster != nil && strings.TrimSpace(vm.Cluster.Name) != "":
			placed++
		}
	}

	summary := fmt.Sprintf("read NetBox at %s: %s visible", m.instanceLabel(), plural(*page.Count, "virtual machine"))
	if len(names) > 0 {
		// The sample is what turns a pass into evidence: an operator who knows their estate recognises
		// these names, and recognises immediately when they belong to the staging instance. It is bounded
		// by the server-side limit above, so it can never grow with the estate.
		summary += " (" + strings.Join(names, ", ")
		if *page.Count > len(names) {
			summary += ", …"
		}
		summary += fmt.Sprintf("); %d of the %d read carry a placement TG can turn into an edge", placed, len(names))
	}

	// An empty page with a 2xx is a PASS with a warning, not a failure: the credential and the endpoint are
	// proven, and an estate can legitimately have no VMs. But it is also exactly what a token whose object
	// permissions filter every VM away looks like — NetBox enforces object permissions by FILTERING list
	// results, returning 200 with an empty page rather than 403 — so a module that will contribute nothing
	// must not read as an unqualified success.
	detail := ""
	switch {
	case *page.Count == 0:
		detail = "the token was accepted but NetBox reports no virtual machines at all. Either this " +
			"instance genuinely has none, or the token's object permissions filter every VM away — NetBox " +
			"enforces those by filtering list results, so a permission problem looks exactly like an empty " +
			"estate here. If VMs are expected, check the token's user has the virtualization > virtual " +
			"machine VIEW permission with no restricting filter."
	case placed == 0 && len(names) > 0:
		detail = "the read works, but none of the virtual machines on this page has a device or a cluster " +
			"set, so this module would contribute NO placement edges from them — a blast radius over NetBox " +
			"placement would come back empty rather than wrong. Set the host device (or the virtualization " +
			"cluster) on your VMs in NetBox."
	}
	return selftest.Result{Summary: summary, Detail: detail}, nil
}

// instanceLabel renders the configured endpoint for display, and it renders the HOST ONLY.
//
// Naming the instance is the point of the Summary — "reached NetBox" cannot distinguish production from
// the staging clone, "reached NetBox at netbox.example" can. Printing the raw base URL would be simpler and
// wrong: a URL may legally carry userinfo (https://user:token@netbox.example), and Result is rendered in a
// dialog and pasted into tickets, so the one string that must never carry credential material is exactly
// this one. url.Host drops userinfo by construction; a URL too malformed to parse degrades to a phrase
// rather than to its own raw text.
func (m *Module) instanceLabel() string {
	if u, err := url.Parse(m.baseURL); err == nil && u.Host != "" {
		return u.Host
	}
	return "the configured NetBox URL"
}

// classifySelfTestFailure turns a failed read into something an operator can act on. "error" tells them
// nothing; "the token authenticated but cannot list virtual machines" tells them exactly which permission
// to grant.
//
// It classifies on the SHAPE of the failure — the HTTP status code first, then the transport class — and
// never on NetBox's prose, which differs between versions and deployments. Anything it cannot place falls
// through to the raw error rather than to an invented diagnosis: a wrong diagnosis sends an operator to
// re-issue a token that was never the problem.
func classifySelfTestFailure(err error) string {
	switch code := statusFromDoError(err); {
	case code == 401:
		return "the API token was rejected — it is wrong, expired, or has been revoked. Save a new " +
			"read-only token and test again."
	case code == 403:
		return "the token authenticated but the account behind it may not list virtual machines. Grant its " +
			"user or group the virtualization > virtual machine VIEW object permission (read-only — this " +
			"credential sits on the triage plane and must never be able to write the CMDB)."
	case code == 404:
		return "there is no NetBox API at that URL: /api/virtualization/virtual-machines/ does not exist " +
			"there. The base URL must be the scheme+host NetBox is served on, with no /api suffix and no " +
			"path prefix."
	case code == 429:
		return "NetBox is rate-limiting this token. The read was refused rather than failed — retry, and " +
			"if it persists check what else is using this credential."
	case code >= 500:
		return fmt.Sprintf("NetBox answered with a server error (status %d). The URL and the token are "+
			"reaching it, so this is a NetBox-side fault rather than a TG configuration one.", code)
	case code != 0:
		return fmt.Sprintf("NetBox refused the read with status %d.", code)
	}

	s := strings.ToLower(err.Error())
	switch {
	case strings.Contains(s, "resolve token"):
		return "the API token could not be READ from the secret backend — the token reference is wrong, or " +
			"the backend is unreachable. This is a TG-side problem, not a NetBox one: nothing was sent."
	case strings.Contains(s, "x509") || strings.Contains(s, "certificate") || strings.Contains(s, "tls"):
		return "the TLS certificate could not be verified — TG refuses to read the CMDB from a host it " +
			"cannot authenticate. Install the issuing CA in the worker's trust store, or fix the " +
			"certificate; do not work around it by changing the URL to http."
	case strings.Contains(s, "timeout") || strings.Contains(s, "deadline") ||
		strings.Contains(s, "no such host") || strings.Contains(s, "connection refused") ||
		strings.Contains(s, "connection reset") || strings.Contains(s, "eof"):
		return "NetBox could not be reached — check the base URL resolves, that the host is up, and that " +
			"the worker is allowed to reach it on that port."
	default:
		return err.Error()
	}
}

// statusFromDoError recovers the HTTP status from the error do() formats:
//
//	netbox: GET <path>: status 403: <server body>
//
// It reads OUR OWN frame — the first ": status " in the string, written before the server's body is
// appended — rather than searching the whole error for a three-digit number. That distinction is what keeps
// a NetBox error body that happens to mention 403 from being reported as a permission fault when the real
// status was 500. A transport failure has no status and yields 0, which routes classification to the
// transport arm instead.
func statusFromDoError(err error) int {
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

// plural renders a count with its noun so the Summary reads as a sentence rather than as a log line: an
// operator reading "1 virtual machines visible" wonders whether the probe counted correctly.
func plural(n int, noun string) string {
	if n == 1 {
		return "1 " + noun
	}
	return strconv.Itoa(n) + " " + noun + "s"
}
