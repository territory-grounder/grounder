package reconcile

// rank orders resolution types best-first so a per-incident rollup keeps the best outcome an incident
// ever reached. auto_resolved (fully autonomous success) is the best; deferred (still open) the worst.
func rank(r ResolutionType) int {
	switch r {
	case ResAutoResolved:
		return 4
	case ResHumanResolved:
		return 3
	case ResEscalated:
		return 2
	default: // ResDeferred / unknown
		return 1
	}
}

// BestOutcomeRollup records the BEST outcome per incident (keyed by external_ref), so an alert storm —
// many events for one incident — cannot inflate the auto-resolve denominator. It counts incidents, not
// events (REQ-205). [R] single-org (org-global rollup, ADR-0010).
type BestOutcomeRollup struct {
	byIncident map[string]ResolutionType
}

// NewBestOutcomeRollup returns an empty rollup.
func NewBestOutcomeRollup() *BestOutcomeRollup {
	return &BestOutcomeRollup{byIncident: map[string]ResolutionType{}}
}

// Record folds one event's outcome into its incident's best-so-far. Recording the same incident many
// times leaves the incident count at one.
func (r *BestOutcomeRollup) Record(externalRef string, res ResolutionType) {
	if cur, ok := r.byIncident[externalRef]; !ok || rank(res) > rank(cur) {
		r.byIncident[externalRef] = res
	}
}

// IncidentCount is the number of distinct incidents (the auto-resolve DENOMINATOR).
func (r *BestOutcomeRollup) IncidentCount() int { return len(r.byIncident) }

// AutoResolvedCount is the number of distinct incidents whose best outcome was auto_resolved.
func (r *BestOutcomeRollup) AutoResolvedCount() int {
	n := 0
	for _, res := range r.byIncident {
		if res == ResAutoResolved {
			n++
		}
	}
	return n
}

// Best returns the recorded best outcome for an incident.
func (r *BestOutcomeRollup) Best(externalRef string) (ResolutionType, bool) {
	res, ok := r.byIncident[externalRef]
	return res, ok
}
