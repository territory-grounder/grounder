package pveliveness

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"

	"github.com/territory-grounder/grounder/core/config"
	"github.com/territory-grounder/grounder/core/selftest"
)

// THE PROXMOX-LIVENESS TEST BUTTON (core/selftest.Tester).
//
// WHAT THE DIALOG PROMISES. descriptor.go declares Test.Verb "read the Proxmox guest list once (GET
// /api2/json/cluster/resources) and report how many allowlisted guests it matched", and this file is
// exactly that sentence: it resolves the SAME TG_PROXMOX_TOKEN_REF the poller resolves on every tick,
// issues the SAME single GET /api2/json/cluster/resources?type=vm through the SAME injected transport (the
// TLS posture TG_PROXMOX_INSECURE selected), and reports both what Proxmox showed and how much of the
// managed-guest allowlist was actually in it.
//
// WHY THE MATCH COUNT, NOT JUST "THE TOKEN WORKS". Proxmox authorises per object: a token that
// authenticates perfectly and can list /cluster/resources may still be scoped — by a pool ACL, or by the
// API token's own privilege separation — so that the very guests TG manages are absent from the answer.
// That failure is silent and total: FetchActive iterates the rows it was shown, so a guest it never sees
// can never transition, and this detector — TG's fastest, the one that beats the 6–11 minute LibreNMS push
// by an order of magnitude — simply never fires for it. A probe that stopped at "authenticated" would be
// green for a detector that is structurally blind, which is the exact class of lie the TEST button exists
// to end. The same read also catches the ordinary version of this: a guest NAME misspelled in
// TG_PROXMOX_ALLOWED_GUESTS, or a vmid typed where a name belongs.
//
// WHY ZERO MATCHES AND AN EMPTY ALLOWLIST ARE BOTH RED. Either way this detector can never raise an
// incident, and a green button next to a detector that detects nothing is worse than no button: it is a
// certificate that somebody checked.
//
// READ-ONLY. One HTTP GET. This module never actuates — mutation of a guest belongs to the actuation lane
// behind the mode chokepoint, and a probe that started or stopped anything from a settings dialog would be
// an unreviewed actuation with no proposal, no approval and no ledger entry.
//
// WHAT A GREEN RESULT DOES NOT PROVE. That the poll interval is set (with it empty the detector does not
// exist), that Temporal will accept the triage it mints, or that the guests it matched are healthy — a
// matched guest may be listed as stopped right now, which the Summary says rather than hides.

// probeBodyLimit bounds what the probe reads from the response. /cluster/resources returns the whole
// cluster inventory and grows with the estate; the poller caps it at 4 MiB and so does this.
const probeBodyLimit = 4 << 20

// probeNameSample bounds how many guest names are named in the operator's answer. Naming every guest of a
// large cluster would push the one thing that matters — the counts — off the end of the line.
const probeNameSample = 5

// probeResourcesPath is the one read this probe performs: identical to fetchResources' URL, so the probe
// cannot drift away from the endpoint the detector actually depends on.
const probeResourcesPath = "/api2/json/cluster/resources?type=vm"

// SelfTest implements core/selftest.Tester.
//
// The operator argument is ignored: the probe has no outward side effect — nothing is created in Proxmox,
// so there is no event in anyone's cluster log that would need a named author.
func (s *Source) SelfTest(ctx context.Context, _ string) (selftest.Result, error) {
	if strings.TrimSpace(s.baseURL) == "" {
		return selftest.Result{
				Summary: "no Proxmox base URL is configured, so nothing was read",
				Detail: "TG_PROXMOX_BASE_URL is empty. Set it to the API root of a node or the cluster " +
					"(e.g. https://pve.example:8006) and restart the worker — it is captured at boot. The " +
					"same value drives the actuation lane.",
			},
			errors.New("pveliveness: no Proxmox base URL configured")
	}

	rows, err := s.probeResources(ctx)
	if err != nil {
		return selftest.Result{
			Summary: "could not read the Proxmox guest list at " + hostOf(s.baseURL),
			Detail:  classifyProbeFailure(s.tokenRef, err),
		}, err
	}

	// Count what was actually shown, and match it against the allowlist by the same rule FetchActive uses:
	// type lxc/qemu, matched on the trimmed guest NAME. Using a different rule here would let the probe pass
	// on guests the detector would ignore.
	var guests int
	seen := make(map[string]string, len(rows)) // allowlisted guest name → status as observed
	for _, r := range rows {
		if r.Type != "lxc" && r.Type != "qemu" {
			continue
		}
		guests++
		name := strings.TrimSpace(r.Name)
		if name != "" && s.allowed[name] {
			seen[name] = strings.TrimSpace(r.Status)
		}
	}

	base := fmt.Sprintf("read the Proxmox guest list at %s — %d guest(s) visible", hostOf(s.baseURL), guests)

	if len(s.allowed) == 0 {
		// The read proves the endpoint and the credential; the module is still inert. Say both, and fail:
		// with no managed guests this detector watches nothing, by construction (FetchActive returns
		// immediately without even calling the API).
		return selftest.Result{
				Summary: base + ", but no managed guests are configured",
				Detail: "The Proxmox API answered and the token was accepted, so the connection is sound — " +
					"but TG_PROXMOX_ALLOWED_GUESTS is empty and this detector watches NOTHING. Every guest " +
					"named there is a guest TG will investigate when it goes down (and that the actuation " +
					"lane may start), which is why it is not defaulted.",
			},
			errors.New("pveliveness: no managed guests configured")
	}

	missing := missingGuests(s.allowed, seen)
	matched := len(seen)
	summary := fmt.Sprintf("%s, %d of %d managed guest(s) matched", base, matched, len(s.allowed))
	if matched > 0 {
		summary += " (" + describeStatuses(seen) + ")"
	}

	if matched == 0 {
		// Authentication succeeded and the estate was listed, yet not one managed guest was in it. The
		// detector is blind: no transition it is watching for can ever be observed.
		return selftest.Result{
				Summary: summary,
				Detail: "The token was accepted and Proxmox returned " + strconv.Itoa(guests) + " guest(s), " +
					"but NONE of the managed guests (" + sample(sortedKeys(s.allowed)) + ") was among them, so " +
					"this detector can never fire. Either the names in TG_PROXMOX_ALLOWED_GUESTS do not match " +
					"the guests' Proxmox names (they are NAMES, not vmids), or this API token cannot see " +
					"those guests — a Proxmox token is scoped by pool/ACL, and a token with privilege " +
					"separation on sees only what its own ACL grants.",
			},
			errors.New("pveliveness: no managed guest was visible in the Proxmox guest list")
	}

	res := selftest.Result{Summary: summary}
	if len(missing) > 0 {
		// A partial match is a pass — the read did what the dialog promised — with the remainder named,
		// because each missing guest is one TG will never detect a fault for.
		// THE ORDER OF THESE CAUSES IS EVIDENCE-BASED, not alphabetical. The first real occurrence
		// (2026-08-02) named three guests that all existed, were all running, and were all on one node:
		// the API TOKEN could not see them. This source deliberately reuses the actuation-plane token
		// rather than the estate-read one, and that token is scoped to the guests TG may act on — so a
		// name in the allowlist that is outside the token's scope is the EXPECTED disagreement, not a
		// typo. Leading with spelling sent the first reader looking for a mistake that was not there.
		res.Detail = "NOT visible to THIS token, so a fault on them can never be detected: " + sample(missing) +
			". This source reads with the actuation-plane token (TG_PROXMOX_TOKEN_REF), which is scoped to " +
			"the guests TG may act on — narrower than the estate-read token. Most likely the allowlist and " +
			"that token's ACL disagree: either grant it access to those guests or drop them from " +
			"TG_PROXMOX_ALLOWED_GUESTS. Less likely: the guest was renamed or removed, or the name is a " +
			"vmid rather than a Proxmox guest NAME. Note this read (/cluster/resources) is cluster " +
			"inventory and does NOT involve the guest agent — a guest missing here is not a qemu-guest-agent " +
			"problem."
	}
	return res, nil
}

// compile-time proof the liveness detector satisfies the optional self-test capability.
var _ selftest.Tester = (*Source)(nil)

// probeResources issues the one authenticated read.
//
// It deliberately does not call fetchResources, even though it sends the identical request through the
// identical transport with the identical credential: fetchResources flattens the failure into a formatted
// string ("status %d"), and choosing an operator-facing diagnosis by re-parsing our own message text turns
// a harmless wording change into a silently wrong diagnosis. Keeping the STATUS CODE as a value lets the
// classifier switch on the SHAPE of the failure instead.
func (s *Source) probeResources(ctx context.Context) ([]resourcesRow, error) {
	token, err := s.tokenRef.Resolve()
	if err != nil {
		return nil, &probeFault{stage: faultCredential, err: err}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.baseURL+probeResourcesPath, nil)
	if err != nil {
		return nil, &probeFault{stage: faultRequest, err: err}
	}
	req.Header.Set("Authorization", "PVEAPIToken="+token)
	req.Header.Set("Accept", "application/json")

	doer := s.http
	if doer == nil {
		doer = http.DefaultClient // only reachable if a caller built the Source by hand; New defaults it
	}
	resp, err := doer.Do(req)
	if err != nil {
		return nil, &probeFault{stage: faultTransport, err: err}
	}
	defer resp.Body.Close()
	// One byte MORE than the cap, so a body that hit the cap is DISTINGUISHABLE from one that merely ended
	// there. Without that byte a truncated cluster inventory is indistinguishable from malformed JSON, and the
	// operator would be told a healthy Proxmox is "not a Proxmox API" — a diagnosis that sends them to fix a
	// base URL that is correct.
	body, _ := io.ReadAll(io.LimitReader(resp.Body, probeBodyLimit+1))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, &probeFault{stage: faultStatus, status: resp.StatusCode}
	}
	if len(body) > probeBodyLimit {
		return nil, &probeFault{stage: faultTooLarge}
	}
	// Proxmox always wraps its answer in {"data": …}. A body that decodes but has no data key is not this
	// API — most often a captive portal or a reverse proxy in front of the wrong host.
	var wrap struct {
		Data []resourcesRow `json:"data"`
	}
	if err := json.Unmarshal(body, &wrap); err != nil {
		return nil, &probeFault{stage: faultDecode, err: err}
	}
	return wrap.Data, nil
}

// probeStage names WHERE the read broke, so the diagnosis is chosen from a value rather than from prose.
type probeStage int

const (
	faultCredential probeStage = iota // the token reference did not resolve — a TG-side fault
	faultRequest                      // the base URL could not form a request
	faultTransport                    // DNS / connect / TLS / deadline — nothing was answered
	faultStatus                       // Proxmox answered with a non-2xx
	faultDecode                       // 2xx, but the body is not a Proxmox API envelope
	faultTooLarge                     // 2xx, but the inventory exceeded the 4 MiB the poller itself reads
)

// probeFault carries the SHAPE of a failure — the stage, and the HTTP status when there was one.
//
// Its Error() is what lands in a ticket when nothing catches it, so it names the endpoint and the status
// and nothing else: never the API token, never the response body, and never the raw base URL (which could
// carry userinfo credentials in a pathological configuration).
type probeFault struct {
	stage  probeStage
	status int
	err    error
}

func (f *probeFault) Error() string {
	switch f.stage {
	case faultStatus:
		return fmt.Sprintf("pveliveness: GET %s: status %d", probeResourcesPath, f.status)
	case faultCredential:
		return fmt.Sprintf("pveliveness: resolve token: %v", f.err)
	case faultDecode:
		return fmt.Sprintf("pveliveness: GET %s: response is not a Proxmox API envelope", probeResourcesPath)
	case faultTooLarge:
		return fmt.Sprintf("pveliveness: GET %s: answer exceeded the %d-byte read this source performs",
			probeResourcesPath, probeBodyLimit)
	default:
		// urlFaultReason, not %v: this branch carries the request-build and transport faults, and a *url.Error
		// prints the URL it was given. net/http redacts the password on the transport path but net/url does not
		// on the parse path, and THIS string is the one that reaches a ticket when nothing catches it.
		return fmt.Sprintf("pveliveness: GET %s: %s", probeResourcesPath, urlFaultReason(f.err))
	}
}

func (f *probeFault) Unwrap() error { return f.err }

// classifyProbeFailure turns a failed read into a sentence an operator can act on, classifying on the SHAPE
// of the fault (stage, status code, transport class) and falling through to the raw error rather than
// inventing a diagnosis — a confident wrong explanation sends someone to fix the wrong system.
func classifyProbeFailure(ref config.SecretRef, err error) string {
	refText := refLabel(ref)
	var f *probeFault
	if !errors.As(err, &f) {
		return trimErr(err)
	}
	switch f.stage {
	case faultCredential:
		return "the Proxmox API token could not be READ from its reference " + refText + " — the reference " +
			"is wrong, or the secret backend is unreachable. This is a TG-side fault, not a Proxmox one, " +
			"and it also stops the actuation lane, which shares this credential (" + trimErr(f.err) + ")."
	case faultRequest:
		return "the configured base URL is not usable as a URL — it must look like https://pve.example:8006 " +
			"(" + urlFaultReason(f.err) + ")."
	case faultTransport:
		return classifyTransport(f.err)
	case faultDecode:
		return "that address answered, but not with a Proxmox API envelope — it is usually a reverse proxy " +
			"or a different application at that URL. The base URL must be the API root, e.g. " +
			"https://pve.example:8006, with no /api2 and no trailing path."
	case faultTooLarge:
		// NOT a misconfiguration, and not the probe's own limitation either: the poller reads /cluster/resources
		// under the SAME 4 MiB cap, so an inventory this large breaks detection too. Saying "that is not a
		// Proxmox API" — which a truncated body otherwise looks exactly like — would hide a real fault behind a
		// wrong one.
		return "Proxmox answered with a cluster inventory larger than the 4 MiB this source reads. The token " +
			"WORKED and the address is right — but the poller reads /cluster/resources under the same bound, " +
			"so guest liveness is NOT being detected on this cluster. Narrow the read (a per-node endpoint) or " +
			"raise the cap; this is a TG limit, not a Proxmox fault."
	case faultStatus:
		switch {
		case f.status == http.StatusUnauthorized:
			return "Proxmox rejected the API token (401) — the token id or secret is wrong, or the token has " +
				"been deleted or expired. Rotate it where it is owned (the Proxmox connector, " + refText +
				"); this detector only borrows that credential."
		case f.status == http.StatusForbidden:
			return "the token authenticated but Proxmox refused the read (403) — it lacks permission on " +
				"/cluster/resources. Grant its user an audit-level role on / (or the relevant pool); note " +
				"that an API token with privilege separation enabled needs its OWN ACL entry, not just its " +
				"user's."
		case f.status == http.StatusNotFound:
			return "there is no Proxmox API at that address (404) — the base URL must be the API root " +
				"(https://pve.example:8006), with no /api2 and no trailing path."
		case f.status >= 500:
			return fmt.Sprintf("Proxmox is reachable but answered %d — the node itself is unhealthy (a "+
				"cluster quorum or pvedaemon problem); this is not a TG credential fault.", f.status)
		default:
			return fmt.Sprintf("Proxmox answered GET %s with an unexpected status %d.", probeResourcesPath, f.status)
		}
	default:
		return trimErr(err)
	}
}

// classifyTransport names the transport class of a failure that never reached an HTTP status.
func classifyTransport(err error) string {
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return "the read did not finish inside the test's time budget — the address may be blackholed, or " +
			"this node is answering too slowly to poll."
	}
	var cve *tls.CertificateVerificationError
	var unknownCA x509.UnknownAuthorityError
	var hostErr x509.HostnameError
	if errors.As(err, &cve) || errors.As(err, &unknownCA) || errors.As(err, &hostErr) {
		return "the TLS certificate was refused — a PVE node serving its own self-signed certificate on " +
			":8006 is the usual cause. Turn on 'Skip TLS verification' for Proxmox if that is what this is, " +
			"knowing TG then hands its Proxmox token to whatever answers at that address."
	}
	return "the Proxmox API could not be reached at all — DNS, routing, port or TLS. Check the base URL " +
		"(the API port is normally 8006) and that the node is up (" + trimErr(err) + ")."
}

// missingGuests returns the managed guests that were NOT in the answer, sorted for a stable message.
func missingGuests(allowed map[string]bool, seen map[string]string) []string {
	var out []string
	for name := range allowed {
		if _, ok := seen[name]; !ok {
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out
}

// describeStatuses summarises the matched guests by the status Proxmox reported, e.g. "3 running, 1
// stopped". A matched-but-stopped guest is not a probe failure — it is a fact about the estate right now,
// and one an operator pressing TEST during an incident wants to see rather than have averaged away.
func describeStatuses(seen map[string]string) string {
	counts := map[string]int{}
	for _, st := range seen {
		if st == "" {
			st = "unknown"
		}
		counts[st]++
	}
	keys := make([]string, 0, len(counts))
	for k := range counts {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, strconv.Itoa(counts[k])+" "+k)
	}
	return strings.Join(parts, ", ")
}

// sortedKeys returns a map's keys in a stable order.
func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// sample renders at most probeNameSample names, so a large allowlist cannot flood the dialog.
func sample(names []string) string {
	if len(names) <= probeNameSample {
		return strings.Join(names, ", ")
	}
	return strings.Join(names[:probeNameSample], ", ") +
		fmt.Sprintf(" and %d more", len(names)-probeNameSample)
}

// refLabel renders a secret REFERENCE for display.
//
// A reference (env:X, file:/p, bao:…#key) is safe to show — core/config says so explicitly, that being the
// point of INV-13. A value with NO scheme prefix is not a reference at all: it may BE a pasted plaintext
// secret, and echoing it into an error an operator pastes into a ticket would publish it.
func refLabel(ref config.SecretRef) string {
	text := strings.TrimSpace(string(ref))
	if text == "" {
		return "(unset)"
	}
	if scheme, _, ok := strings.Cut(text, ":"); !ok || strings.TrimSpace(scheme) == "" {
		return "(a token reference with no scheme prefix — use env:, file:, store: or a registered backend " +
			"scheme; TG will not accept an inline literal)"
	}
	return text
}

// hostOf renders the host of the configured base URL. It returns the HOST ONLY, never the whole URL: a base
// URL may carry userinfo (https://user:pass@host) and that must not reach the console or a ticket.
func hostOf(base string) string {
	u, err := url.Parse(strings.TrimSpace(base))
	if err != nil || u.Host == "" {
		return "the configured Proxmox endpoint"
	}
	return u.Host
}

// urlFaultReason renders WHY a URL could not be used, WITHOUT the URL itself.
//
// net/url puts the raw string it failed to parse into *url.Error and prints it verbatim: a base URL of
// https://user:hunter2@pve.example:8006 with a stray control character yields an error containing "hunter2".
// The transport path is safe (net/http replaces the password with *** before wrapping); the PARSE path is
// not, and this credential is the one the ACTUATION lane shares. So a *url.Error is rendered by its REASON
// only; anything else falls through unchanged.
func urlFaultReason(err error) string {
	var ue *url.Error
	if errors.As(err, &ue) && ue.Err != nil {
		return trimErr(ue.Err)
	}
	return trimErr(err)
}

// trimErr bounds an error string before it is shown; the console renders one line.
func trimErr(err error) string {
	if err == nil {
		return ""
	}
	s := strings.TrimSpace(err.Error())
	if len(s) > 200 {
		return s[:200] + "…"
	}
	return s
}
