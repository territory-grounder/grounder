package credential

// RESOLUTION PURPOSE (TG-687, found live 2026-11-08). Source precedence (REQ-1609) answers "which system of record
// wins for this host" — and it answered the same way for an estate-mutating use and for a read-only investigation.
// On NL hosts carrying a per-host OpenBao binding (precedence 10) TG's diagnostic and journal READS therefore
// authenticated with the estate ADMIN key instead of its own guarded read-only key (the native hostdiag allowlist,
// precedence 100, pinned behind tg-readonly-guard by TG-280): the read-only guard was bypassed, and TG's own reads
// appeared in the host's journal as the shared admin key — a sanctioned human — so attribution would have read
// TG's own investigation as a person's deliberate action on any non-carve-out host.
//
// A caller now names the purpose. For PurposeDiagRead, a source that declares itself TG's guarded READ-ONLY
// identity (DiagReadSource) wins whenever it covers the target; every other matching source is recorded as
// shadowed. When no such source covers the target, ordinary precedence decides exactly as before — this adds no
// refusal anywhere, it only stops an admin binding from out-ranking the read-only identity for a read.

// Purpose names WHY a resolution is asked for. It never widens what a caller may obtain; it only selects among
// identities that already cover the target.
type Purpose string

const (
	// PurposeDefault is every resolution that names no purpose: operator-declared source precedence decides.
	PurposeDefault Purpose = ""
	// PurposeDiagRead is a read-only investigation read (host diagnostics, journal / audit actor evidence,
	// estate discovery): TG's guarded read-only identity is preferred over any higher-precedence source.
	PurposeDiagRead Purpose = "diag-read"
)

// DiagReadSource is the OPTIONAL capability a CredentialSource declares when every identity it yields is TG's own
// guarded READ-ONLY investigation key (the native hostdiag allowlist, whose key the host pins behind
// tg-readonly-guard). It is asserted structurally on the registered source, so a source that does not implement it
// is simply not preferred for diagnostic reads.
type DiagReadSource interface {
	ServesDiagRead() bool
}
