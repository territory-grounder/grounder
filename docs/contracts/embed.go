// Package contracts embeds the GENERATED wire contracts so the server can serve the exact artifact
// the repo carries. Honesty is inherited, not asserted: `gencontracts -check` fails CI whenever the
// committed artifact drifts from the served route table (REQ-501b/INV-15), so what this package embeds
// is provably the surface the binary registers.
package contracts

import _ "embed"

//go:embed openapi.yaml
var OpenAPI []byte

//go:embed asyncapi.yaml
var AsyncAPI []byte
