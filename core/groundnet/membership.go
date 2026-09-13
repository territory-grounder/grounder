package groundnet

import (
	"errors"
	"fmt"
)

// Authority is the org-admin capability required to change a node's groundnet posture. Enabling ANY
// federation capability is an org-admin decision (REQ-2111); a caller without it cannot enable
// anything, so the default-off posture holds against every non-admin path. The principal is recorded
// in the governed audit record; it is an operator identity, never estate data.
type Authority struct {
	principal string
}

// OrgAdmin constructs an org-admin authority for the named principal. An empty principal is no
// authority.
func OrgAdmin(principal string) Authority { return Authority{principal: principal} }

func (a Authority) ok() bool { return a.principal != "" }

// Capability names a togglable federation capability.
type Capability string

const (
	CapMember  Capability = "member"
	CapExport  Capability = "export"
	CapConsume Capability = "consume"
	CapPublic  Capability = "public-tier"
)

// ErrNotOrgAdmin is returned when a posture change is attempted without org-admin authority.
var ErrNotOrgAdmin = errors.New("groundnet: a groundnet posture change requires org-admin authority (REQ-2111)")

// PostureChange is the governed audit record of a posture change (REQ-2111): which capability moved,
// to what, by which org-admin principal, when. A caller writes it to the append-only ledger; it
// carries no estate data.
type PostureChange struct {
	Capability Capability `json:"capability"`
	Enabled    bool       `json:"enabled"`
	Principal  string     `json:"principal"`
	At         int64      `json:"at"` // unix seconds
}

// SetCapability enables or disables one federation capability, requiring org-admin authority and
// returning the resulting posture plus the governed audit record (REQ-2111). WITHOUT authority it
// refuses with ErrNotOrgAdmin and leaves the posture unchanged — default-off cannot be lifted except
// by a deliberate, audited org-admin decision.
func SetCapability(p Posture, cap Capability, enabled bool, auth Authority, at int64) (Posture, PostureChange, error) {
	if !auth.ok() {
		return p, PostureChange{}, ErrNotOrgAdmin
	}
	switch cap {
	case CapMember:
		p.Member = enabled
	case CapExport:
		p.Export = enabled
	case CapConsume:
		p.Consume = enabled
	case CapPublic:
		p.PublicTier = enabled
	default:
		return p, PostureChange{}, fmt.Errorf("groundnet: unknown capability %q", cap)
	}
	return p, PostureChange{Capability: cap, Enabled: enabled, Principal: auth.principal, At: at}, nil
}

// UnrecallableNotice is the acknowledgement an org admin is shown at opt-in (REQ-2114): a shared
// chunk cannot be recalled, so the export decision is the final one.
const UnrecallableNotice = "a shared chunk is UNRECALLABLE once emitted — the export decision is the last point of control; there is no delete after export (REQ-2114)"

// GovernedRecord marks an emitted chunk as an unrecallable governed record declaring its retention
// and provenance (REQ-2114 / INV-14). It is stamped at emit time (the last point of control) and
// carries no estate data — Subject is the content-address and ReceiptRef the provenance anchor.
type GovernedRecord struct {
	Subject      string `json:"subject"`       // the chunk content-address
	Unrecallable bool   `json:"unrecallable"`  // always true — declares no-delete-after-export
	Retention    string `json:"retention"`     // the retention class of this governed record
	ReceiptRef   string `json:"receipt_ref"`   // the provenance anchor (the Receipt's subject)
}

// NewGovernedRecord stamps the unrecallable governed record for an emitted chunk (REQ-2114).
func NewGovernedRecord(subject, retention, receiptRef string) GovernedRecord {
	return GovernedRecord{Subject: subject, Unrecallable: true, Retention: retention, ReceiptRef: receiptRef}
}
