package groundnet

// Posture is a node's groundnet participation stance. Every capability is OFF in the zero value: a
// fresh node federates NOTHING until an org admin deliberately enables each one (REQ-2111). The
// capabilities are INDEPENDENT — in particular, consumption is never gated behind export or any
// contribution measure (REQ-2112), so a sensitive-estate operator that shares nothing still
// consumes freely and is never pressured to over-share.
type Posture struct {
	Member     bool // an authenticated groundnet member (org-admin enabled)
	Export     bool // Emit permitted — the node may publish wisdom
	Consume    bool // Ingest permitted — the node may consume wisdom
	PublicTier bool // participate in the public tier (provably zero-estate-specific distillate only)
}

// DefaultPosture is the born state: every capability off. A fresh node federates nothing until an
// org admin turns something on (REQ-2111).
func DefaultPosture() Posture { return Posture{} }

// MayEmit reports whether the node may publish wisdom: it requires membership AND the export toggle.
// This is the LAST point of control before a chunk leaves the estate (REQ-2114) — a caller MUST
// consult it before Emit.
func (p Posture) MayEmit() bool { return p.Member && p.Export }

// MayConsume reports whether the node may ingest wisdom. It depends ONLY on membership and the
// consume toggle — NEVER on Export, on how much the node has contributed, or on any ratio
// (REQ-2112). Consumption is never gated behind contribution.
func (p Posture) MayConsume() bool { return p.Member && p.Consume }

// MayUsePublicTier reports whether the node participates in the public tier. The public tier exists
// ONLY for provably zero-estate-specific distillate (REQ-2111); a caller must still prove a specific
// chunk is public-safe before publishing it publicly (the generalizable-by-type projection, A1, is
// that proof surface).
func (p Posture) MayUsePublicTier() bool { return p.Member && p.PublicTier }
