package main

// TG's OWN identity at the composition root — the self-identity resolvers + the acting-domain set, carved
// out of main() (TG-501 LOC-debt paydown). 'Remedy is code, not config': each self-identity is DERIVED
// from the domain's own credential (never a ruleset token an attacker could be named in) and rendered as
// sshd/journal log it, so it survives key rotation and no config surface can assert it. tgActuatesIn names
// the domains where a TG action can appear in the evidence and therefore needs a self-actor. Behaviour is
// unchanged by the move; the self_ssh_actor/reader + attribution_selfactor unit tests pin it.

import (
	"sort"
	"strings"

	"golang.org/x/crypto/ssh"

	"github.com/territory-grounder/grounder/core/config"
	"github.com/territory-grounder/grounder/modules/observability/hostdiag"
	"github.com/territory-grounder/grounder/modules/observability/syslogng"
)

// resolveSelfActor resolves the platform's own actuation identity from the ACTUATION credential
// (TG_PROXMOX_TOKEN_REF) — deliberately NOT the estate-READ token (TG_PVE_TOKEN_REF): self-recognition
// must key on the identity that actually actuates, or TG reads its OWN heals as third-party changes
// (suspicious) on non-pool hosts. Kept as a seam over a getenv-like func so a test can pin that the source
// is the actuation ref, not the read ref (regression guard for b9212f8). Empty on an unresolvable/malformed
// token — the caller then registers no self identity and self-recognition is simply inert (safe).
func resolveSelfActor(get func(k, def string) string) string {
	tok, err := config.SecretRef(get("TG_PROXMOX_TOKEN_REF", "")).Resolve()
	if err != nil {
		return ""
	}
	return selfPrincipalFromToken(tok)
}

// tgActuatesIn is the set of evidence domains TG ACTUATES in — the domains where a TG action can appear in
// the evidence and therefore needs a self-identity to match. It is stated at the composition root because
// composition is what wires actuation; core/attribution must not infer it.
//
// EVERY MEMBER IS DERIVED FROM THE OP-CLASS REGISTRY'S CLOSED EFFECT-KIND ENUMERATION, and
// TestTheActingDomainSetMatchesTheActuationSurface holds it there:
//
//	effect_kind ""                   the SSH lane  -> "journal"  (sshd records the actuation key's login)
//	effect_kind "awx-launch"         AWX           -> "awx"      (TG's runs land in AWX job history)
//	effect_kind "proxmox-lifecycle"  Proxmox       -> "pve"      (vzstart/vzstop land in the task log)
//	effect_kind "k8s-declarative"    gitops-mr     -> "gitops-mr" (a proposed MR lands in GitLab; the lane is DARK
//	                                               until the owner-present slice-4 arm, and its self-identity
//	                                               resolver is inert until then — like the SSH lane with no key.
//	                                               DomainConfigGaps is armed-gated, so a dark lane reports no gap.)
//
// ★ netbox is DELIBERATELY ABSENT and that absence is the point. Its reader declares ReadOnly() and nothing
// in the tree posts, puts, patches or deletes there, so TG can never appear in a NetBox changelog and has no
// self-identity to match. Reporting a self-actor gap for it would send an operator to wire something that
// cannot change any outcome — and each false warning spends attention and buys distrust of the next.
//
// A hand-written literal here would go stale silently: a mutation dropping "awx" left every test green until
// the oracle below existed, and dropping it SILENCES a real gap rather than creating a visible one.
var tgActuatesIn = map[string]bool{"pve": true, "journal": true, "awx": true, "gitops-mr": true}

// resolveSelfSSHActor derives TG's OWN identity in the `journal` domain from the ACTUATION SSH KEY — the
// same credential that mutates the estate — and renders it exactly as sshd logs it: `<user>!SHA256:<fp>`.
//
// ★ THIS IS THE "REMEDY IS CODE, NOT CONFIG" SHAPE, APPLIED. `ParseConfig` deliberately refuses to read
// SelfActors from the ruleset, because a self-identity an operator can TYPE is one an attacker can be named
// in. So a domain's self-identity has to be derived from that domain's own credential at the composition
// root, the way "pve" already derives its `user@realm!tokenid` from the actuation token. This is the second
// domain to get one, and it inherits the same properties: it survives a key rotation, it cannot be asserted
// by anyone who does not hold the key, and it is not writable from any config surface.
//
// It fails CLOSED to "" — an unresolvable or unparseable key yields no self-identity rather than a guess,
// and the boot-time config-gap report then says the domain has none.
func resolveSelfSSHActor(get func(k, def string) string) string {
	pem, err := config.SecretRef(get("TG_ACTUATION_SSH_KEY", "")).Resolve()
	if err != nil || strings.TrimSpace(pem) == "" {
		return ""
	}
	signer, err := ssh.ParsePrivateKey([]byte(pem))
	if err != nil {
		return ""
	}
	user := strings.TrimSpace(get("TG_ACTUATION_SSH_USER", "root"))
	if user == "" {
		user = "root"
	}
	return user + "!" + ssh.FingerprintSHA256(signer.PublicKey())
}

// resolveSelfSSHReaders derives TG's OWN read-only INVESTIGATION identities in the `journal` domain from the
// HOSTDIAG SSH KEYS — the credentials TG authenticates with when it logs into a faulted host to DIAGNOSE it —
// AND the SYSLOGNG SSH KEYS it authenticates with when it logs into a per-site syslog server to READ that site's
// device logs (TG-457), and renders each exactly as sshd logs it and the journal reader parses it:
// `<user>!SHA256:<fp>` (journal.go), the same shape resolveSelfSSHActor produces for the actuation key. Both key
// sets feed ONE returned reader-set (deduped + sorted), so a key shared between the two lanes is one identity.
//
// ★ WHY THIS IS SEPARATE FROM THE ACTUATION SELF-ACTOR. hostdiag's classify-SSH login lands in the faulted
// host's auth journal DURING triage — AFTER the fault, as part of TG's own investigation. It is TG READING the
// subject, not TG HEALING it. The syslogng log-collection login is the SAME read-not-heal class: when the fault
// is ON the syslog server, TG's own device-log read lands in THAT host's auth journal during triage. So each
// must be recognised as TG's own (or it reads attributed-suspicious and security-escalates TG's own diagnostic
// access, refusing a legitimately-approved heal — the live TG-453 defect), but neither may be treated as the
// actuation identity: a read accomplishes no remediation. Attribute keeps the two apart — a SelfActor match can
// mint attributed-self, a SelfReader match mints NO candidate.
//
// Derived from the KEY, never a config token (the same "remedy is code, not config" shape as the actuation
// self-actor): a reader identity an operator can type is one an attacker can be named in. Rows whose key ref
// does not resolve or parse are skipped (fail-soft — that reader's logins keep reading suspicious, the
// pre-TG-453 behaviour, never a relaxation); the set is de-duplicated + sorted for a deterministic boot. Reads
// through the caller's getter so it is PLANE-SCOPED like the hostdiag and syslogng tools themselves: it resolves
// the reader identities on the triage plane (which holds the hostdiag + syslogng keys and runs attribution) and
// none on the actuation plane (which withholds them and runs no read logins) — exactly where each is correct.
func resolveSelfSSHReaders(get func(k, def string) string) []string {
	seen := map[string]bool{}
	// fold resolves one read-only SSH credential (login user + key REFERENCE) to the `<user>!SHA256:<fp>`
	// identity sshd logs and the journal reader parses, and records it. A blank user or a key ref that does
	// not resolve/parse contributes nothing (fail-soft: that reader keeps reading suspicious, the pre-TG-453
	// behaviour — never a fabricated self-identity that would amnesty whatever matches it). The dedupe collapses
	// a key shared across rows or across the two lanes (hostdiag + syslogng) to a single identity.
	fold := func(sshUser string, keyRef config.SecretRef) {
		user := strings.TrimSpace(sshUser)
		if user == "" {
			return
		}
		pem, err := keyRef.Resolve()
		if err != nil || strings.TrimSpace(pem) == "" {
			return
		}
		signer, err := ssh.ParsePrivateKey([]byte(pem))
		if err != nil {
			return
		}
		seen[user+"!"+ssh.FingerprintSHA256(signer.PublicKey())] = true
	}
	// hostdiag classify-SSH logins (site|hostglob|user|keyref): TG READING a faulted host to diagnose it.
	for _, a := range hostdiag.ParseAccess(get("TG_HOSTDIAG_DEPLOYMENTS", "")) {
		fold(a.SSHUser, a.KeyRef)
	}
	// syslogng device-log reads (site|host|user|keyref|basepath): TG READING a per-site syslog server. A fault
	// ON that server lands this login in its own auth journal during triage — the same read-not-heal class as
	// hostdiag, so its identity must be recognised as TG's own too (TG-457).
	for _, s := range syslogng.ParseServers(get("TG_SYSLOGNG_DEPLOYMENTS", "")) {
		fold(s.SSHUser, s.KeyRef)
	}
	if len(seen) == 0 {
		return nil
	}
	out := make([]string, 0, len(seen))
	for k := range seen {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
