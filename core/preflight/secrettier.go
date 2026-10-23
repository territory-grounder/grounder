package preflight

// The TIERED BACKEND SELECTOR (spec/024 T-024-8, REQ-2408 — honest trust posture, do not oversell).
//
// A deployment's secret posture is not "backend or no backend": the backends differ in what their
// IRREDUCIBLE on-host credential is, and that difference is the whole security argument. This selector
// enumerates the configured backends, labels each with its tier and the secret-zero it relocates rather
// than eliminates, picks the best AVAILABLE one as the recommended default — and NEVER offers a
// plaintext scheme as a tier, however convenient, because "env: is available" is exactly the sentence
// REQ-2400 exists to make un-sayable.
//
// It DECIDES NOTHING at runtime. Every reference still resolves through the scheme it names; this is the
// boot-time report that tells an operator which posture they are actually running, and the recommendation
// they should migrate toward. A control that silently rewrote references would be a second authority over
// secrets, which is the failure this file is the opposite of.

import (
	"fmt"
	"sort"
	"strings"
)

// Tier ranks a backend by what its irreducible on-host credential is. Lower is stronger.
type Tier int

const (
	// TierMachineIdentity — the host authenticates by PRESENTING an identity it IS (a CA-signed client
	// certificate) or by consuming a single-use, response-wrapped bootstrap. No durable secret on disk;
	// the trust root is relocated to the CA / orchestrator, which REQ-2407 says plainly rather than
	// claiming trust was eliminated.
	TierMachineIdentity Tier = iota
	// TierMachineToken — a machine-plane backend (OpenBao/Vault) reached with a durable role credential
	// or bearer token on disk. Leased, scoped, individually revocable secrets; the token is secret-zero.
	TierMachineToken
	// TierHomelabVault — a human password manager repurposed as a machine store (Vaultwarden, Passbolt).
	// Real secrets at rest, but the on-host credential is an UNSCOPABLE master credential and there are
	// no leased/scoped/individually-revocable secrets (REQ-2405/2406/2408). Never a default.
	TierHomelabVault
	// TierLocalStore — the sealed store in TG's own database (store:). Acceptable for a homelab tier
	// where the DB is the trust boundary anyway; its master key is the irreducible credential.
	TierLocalStore
)

func (t Tier) String() string {
	switch t {
	case TierMachineIdentity:
		return "machine-identity"
	case TierMachineToken:
		return "machine-token"
	case TierHomelabVault:
		return "homelab-vault"
	default:
		return "local-store"
	}
}

// Backend is one candidate secret backend and the honest statement of what it does and does not give.
type Backend struct {
	Scheme    string // the SecretRef scheme it serves ("bao", "vw", "passbolt", "store")
	Name      string // human label
	Tier      Tier
	Available bool // the deployment configured it AND this build implements its resolver
	// Implemented is false for a backend the SPEC declares but this build does not yet resolve. It is a
	// separate field from Available on purpose: "you configured it" and "this binary can use it" are
	// different facts, and collapsing them is how an operator ends up pointing at a server nothing reads.
	Implemented bool
	SecretZero  string // the irreducible on-host credential it RELOCATES secret-zero to
	Caveat      string // what it does NOT provide (leases, scoping, revocation), stated up front
}

// Availability is what the selector needs to know about a deployment. The composition root fills it from
// its own already-read configuration — this package reads no environment of its own, so the report can
// never disagree with the process that produced it.
type Availability struct {
	BaoAddr        string // TG_OPENBAO_ADDR
	BaoCert        string // TG_OPENBAO_CERT (mTLS machine identity)
	BaoWrapToken   string // TG_OPENBAO_WRAP_TOKEN_REF (single-use wrapped bootstrap)
	BaoJWT         string // TG_OPENBAO_JWT_REF (k8s service-account identity)
	VaultwardenURL string // TG_VAULTWARDEN_ADDR
	PassboltURL    string // TG_PASSBOLT_ADDR
	SealedStore    bool   // the sealed-secret store is wired (store: resolves)
}

// Backends returns every candidate in tier order, each labeled available or not. A backend is available
// only when the deployment actually configured it — a compiled-in resolver nobody pointed at a server is
// NOT a posture, and reporting it as one is the overselling REQ-2408 forbids.
func Backends(a Availability) []Backend {
	bao := strings.TrimSpace(a.BaoAddr) != ""
	identity := bao && (strings.TrimSpace(a.BaoCert) != "" || strings.TrimSpace(a.BaoWrapToken) != "" || strings.TrimSpace(a.BaoJWT) != "")
	return []Backend{
		{
			Scheme: "bao", Name: "OpenBao/Vault (machine identity)", Tier: TierMachineIdentity,
			Implemented: true, Available: identity,
			SecretZero: "none on disk — a CA-signed client certificate, a single-use wrapped SecretID, or a cluster-issued JWT",
			Caveat:     "the trust root is RELOCATED to the CA / orchestrator / cluster API, not eliminated (REQ-2407)",
		},
		{
			Scheme: "bao", Name: "OpenBao/Vault (durable role credential)", Tier: TierMachineToken,
			Implemented: true, Available: bao && !identity,
			SecretZero: "the AppRole SecretID or bootstrap token on disk",
			Caveat:     "that one credential is unscoped by construction; everything BELOW it is leased, scoped and individually revocable",
		},
		{
			Scheme: "vw", Name: "Vaultwarden / Bitwarden", Tier: TierHomelabVault,
			Implemented: true, Available: strings.TrimSpace(a.VaultwardenURL) != "",
			SecretZero: "the account MASTER PASSWORD (unscopable — it unlocks the whole vault)",
			Caveat:     "no leased, scoped or individually-revocable secrets; it RELOCATES secret-zero rather than removing it (REQ-2405/2408)",
		},
		{
			Scheme: "passbolt", Name: "Passbolt", Tier: TierHomelabVault,
			Implemented: true, Available: strings.TrimSpace(a.PassboltURL) != "",
			SecretZero: "the robot's OpenPGP PRIVATE KEY and its passphrase",
			Caveat:     "no leased, scoped or individually-revocable secrets; it RELOCATES secret-zero rather than removing it (REQ-2406/2408)",
		},
		{
			Scheme: "store", Name: "TG sealed store (Postgres)", Tier: TierLocalStore,
			Implemented: true, Available: a.SealedStore,
			SecretZero: "the seal master key",
			Caveat:     "the database is the trust boundary; a DB compromise plus the master key is total",
		},
	}
}

// Recommend picks the strongest AVAILABLE backend and returns it with the reason. ok=false means the
// deployment has configured NO backend at all — which is a real answer ("you are on plaintext schemes")
// and never a silent fallback: this function has no plaintext option to return, by construction.
func Recommend(a Availability) (Backend, bool) {
	all := Backends(a)
	avail := make([]Backend, 0, len(all))
	for _, b := range all {
		if b.Available {
			avail = append(avail, b)
		}
	}
	if len(avail) == 0 {
		return Backend{}, false
	}
	sort.SliceStable(avail, func(i, j int) bool { return avail[i].Tier < avail[j].Tier })
	return avail[0], true
}

// TierReport renders the boot-time posture: every backend with its tier, availability, the secret-zero it
// relocates to and what it does not provide, plus the recommendation. It NAMES NO VALUE — only schemes,
// tiers and configuration presence — so it is safe to log verbatim (INV-13).
func TierReport(a Availability) string {
	var b strings.Builder
	b.WriteString("secret backends by tier (spec/024 REQ-2408 — each RELOCATES secret-zero, none eliminates it):")
	for _, be := range Backends(a) {
		state := "not configured"
		switch {
		case !be.Implemented:
			state = "NOT IMPLEMENTED in this build"
		case be.Available:
			state = "AVAILABLE"
		}
		fmt.Fprintf(&b, "\n  [%s] %s (%s:) — %s; secret-zero: %s; caveat: %s",
			be.Tier, be.Name, be.Scheme, state, be.SecretZero, be.Caveat)
	}
	if rec, ok := Recommend(a); ok {
		fmt.Fprintf(&b, "\n  → running the %s tier: %s. The primary RECOMMENDED backend stays OpenBao/Vault (REQ-2408); "+
			"a homelab tier is never a default for a new installation.", rec.Tier, rec.Name)
	} else {
		b.WriteString("\n  → NO secret backend is configured: every reference resolves through a plaintext-bearing " +
			"scheme (env:/file:) or fails closed. That is the posture TG_SECRET_POLICY=enforce refuses to boot on; " +
			"plaintext is not offered here as a tier because it is not one.")
	}
	return b.String()
}
