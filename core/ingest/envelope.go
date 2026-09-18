// Package ingest is the boundary between the outside estate and the governed spine: it turns an
// untrusted provider alert into one canonical, per-field-validated IncidentEnvelope, runs the
// deterministic dedup → flap → burst → correlate chain in code BEFORE any model is spent, and
// publishes a triage.requested event keyed by external_ref.
//
// Provenance: [O] INV-04 (typed envelope, per-field grammar, reject-before-enqueue), spec/006 REQ-502 ·
// [R] paradigm-rule 1 (single-org; the correlation key is external_ref, ADR-0010) · [F] "deterministic
// code acts before any model" (the predecessor's pre-model suppression chain).
//
// Structural guarantee (INV-04): the unsanitized provider body lives ONLY in RawEvent behind an
// unexported field, consumed only by Normalize. IncidentEnvelope — the value every downstream stage
// receives — carries no raw bytes, so no later stage can compile against, or re-parse, the untrusted
// body. A missing or malformed identifier is a loud validation error, never a silently-empty field.
package ingest

import (
	"crypto/sha256"
	"encoding/hex"
	"net"
	"time"
)

// Severity is the normalized alert severity. The zero value is SeverityUnknown, which the validator
// rejects — an unrecognized severity is never silently treated as low.
type Severity int

const (
	SeverityUnknown  Severity = iota // zero value — INVALID; a raw event must map to a known severity
	SeverityInfo                     // informational, no action
	SeverityWarning                  // degraded, investigate
	SeverityCritical                 // service-affecting
)

func (s Severity) String() string {
	switch s {
	case SeverityInfo:
		return "info"
	case SeverityWarning:
		return "warning"
	case SeverityCritical:
		return "critical"
	default:
		return "unknown"
	}
}

// IncidentEnvelope is the single canonical, fully-validated representation of an incident crossing the
// boundary. Every field is a typed, grammar-checked value; it carries NO raw provider bytes. The
// correlation key is ExternalRef (unique within the org's own trackers, ADR-0010).
// LabelTransition / TransitionRecovery mark an envelope carrying a provider RECOVERY transition (a fault's
// alert went back UP) on the Labels map, so the front door can route it as clear-evidence rather than a fresh
// incident (spec/012 clear-confirm). A shared, exported home so both the producing ingester (e.g.
// modules/ingest/librenms) and the consuming front door (core/httpapi) key on the same string without either
// importing the other. Data-only, never control (INV-08).
const (
	LabelTransition    = "transition"
	TransitionRecovery = "recovery"
)

type IncidentEnvelope struct {
	ExternalRef string            // correlation key; join key for dedup/session/RAG/ledger
	SourceID    string            // the authenticated ingest source (e.g. "prometheus-dc1")
	AlertRule   string            // the provider rule name (validated slug)
	Severity    Severity          // exhaustive enum; SeverityUnknown is rejected
	Host        string            // RFC-1123 hostname (validated), or empty if not host-scoped
	IP          net.IP            // parsed IP (net.ParseIP), or nil if none supplied
	Site        string            // estate label (descriptive filter, NOT a security boundary — ADR-0010)
	Summary     string            // bounded human-readable summary (data, never interpolated as control)
	Labels      map[string]string // provider labels, keys/values length-bounded
	ObservedAt  time.Time         // provider event time (validated: not future-dated, not negative-age)
	ReceivedAt  time.Time         // when the platform received it
}

// RawEvent is the untrusted inbound claim produced by an ingest adapter from a provider payload. Its
// candidate fields are strings (unvalidated); the original payload bytes are held in an UNEXPORTED
// field so nothing past Normalize can read the unsanitized body (INV-04). Construct it with NewRawEvent.
type RawEvent struct {
	SourceID    string
	ExternalRef string
	AlertRule   string
	Severity    string // provider severity string, mapped by the validator
	Host        string
	IP          string
	Site        string
	Summary     string
	Labels      map[string]string
	ObservedAt  time.Time

	payload []byte // the raw provider body — unexported; never surfaced downstream
}

// NewRawEvent builds a RawEvent, retaining the original payload behind the unexported field. The
// payload is copied so the caller cannot mutate it after construction.
func NewRawEvent(sourceID string, payload []byte) RawEvent {
	cp := make([]byte, len(payload))
	copy(cp, payload)
	return RawEvent{SourceID: sourceID, payload: cp}
}

// DedupKey is the content-addressed identity used to collapse repeats: sha256 over the stable
// (source, rule, host, severity, summary) tuple. Two alerts with the same DedupKey inside the dedup
// window are the same alert. It is deliberately independent of timestamps and labels so a re-fire of
// the same condition dedups. [F] predecessor dedup(sha256 + line-count / window).
func (e IncidentEnvelope) DedupKey() string {
	h := sha256.New()
	// Length-prefixed writes so field boundaries cannot be forged by shifting bytes across fields.
	for _, f := range []string{e.SourceID, e.AlertRule, e.Host, e.Severity.String(), e.Summary} {
		var lenbuf [8]byte
		n := len(f)
		for i := 7; i >= 0; i-- {
			lenbuf[i] = byte(n)
			n >>= 8
		}
		h.Write(lenbuf[:])
		h.Write([]byte(f))
	}
	return hex.EncodeToString(h.Sum(nil))
}

// CorrelationKey is the key alerts are grouped by for correlation: the external_ref when present,
// otherwise the (site, host) pair. It is never a bare vendor id (ADR-0010).
func (e IncidentEnvelope) CorrelationKey() string {
	if e.ExternalRef != "" {
		return "ref:" + e.ExternalRef
	}
	return "host:" + e.Site + "/" + e.Host
}
