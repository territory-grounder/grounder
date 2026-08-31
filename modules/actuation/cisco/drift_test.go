package cisco

import (
	"strings"
	"testing"
)

// THE FALSE-POSITIVE PROBLEM IS THE WHOLE PROBLEM. Two reads of an UNCHANGED device differ on every counter,
// uptime and timestamp. A sensor that reported those as drift gets ignored within a day — and an ignored
// sensor is worse than none, because then its silence means nothing either.
func TestVolatileFieldsAreNotDrift(t *testing.T) {
	first := map[string]string{
		"access-lists": "access-list outside_in extended permit tcp any host 10.0.0.5 eq 443 (hitcnt=1842)\n" +
			"access-list outside_in extended deny ip any any (hitcnt=99013)",
		"interfaces": "GigabitEthernet0/1 is up, line protocol is up\n" +
			"  5 minute input rate 12000 bits/sec\n" +
			"  184320 packets input, 9928311 bytes\n" +
			"  Last clearing of show interface counters 3 days",
	}
	// The SAME device, read again a while later: counters moved, nothing was configured.
	second := map[string]string{
		"access-lists": "access-list outside_in extended permit tcp any host 10.0.0.5 eq 443 (hitcnt=2011)\n" +
			"access-list outside_in extended deny ip any any (hitcnt=104778)",
		"interfaces": "GigabitEthernet0/1 is up, line protocol is up\n" +
			"  5 minute input rate 9000 bits/sec\n" +
			"  201455 packets input, 10944002 bytes\n" +
			"  Last clearing of show interface counters 4 days",
	}
	g, err := NewFingerprint(first)
	if err != nil {
		t.Fatal(err)
	}
	l, err := NewFingerprint(second)
	if err != nil {
		t.Fatal(err)
	}
	drift, err := CompareFingerprints(g, l)
	if err != nil {
		t.Fatal(err)
	}
	if len(drift) != 0 {
		t.Fatalf("an unchanged device reported %d drift item(s) — the sensor would be ignored within a day: %+v", len(drift), drift)
	}
	if g.Digest() != l.Digest() {
		t.Error("the digest must also be stable across volatile-only differences")
	}
}

// ...and it must still SEE a real configuration change, or the normalizer has just deleted the signal.
func TestARealConfigChangeIsDrift(t *testing.T) {
	g, err := NewFingerprint(map[string]string{
		"access-lists": "access-list outside_in extended permit tcp any host 10.0.0.5 eq 443 (hitcnt=1)\n" +
			"access-list outside_in extended deny ip any any (hitcnt=2)",
	})
	if err != nil {
		t.Fatal(err)
	}
	// Someone added a permit by hand — the exact out-of-band edit the IaC auto-sync would silently ratify.
	l, err := NewFingerprint(map[string]string{
		"access-lists": "access-list outside_in extended permit tcp any host 10.0.0.5 eq 443 (hitcnt=9)\n" +
			"access-list outside_in extended permit tcp any host 192.0.2.9 eq 22 (hitcnt=4)\n" +
			"access-list outside_in extended deny ip any any (hitcnt=7)",
	})
	if err != nil {
		t.Fatal(err)
	}
	drift, err := CompareFingerprints(g, l)
	if err != nil {
		t.Fatal(err)
	}
	if len(drift) != 1 || drift[0].Kind != DriftAdded || !strings.Contains(drift[0].Line, "192.0.2.9") {
		t.Fatalf("the hand-added permit must be reported exactly once as added, got %+v", drift)
	}
	if g.Digest() == l.Digest() {
		t.Error("a real change must move the digest")
	}
}

// A REMOVED line is drift too — and in the direction that matters most for a firewall (a deny that vanished).
func TestARemovedLineIsDrift(t *testing.T) {
	g, _ := NewFingerprint(map[string]string{"access-lists": "access-list x extended deny ip any any\naccess-list x extended permit tcp any any eq 443"})
	l, _ := NewFingerprint(map[string]string{"access-lists": "access-list x extended permit tcp any any eq 443"})
	drift, err := CompareFingerprints(g, l)
	if err != nil {
		t.Fatal(err)
	}
	if len(drift) != 1 || drift[0].Kind != DriftRemoved || !strings.Contains(drift[0].Line, "deny ip any any") {
		t.Fatalf("the vanished deny must be reported as removed, got %+v", drift)
	}
}

// AN EMPTY READ IS NOT AN EMPTY DEVICE — the load-bearing refusal. A truncated session, a pager that
// swallowed the output, or a renamed command must never become "the whole ACL was deleted", which is the
// loudest possible false alarm and exactly what a naive differ produces.
func TestAnEmptyReadIsACollectionFailureNotATotalDeletion(t *testing.T) {
	if _, err := NewFingerprint(map[string]string{"access-lists": ""}); err == nil {
		t.Fatal("an empty section must be refused as a collection failure")
	}
	if _, err := NewFingerprint(map[string]string{"access-lists": "   \n\n  "}); err == nil {
		t.Fatal("a whitespace-only section must be refused too")
	}
	if _, err := NewFingerprint(map[string]string{}); err == nil {
		t.Fatal("a fingerprint over no sections at all must be refused")
	}
}

// A section that is in the golden but was NOT collected live is a collection GAP, not a wholesale deletion.
func TestAMissingLiveSectionIsRefusedNotReportedAsRemoved(t *testing.T) {
	g, _ := NewFingerprint(map[string]string{
		"access-lists": "access-list x extended deny ip any any",
		"interfaces":   "GigabitEthernet0/1 is up, line protocol is up",
	})
	l, _ := NewFingerprint(map[string]string{
		"access-lists": "access-list x extended deny ip any any",
	})
	drift, err := CompareFingerprints(g, l)
	if err == nil {
		t.Fatalf("a section missing from the live read must be refused, got %d drift item(s): %+v", len(drift), drift)
	}
	if !strings.Contains(err.Error(), "interfaces") {
		t.Errorf("the refusal must name the uncollected section, got: %v", err)
	}
}

// Both empty-fingerprint directions are refused: an empty golden would report every live line as drift, and
// an empty live would report the whole golden as removed.
func TestEmptyFingerprintsAreRefusedBothWays(t *testing.T) {
	good, _ := NewFingerprint(map[string]string{"a": "line one"})
	if _, err := CompareFingerprints(Fingerprint{}, good); err == nil {
		t.Error("an empty golden must be refused")
	}
	if _, err := CompareFingerprints(good, Fingerprint{}); err == nil {
		t.Error("an empty live fingerprint must be refused")
	}
}

// Platform reordering is not drift: the fingerprint is order-independent, so a device that lists the same
// configuration in a different order compares equal.
func TestReorderingIsNotDrift(t *testing.T) {
	g, _ := NewFingerprint(map[string]string{"x": "bravo line\nalpha line\ncharlie line"})
	l, _ := NewFingerprint(map[string]string{"x": "charlie line\nalpha line\nbravo line"})
	drift, err := CompareFingerprints(g, l)
	if err != nil {
		t.Fatal(err)
	}
	if len(drift) != 0 {
		t.Fatalf("reordering must not be drift, got %+v", drift)
	}
}

// The sensor must never be pointed at a credential-bearing section: a fingerprint is a comparison key, not a
// config backup, and TG has no business holding pre-shared keys in one.
func TestTheSensorsSectionsAreNotCredentialBearing(t *testing.T) {
	for _, c := range DefaultCatalog() {
		if why := RefuseCredentialBearing(c.Argv); why != "" {
			t.Errorf("catalog entry %q would put credentials in a fingerprint: %s", c.Name, why)
		}
	}
}
