package credential

// ORACLES FOR THE APPROLE BOOTSTRAP (TG-153).
//
// THE DEFECT, found by deploying the credential-plane split on a real host. modules/bootstrap has
// implemented approle since it was written, but DeliveryConfig.Validate recognised only mTLS or a token
// ref — so a worker configured with TG_OPENBAO_AUTH_METHOD=approle and both refs was refused at boot:
//
//	credential delivery: substrate address is set but no bootstrap credential
//	(neither an mTLS cert/key nor TG_OPENBAO_TOKEN_REF) — fail closed
//
// Two halves of the same feature disagreeing about what a valid bootstrap is. It BLOCKED the split, and
// not incidentally: the split's whole mechanism is two workers holding two DISTINCT OpenBao identities,
// and approle is the only bootstrap that gives one host two identities. mTLS cannot — both containers
// present the same host certificate, so they receive the same policy and the split collapses into the
// single blast radius it exists to break.

import "testing"

const (
	addr    = "https://openbao.example:8200"
	roleRef = "file:/secrets/tg-triage-role-id"
	sidRef  = "file:/secrets/tg-triage-secret-id"
)

// KILLING MUTATION: remove the approleBootstrap branch from Validate (the shipped state). RED — this is
// the exact configuration the live actuation worker was refused with.
func TestAnApproleBootstrapIsAccepted(t *testing.T) {
	c := DeliveryConfig{Addr: addr, RoleIDRef: roleRef, SecretIDRef: sidRef}
	if err := c.Validate(); err != nil {
		t.Fatalf("a fully-configured approle bootstrap was REFUSED: %v\n\n"+
			"This is what blocked the credential-plane split on the live deployment: approle is the only "+
			"bootstrap that gives one host two OpenBao identities, and without it the triage and actuation "+
			"workers must share one — which is the blast radius the split exists to break.", err)
	}
}

// Half-configured must FAIL CLOSED and say which half is missing. A lone role-id is a public identifier
// that authenticates nothing; a lone secret-id has no role to present it for.
func TestAHalfConfiguredApproleFailsClosed(t *testing.T) {
	for name, c := range map[string]DeliveryConfig{
		"role-id only":   {Addr: addr, RoleIDRef: roleRef},
		"secret-id only": {Addr: addr, SecretIDRef: sidRef},
	} {
		err := c.Validate()
		if err == nil {
			t.Errorf("%s was accepted — a half-configured bootstrap must never silently degrade", name)
			continue
		}
		if !contains(err.Error(), "approle bootstrap requires BOTH") {
			t.Errorf("%s failed with %q, which does not tell the operator which half is missing", name, err)
		}
	}
}

// mTLS must still WIN when both are configured: a certificate is an identity the host IS, an approle
// secret-id is a secret it HOLDS. Ranking is a security property, not a preference.
func TestMtlsStillOutranksApprole(t *testing.T) {
	c := DeliveryConfig{Addr: addr, CertPath: "/secrets/tg.crt", KeyPath: "/secrets/tg.key",
		RoleIDRef: roleRef, SecretIDRef: sidRef}
	if err := c.Validate(); err != nil {
		t.Fatalf("cert+approle together was refused: %v", err)
	}
	if !c.certBootstrap() {
		t.Fatal("certBootstrap() is false with both paths set — the higher-assurance path would be skipped")
	}
}

// The control that keeps the gate meaningful: NO bootstrap at all must still fail, and the message must
// now name all three options rather than the two it used to.
func TestNoBootstrapAtAllStillFailsClosed(t *testing.T) {
	err := DeliveryConfig{Addr: addr}.Validate()
	if err == nil {
		t.Fatal("a substrate address with NO bootstrap credential was accepted — bao: refs would resolve " +
			"against an unauthenticated client, or silently degrade")
	}
	for _, want := range []string{"mTLS", "approle", "TG_OPENBAO_TOKEN_REF"} {
		if !contains(err.Error(), want) {
			t.Errorf("the failure message does not mention %q — an operator cannot tell what to configure: %v", want, err)
		}
	}
}

// VACUITY FLOOR: substrate OFF must remain a no-op, or this whole validator starts failing deployments
// that never asked for a substrate.
func TestSubstrateOffIsStillANoOp(t *testing.T) {
	if err := (DeliveryConfig{}).Validate(); err != nil {
		t.Fatalf("substrate OFF now fails validation (%v) — every deployment without OpenBao would refuse to boot", err)
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
