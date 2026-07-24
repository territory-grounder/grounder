package cpconfig

import (
	"errors"
	"fmt"
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
	return k, nil
}
