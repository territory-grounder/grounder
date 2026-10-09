package pack

// Availability is what a pack's declarations resolve to against the LIVE registries — the lazy
// not-installed guard, resolved per request rather than stored as a boolean that drifts. A pack whose
// capability is unregistered degrades to fewer reads and says so; it never errors a session and never
// invents an execution path (INV-17's ErrNoExecutionPath is the module-side half of this contract).
type Availability struct {
	// ToolsPresent / ToolsMissing partition the pack's Tools against the registered set. Missing names
	// are REPORTED (they ride the degraded-capabilities lane), never silently dropped.
	ToolsPresent []string
	ToolsMissing []string
	// TransportOK reports whether the declared vendor lane resolves to a registered execution path.
	TransportOK bool
	// Reason carries the not-installed explanation in operator words when TransportOK is false.
	Reason string
}

// Resolve checks a pack against the live world through two injected lookups, so core imports neither the
// agent package nor the module registry: hasTool is agent.ToolSet.Get in production; transportOK is the
// composition root's view of the module registry (modules.Registry.Resolve → ErrNoExecutionPath). A nil
// transportOK with a declared vendor lane fails CLOSED — "no transport resolver wired" is a
// not-installed state, not a pass.
func Resolve(p Pack, hasTool func(string) bool, transportOK func(VendorHint) (bool, string)) Availability {
	av := Availability{TransportOK: true}
	for _, name := range p.Tools {
		if hasTool != nil && hasTool(name) {
			av.ToolsPresent = append(av.ToolsPresent, name)
		} else {
			av.ToolsMissing = append(av.ToolsMissing, name)
		}
	}
	if p.VendorHint.zero() {
		return av
	}
	if transportOK == nil {
		av.TransportOK = false
		av.Reason = "no transport resolver wired for " + p.VendorHint.Transport
		return av
	}
	ok, reason := transportOK(p.VendorHint)
	av.TransportOK = ok
	if !ok {
		av.Reason = reason
		if av.Reason == "" {
			av.Reason = p.VendorHint.Transport + ": no execution path"
		}
	}
	return av
}
