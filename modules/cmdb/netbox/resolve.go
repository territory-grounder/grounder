package netbox

import (
	"encoding/json"
	"fmt"

	cmdb "github.com/territory-grounder/grounder/adapters/cmdb"
)

// endpointFor maps a tracker-agnostic entity kind onto its NetBox list endpoint.
func endpointFor(kind string) (string, bool) {
	switch kind {
	case "device":
		return "/api/dcim/devices/", true
	case "vm":
		return "/api/virtualization/virtual-machines/", true
	case "ip":
		return "/api/ipam/ip-addresses/", true
	case "vlan":
		return "/api/ipam/vlans/", true
	case "interface":
		return "/api/dcim/interfaces/", true
	default:
		return "", false
	}
}

// netboxObject is the subset of a NetBox object this module maps to an Entity. NetBox uses "name" for
// most objects, "address" for IP addresses, and always provides a "display" label.
type netboxObject struct {
	Name    string `json:"name"`
	Display string `json:"display"`
	Address string `json:"address"`
	Status  struct {
		Value string `json:"value"`
	} `json:"status"`
	Site struct {
		Name string `json:"name"`
	} `json:"site"`
}

// Change is one NetBox object-change record — an entry in the entity's changelog exposed to triage.
type Change struct {
	ID         int64  `json:"id"`
	Action     string `json:"action"` // "created" | "updated" | "deleted"
	Time       string `json:"time"`
	ChangedBy  string `json:"user_name"`
	ObjectRepr string `json:"object_repr"`
}

// toEntity maps a NetBox object response onto the canonical Entity, keyed by the id that was resolved.
func toEntity(kind, id string, body []byte) (cmdb.Entity, error) {
	var o netboxObject
	if err := json.Unmarshal(body, &o); err != nil {
		return cmdb.Entity{}, fmt.Errorf("netbox: malformed %s response: %w", kind, err)
	}
	name := o.Name
	if name == "" {
		name = o.Address
	}
	if name == "" {
		name = o.Display
	}
	attrs := map[string]string{}
	if o.Status.Value != "" {
		attrs["status"] = o.Status.Value
	}
	if o.Site.Name != "" {
		attrs["site"] = o.Site.Name
	}
	if o.Display != "" {
		attrs["display"] = o.Display
	}
	return cmdb.Entity{ID: id, Kind: kind, Name: name, Attributes: attrs}, nil
}
