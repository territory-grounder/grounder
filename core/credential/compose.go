package credential

// The REQ-1604 authN compose layer (spec/016 T-016-5, TG-98): the actuation interceptor consults this
// AFTER the policy engine (spec/015) returned a non-deny verdict and BEFORE the effect executes, so
// AUTHENTICATION is a distinct control layer that composes with — and neither replaces nor is replaced
// by — AUTHORIZATION. The composer resolves the TARGET's declared identity through the same
// host/group/device-class primitives the policy engine matches on (REQ-1605, one estate object-model)
// and refuses, at its own gate with its own audit row, a target the operator declared no identity for.
//
// What this slice deliberately does NOT do: switch the effect leaf's key. Today's leaves authenticate
// with the deployment's static actuation identity; the per-target key handoff is the JIT/SSH-CA lane
// (spec/022 T-022-3, live-attended). This layer delivers the ORDER, the REFUSAL, and the AUDIT — the
// control-composition property REQ-1604 names — and the resolved bundle stays reference-only.

import "context"

// Composer resolves a target's actuation identity as the interceptor's authn-compose gate. Fail-closed
// end to end: a nil receiver, a nil resolver, and an unresolvable/ambiguous target all refuse.
type Composer struct{ r *AuditedResolver }

// NewComposer builds the compose layer over the audited resolver, so EVERY compose-time resolution is
// appended to the credential_resolution audit like any other (REQ-1620's audit-every-resolution).
func NewComposer(r *AuditedResolver) *Composer { return &Composer{r: r} }

// Compose resolves the identity for one policy-authorized target. It returns the winning rule id (the
// provenance the gate row records) or a refusal error. The bundle itself is deliberately NOT returned:
// this layer decides "an identity is declared and resolves", and the secret material stays behind its
// references until the leaf's own use-time resolution.
func (c *Composer) Compose(ctx context.Context, targetHost string) (string, error) {
	if c == nil || c.r == nil {
		return "", ErrUnresolved
	}
	b, err := c.r.Resolve(ctx, Target{Host: targetHost})
	if err != nil {
		return "", err
	}
	return b.RuleID(), nil
}
