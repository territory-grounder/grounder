package risk

// hasBoundEvidence reports whether at least one evidence ref is admissible on all four axes
// (captured, successful, recent, target-relevant). The silent_cognition_guard uses this to strip an
// [AUTO-RESOLVE] whose response cites no bound orchestrator-captured ToolResult — a bare code fence or
// agent free-text is never sufficient (REQ-008, INV-11).
func hasBoundEvidence(refs []EvidenceRef) bool {
	for _, e := range refs {
		if e.Bound() {
			return true
		}
	}
	return false
}
