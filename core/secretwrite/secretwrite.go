// Package secretwrite is the ONE path by which an operator's submitted secret reaches the secret backend.
//
// It exists to make three things structural rather than remembered.
//
//  1. THE PATH IS RESOLVED SERVER-SIDE, NEVER SUBMITTED. A request names a module (surface + source type),
//     not a location. The KV path comes from that module's published descriptor. A client that could name
//     the path could overwrite any secret the writer's credential can reach — including another module's,
//     or the platform's own — and the credential is necessarily allowed to write somewhere.
//
//  2. THE VALUE NEVER TOUCHES POSTGRES, A LEDGER ROW, OR A TEMPORAL HISTORY. The config store is a
//     database table and its writes are ledgered; a Temporal workflow argument is durably recorded in
//     history and replayed. TG's existing config write goes through Temporal for exactly the right reasons
//     (single writer, ledger-before-commit) and is therefore the WRONG carrier for a secret. This path is
//     deliberately direct: submit -> validate -> backend, with an audit record that names the act and
//     never the material.
//
//  3. THE RESULT SAYS WHAT HAPPENED WITHOUT SAYING WHAT WAS WRITTEN. An operator needs to know the write
//     landed, at which path, and when. They never need the value back, and no read path returns it.
package secretwrite

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"
)

// MaxSecretLen bounds a submitted secret. Generous enough for a JWT or a PEM-ish token, small enough that
// a paste accident is refused rather than stored.
const MaxSecretLen = 8192

var (
	// ErrUnknownModule — no descriptor names this module, so no path can be resolved for it.
	ErrUnknownModule = errors.New("secretwrite: unknown module")
	// ErrNoSecretLane — the module declares no secret, so there is nothing to write.
	ErrNoSecretLane = errors.New("secretwrite: module declares no secret lane")
	// ErrValueBounds — empty, oversized, or not clean UTF-8.
	ErrValueBounds = errors.New("secretwrite: value out of bounds")
)

// Lane is the resolved destination for one module's secret: where it goes and under which key.
type Lane struct {
	Surface    string
	SourceType string
	KVPath     string // e.g. "secret/data/tg/matrix"
	Field      string // e.g. "token"
}

// LaneResolver answers "where does this module's secret live", from the module's own descriptor.
//
// It is an interface so this package stays free of the modules layer: core does not import modules
// anywhere in TG, and a secret writer is the last place to start.
type LaneResolver interface {
	Lane(surface, sourceType string) (Lane, error)
}

// Backend is the secret store. Implemented by the OpenBao/Vault client.
type Backend interface {
	WriteKV(ctx context.Context, apiPath string, data map[string]string) error
}

// Outcome is what the operator is told. It carries no secret material by construction — there is no field
// it could travel in.
type Outcome struct {
	Surface    string `json:"surface"`
	SourceType string `json:"source_type"`
	KVPath     string `json:"kv_path"`
	Field      string `json:"field"`
}

// Writer performs the validated write.
type Writer struct {
	Lanes   LaneResolver
	Backend Backend
}

// Write validates and stores one module secret.
//
// The value is the LAST argument and is never logged, wrapped into an error, or returned. Errors name the
// module and the bound, never the material — an error string is the most commonly copied text in an
// incident and must be safe to paste.
func (w Writer) Write(ctx context.Context, surface, sourceType, value string) (Outcome, error) {
	if w.Lanes == nil || w.Backend == nil {
		return Outcome{}, errors.New("secretwrite: not wired")
	}
	lane, err := w.Lanes.Lane(surface, sourceType)
	if err != nil {
		return Outcome{}, err
	}
	if lane.KVPath == "" || lane.Field == "" {
		return Outcome{}, fmt.Errorf("%w: %s/%s", ErrNoSecretLane, surface, sourceType)
	}
	if err := validateValue(value); err != nil {
		return Outcome{}, err
	}
	if err := w.Backend.WriteKV(ctx, lane.KVPath, map[string]string{lane.Field: value}); err != nil {
		// The backend error may name the path; it must never name the value, and nothing here adds it.
		return Outcome{}, fmt.Errorf("secretwrite: %s/%s: %w", surface, sourceType, err)
	}
	return Outcome{Surface: lane.Surface, SourceType: lane.SourceType, KVPath: lane.KVPath, Field: lane.Field}, nil
}

// validateValue bounds the submission. A secret is opaque, so the only honest checks are size and
// encoding — anything cleverer would be guessing at a format and rejecting a legitimate credential.
//
// Control characters are refused because they are almost always a copy-paste artefact (a trailing newline
// from a terminal, a smuggled CR) and a token silently stored with one appended fails authentication in a
// way that looks like a wrong secret rather than a mangled one.
func validateValue(v string) error {
	if v == "" || len(v) > MaxSecretLen || !utf8.ValidString(v) {
		return ErrValueBounds
	}
	if strings.TrimSpace(v) != v {
		return fmt.Errorf("%w: leading or trailing whitespace (a paste artefact that would fail "+
			"authentication as though the secret were wrong)", ErrValueBounds)
	}
	for _, r := range v {
		if r < 0x20 || r == 0x7f {
			return fmt.Errorf("%w: control character", ErrValueBounds)
		}
	}
	return nil
}
