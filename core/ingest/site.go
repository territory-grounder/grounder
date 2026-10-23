package ingest

import (
	"sort"
	"strings"
)

// Site vocabulary — TG-456. The estate deploys into a CLOSED set of sites, and every STORED / STAMPED /
// JOINABLE site must speak ONE vocabulary: the deployment-key form (dc1, dc2), the same tokens
// TG_*_DEPLOYMENTS carry. Alert sources historically presented the site in several spellings —
// "NL"/"GR" (LibreNMS templates, pve-liveness config), "nl"/"gr" (Prometheus/k8s labels),
// "dc1"/"dc2" (NetBox site names / host-name prefixes) — so a site-based join silently missed
// rows whenever two spellings met (e.g. action_execution.site held both "NL" and "dc1"). CanonicalizeSite
// folds every known spelling to the deployment-key form at the ingest boundary (Normalize), so env.Site —
// and everything derived from it (the action_execution stamp via Prediction.Site, ObservedAlert.Site,
// suppression, session_triage) — carries the one canonical vocabulary. An unknown spelling passes through
// unchanged (trimmed + lowercased): the estate makes no site claim it cannot ground, and a fabricated
// mapping would be worse than an honest passthrough.
//
// The estate-DERIVED vocabulary (estate.Graph.SiteOf) is intentionally NOT folded here: it answers only from
// graph data and is compared exclusively against itself in the verifier's coincidental-cross-site filter, so
// its self-consistency is what matters there, not its spelling.

// declaredSites is the CLOSED set of sites this estate deploys into, in the canonical deployment-key form.
// It is the subset-guard authority: a stored / stamped site outside this set is a vocabulary bug.
var declaredSites = map[string]struct{}{
	"dc1": {},
	"dc2": {},
}

// IsDeclaredSite reports whether s is a canonical declared deployment site.
func IsDeclaredSite(s string) bool {
	_, ok := declaredSites[s]
	return ok
}

// DeclaredSites returns the declared deployment sites in canonical form, sorted (for messages/enumeration).
func DeclaredSites() []string {
	out := make([]string, 0, len(declaredSites))
	for s := range declaredSites {
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}

// CanonicalizeSite folds a raw site string to the canonical deployment-key form (dc1 / dc2). It is
// idempotent (a canonical input returns unchanged) and total (an unknown input returns its trimmed +
// lowercased self, never an error and never a guessed site). This is the SINGLE site-vocabulary authority;
// call it at every boundary that produces or stores a site.
func CanonicalizeSite(raw string) string {
	s := strings.ToLower(strings.TrimSpace(raw))
	if s == "" {
		return ""
	}
	// Strip separators so "dc1", "dc1" and "nllei" share one cluster key.
	key := strings.NewReplacer("-", "", "_", "", " ", "").Replace(s)
	switch {
	case key == "nl" || strings.HasPrefix(key, "nllei"):
		return "dc1"
	case key == "gr" || strings.HasPrefix(key, "grskg"):
		return "dc2"
	}
	return s
}
