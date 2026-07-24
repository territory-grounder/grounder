package schedule

import (
	"strings"
	"time"
)

// WindowKind classifies a derived time window. The zero value is KindUnspecified, which NEVER sanctions an
// actuation — an unclassified window is treated as no window at all (fail closed, consistent with the
// mechanical-core convention that a zero enum is the most restrictive option).
type WindowKind uint8

const (
	// KindUnspecified is the zero value: not a recognised window, sanctions nothing.
	KindUnspecified WindowKind = iota
	// KindMaintenance is a sanctioned change window — actuation is time-sanctioned while now is inside it.
	KindMaintenance
	// KindFreeze is a change-freeze — actuation is NOT sanctioned while now is inside it, and a freeze
	// OVERRIDES an overlapping maintenance window (deny-overrides).
	KindFreeze
)

// String renders the kind for logs/reasons.
func (k WindowKind) String() string {
	switch k {
	case KindMaintenance:
		return "maintenance"
	case KindFreeze:
		return "freeze"
	default:
		return "unspecified"
	}
}

// Directive is the operator's intent parsed from a scheduler event's free text (its notes, falling back to
// its title). TG does not require a scheduler with a native "maintenance window" type; instead the operator
// TAGS an ordinary scheduler event with a compact, vendor-neutral directive and TG derives the window from
// the event's own recurrence + timezone. The grammar is whitespace/comma-separated `key=value` tokens:
//
//	tg-window=maintenance   -> a sanctioned change window (a bare `tg-window` also means maintenance)
//	tg-window=freeze        -> a change-freeze window (overrides maintenance)
//	tg-duration=90m         -> window length as a Go duration (the recurrence gives the START; END = START+duration)
//	tg-target=dc1*      -> the estate host/glob the window scopes to (else the event's own target is used)
//
// Keys are case-insensitive. A text with no `tg-window` token yields Present=false — an untagged event is a
// plain scheduled job, never a window.
type Directive struct {
	Present  bool          // a tg-window token was found
	Kind     WindowKind    // KindMaintenance or KindFreeze when Present
	Duration time.Duration // 0 when tg-duration is absent/invalid (the connector applies its default)
	Target   string        // "" when tg-target is absent (the connector falls back to the event target)
}

// tokenizer splits directive text on whitespace and commas (both are token separators).
func splitDirectiveTokens(text string) []string {
	return strings.FieldsFunc(text, func(r rune) bool {
		return r == ' ' || r == '\t' || r == '\n' || r == '\r' || r == ','
	})
}

// ParseDirective extracts a Directive from free text. It is total (never errors): an unparseable duration is
// left as 0 (the connector applies its configured default), and text with no tg-window token yields
// Present=false. The LAST occurrence of a repeated key wins.
func ParseDirective(text string) Directive {
	var d Directive
	for _, tok := range splitDirectiveTokens(text) {
		key, val, hasEq := strings.Cut(tok, "=")
		key = strings.ToLower(strings.TrimSpace(key))
		val = strings.TrimSpace(val)
		switch key {
		case "tg-window":
			d.Present = true
			switch strings.ToLower(val) {
			case "freeze":
				d.Kind = KindFreeze
			case "", "maintenance", "maint":
				d.Kind = KindMaintenance
			default:
				// an unrecognised kind is fail-closed: mark present but leave Kind unspecified so it
				// sanctions nothing (the connector skips-with-record rather than guessing).
				d.Kind = KindUnspecified
			}
		case "tg-duration":
			if hasEq {
				if pd, err := time.ParseDuration(val); err == nil && pd > 0 {
					d.Duration = pd
				}
			}
		case "tg-target":
			if hasEq {
				d.Target = val
			}
		}
	}
	return d
}
