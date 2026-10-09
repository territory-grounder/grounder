package wiring

import (
	"fmt"
	"os"
	"reflect"
	"strings"
	"time"
)

// AcceptDarkEnv names the seams an operator has consciously accepted as dark, comma-separated. It exists
// only for Critical seams, and it is deliberately an ENVIRONMENT variable rather than a source constant:
// an environment key is forwarded through the compose file (asserted by deploy's env-parity oracle),
// appears in the boot report, and is recorded in the ledger row. A source-level override would be a
// one-line edit that reads like ordinary configuration.
const AcceptDarkEnv = "TG_WIRING_ACCEPT_DARK"

// maxDarkHorizon bounds how far out a Because may be dated. Without a cap, "expires" is decoration: the
// first waiver would be written with a five-year expiry and never looked at again.
const maxDarkHorizon = 180 * 24 * time.Hour

// State is what a seam turned out to be. The zero value is DarkUnrecorded on purpose — a seam nothing
// touched must not read as live.
type State int

const (
	// DarkUnrecorded: no Bind and no Absent ran for this seam. Either a code path forgot it, or a branch
	// exists that nobody wrote a declaration for — the if/else-if with no else, which is how the
	// notifier's zero-notifier case stayed silent.
	DarkUnrecorded State = iota
	// DarkUnbound: Bind ran and the value it was handed was nil. The seam was written as if wired.
	DarkUnbound
	// DeclaredDark: Absent ran with a valid Because. Honest, bounded, and expires.
	DeclaredDark
	// AcceptedDark: a Critical seam declared dark AND named by the operator in AcceptDarkEnv.
	AcceptedDark
	// Live: Bind ran and reflection proved the value is usable.
	Live
)

func (s State) String() string {
	switch s {
	case DarkUnbound:
		return "dark-unbound"
	case DeclaredDark:
		return "declared-dark"
	case AcceptedDark:
		return "accepted-dark"
	case Live:
		return "live"
	default:
		return "dark-unrecorded"
	}
}

// dark reports whether this state means "an operator would not be reached".
func (s State) dark() bool { return s != Live }

// Because is why a seam is deliberately not wired. Every field is required, so a declaration costs a
// sentence of thought rather than a keystroke, and Expiry makes it perishable.
type Because struct {
	Reason      string
	Consequence string
	Owner       string
	Ticket      string
	Expiry      time.Time
}

func (b Because) validate(now time.Time) error {
	var missing []string
	for name, v := range map[string]string{
		"Reason": b.Reason, "Consequence": b.Consequence, "Owner": b.Owner, "Ticket": b.Ticket,
	} {
		if strings.TrimSpace(v) == "" {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		// Sorted for a deterministic message — an error that reorders between runs is a diff nobody trusts.
		return fmt.Errorf("wiring: Because is incomplete (missing %s)", strings.Join(sorted(missing), ", "))
	}
	if b.Expiry.IsZero() {
		return fmt.Errorf("wiring: Because requires an Expiry — a waiver with no end is a permanent one")
	}
	if !b.Expiry.After(now) {
		return fmt.Errorf("wiring: Because expired at %s", b.Expiry.UTC().Format(time.RFC3339))
	}
	if b.Expiry.Sub(now) > maxDarkHorizon {
		return fmt.Errorf("wiring: Because expiry %s is more than %d days out",
			b.Expiry.UTC().Format(time.RFC3339), int(maxDarkHorizon.Hours()/24))
	}
	return nil
}

func sorted(in []string) []string {
	out := append([]string(nil), in...)
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j] < out[j-1]; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}

type record struct {
	state  State
	detail string
}

// Manifest collects what the composition root actually did. It is written once at boot, then read.
type Manifest struct {
	now      func() time.Time
	acceptor func(string) string
	recorded map[Seam]record
	// errs collects declaration errors (an invalid Because, an unaccepted Critical waiver). They surface
	// as findings rather than panics: a live worker must not die because a waiver's ticket field is empty.
	errs []string
}

// New builds an empty Manifest reading the real clock and environment.
func New() *Manifest {
	return &Manifest{now: time.Now, acceptor: os.Getenv, recorded: map[Seam]record{}}
}

// newFor builds a Manifest with injected clock and environment, for the oracles.
func newFor(now func() time.Time, getenv func(string) string) *Manifest {
	return &Manifest{now: now, acceptor: getenv, recorded: map[Seam]record{}}
}

// Bind records a seam as LIVE only if v is genuinely usable, and returns v unchanged so it sits in value
// position at the assignment it describes.
//
// THERE IS NO `live bool` PARAMETER, AND THERE MUST NEVER BE ONE. The whole mechanism rests on the
// caller being unable to assert liveness: liveness is read off the value by reflection. A boolean would
// restore exactly the failure this package exists to prevent — a call site that says "wired" beside a
// nil sink, which is what `deps.Notify` looked like for the entire life of the defect.
func Bind[T any](m *Manifest, s Seam, v T) T {
	if m == nil {
		return v
	}
	if rv := reflect.ValueOf(v); usable(rv) {
		// A usable OUTER value is not enough. notifierPager{notify: nil} is a perfectly non-nil struct
		// whose Page() returns success while reaching nobody — "wired but functionally dark" is its own
		// defect class, and the outer nil check cannot see it. One level deep, by design: deeper walking
		// buys diminishing returns and a lot of reflection on hot types.
		if field, holed := firstNilRequiredField(rv); holed {
			m.record(s, record{state: DarkUnbound,
				detail: fmt.Sprintf("%T is non-nil but its required field %q is nil — wired, and functionally dark", v, field)})
			return v
		}
		m.record(s, record{state: Live})
	} else {
		m.record(s, record{state: DarkUnbound,
			detail: fmt.Sprintf("Bind received an unusable %T (nil)", v)})
	}
	return v
}

// usable reports whether a bound value can actually be called or dereferenced. An invalid Value (a nil
// interface handed to a generic parameter) is not usable; neither is a typed nil.
func usable(rv reflect.Value) bool {
	if !rv.IsValid() {
		return false
	}
	switch rv.Kind() {
	case reflect.Func, reflect.Interface, reflect.Ptr, reflect.Map, reflect.Slice, reflect.Chan:
		return !rv.IsNil()
	default:
		return true
	}
}

// Absent records a seam as deliberately not wired, and returns the zero value so the caller's assignment
// still type-checks. It must be called in the SAME conditional branch that would have called Bind —
// which makes "declared dark" a visible diff in the composition root rather than a line in a file nobody
// opens.
func Absent[T any](m *Manifest, s Seam, why Because) T {
	var zero T
	if m == nil {
		return zero
	}
	if err := why.validate(m.now()); err != nil {
		m.errs = append(m.errs, fmt.Sprintf("%s: %v", s, err))
		m.record(s, record{state: DarkUnrecorded, detail: err.Error()})
		return zero
	}
	st := DeclaredDark
	if sp, ok := spec(s); ok && sp.Criticality == Critical {
		if !m.accepted(s) {
			m.errs = append(m.errs, fmt.Sprintf(
				"%s is CRITICAL: a Because is not sufficient — set %s=%s to accept it consciously", s, AcceptDarkEnv, s))
		} else {
			st = AcceptedDark
		}
	}
	m.record(s, record{state: st, detail: fmt.Sprintf("%s (owner %s, %s, expires %s)",
		why.Reason, why.Owner, why.Ticket, why.Expiry.UTC().Format("2006-01-02"))})
	return zero
}

// accepted reports whether the operator named this seam in AcceptDarkEnv.
func (m *Manifest) accepted(s Seam) bool {
	if m.acceptor == nil {
		return false
	}
	for _, part := range strings.Split(m.acceptor(AcceptDarkEnv), ",") {
		if strings.TrimSpace(part) == string(s) {
			return true
		}
	}
	return false
}

// record keeps the WORST outcome seen for a seam. Two branches cannot both run at boot, but a future
// refactor that binds in one place and declares in another must not be able to launder a dark seam into
// a live one by ordering.
func (m *Manifest) record(s Seam, r record) {
	if prev, ok := m.recorded[s]; ok && prev.state < r.state {
		return
	}
	m.recorded[s] = r
}

// firstNilRequiredField reports the first field tagged `wiring:"required"` that is nil, walking one
// level into a struct (or a pointer to one). It is how a non-nil value with a hole in it is caught.
func firstNilRequiredField(rv reflect.Value) (string, bool) {
	for rv.Kind() == reflect.Ptr {
		if rv.IsNil() {
			return "", false // the outer nil check already owns this case
		}
		rv = rv.Elem()
	}
	if rv.Kind() != reflect.Struct {
		return "", false
	}
	t := rv.Type()
	for i := 0; i < t.NumField(); i++ {
		if t.Field(i).Tag.Get("wiring") != "required" {
			continue
		}
		f := rv.Field(i)
		// An unexported field is not addressable through reflection for reads of some kinds, but Kind and
		// IsNil are always safe, which is all this needs.
		switch f.Kind() {
		case reflect.Func, reflect.Interface, reflect.Ptr, reflect.Map, reflect.Slice, reflect.Chan:
			if f.IsNil() {
				return t.Field(i).Name, true
			}
		}
	}
	return "", false
}
