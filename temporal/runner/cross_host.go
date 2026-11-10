package runner

import (
	"fmt"
	"strings"

	"github.com/territory-grounder/grounder/core/actuate"
	"github.com/territory-grounder/grounder/core/manifest"
)

// actionOnOtherHost reports whether the action would change a host other than the incident's subject (TG-701). Hosts
// are compared by RESOURCE (TG-664), so the same guest spelled by its node + params.guest, by its FQDN or in another
// case is one host, and only a genuinely different guest or host counts. An empty subject (an in-flight session from
// before the field existed, or a caller with no incident) or an action naming no target abstains: the other gates
// judge those.
func actionOnOtherHost(subject, target, opClass string, params map[string]string) (bool, string) {
	if strings.TrimSpace(subject) == "" {
		return false, ""
	}
	path := actuate.ResourcePath(manifest.Action{OpClass: opClass, Target: target, Params: params})
	if path == actuate.ResourceKeyNoTarget {
		return false, ""
	}
	acted := actuate.ResourceHost(path)
	alerted := actuate.ResourceHost(actuate.ResourcePath(manifest.Action{Target: subject}))
	if acted == alerted {
		return false, ""
	}
	return true, fmt.Sprintf("incident subject %q, action resource host %q", alerted, acted)
}
