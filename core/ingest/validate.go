package ingest

import (
	"errors"
	"fmt"
	"net"
	"regexp"
	"strings"
	"time"
)

// Field bounds. These are grammar, not policy: an identifier longer than this is malformed, not
// merely large, and is rejected at the boundary rather than truncated (which would corrupt joins).
const (
	maxExternalRefLen = 256
	maxAlertRuleLen   = 200
	maxHostLen        = 253 // RFC 1035 max FQDN length
	maxSummaryLen     = 4096
	maxSiteLen        = 64
	maxLabelKeyLen    = 128
	maxLabelValLen    = 1024
	maxLabels         = 64
	// clockSkew bounds how far in the future a provider timestamp may be before it is rejected as
	// malformed (defends the dedup window against future-dated entries — BEH-6 REQ note).
	clockSkew = 5 * time.Minute
	// maxAge bounds how old an observed timestamp may be; older is treated as a malformed replay.
	maxAge = 30 * 24 * time.Hour
)

var (
	// hostnameRe is an RFC-1123 hostname/FQDN: labels of [a-z0-9-] not starting/ending with '-',
	// joined by dots. Case-insensitive; empty host is allowed (validated separately as "not host-scoped").
	hostnameRe = regexp.MustCompile(`^(?i)([a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?)(\.[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?)*$`)
	// slugRe bounds alert-rule / external-ref / site to a printable, join-safe charset (no control
	// chars, whitespace, or shell/SQL metacharacters — those are structurally impossible downstream
	// anyway, but a malformed identifier is rejected here so it never reaches a join or a log line).
	slugRe = regexp.MustCompile(`^[A-Za-z0-9._:@/+-]+$`)
)

// Validation errors are sentinel-wrapped so callers (and the reject-before-enqueue path) can branch
// on the class without string matching.
var (
	ErrMissingField   = errors.New("ingest: required field missing")
	ErrBadSeverity    = errors.New("ingest: unrecognized severity")
	ErrBadHostname    = errors.New("ingest: malformed hostname")
	ErrBadIP          = errors.New("ingest: malformed IP")
	ErrBadExternalRef = errors.New("ingest: malformed external_ref")
	ErrBadAlertRule   = errors.New("ingest: malformed alert_rule")
	ErrTooLong        = errors.New("ingest: field exceeds bound")
	ErrBadTimestamp   = errors.New("ingest: malformed or out-of-window timestamp")
)

// parseSeverity maps a provider severity string to the typed enum via an exhaustive switch. An
// unrecognized value is rejected (never defaulted to a low severity). Common provider synonyms are
// folded; the set is closed.
func parseSeverity(s string) (Severity, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "critical", "crit", "page", "emergency", "fatal", "p1":
		return SeverityCritical, nil
	case "warning", "warn", "high", "p2":
		return SeverityWarning, nil
	case "info", "information", "informational", "low", "none", "notice", "p3", "p4":
		return SeverityInfo, nil
	default:
		return SeverityUnknown, fmt.Errorf("%w: %q", ErrBadSeverity, s)
	}
}

// validateHost checks an optional RFC-1123 hostname. Empty is valid (not host-scoped).
func validateHost(h string) error {
	if h == "" {
		return nil
	}
	if len(h) > maxHostLen {
		return fmt.Errorf("%w: hostname %d > %d", ErrTooLong, len(h), maxHostLen)
	}
	if !hostnameRe.MatchString(h) {
		return fmt.Errorf("%w: %q", ErrBadHostname, h)
	}
	return nil
}

// validateSlug checks an identifier against the join-safe grammar with a length bound.
func validateSlug(field, v string, max int, badErr error) error {
	if len(v) > max {
		return fmt.Errorf("%w: %s %d > %d", ErrTooLong, field, len(v), max)
	}
	if !slugRe.MatchString(v) {
		return fmt.Errorf("%w: %q", badErr, v)
	}
	return nil
}

// validateTimestamp rejects a future-dated (beyond clock skew) or absurdly old observed time. now is a
// parameter so the check is deterministic under test.
func validateTimestamp(observed, now time.Time) error {
	if observed.IsZero() {
		return fmt.Errorf("%w: observed_at is zero", ErrMissingField)
	}
	if observed.After(now.Add(clockSkew)) {
		return fmt.Errorf("%w: observed_at %s is future-dated", ErrBadTimestamp, observed.UTC())
	}
	if observed.Before(now.Add(-maxAge)) {
		return fmt.Errorf("%w: observed_at %s is older than %s", ErrBadTimestamp, observed.UTC(), maxAge)
	}
	return nil
}

// validateIP parses an optional IP; empty is valid (none supplied).
func validateIP(s string) (net.IP, error) {
	if s == "" {
		return nil, nil
	}
	ip := net.ParseIP(s)
	if ip == nil {
		return nil, fmt.Errorf("%w: %q", ErrBadIP, s)
	}
	return ip, nil
}

// validateLabels bounds the label map cardinality and key/value lengths.
func validateLabels(in map[string]string) (map[string]string, error) {
	if len(in) > maxLabels {
		return nil, fmt.Errorf("%w: %d labels > %d", ErrTooLong, len(in), maxLabels)
	}
	out := make(map[string]string, len(in))
	for k, v := range in {
		if len(k) > maxLabelKeyLen || len(v) > maxLabelValLen {
			return nil, fmt.Errorf("%w: label %q", ErrTooLong, k)
		}
		out[k] = v
	}
	return out, nil
}
