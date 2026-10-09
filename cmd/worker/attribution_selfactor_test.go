package main

import "testing"

// The actor-attribution self-identity MUST derive from the ACTUATION credential (TG_PROXMOX_TOKEN_REF),
// never the estate-READ token (TG_PVE_TOKEN_REF). Keying self-recognition on the read token makes TG read
// its OWN tg-actuate heals as third-party changes (suspicious) on non-pool hosts. This pins that source
// (regression guard for b9212f8) AND the "PRINCIPAL=SECRET" parse.
func TestResolveSelfActorUsesActuationCredentialNotReadToken(t *testing.T) {
	t.Setenv("TG_PROXMOX_TOKEN_REF", "env:ACT_TOK")
	t.Setenv("ACT_TOK", "root@pam!tg-actuate=actuation-secret")
	t.Setenv("TG_PVE_TOKEN_REF", "env:READ_TOK")
	t.Setenv("READ_TOK", "root@pam!tg-estate=read-secret")

	if got := resolveSelfActor(getenv); got != "root@pam!tg-actuate" {
		t.Fatalf("self identity must be the ACTUATION principal, got %q (a read-token principal here means the b9212f8 fix regressed)", got)
	}
}

func TestSelfPrincipalFromToken(t *testing.T) {
	cases := map[string]string{
		"root@pam!tg-actuate=secret": "root@pam!tg-actuate",
		"root@pam=secret":            "root@pam",
		"no-separator":               "",
		"=leadingsep":                "",
		"":                           "",
	}
	for in, want := range cases {
		if got := selfPrincipalFromToken(in); got != want {
			t.Fatalf("selfPrincipalFromToken(%q) = %q, want %q", in, got, want)
		}
	}
}
