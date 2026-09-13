package main

// pve_liveness_config.go — WHICH PROXMOX CREDENTIAL THE LIVENESS DETECTOR READS WITH (TG-350).
//
// THE DEFECT. `pve-liveness` is TG's fastest detector: it polls guest status every ~45s and mints a triage on
// a running→stopped edge, an order of magnitude ahead of the ~6–11 min LibreNMS device-down push. It is also
// PLANE-SCOPED to TRIAGE — `TG_PVE_LIVENESS_POLL_INTERVAL` is on triagePlaneEnvKeys, because minting a triage
// session is untrusted-content intake and the actuation worker must not do it.
//
// It read its endpoint and credential from `TG_PROXMOX_BASE_URL` / `TG_PROXMOX_TOKEN_REF` — the ACTUATION
// lane's guest-lifecycle WRITE token, which `actuationPlaneEnvKeys` withholds from the triage plane by
// design. The two constraints cannot both hold. When the plane split (TG-153) landed on 2026-07-31 the
// detector lost its credential and went silent; `ingest_alert` holds 200 pve-liveness rows, the last one
// delivered 2026-07-31 23:40. Nothing was misconfigured — a least-privilege change had a silent second
// effect, and the only symptom was a boot line saying the poller was idle.
//
// A GET WITH A WRITE CREDENTIAL IS THE WRONG CREDENTIAL REGARDLESS OF THE PLANE. The detector never actuates
// (INV: read-only, GET /cluster/resources). The estate READ token — `TG_PVE_URL` / `TG_PVE_TOKEN_REF`, which
// the triage worker already holds and which OpenBao classifies read-triage — is both the correct credential
// and the one that is already on the process.
//
// THE PAIR IS RESOLVED AS A UNIT, AND THAT IS THE POINT. Endpoint, credential AND TLS-verification flag come
// from the same pair or from neither. TG's two Proxmox flags disagree on this estate right now
// (`TG_PVE_INSECURE=true`, `TG_PROXMOX_INSECURE` unset) against a self-signed endpoint, so resolving the
// token from one pair and the TLS flag from the other would swap a missing-credential failure for a
// certificate failure and change nothing an operator can see: a poller that cannot complete its GET reports
// no down guests, which is byte-identical to an estate where nothing is down.

import (
	"context"
	"log"
	"strings"
	"sync"

	"github.com/territory-grounder/grounder/core/config"
)

// pveLivenessPair is one complete way to talk to Proxmox read-only: where, with what, and whether to verify
// the certificate. The env keys travel with the values because every message this feeds — the armed line, the
// idle line, the TLS-disagreement warning — must name the keys an operator would actually edit.
type pveLivenessPair struct {
	name        string // "estate-read" | "actuation-write"
	baseURL     string
	tokenRef    string
	insecure    bool
	urlKey      string
	tokenKey    string
	insecureKey string
}

// pveLivenessTLSFlagDefault is the flag the TLS-disagreement report attributes the detector to when NO pair
// resolves. It stays on the actuation flag — an unconfigured detector is not talking to Proxmox at all, and
// naming the read flag would assert a routing that is not in effect.
const pveLivenessTLSFlagDefault = "TG_PROXMOX_INSECURE"

// resolvePVELivenessPair picks the endpoint+credential+TLS flag the detector reads with, preferring the
// estate READ pair. `get` MUST be planeEnv, not getenv: on the triage plane that makes the actuation
// fallback resolve to "" at ACQUISITION, so the write token is never handed to this process — the same
// property the rest of credential_plane.go relies on, rather than a check placed after the value is in hand.
//
// The fallback exists for `both`-plane deployments (the default posture, and every install that has not
// split): they configure only the TG_PROXMOX_* pair, and this change must not take their detector away.
func resolvePVELivenessPair(get func(k, def string) string) (pveLivenessPair, bool) {
	read := pveLivenessPair{
		name:        "estate-read",
		baseURL:     strings.TrimSpace(get("TG_PVE_URL", "")),
		urlKey:      "TG_PVE_URL",
		insecure:    truthyValue(get("TG_PVE_INSECURE", "")),
		insecureKey: "TG_PVE_INSECURE",
	}
	// TG_PVE_RO_TOKEN_REF first: an operator who has bothered to declare a read-ONLY reference means it to be
	// the one a read path uses. TG_PVE_TOKEN_REF is the estate reader's own, still a read credential.
	for _, k := range []string{"TG_PVE_RO_TOKEN_REF", "TG_PVE_TOKEN_REF"} {
		if v := strings.TrimSpace(get(k, "")); v != "" {
			read.tokenRef, read.tokenKey = v, k
			break
		}
	}
	if read.baseURL != "" && read.tokenRef != "" {
		return read, true
	}

	write := pveLivenessPair{
		name:        "actuation-write",
		baseURL:     strings.TrimSpace(get("TG_PROXMOX_BASE_URL", "")),
		urlKey:      "TG_PROXMOX_BASE_URL",
		tokenRef:    strings.TrimSpace(get("TG_PROXMOX_TOKEN_REF", "")),
		tokenKey:    "TG_PROXMOX_TOKEN_REF",
		insecure:    truthyValue(get("TG_PROXMOX_INSECURE", "")),
		insecureKey: pveLivenessTLSFlagDefault,
	}
	if write.baseURL != "" && write.tokenRef != "" {
		return write, true
	}
	return pveLivenessPair{}, false
}

// pveLivenessTLSFlagKey names the flag that governs the detector's certificate verification under the
// current configuration, so pveTLSFlagDisagreement can attribute paths to flags truthfully instead of from a
// comment that was accurate before this file existed.
func pveLivenessTLSFlagKey(get func(k, def string) string) string {
	if p, ok := resolvePVELivenessPair(get); ok {
		return p.insecureKey
	}
	return pveLivenessTLSFlagDefault
}

// resolvePVELivenessGuests returns the guests the detector watches and the key they came from.
//
// The allowlist is a FAIL-SAFE, not a filter for convenience: empty ⇒ the detector fires for nothing, so an
// unrelated or infra guest going down for maintenance never mints a triage. That invariant is preserved here
// exactly — this function widens WHERE the list may be configured, never what an empty list means.
//
// TG_PVE_LIVENESS_GUESTS is new and triage-side. TG_PROXMOX_ALLOWED_GUESTS is the actuation lane's
// "guests TG manages" list; it is not a credential and is on neither plane list, so it still resolves under
// `both` and on any deployment that forwards it — which is what keeps existing installs unchanged.
func resolvePVELivenessGuests(get func(k, def string) string) ([]string, string) {
	for _, k := range []string{"TG_PVE_LIVENESS_GUESTS", "TG_PROXMOX_ALLOWED_GUESTS"} {
		if g := splitTokens(get(k, "")); len(g) > 0 {
			return g, k
		}
	}
	return nil, ""
}

// reportTLSSkip asks whether an insecure-transport flag is actually earning its keep, and says so in the
// boot log (TG-367).
//
// It replaces a claim with a measurement. Both TLS-skip lines here used to end "— internal self-signed
// endpoint", which was true of LibreNMS and false of Proxmox: that endpoint serves a public Let's Encrypt
// wildcard and only failed because TG addressed it by a short hostname the wildcard cannot match. The
// prose was reviewed and believed for as long as the flag existed, because nothing could contradict it.
//
// Deliberately advisory: it never refuses the boot and never alters the flag. A probe that could take the
// deployment down on a transient network fault would be a worse defect than the one it reports, and
// removing a skip is a config change an operator must make deliberately. It runs in its own goroutine so
// a hanging endpoint delays no other wiring.
// Deduplicated by endpoint: TG_PVE_INSECURE gates three separate constructions (the estate source, the
// actor-evidence reader, and the liveness detector's transport) against ONE endpoint. Every site calls
// this — so the wiring guard stays a simple "each skip reports" rule — and the boot log still says it once.
var tlsSkipReported sync.Map

func reportTLSSkip(endpoint string) {
	if _, seen := tlsSkipReported.LoadOrStore(strings.TrimSpace(endpoint), true); seen {
		return
	}
	go func() {
		v := config.SkipIsNecessary(context.Background(), endpoint, nil)
		log.Printf("config: %s", v)
	}()
}
