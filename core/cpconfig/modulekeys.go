package cpconfig

import (
	"sort"
	"strings"
	"sync/atomic"
)

// MODULE CONFIGURATION KEYS — supplied by the modules layer, never imported by it.
//
// Registry() is the compiled control-plane registry: 11 keys, 4 console-writable, and ZERO module keys.
// So a console write of a connector setting is rejected as an unknown key, and even if it were accepted
// cmd/worker referenced this package zero times — the write was inert in both directions.
//
// The fix cannot be "import the module catalog here". Nothing in core/ imports modules/ today, and
// inverting that layering to fetch a list of strings would make the safety core depend on the connector
// fleet. Instead the composition root PUSHES the module keys in at boot, derived from each module's
// published descriptor. core stays a leaf; modules depend on core, as they already do.
//
// WHY AN ATOMIC RATHER THAN A PLAIN SLICE. Registry() is read by HTTP handlers on request goroutines
// while the composition root is still assembling. A plain package var would be a data race that shows up
// as a torn read under load and never in a test.
var moduleKeys atomic.Pointer[[]Key]

// ModuleKeyPrefix is the namespace every module key lives under. It is checked rather than assumed: a
// module descriptor that claimed "safety.may_actuate" would otherwise put a law-pinned governance
// control behind a connector's settings dialog.
const ModuleKeyPrefix = "module."

// SetModuleKeys installs the module-supplied configuration keys.
//
// Keys not under ModuleKeyPrefix are DROPPED rather than rejected loudly, because this runs at boot and a
// malformed descriptor must not be able to prevent the worker from starting — the descriptor validator
// (modules/desc) is where that is a hard failure, and deploy/module_descriptor_test.go makes it a test
// failure. Here the posture is: a key that does not belong in this namespace simply does not exist.
func SetModuleKeys(keys []Key) {
	clean := make([]Key, 0, len(keys))
	seen := map[string]bool{}
	for _, k := range keys {
		if !strings.HasPrefix(k.Name, ModuleKeyPrefix) || seen[k.Name] {
			continue
		}
		// A module key can never be law-pinned: law lives in the compiled registry, and a module that
		// could declare its own law would be declaring itself exempt from review.
		k.Law = false
		seen[k.Name] = true
		clean = append(clean, k)
	}
	sort.Slice(clean, func(i, j int) bool { return clean[i].Name < clean[j].Name })
	moduleKeys.Store(&clean)
}

// ModuleKeys returns the installed module keys (empty before the composition root sets them).
func ModuleKeys() []Key {
	if p := moduleKeys.Load(); p != nil {
		return *p
	}
	return nil
}
