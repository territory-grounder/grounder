package credential

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"

	"github.com/territory-grounder/grounder/core/config"
)

// ---------------------------------------------------------------------------------------------------------
// AUDITED RESOLUTION SEAM (spec/016, REQ-1604/1617).
//
// AuditedResolver is the ONE machine-plane resolution entry point both the read-only investigation modules
// (hostdiag SSH reads, ACTIVE now) and the actuation effect leaf (spec/013 interceptor, gated OFF) consult in
// place of a hardcoded per-host identity. It runs the target through the SyncEngine (synced sources + the
// native fallback), appends exactly ONE non-secret credential_resolution audit row (REQ-1617), and returns
// the resolved Bundle or a typed fail-closed refusal (REQ-1602). Resolution happens AFTER policy authorizes
// and BEFORE execute (REQ-1604): authentication is a distinct control layer that composes with authorization.
//
// It NEVER falls back to a default/global/last-used identity: an unmatched, ambiguous, or unresolvable target
// returns the zero Bundle with ErrUnresolved/ErrAmbiguous, and the audit row records the refusal.
//
// The audit row carries only NON-SECRET identity metadata — the target label, matched rule/selector, winning
// source (or native fallback), the resolved login user, the connection scheme, and the SCHEME of the key
// reference (e.g. "file"/"env") — NEVER key material and never the ref value beyond its scheme (INV-13).
// ---------------------------------------------------------------------------------------------------------

// ResolutionOutcome is the terminal outcome of one Resolve, recorded on the audit row.
type ResolutionOutcome string

const (
	// OutcomeResolved — a single bundle resolved for the target.
	OutcomeResolved ResolutionOutcome = "resolved"
	// OutcomeUnresolved — no source/rule covered the target (fail closed).
	OutcomeUnresolved ResolutionOutcome = "unresolved"
	// OutcomeAmbiguous — the target matched equal-specificity rules or equal-precedence sources (fail closed).
	OutcomeAmbiguous ResolutionOutcome = "ambiguous"
)

// ResolutionEvent is the NON-SECRET record of one Resolve (REQ-1617). Every field is safe to persist and
// surface: it holds identity METADATA only, never a secret value or a full SecretRef — at most a ref's scheme.
type ResolutionEvent struct {
	Target       string            // non-secret target label (host / resource / group / device-class)
	ExternalRef  string            // the NON-SECRET incident correlation id this resolution served (spec/020 REQ-2015); "" when resolved outside an incident
	Plane        Plane             // always machine for host-reachability resolution
	Outcome      ResolutionOutcome // resolved / unresolved / ambiguous
	Source       string            // winning source id; "" when resolved from the native fallback or unresolved
	Native       bool              // true when resolved from the native fallback store
	RuleID       string            // matched rule / winning source entry id (provenance); "" when unresolved
	User         string            // resolved login user (non-secret); "" when unresolved
	Scheme       ConnectionScheme  // resolved connection scheme; "" when unresolved
	KeyRefScheme string            // the SCHEME of the key reference only (env/file/store/vault/bao) — never the ref value or material
	Shadowed     []string          // lower-precedence sources that also matched (non-secret provenance)
	Err          string            // non-secret refusal text; empty on resolved
}

// resolveCtxKey carries the NON-SECRET incident correlation id (external_ref) into a Resolve so the audit row
// joins the decision-tracer walk (spec/020 REQ-2015). It rides the context so NO call site's signature changes:
// the investigation activity sets it once before the agent loop, and every tool-invoked Resolve underneath
// inherits it. A resolution outside any incident (e.g. a boot-time probe) simply carries an empty ref.
type resolveCtxKey struct{}

// WithExternalRef returns a context that carries the incident external_ref for credential-resolution auditing.
// An empty ref is a no-op (the resolution records an empty correlation key, exactly as before).
func WithExternalRef(ctx context.Context, externalRef string) context.Context {
	if externalRef == "" {
		return ctx
	}
	return context.WithValue(ctx, resolveCtxKey{}, externalRef)
}

// externalRefFromContext reads the incident external_ref a Resolve should stamp on its audit row ("" if unset).
func externalRefFromContext(ctx context.Context) string {
	if v, ok := ctx.Value(resolveCtxKey{}).(string); ok {
		return v
	}
	return ""
}

// ResolutionSink durably appends a ResolutionEvent to the tamper-evident audit projection (REQ-1617). The
// pgx impl writes migration 0018's append-only credential_resolution table; the in-memory twin drives the CI
// oracles. It NEVER receives (nor can it write) a secret value — ResolutionEvent is secret-free by
// construction (INV-13). Append is best-effort at the call site: a projection write failure must not make an
// authorized target un-investigable, so the resolver returns the bundle regardless — but the fail-CLOSED
// security decision (refuse an unresolved target) is the resolver's, never the sink's.
type ResolutionSink interface {
	AppendResolution(ctx context.Context, ev ResolutionEvent) error
}

// sinkBox boxes the ResolutionSink so it can be swapped atomically after construction (the worker builds the
// resolver before its DB-backed sink exists, then installs the sink once the pool is up — mirroring the
// credential-state publish late-binding).
type sinkBox struct{ s ResolutionSink }

// AuditedResolver wraps a SyncEngine with the audit sink. It is safe for concurrent use.
type AuditedResolver struct {
	engine *SyncEngine
	sink   atomic.Pointer[sinkBox]
}

// NewAuditedResolver builds the resolver over a SyncEngine (the native fallback + registered sources). The
// audit sink is unset initially — resolution still fails closed and returns bundles; it simply appends no row
// until SetSink installs one. A nil engine is a valid fail-closed resolver (refuses every target).
func NewAuditedResolver(engine *SyncEngine) *AuditedResolver {
	return &AuditedResolver{engine: engine}
}

// SetSink installs (or replaces) the audit sink. Call it once the durable store exists; a nil sink disables
// row appends without affecting resolution.
func (r *AuditedResolver) SetSink(s ResolutionSink) {
	if r == nil {
		return
	}
	r.sink.Store(&sinkBox{s: s})
}

// Resolve resolves a machine-plane Target to a Bundle through the engine, appends one non-secret audit row,
// and returns the bundle or a typed fail-closed refusal (REQ-1602/1604/1617). On ErrUnresolved/ErrAmbiguous
// it returns the ZERO Bundle and the error — NEVER a default identity — and records the refusal outcome.
func (r *AuditedResolver) Resolve(ctx context.Context, t Target) (Bundle, error) {
	ev := ResolutionEvent{Target: targetLabel(t), ExternalRef: externalRefFromContext(ctx), Plane: PlaneMachine}
	if r == nil || r.engine == nil {
		ev.Outcome = OutcomeUnresolved
		ev.Err = ErrUnresolved.Error()
		r.append(ctx, ev)
		return Bundle{}, ErrUnresolved
	}

	res, err := r.engine.Resolve(t)
	if err != nil {
		ev.Outcome = OutcomeUnresolved
		if errors.Is(err, ErrAmbiguous) {
			ev.Outcome = OutcomeAmbiguous
		}
		ev.Err = err.Error()
		r.append(ctx, ev)
		return Bundle{}, err
	}

	b := res.Bundle
	ev.Outcome = OutcomeResolved
	ev.Source = res.Source
	ev.Native = res.Native
	ev.RuleID = b.RuleID()
	ev.User = b.User()
	ev.Scheme = b.Scheme()
	ev.KeyRefScheme = refScheme(b.SSHKeyRef())
	ev.Shadowed = res.Shadowed
	r.append(ctx, ev)
	return b, nil
}

// append writes the audit row best-effort: a nil/unset sink is a no-op, and a write error is swallowed here
// (the caller already holds the security-relevant resolution outcome). The event is secret-free, so nothing
// sensitive can leak through this path.
func (r *AuditedResolver) append(ctx context.Context, ev ResolutionEvent) {
	if r == nil {
		return
	}
	box := r.sink.Load()
	if box == nil || box.s == nil {
		return
	}
	_ = box.s.AppendResolution(ctx, ev)
}

// refScheme returns the SCHEME token of a SecretRef (the part before the first ':', e.g. "file"/"env"/
// "store"/"vault"/"bao") — safe to record. It NEVER returns the reference path/value or any key material. An
// empty or scheme-less ref yields "".
func refScheme(ref config.SecretRef) string {
	s := string(ref)
	if i := strings.IndexByte(s, ':'); i > 0 {
		return s[:i]
	}
	return ""
}
