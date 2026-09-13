package trace

import "strings"

// GeneralizableLayer is the de-identified, federation-shareable projection of a session
// (REQ-2017). By CONSTRUCTION it holds only KINDS, a verified outcome, and content-addressed
// artifact refs — it has NO field for a host, address, topology edge, credential identity,
// ticket id, raw transcript, rule id, or target string. The estate-specific layer of a
// SessionTrace (Host, ExternalRef, ActionID, PlanHash, and every per-step Rule / Reason /
// PlanOps target / credential ref) therefore has NO slot here: de-identification is a SCHEMA
// property enforced by the TYPE, not a scrubber that could be misconfigured.
//
// This is the "generalizable layer" half of the two-layer trace schema, and the input TYPE a
// FUTURE federated export (the spec/021 groundnet Emit seam, REQ-2101/2108) sources from — an
// exporter cannot read estate data because this type does not carry it. v1 shares nothing off
// the instance (REQ-2017): this type only makes the separation a schema property so that a
// future export needs no schema rewrite. core/trace does not import the federation contract;
// the contract imports this.
type GeneralizableLayer struct {
	AlertClass string        `json:"alert_class"`         // generalized alert category (a KIND), folded to a known class
	OpClass    string        `json:"op_class"`            // the CLASS of remediation op (a registered slug), never a command/target
	Reversible bool          `json:"reversible"`          // op-class governance property
	BlastClass string        `json:"blast_class"`         // op-class governance property
	Band       string        `json:"band"`                // the governance band (a KIND)
	Verdict    string        `json:"verdict"`             // the mechanical verified outcome (a KIND), never a raw trace
	Confidence float64        `json:"confidence"`          // the stated 0..1 confidence
	Artifacts  []ArtifactRef `json:"artifacts,omitempty"` // content-addressed graduated-artifact refs
}

// ArtifactRef is a content-addressed reference to a graduated artifact — a hash, never an
// estate URL or path.
type ArtifactRef struct {
	Kind string `json:"kind"` // runbook | skill | rubric
	Ref  string `json:"ref"`  // content-address hash (e.g. "sha256:…"), never a URL
}

// GeneralizableClasses carries the semantic classifications a caller has derived for a session
// from GENERALIZABLE sources — the op-class registry (opschema) and the alert taxonomy — plus
// the graduated-artifact refs. The projection FOLDS the class strings against the caller's
// allowlists so a mis-supplied or estate-influenced value cannot pass through: the estate-free
// guarantee holds even if a caller hands the projection a bad string.
type GeneralizableClasses struct {
	OpClass           string
	AlertClass        string
	Reversible        bool
	BlastClass        string
	Artifacts         []ArtifactRef
	KnownOpClasses    []string // the registered op-class slugs (opschema) — the allowlist for OpClass
	KnownAlertClasses []string // the alert taxonomy — the allowlist for AlertClass
	KnownBlastClasses []string // the blast-class governance vocabulary — the allowlist for BlastClass
}

// ClassOther is the fold target for a class outside its known set — the sessionspan
// containment discipline (core/sessionspan): an unrecognised (possibly estate-influenced)
// value loses fidelity to "other" rather than passing through.
const ClassOther = "other"

// ClassUnset is the fold target for an empty class — distinct from "other" so a missing
// classification reads as missing, not as an unrecognised value.
const ClassUnset = "unset"

// ProjectGeneralizable projects a SessionTrace into its GENERALIZABLE layer (REQ-2017). The
// trace's estate-specific fields have no slot in the output type, so the projection CANNOT
// emit them; the generalizable governance fields (band, verified verdict, confidence) are read
// from the trace, and the semantic classes are folded against the caller's allowlists.
func ProjectGeneralizable(t SessionTrace, c GeneralizableClasses) GeneralizableLayer {
	return GeneralizableLayer{
		AlertClass: foldClass(c.AlertClass, c.KnownAlertClasses),
		OpClass:    foldClass(c.OpClass, c.KnownOpClasses),
		Reversible: c.Reversible,
		BlastClass: foldClass(c.BlastClass, c.KnownBlastClasses),
		Band:       t.Band,
		Verdict:    t.Verdict,
		Confidence: t.Confidence,
		Artifacts:  sanitizeArtifacts(c.Artifacts),
	}
}

// foldClass returns v if it is in the known set, "unset" if empty, else "other" — the
// sessionspan.Build containment discipline. An EMPTY allowlist folds every non-empty value to
// "other": a caller that forgot the taxonomy loses fidelity, never containment.
func foldClass(v string, known []string) string {
	if v == "" {
		return ClassUnset
	}
	for _, k := range known {
		if v == k {
			return v
		}
	}
	return ClassOther
}

// sanitizeArtifacts keeps only refs that are content-addressed hashes ("<algo>:<hex>"). A ref
// that is a URL, a path, or anything else is DROPPED — an estate URL must never ride out as an
// "artifact ref" — and the kind is folded to the known set.
func sanitizeArtifacts(in []ArtifactRef) []ArtifactRef {
	if len(in) == 0 {
		return nil
	}
	out := make([]ArtifactRef, 0, len(in))
	for _, a := range in {
		if isContentAddress(a.Ref) {
			out = append(out, ArtifactRef{Kind: foldArtifactKind(a.Kind), Ref: a.Ref})
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// isContentAddress reports whether ref is a "<algo>:<hex>" content address (a hash), not a URL
// or path. It requires a known hash-algo prefix AND a hex body of EXACTLY that algorithm's
// digest length: without the length bound a hex-encoded estate string (a URL, a path) of
// arbitrary length would pass the charset check and decode back to readable text — a
// de-identification bypass. A genuine content hash is a fixed-width opaque digest, so
// "https://…", "/path/…", and a short or over-long hex body are all rejected.
func isContentAddress(ref string) bool {
	algo, body, ok := strings.Cut(ref, ":")
	if !ok {
		return false
	}
	var wantHex int
	switch algo {
	case "sha256":
		wantHex = 64
	case "sha384":
		wantHex = 96
	case "sha512":
		wantHex = 128
	default:
		return false
	}
	if len(body) != wantHex {
		return false
	}
	for _, r := range body {
		if !((r >= '0' && r <= '9') || (r >= 'a' && r <= 'f') || (r >= 'A' && r <= 'F')) {
			return false
		}
	}
	return true
}

// foldArtifactKind bounds the artifact kind to the known set (unknown -> "other").
func foldArtifactKind(k string) string {
	switch k {
	case "runbook", "skill", "rubric":
		return k
	default:
		return ClassOther
	}
}
