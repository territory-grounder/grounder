// Package cmdb is the stable interface for the CMDB surface: the authoritative entity-resolution source
// every ingested payload is re-read against before dispatch.
//
// Provenance: [O] INV-05 (the payload is a claim, the CMDB record is the fact — every ingest payload is
// re-read against its system of record by id before dispatch), spec/008. NetBox is the day-1 backend.
package cmdb

import "context"

// Entity is a resolved configuration item (device, VM, IP, VLAN, or interface) keyed by id.
type Entity struct {
	ID   string
	Kind string // "device" | "vm" | "ip" | "vlan" | "interface"
	Name string
	// Attributes carries resolved fields; it is data, never control flow.
	Attributes map[string]string
}

// CMDB resolves entities by id and is the target of the INV-05 re-read before dispatch.
type CMDB interface {
	// SourceType is the source/vendor slug (e.g. "netbox").
	SourceType() string
	// Resolve returns the authoritative entity of the given kind by id. A payload's claimed fields are
	// reconciled against this record before dispatch.
	Resolve(ctx context.Context, kind, id string) (Entity, error)
}
