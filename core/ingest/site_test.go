package ingest

import "testing"

// TG-456 — THE SUBSET GUARD (the killing test). Every raw site spelling the actuation/ingest path can present
// must, after the real production Normalize boundary, be a member of the CLOSED declared-deployment-site set
// {dc1, dc2}. Before the boundary canonicalized (env.Site = raw.Site verbatim), a raw "NL" was stored
// as "NL" — NOT a declared site — so a site-scoped join against a "dc1" row silently missed it. This test
// drives the REAL Normalize, so it goes RED on the pre-normalization code and GREEN once the boundary folds.
func TestActuationSiteVocabularyIsSubsetOfDeclaredSites(t *testing.T) {
	// Every spelling this estate's alert sources have produced: LibreNMS "NL"/"GR", pve-liveness
	// TG_PVE_LIVENESS_SITE, Prometheus/k8s "nl"/"gr" labels, NetBox "dc1"/"dc2" — plus the canonical
	// form itself (idempotence) and whitespace/case noise.
	// (whitespace/empty are rejected by Normalize's slug validation upstream, and their trimming is covered
	// by the direct CanonicalizeSite mapping test below — here we feed only valid slugs.)
	raws := []string{"NL", "GR", "nl", "gr", "dc1", "dc2", "dc1", "dc2", "NLLEI01"}
	for _, raw := range raws {
		r := NewRawEvent("librenms-x", nil)
		r.ExternalRef = "TG-456-guard"
		r.AlertRule = "Device-Down"
		r.Severity = "critical"
		r.Host = "dc1pve01"
		r.Site = raw
		r.ObservedAt = testNow
		e, err := Normalize(r, testNow)
		if err != nil {
			t.Fatalf("Normalize(site=%q): unexpected error %v", raw, err)
		}
		if !IsDeclaredSite(e.Site) {
			t.Fatalf("raw site %q normalized to %q, which is NOT a declared deployment site %v — the actuation "+
				"stamp (action_execution.site, via Prediction.Site) would store an out-of-vocabulary site and "+
				"every site-scoped join would silently miss it", raw, e.Site, DeclaredSites())
		}
	}
}

// TestCanonicalizeSiteMapping pins the fold from each known spelling to its deployment-key form, plus the
// idempotence, empty, and honest-passthrough (unknown → lowercased self, never a guessed site) properties.
func TestCanonicalizeSiteMapping(t *testing.T) {
	cases := map[string]string{
		"NL": "dc1", "nl": "dc1", "dc1": "dc1", "dc1": "dc1", "NLLEI01": "dc1",
		"dc1pve01": "dc1", // a host-name-shaped value still folds to its site cluster
		"GR":           "dc2", "gr": "dc2", "dc2": "dc2", "dc2": "dc2",
		" dc1 ": "dc1", "": "",
		"ch": "ch", "zz": "zz", // unknown → honest passthrough (lowercased), never a fabricated site
	}
	for raw, want := range cases {
		if got := CanonicalizeSite(raw); got != want {
			t.Errorf("CanonicalizeSite(%q) = %q, want %q", raw, got, want)
		}
	}
	// The declared set is exactly the two canonical deployment sites.
	if got := DeclaredSites(); len(got) != 2 || got[0] != "dc2" || got[1] != "dc1" {
		t.Fatalf("DeclaredSites() = %v, want [dc2 dc1]", got)
	}
}
