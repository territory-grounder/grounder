package cpconfig

import (
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"
)

// Write-side validation (task #27 Phase C, spec/006 REQ-523). The registry stays the single source of
// truth: a write is legal ONLY for a registered, console-writable, non-LAW key. The LAW refusal here is
// the WRITE-side half of the resolver clamp — a LAW key can neither be overridden at resolve time nor
// even recorded as an override. Validation runs TWICE by design: at the HTTP surface (fast, honest
// status codes) and again inside the worker's single-writer activity (the authority) — the surface can
// never be the only line.

var (
	// ErrUnknownKey refuses a write to a key absent from the compiled registry (404 at the surface).
	ErrUnknownKey = errors.New("cpconfig: unknown configuration key")
	// ErrLawPinned refuses a write to a LAW key — the clamp is the law (422 at the surface).
	ErrLawPinned = errors.New("cpconfig: key is pinned by law and can never be overridden")
	// ErrNotWritable refuses a write to a boot-only key (422 at the surface).
	ErrNotWritable = errors.New("cpconfig: key is not console-writable (boot-only)")
	// ErrValueBounds refuses an empty, oversized, or non-printable value (400 at the surface).
	ErrValueBounds = errors.New("cpconfig: value out of bounds (1..2048 chars, no control characters)")
	// ErrValueShape refuses a value the FIELD's own descriptor forbids — wrong form, too long, too many
	// entries (400 at the surface, TG-262). Distinct from ErrValueBounds: that one is the store's limit,
	// this one is the module's own declared contract.
	ErrValueShape = errors.New("cpconfig: value does not match the field's declared shape")
	// ErrNoOverride refuses a clear for a key that has no stored override (404 at the surface, TG-261).
	ErrNoOverride = errors.New("cpconfig: key has no stored override to clear")
)

// MaxValueLen bounds a console-written value.
const MaxValueLen = 2048

// Lookup resolves a registered key by name.
func Lookup(name string) (Key, bool) {
	for _, k := range Registry() {
		if k.Name == name {
			return k, true
		}
	}
	return Key{}, false
}

// ValidateClear is the CLEAR-legality check (TG-261): the same registry rules as a write, minus the
// value. Clearing is how an operator takes an override back — before this existed the route was POST-only
// and ValidateWrite refused an empty value, so a saved setting could be corrected but never REMOVED, and
// the only writer able to touch the row runs inside the worker that the row might be stopping from
// booting. A key that is not registered, is LAW, or is not console-writable cannot be cleared for exactly
// the reasons it could not be written.
func ValidateClear(name string) (Key, error) {
	k, ok := Lookup(name)
	if !ok {
		return Key{}, fmt.Errorf("%w: %q", ErrUnknownKey, name)
	}
	if k.Law {
		return Key{}, fmt.Errorf("%w: %q", ErrLawPinned, name)
	}
	if !k.ConsoleWritable {
		return Key{}, fmt.Errorf("%w: %q", ErrNotWritable, name)
	}
	return k, nil
}

// ValidateWrite is the one write-legality check: registered, non-LAW, console-writable, bounded value.
// It returns the registry Key so callers act on the compiled identity, never on client-supplied text.
func ValidateWrite(name, value string) (Key, error) {
	k, ok := Lookup(name)
	if !ok {
		return Key{}, fmt.Errorf("%w: %q", ErrUnknownKey, name)
	}
	if k.Law {
		return Key{}, fmt.Errorf("%w: %q", ErrLawPinned, name)
	}
	if !k.ConsoleWritable {
		return Key{}, fmt.Errorf("%w: %q", ErrNotWritable, name)
	}
	if value == "" || len(value) > MaxValueLen || !utf8.ValidString(value) {
		return Key{}, ErrValueBounds
	}
	for _, r := range value {
		if r < 0x20 || r == 0x7f {
			return Key{}, ErrValueBounds
		}
	}
	// THE FIELD'S OWN CONTRACT (TG-262). Checked last, so a value that fails the store's basic bounds is
	// still reported as a bounds error rather than a shape one.
	if why := shapeFault(k, value); why != "" {
		return Key{}, fmt.Errorf("%w: %s: %s%s", ErrValueShape, name, why, helpSuffix(k))
	}
	return k, nil
}

// helpSuffix appends the field's own explanation to a refusal, so the operator is told what IS allowed.
func helpSuffix(k Key) string {
	if h := strings.TrimSpace(k.Help); h != "" {
		return " — " + h
	}
	return ""
}

// shapeFault reports why value violates the field's declared shape, or "" when it is well formed.
//
// It is the SAME judgement the worker's resolver applies at boot (cmd/worker/boot_config.go), deliberately
// duplicated rather than shared: this package may not import modules/, and a value the console accepts
// must be one the worker will serve. The oracle that keeps them agreeing is the shared table of cases.
func shapeFault(k Key, value string) string {
	if k.MaxLen > 0 && len(value) > k.MaxLen {
		return fmt.Sprintf("%d bytes exceeds the field's %d-byte limit", len(value), k.MaxLen)
	}
	entries := []string{value}
	switch k.Type {
	case "idlist", "kvmap":
		entries = entries[:0]
		for _, e := range strings.Split(value, ",") {
			if e = strings.TrimSpace(e); e != "" {
				entries = append(entries, e)
			}
		}
		if k.MaxItems > 0 && len(entries) > k.MaxItems {
			return fmt.Sprintf("%d entries exceeds the field's limit of %d", len(entries), k.MaxItems)
		}
	}
	if k.Pattern != "" {
		re, err := regexp.Compile(k.Pattern)
		if err != nil {
			return "" // a descriptor's own broken pattern is a repo defect, not an operator error
		}
		for _, e := range entries {
			if !re.MatchString(e) {
				return fmt.Sprintf("%q does not match the field's required form", e)
			}
		}
	}
	switch k.Type {
	case "duration":
		if _, err := time.ParseDuration(strings.TrimSpace(value)); err != nil {
			return "not a duration (want forms like 30s, 5m, 2h)"
		}
	case "bool":
		switch strings.ToLower(strings.TrimSpace(value)) {
		case "1", "true", "yes", "on", "0", "false", "no", "off":
		default:
			return "not a boolean (want true/false)"
		}
	case "url":
		u, err := url.Parse(strings.TrimSpace(value))
		if err != nil || u.Scheme == "" || u.Host == "" {
			return "not an absolute URL (want scheme://host[:port])"
		}
	}
	return ""
}
