package preflight

// spec/024 T-024-8 (REQ-2408) — the selector's oracles. The claims:
//
//   1. It NEVER offers a plaintext scheme as a tier: env:/file:/literal appear nowhere in the candidate
//      set or the report, and with no backend configured the report SAYS so rather than falling back.
//   2. Tier order is by irreducible on-host credential: machine identity > machine token > homelab
//      vault > local store, and the recommendation is the strongest AVAILABLE one.
//   3. A compiled-in resolver nobody configured is NOT a posture — availability keys on configuration.
//   4. Every backend states the secret-zero it RELOCATES to and what it does not provide; the report
//      names no configured VALUE, so it is safe to log verbatim (INV-13).

import (
	"strings"
	"testing"
)

func TestSelectorNeverOffersPlaintextAsATier(t *testing.T) {
	for _, a := range []Availability{
		{}, // nothing configured
		{BaoAddr: "https://bao:8200"},
		{VaultwardenURL: "https://vw", SealedStore: true},
	} {
		for _, b := range Backends(a) {
			switch b.Scheme {
			case "env", "file", "literal", "":
				t.Fatalf("a plaintext-bearing scheme was offered as a backend tier: %+v", b)
			}
		}
		if strings.Contains(TierReport(a), "[plaintext]") {
			t.Fatal("the report must never label plaintext as a tier")
		}
	}
	// With nothing configured there is no recommendation at all — never a plaintext fallback.
	if b, ok := Recommend(Availability{}); ok {
		t.Fatalf("no configured backend must yield NO recommendation, got %+v", b)
	}
	rep := TierReport(Availability{})
	if !strings.Contains(rep, "NO secret backend is configured") || !strings.Contains(rep, "plaintext is not offered here as a tier") {
		t.Fatalf("the empty posture must be stated plainly, got:\n%s", rep)
	}
}

func TestRecommendPicksTheStrongestAvailableTier(t *testing.T) {
	cases := []struct {
		name string
		a    Availability
		want Tier
	}{
		{"mTLS beats everything", Availability{BaoAddr: "https://bao", BaoCert: "/etc/tg/tg.crt", VaultwardenURL: "https://vw", SealedStore: true}, TierMachineIdentity},
		{"a wrapped bootstrap is machine identity too", Availability{BaoAddr: "https://bao", BaoWrapToken: "file:/run/wrap"}, TierMachineIdentity},
		{"a cluster JWT is machine identity too", Availability{BaoAddr: "https://bao", BaoJWT: "file:/var/run/secrets/token"}, TierMachineIdentity},
		{"a durable bao credential is machine token", Availability{BaoAddr: "https://bao", VaultwardenURL: "https://vw"}, TierMachineToken},
		{"homelab beats the local store", Availability{VaultwardenURL: "https://vw", SealedStore: true}, TierHomelabVault},
		{"passbolt is the same homelab tier as vaultwarden", Availability{PassboltURL: "https://pb", SealedStore: true}, TierHomelabVault},
		{"the sealed store is the floor", Availability{SealedStore: true}, TierLocalStore},
	}
	for _, c := range cases {
		got, ok := Recommend(c.a)
		if !ok {
			t.Errorf("%s: expected a recommendation", c.name)
			continue
		}
		if got.Tier != c.want {
			t.Errorf("%s: recommended %s (%s), want tier %s", c.name, got.Name, got.Tier, c.want)
		}
	}
	// The two OpenBao rows are mutually exclusive: a deployment is on ONE of them, never both, or the
	// report would claim a machine-identity posture alongside the token one it actually has.
	on := 0
	for _, b := range Backends(Availability{BaoAddr: "https://bao", BaoCert: "/c"}) {
		if b.Scheme == "bao" && b.Available {
			on++
		}
	}
	if on != 1 {
		t.Fatalf("exactly one OpenBao row may read AVAILABLE, got %d", on)
	}
}

func TestUnconfiguredResolversAreNotAPostureAndEveryRowIsHonest(t *testing.T) {
	// A compiled-in resolver nobody pointed at a server must read "not configured".
	for _, b := range Backends(Availability{}) {
		if b.Available {
			t.Errorf("nothing is configured, yet %s reads available", b.Name)
		}
		if strings.TrimSpace(b.SecretZero) == "" || strings.TrimSpace(b.Caveat) == "" {
			t.Errorf("%s must state its secret-zero AND what it does not provide: %+v", b.Name, b)
		}
	}
	// The Implemented/Available split is the honest half of this surface: every row this build CAN
	// resolve says so, and availability still keys on configuration alone. (Both homelab backends are
	// implemented as of T-024-5/6; the field remains the seam for the next declared-but-unbuilt one.)
	for _, b := range Backends(Availability{}) {
		if !b.Implemented {
			t.Errorf("%s reads not-implemented, but every backend in this build resolves: %+v", b.Name, b)
		}
	}

	// REQ-2408's doctrine must be visible in the homelab rows themselves, not only in prose elsewhere.
	for _, b := range Backends(Availability{VaultwardenURL: "https://vw", PassboltURL: "https://pb"}) {
		if b.Tier == TierHomelabVault && !strings.Contains(b.Caveat, "RELOCATES secret-zero") {
			t.Errorf("%s must say it relocates rather than removes secret-zero: %q", b.Name, b.Caveat)
		}
	}
	rep := TierReport(Availability{BaoAddr: "https://bao", BaoCert: "/etc/tg/client.crt"})
	if !strings.Contains(rep, "RECOMMENDED backend stays OpenBao/Vault") {
		t.Errorf("the report must keep OpenBao as the primary recommendation (REQ-2408):\n%s", rep)
	}
	// The report is a posture summary, not a configuration dump: it names schemes and presence only.
	if strings.Contains(rep, "/etc/tg/client.crt") || strings.Contains(rep, "https://bao") {
		t.Errorf("the report must not echo configured VALUES, only presence:\n%s", rep)
	}
}
