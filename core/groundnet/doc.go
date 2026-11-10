// Package groundnet implements TG's binding to the canonical groundnet federation
// contract (spec/021-groundnet): the SCITT RFC 9943 wire envelope, the de-identified
// wisdom payload, and the pseudonymous producer identity — plus, in sibling files as the
// contract is built out, the local transparency Receipt, reputation rollup, and the
// subordinate-not-authority Emit/Ingest seam.
//
// The wire envelope is NOT defined here. It is the canonical groundnet SCITT profile
// (products/ground-net/spec, groundnet.net), which is AUTHORITATIVE; this package conforms
// to it and adds only TG's implementation bindings (REQ-2100). A wisdom unit is a SCITT
// Transparent Statement: a COSE_Sign1 Signed Statement whose protected header carries a
// pseudonymous Issuer (iss), a content-addressed subject (sub), an issuance time (iat), a
// key id (kid), and the payload media type (content_type), augmented with a Transparency
// Service Receipt in the unprotected header.
//
// This package is DORMANT by default and never reaches an actuator. An emitted statement
// is a de-identified, generalizable-only distillate whose estate-specific layer has no
// export path in the contract (REQ-2101). An ingested statement is a subordinate HINT that
// re-graduates against local traffic and local verified outcomes before it earns any
// standing (REQ-2109/2110); it never lifts the constitutional never-auto floor (INV-09),
// the actuation interceptor / mutation keystone (INV-21), or the mode chokepoint. The
// seam is opt-in, default-off (REQ-2111).
package groundnet
