// This file is the typed, bounded `extra_vars` contract of the AWX-job effect (spec/017 T-017-3, REQ-1705,
// TG-110). It is the argv-only chokepoint's analogue for AWX: the effect of a launch is a fixed job-template
// id PLUS typed variables validated against an operator-declared per-template schema — never a free-form
// command string. A job template is NOT a shell escape: the model cannot smuggle a command through
// `extra_vars` because every key must be DECLARED in the template's schema and carry the DECLARED type, and
// an undeclared key is rejected (INV-02 analogue).
//
// The schema is the operator-declared CLOSED allowed set for a template: every key the launch supplies MUST
// appear in the schema and MUST match its declared primitive type. A schema key the launch omits is fine (an
// AWX template supplies its own defaults) — the guarantee is that nothing UNDECLARED is ever passed, not that
// everything declared is present. There is no free-form / passthrough / "extra" bucket by construction.
//
// Provenance: [O] INV-02/INV-06, spec/017 (REQ-1705), TG-110.
package awxjob

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
)

// VarType is one of the closed set of primitive `extra_vars` value types an operator may declare for a
// template variable. There is deliberately NO "any" / free-form type — a variable is one of these three
// primitives or it is not a legal declaration.
type VarType string

const (
	// VarString is a string-valued extra_var.
	VarString VarType = "string"
	// VarNumber is a numeric extra_var (JSON number; decoded as float64 across the wire).
	VarNumber VarType = "number"
	// VarBool is a boolean extra_var.
	VarBool VarType = "bool"
)

// Valid reports whether t is one of the three sanctioned primitive types (an unknown/empty type is not a
// legal schema declaration — fail closed at validation).
func (t VarType) Valid() bool {
	switch t {
	case VarString, VarNumber, VarBool:
		return true
	default:
		return false
	}
}

// ExtraVarsSchema is an operator-declared per-template variable schema: the CLOSED set of allowed
// `extra_vars` keys mapped to their declared primitive type. An empty schema declares that the template
// accepts NO launch-time variables (a launch that supplies any extra_var is then rejected).
type ExtraVarsSchema map[string]VarType

var (
	// ErrUnknownExtraVar is returned when a launch supplies an `extra_vars` key absent from the template's
	// declared schema — the closed-set guarantee that a variable cannot smuggle an undeclared field (REQ-1705).
	ErrUnknownExtraVar = errors.New("awxjob: extra_var key is not declared in the template schema (rejected)")
	// ErrExtraVarType is returned when a declared key's value does not match its declared primitive type.
	ErrExtraVarType = errors.New("awxjob: extra_var value does not match its declared type")
	// ErrBadSchema is returned when a template's declared schema itself contains an illegal (non-primitive) type.
	ErrBadSchema = errors.New("awxjob: extra_vars schema declares an illegal variable type")
)

// Validate asserts a launch's requested `extra_vars` conforms to the template's declared schema (REQ-1705):
//   - the schema itself must declare only legal primitive types (an illegal declaration fails closed);
//   - every supplied key MUST be declared in the schema — an undeclared key is rejected (no free-form field);
//   - every supplied value MUST match its declared type.
//
// Numbers arrive as float64 after a JSON round-trip, so VarNumber accepts float64 (and the integer kinds /
// json.Number for a programmatically-built map). It returns nil when the requested vars are a well-typed
// subset of the schema; otherwise a typed, secret-free error naming the offending key.
func (s ExtraVarsSchema) Validate(vars map[string]any) error {
	for k, t := range s {
		if !t.Valid() {
			return fmt.Errorf("%w: %q declared as %q", ErrBadSchema, k, t)
		}
	}
	// Deterministic key order so a rejection message is stable across runs.
	keys := make([]string, 0, len(vars))
	for k := range vars {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		want, ok := s[k]
		if !ok {
			return fmt.Errorf("%w: %q", ErrUnknownExtraVar, k)
		}
		if !want.matches(vars[k]) {
			return fmt.Errorf("%w: %q wants %s", ErrExtraVarType, k, want)
		}
	}
	return nil
}

// matches reports whether v is a legal value for the declared type. Numbers tolerate the several Go shapes a
// JSON number or a programmatic literal can take (float64 from json.Unmarshal, the int kinds, json.Number).
func (t VarType) matches(v any) bool {
	switch t {
	case VarString:
		_, ok := v.(string)
		return ok
	case VarBool:
		_, ok := v.(bool)
		return ok
	case VarNumber:
		switch n := v.(type) {
		case float64, float32, int, int8, int16, int32, int64, uint, uint8, uint16, uint32, uint64:
			return true
		case json.Number:
			return strings.TrimSpace(n.String()) != ""
		default:
			return false
		}
	default:
		return false
	}
}
