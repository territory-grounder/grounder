package attribution

import (
	"testing"
	"time"
)

// spec/023 REQ-2321 — "A missing authentication event never moves a classification."
//
// The requirement's own reasoning: RADIUS and any authentication-event source may enter only as an
// advisory `auth-observed` correlation, never as a taxonomy-minting Evidence reader and never as a
// classification mover, "because such a record names a network-access-server or login rather than the
// mutated target and cannot satisfy REQ-2312 admissibility. A missing authentication event SHALL NOT
// move a classification — absence is not admissible proof of absence, since key-based, console, and
// local logins bypass RADIUS."
//
// WHY THIS IS NOT A VACUOUS TEST, which is the obvious objection. No authentication reader is wired
// today (measured on the running worker 2026-08-07: the PVE task-log reader and the journal reader are
// armed, and nothing else), so "absence changes nothing" is trivially true if you only ever run
// Attribute WITHOUT such a record. These oracles run it BOTH WAYS instead — with and without an
// auth-shaped record present — and assert the Finding is identical. That is a property about the
// derivation, and it fails the day an auth source becomes admissible, which is exactly the promotion
// the requirement says must be gated on proven authentication-path coverage.
//
// WHAT THESE FOUND. Written first as a hypothesis, the second oracle went RED: REQ-2312 admissibility
// keys on TARGET, and a reader that stamped the INVESTIGATED HOST as its Target — the natural thing for
// a "who logged into web01" reader to do — became admissible, and its unsanctioned login principal then
// minted attributed-suspicious with nothing else in the set. The Target filter was doing REQ-2321's job
// by accident and only for records that named something else.
//
// So the guard is now explicit: advisoryDomains in attribution.go is a CLOSED set of domains that may
// correlate and never adjudicate, checked before admissibility and recorded as a warning rather than
// dropped silently. No authentication reader is wired today, so this is a no-op on the running estate
// and a gate on the next one.

// authEvidence is an auth-observed record in the shape such a reader would emit: the actor is a login
// principal nobody sanctioned, and the Target is the authentication endpoint rather than the mutated
// subject. If admissibility ever stopped keying on Target, this record's unsanctioned actor would mint
// AttributedSuspicious on its own — which, before the advisory guard, is exactly what it did.
func authEvidence(target string, at time.Time) Evidence {
	return Evidence{
		Domain: "radius", Actor: "jdoe@corp", ActionKind: "Access-Accept",
		Target: target, ObservedAt: at, Ref: "radacct-99", Covered: true,
	}
}

// The ABSENCE case, run both ways so it cannot pass by never being exercised.
func TestAMissingAuthenticationEventMovesNothing(t *testing.T) {
	at := epoch.Add(-5 * time.Minute)
	real := ev("pve", "root@pam", "vzstop", "web01", at)

	for _, tc := range []struct {
		name string
		with []Evidence
	}{
		{"sanctioned actor", []Evidence{real}},
		{"unsanctioned actor", []Evidence{ev("pve", "mallory@pam", "vzstop", "web01", at)}},
		{"no evidence at all", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			without := Attribute("web01", "", tc.with, nil, baseCfg)
			// The SAME inputs plus an auth record for a login that did happen, naming the NAS.
			withAuth := Attribute("web01", "", append(append([]Evidence(nil), tc.with...),
				authEvidence("nas01.corp", at)), nil, baseCfg)

			if without.Taxonomy != withAuth.Taxonomy {
				t.Fatalf("an authentication event MOVED the classification: %s -> %s. Absence of a login is not "+
					"proof of absence (key-based, console and local logins bypass RADIUS), so its presence must "+
					"not adjudicate either", without.Taxonomy, withAuth.Taxonomy)
			}
			if len(without.Candidates) != len(withAuth.Candidates) {
				t.Errorf("the auth record changed the CANDIDATE set (%v -> %v) — a second candidate escalates as a "+
					"contradiction, so this moves the outcome even when the taxonomy happens to match",
					without.Candidates, withAuth.Candidates)
			}
			if without.RuleID != withAuth.RuleID {
				t.Errorf("the auth record changed the matched rule id: %q -> %q", without.RuleID, withAuth.RuleID)
			}
		})
	}
}

// The sharpest case, and the one a future auth reader would break first: an auth record whose Target is
// the investigated subject itself. Even then it must not mint a taxonomy on its own — a login on a host
// is not evidence about who mutated it, and REQ-2321 says an auth record is advisory in every shape.
//
// This is the oracle that was RED before the advisory guard existed, and it is the reason the guard is
// a domain check rather than a reliance on the Target filter: the requirement forbids promoting an auth
// source to a taxonomy-minting Evidence reader, and a filter that happens to exclude it for an
// unrelated reason is not that prohibition.
func TestAnAuthRecordNamingTheSubjectIsStillNotAMover(t *testing.T) {
	at := epoch.Add(-5 * time.Minute)
	alone := Attribute("web01", "", []Evidence{authEvidence("web01", at)}, nil, baseCfg)

	if alone.Taxonomy == AttributedSuspicious {
		t.Fatalf("an authentication event alone minted %s — a login is not evidence about who MUTATED the "+
			"target, and REQ-2321 forbids an auth source acting as a taxonomy-minting reader. If an auth "+
			"reader has been wired, this is the gate it must pass first", alone.Taxonomy)
	}
	if alone.Taxonomy != Unattributable {
		t.Errorf("an auth record alone must leave the subject unattributable, got %s", alone.Taxonomy)
	}

	// CASING IS NOT COSMETIC HERE, and this file's own neighbourhood proves it: Attribute already folds
	// case on the subject because "the journal reader lowercases its evidence Target while the
	// PVE/NetBox/AWX/gitops readers pass it through". Readers disagree, so a domain match that is
	// case-SENSITIVE lets a reader emitting "RADIUS" mint a taxonomy the identical lowercase reader
	// could not. Caught by mutation: a case-sensitive IsAdvisoryDomain survived the test above.
	for _, variant := range []string{"RADIUS", "Radius", " radius ", "Auth-Observed"} {
		e := authEvidence("web01", at)
		e.Domain = variant
		got := Attribute("web01", "", []Evidence{e}, nil, baseCfg)
		if got.Taxonomy != Unattributable {
			t.Errorf("domain %q minted %s — the advisory check must fold case and trim, or a reader's own "+
				"spelling decides whether a login can adjudicate", variant, got.Taxonomy)
		}
	}
}

// The VACUITY FLOOR for the pair above. Both assert that adding a record changes nothing — which is
// also what they would report if Attribute ignored EVERY record, or if the fixture were inadmissible
// for an unrelated reason (a stale timestamp, a typo'd host). This drives the same shape with a REAL
// domain and proves the derivation does move when it should.
func TestTheDerivationDoesMoveOnAdmissibleEvidence(t *testing.T) {
	at := epoch.Add(-5 * time.Minute)
	none := Attribute("web01", "", nil, nil, baseCfg)
	if none.Taxonomy != Unattributable {
		t.Fatalf("precondition: no evidence must be unattributable, got %s", none.Taxonomy)
	}
	moved := Attribute("web01", "", []Evidence{ev("pve", "mallory@pam", "vzstop", "web01", at)}, nil, baseCfg)
	if moved.Taxonomy != AttributedSuspicious {
		t.Fatalf("an ADMISSIBLE unsanctioned actor must move the classification to suspicious, got %s — if this "+
			"fails, the two oracles above are passing because nothing moves anything", moved.Taxonomy)
	}
}
