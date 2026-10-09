package main

// egress.go — INSTALL the outbound destination/volume meter (TG-160).
//
// This is the composition-root half of core/egress. Without it the package is a library nobody calls,
// which is the exact shape of defect TG-160 was filed about: docs/THREAT-MODEL.md advertised an "egress"
// step in the interceptor chain while `grep -rn -w -i egress --include=*.go .` returned ZERO hits over
// the whole tree.
//
// WHERE IT SITS AND WHY. installEgressMeter replaces http.DefaultTransport, once, at boot. Measured on
// this tree, 20+ outbound modules build their client as http.DefaultClient or &http.Client{Timeout: …}
// with no Transport of their own, so they all resolve to http.DefaultTransport at call time: the model
// gateway, matrix, netbox, pve, youtrack, jira, slack, teams, mattermost, servicenow, github-issues,
// twilio, librenms, awx, semaphore, vault, oidctoken, awxjob, awxplaybooks, cronicle and the seal transit
// client. One install therefore covers the process's whole HTTP egress surface INCLUDING connectors not
// written yet — a per-module hook would have to be remembered every time and would be forgotten once.
//
// It runs AFTER installBootConfig so the allowlist sees the operator's saved module settings (TG-260:
// a stored value beats the environment, and os.Environ() alone cannot see one), and BEFORE the OpenBao
// credential delivery so that the very first outbound call of the process is already counted.

import (
	"log"
	"os"

	"github.com/territory-grounder/grounder/core/egress"
	"github.com/territory-grounder/grounder/modules/credsource/openbao"
)

// egressMeter is the installed meter. Package-level because two places need it after boot: the admin
// /metrics exposition, and estateHTTPClient — the ONE in-tree client that installs its own Transport.
// nil until installEgressMeter runs (tests that never boot the worker see no meter and no behaviour).
var egressMeter *egress.Meter

// The two knobs, named in the getenv calls as STRING LITERALS rather than through the constants below.
// That is not a style choice: deploy/envparity_test.go finds an env key by reading the literal first
// argument of a getenv call, and it is the guard that fails CI when a binary reads a variable
// docker-compose.yml never forwards — the gap that has shipped three times in this repo. A constant here
// would be invisible to it, and TG_EGRESS_ALLOW would silently resolve empty inside the container while
// looking configured in .env.
const (
	// egressAllowEnv is the operator's escape hatch: extra destinations the endpoint scan cannot derive
	// (a redirect target, a CDN, an egress proxy). CSV/whitespace, bare hosts or URLs, "*.example.org" ok.
	egressAllowEnv = "TG_EGRESS_ALLOW"
	// egressModeEnv selects meter (default) or enforce.
	egressModeEnv = "TG_EGRESS_MODE"
)

// installEgressMeter compiles the allowlist from the deployment's OWN declared endpoints, wraps
// http.DefaultTransport, and returns the meter for the /metrics surface. Safe by construction: in the
// default ModeMeter it changes no request and no response, it only counts.
// The decision itself lives in egress.Install (TG-324), shared with cmd/grounder — which had NO meter at
// all until then, while sitting on both `tg-egress` and the published `tg-frontdoor`. What stays here is
// the part that is genuinely this root's: the two getenv literals (see the const block above for why they
// must not move) and the effectiveEnviron fold.
func installEgressMeter() *egress.Meter {
	m := egress.Install(egress.InstallConfig{
		Environ:   effectiveEnviron(),
		Extra:     getenv("TG_EGRESS_ALLOW", ""),
		ModeRaw:   getenv("TG_EGRESS_MODE", string(egress.ModeMeter)),
		Component: "worker",
		Logf:      log.Printf,
	})
	// Kept package-level for the two post-boot readers: the admin /metrics exposition, and
	// estateHTTPClient — the ONE in-tree client that installs its own Transport and so must be handed the
	// meter explicitly rather than inheriting it from http.DefaultTransport.
	egressMeter = m
	return m
}

// effectiveEnviron is os.Environ() with the operator's saved module settings folded ON TOP, so the
// allowlist reflects what the process will actually dial. Without the fold, a connector configured
// entirely through the console (TG-260) would be invisible to the scan and its legitimate traffic would
// be reported as exfil — the false positive that gets a security meter muted in week one.
func effectiveEnviron() []string {
	out := append([]string(nil), os.Environ()...)
	if cfg := bootCfg.Load(); cfg != nil {
		for k, v := range cfg.byEnvKey {
			out = append(out, k+"="+v)
		}
	}
	return out
}

// meteredBaoTransport hands this process's meter to the OpenBao delivery client (TG-415).
//
// The worker has metered since TG-160 and enforces on 33 declared destinations with 275 counted requests
// — genuinely working, and its OpenBao calls were never among them. vault.New builds its own
// http.Transport for the CA / mTLS config, and a client with its own Transport never reaches
// http.DefaultTransport, where this meter installs. So the enforce claim was narrower than it read.
//
// Same shape as estateHTTPClient above: the ONE other in-tree client that owns its Transport and is
// therefore handed the meter explicitly instead of inheriting it.
//
// Empty when no meter is installed — the delivery client must still be built, because refusing to resolve
// secrets in order to measure them trades a blind spot for an outage.
func meteredBaoTransport() []openbao.WireOption {
	if egressMeter == nil {
		return nil
	}
	return []openbao.WireOption{openbao.WithTransportWrap(egressMeter.Wrap)}
}
