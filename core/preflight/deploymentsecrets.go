package preflight

import (
	"fmt"
	"strings"

	"github.com/territory-grounder/grounder/core/config"
)

// THE SECRETS NO BINARY EVER DECLARED (TG-278, closed here as part of TG-284).
//
// The boot gate polices what a caller enumerates, and both binaries enumerated only their OWN *_REF reads.
// Four credentials in this deployment were therefore outside every list, and the 2026-08-04 audit of the
// live box found all four in plaintext while the gate ran enforce and reported green:
//
//	TG_AM_INGEST_TOKEN  64-char literal   the Alertmanager push-ingest bearer  (grounder env)
//	TG_OPUS_SIDECAR_KEY 32-char literal   the tg-claude-proxy bearer           (litellm env)
//	LIBRENMS_TOKEN      32-char literal   the NL LibreNMS API token            (worker env)
//	LIBRENMS_GR_TOKEN   64-char literal   the GR LibreNMS API token            (worker env)
//
// They divide into two kinds and are fixed differently, because "add a *_REF variant" is only a fix where
// something will READ that variant. A reference knob nothing resolves is worse than no knob: an operator
// who sets it to bao: gets a green gate and a credential that never moved.
//
//  1. AM + OPUS had NO reference path at all (`grep -c TG_AM_INGEST_TOKEN_REF cmd/ core/ modules/` was 0),
//     so no configuration could move them to a backend. Each gets a real *_REF variant here, defaulting to
//     `env:<NAME>` so an existing deployment resolves exactly as it does today — and each has a REAL
//     consumer in this same MR: cmd/grounder provisions the `prometheus-alertmanager` sources row from
//     TG_AM_INGEST_TOKEN_REF (the same boot provisioner LibreNMS already uses), and deploy/docker-compose.yml
//     resolves TG_OPUS_SIDECAR_KEY_REF through the litellm-secrets tg-secretenv init into the litellm
//     container (the same path LITELLM_MASTER_KEY and the provider keys already take, spec/024 REQ-2403).
//
//  2. The LibreNMS tokens ALREADY had a reference path: the third field of a TG_LIBRENMS_DEPLOYMENTS row is
//     a SecretRef and has always accepted bao:. What they lacked was ENUMERATION — nothing read that field
//     into the gate. LibrenmsDeploymentEntries reads the refs out of the compound variable so they are
//     policed where they actually live. A separate LIBRENMS_TOKEN_REF / LIBRENMS_GR_TOKEN_REF knob was
//     deliberately NOT added: nothing would read it, it would compete with the row field that IS read, and
//     the raw values stay visible regardless because the shape scan (envshape.go) sees them directly.
//
// All four are BUSINESS secrets (not exempt): every one of them can resolve from OpenBao.

// LiteralOnlySecretRefs are the *_REF variables introduced for the credentials that had no reference path.
// Default is the behaviour-preserving code default: an operator who sets nothing keeps today's resolution
// (and, correctly, today's plaintext VIOLATION — the gate is supposed to see it).
var LiteralOnlySecretRefs = []struct{ Name, Raw, Default string }{
	{Name: "TG_AM_INGEST_TOKEN_REF", Raw: "TG_AM_INGEST_TOKEN", Default: "env:TG_AM_INGEST_TOKEN"},
	{Name: "TG_OPUS_SIDECAR_KEY_REF", Raw: "TG_OPUS_SIDECAR_KEY", Default: "env:TG_OPUS_SIDECAR_KEY"},
}

// DefaultRefFor returns the behaviour-preserving `env:<NAME>` default for one of the *_REF variables above,
// or "" for any other name. It exists so the binary that CONSUMES a ref and the gate that POLICES it read
// the same default from one place: the two agreeing by coincidence is how a knob ends up meaning different
// things at the two ends of the same MR.
func DefaultRefFor(name string) string {
	for _, r := range LiteralOnlySecretRefs {
		if r.Name == name {
			return r.Default
		}
	}
	return ""
}

// DeploymentSecretEntries returns the deployment-wide business secrets a binary must police: the *_REF
// variants above and every LibreNMS per-site token reference declared in TG_LIBRENMS_DEPLOYMENTS. get is the
// caller's env accessor.
//
// A *_REF is enumerated only when THIS PROCESS actually has the credential — the ref is set explicitly, or
// the raw variable it defaults to is present. That condition is the difference between a gate and a wall of
// noise: TG_OPUS_SIDECAR_KEY lives in the litellm container and TG_AM_INGEST_TOKEN in the grounder, so
// applying the `env:<NAME>` default unconditionally would fail the WORKER's boot over two credentials it
// does not hold and cannot fix. It is the same rule CheckSecretPolicy already applies to an empty ref: a
// secret this process does not have is not this process's plaintext violation.
//
// Both binaries call this. That is deliberate — these are credentials of the DEPLOYMENT, and whichever
// process holds one must be the one that reports it.
func DeploymentSecretEntries(get func(string) string) []SecretEntry {
	out := make([]SecretEntry, 0, len(LiteralOnlySecretRefs)+2)
	for _, r := range LiteralOnlySecretRefs {
		switch ref := strings.TrimSpace(get(r.Name)); {
		case ref != "":
			out = append(out, SecretEntry{Name: r.Name, Ref: config.SecretRef(ref)})
		case strings.TrimSpace(get(r.Raw)) != "":
			// No ref set, but the plaintext IS here: report it under the ref name that fixes it, so the
			// error tells the operator which variable to set rather than only which one to delete.
			out = append(out, SecretEntry{Name: r.Name, Ref: config.SecretRef(r.Default)})
		}
	}
	out = append(out, LibrenmsDeploymentEntries(get("TG_LIBRENMS_DEPLOYMENTS"))...)
	// ★ THE SSH KEY REFS, WHICH THIS GATE COULD NOT SEE (TG-302).
	//
	// TG-284 taught this enumerator to read a credential out of a COMPOUND deployment variable, and then
	// taught it about exactly one: TG_LIBRENMS_DEPLOYMENTS. The two SSH read lanes declare their keys the
	// same way — field 3 of a ';'-separated row — and stayed invisible, so on 2026-08-04 the live
	// deployment ran `TG_SECRET_POLICY=enforce`, reported 0 violations, and read every host with
	// `file:/secrets/one_key`: a private key on a shared bind mount, which is a plaintext-bearing scheme
	// this gate is supposed to refuse.
	//
	// It looked fine from two directions at once, which is why it lasted. This gate did not enumerate the
	// variable, so it had nothing to judge; and CheckSSHKeys DOES read the same variable but only asserts
	// the key resolves and parses, so the boot log said "credential preflight OK — 2 SSH key ref(s)
	// resolve+parse" about a key that authenticates to nothing (TG-300). One gate was silent and the other
	// was reassuring.
	//
	// There is no technical reason for the file: scheme here. TG_ACTUATION_SSH_KEY is already
	// bao:secret/data/tg/actuator#key and resolves through the same code.
	out = append(out, SSHDeploymentEntries(get)...)
	// ★ THE REF THAT ACTUALLY WINS (TG-306).
	//
	// TG-302/TG-304 taught this enumerator to see the SSH key refs inside TG_SYSLOGNG_DEPLOYMENTS and
	// TG_HOSTDIAG_DEPLOYMENTS. It turned out those are not the refs the agent authenticates with.
	//
	// The credential engine resolves most-specific-wins across sources, and AWX registers at precedence 20
	// while the native hostdiag source registers at 100. So the AWX rule wins every time, and the key it
	// hands out comes from TG_AWX_CRED_REF_MAP — a variable no preflight and no policy gate enumerated.
	// Measured on the live box 2026-08-04: 35 of 36 resolutions in 24h were `source=awx rule=jt:60:cred:24
	// shadowed=native-hostdiag`, i.e. the hostdiag ref was configured, preflighted, and never used.
	//
	// The consequence was not theoretical. That map pointed at an UNRESTRICTED estate root key
	// (`ssh -i <ref> root@host id` -> `uid=0(root)`), so the gate reported a clean deployment while the
	// worker held root on the fleet. And setting this variable back to a `file:` ref would have kept the
	// boot green under enforce, because nothing looked at it.
	return append(out, AWXCredRefMapEntries(get("TG_AWX_CRED_REF_MAP"))...)
}

// AWXCredRefMapEntries extracts the SecretRef from each "<AWX credential name>=<ref>" pair of
// TG_AWX_CRED_REF_MAP, named for the variable AND the credential so a violation says which binding is
// still on disk.
//
// It parses leniently on purpose — a malformed pair contributes nothing rather than failing the gate.
// modules/bootstrap.parseAWXCredRefMap is the authority on the format and fails closed there; a second
// strict parser here would fail the boot twice for one typo, and worse, could disagree about which pairs
// exist and police a set the engine does not use.
func AWXCredRefMapEntries(spec string) []SecretEntry {
	var out []SecretEntry
	for _, pair := range strings.Split(spec, ";") {
		pair = strings.TrimSpace(pair)
		if pair == "" {
			continue
		}
		// The AWX credential NAME can contain '=' — split on the LAST one, which is where the ref starts.
		i := strings.LastIndex(pair, "=")
		if i <= 0 || i == len(pair)-1 {
			continue
		}
		name, ref := strings.TrimSpace(pair[:i]), strings.TrimSpace(pair[i+1:])
		if name == "" || ref == "" {
			continue
		}
		out = append(out, SecretEntry{Name: "TG_AWX_CRED_REF_MAP[" + name + "]", Ref: config.SecretRef(ref)})
	}
	return out
}

// SSHDeploymentEntries extracts the key SecretRef from every row of the SSH deployment variables, so the
// policy gate judges them by the same rule as every other business credential.
//
// These are NOT bootstrap credentials and must not be exempted as such. The OpenBao writer credentials and
// the seal token are file: forever because they authenticate TO the backend and cannot come FROM it. An SSH
// key used to read estate hosts has no such constraint — it is an ordinary business secret that happens to
// be delivered as a file, and calling it "bootstrap" is how it stayed on disk.
func SSHDeploymentEntries(get func(string) string) []SecretEntry {
	var out []SecretEntry
	// name, variable, and which field of a row holds the key reference.
	for _, v := range []struct {
		env      string
		refField int
	}{
		{"TG_SYSLOGNG_DEPLOYMENTS", 3}, // site|host|user|KEYREF|basepath[|tag]
		{"TG_HOSTDIAG_DEPLOYMENTS", 3}, // site|hostglob|user|KEYREF
	} {
		for _, f := range splitRows(get(v.env)) {
			if len(f) <= v.refField {
				continue
			}
			ref := strings.TrimSpace(f[v.refField])
			if ref == "" {
				continue // no credential declared on this row — nothing to police
			}
			site := at(f, 0)
			if site == "" {
				site = "?"
			}
			out = append(out, SecretEntry{Name: fmt.Sprintf("%s[%s]", v.env, site), Ref: config.SecretRef(ref)})
		}
	}
	return out
}

// LibrenmsDeploymentEntries extracts the token SecretRef from each row of a TG_LIBRENMS_DEPLOYMENTS spec
// (`site|baseurl|tokenref[|timezone]`, ';'-separated — the same shape cmd/worker's librenmsDeployments
// parses, and the same row parsing SSHKeyRefsFromEnv already does for the SSH deployment vars). Each entry
// is named for the variable AND the site, so a violation says which site's token is still plaintext.
//
// A row with no token reference contributes nothing: there is no credential there to police. An empty spec
// yields no entries — LibreNMS is simply not configured.
func LibrenmsDeploymentEntries(spec string) []SecretEntry {
	var out []SecretEntry
	for _, f := range splitRows(spec) {
		if len(f) < 3 {
			continue
		}
		ref := strings.TrimSpace(f[2])
		if ref == "" {
			continue
		}
		site := at(f, 0)
		if site == "" {
			site = "?"
		}
		out = append(out, SecretEntry{Name: fmt.Sprintf("TG_LIBRENMS_DEPLOYMENTS[%s]", site), Ref: config.SecretRef(ref)})
	}
	return out
}
