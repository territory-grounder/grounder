package credential

import "errors"

// ErrUnresolved is THE fail-closed sentinel (REQ-1602, INV-09): no credential bundle resolves for the
// target, so the target is neither investigable nor actuatable. Any error, unmatched, or ambiguous path in
// this engine returns ErrUnresolved (or an error that wraps it) together with the ZERO Bundle — there is NO
// fallback to a default, global, or last-used identity. This is the resolver's zero value: an engine with no
// rules refuses everything, and a Bundle's zero value is invalid, so fail-closed holds by construction and
// under any empty or partially-synced store state — it is not a runtime check that a caller can skip.
var ErrUnresolved = errors.New("credential: unresolved target — refuse (no identity; fail closed)")

// ErrAmbiguous is returned when a target matches more than one rule of EQUAL specificity (REQ-1606) — or, in
// a later slice, when declared source precedence does not disambiguate (REQ-1609). It WRAPS ErrUnresolved:
// an ambiguous resolution is a refusal, never an arbitrary pick, so `errors.Is(err, ErrUnresolved)` is true
// for every fail-closed outcome and a caller can treat all refusals uniformly.
var ErrAmbiguous = errAmbiguous{}

type errAmbiguous struct{}

func (errAmbiguous) Error() string {
	return "credential: ambiguous target — refuse (equal-specificity conflict; fail closed)"
}

// Unwrap makes ErrAmbiguous satisfy errors.Is(err, ErrUnresolved): every refusal is fail-closed.
func (errAmbiguous) Unwrap() error { return ErrUnresolved }

// IsRefused reports whether err is any fail-closed refusal from the engine (unresolved or ambiguous). It is
// the single predicate a caller uses to decide "TG cannot reach this target" — on true, the caller MUST NOT
// proceed with any identity.
func IsRefused(err error) bool { return err != nil && errors.Is(err, ErrUnresolved) }
