package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/territory-grounder/grounder/core/attribution"
	"github.com/territory-grounder/grounder/modules/actorevidence/journal"
)

// TG-453: THE EDGE THAT JOINS THE LAW TO THE PROOF for the READER self-identity — the same round-trip the
// actuation self-actor lives on. Two independent pieces of code must agree on one string: the composition root
// DERIVES TG's own hostdiag reader identity from the hostdiag KEY, and the journal reader PARSES an identity
// out of the line sshd writes when that key logs in. If they disagree by a character, TG never recognises its
// own diagnostic login — it keeps reading attributed-suspicious and SECURITY-ESCALATES its own investigation,
// refusing a legitimately-approved heal (the live defect). So the oracle is a ROUND TRIP over a REAL key, not
// two literals that could drift together.
func TestSelfSSHReaderIsEXACTLYWhatSSHDLogs(t *testing.T) {
	path, pub := writeKey(t)
	readers := resolveSelfSSHReaders(envGetter(map[string]string{
		"TG_HOSTDIAG_DEPLOYMENTS": "dc1|*|root|file:" + path,
	}))
	if len(readers) != 1 {
		t.Fatalf("one hostdiag key ⇒ one reader identity, got %v", readers)
	}
	self := readers[0]
	if !strings.HasPrefix(self, "root!SHA256:") {
		t.Errorf("reader identity %q is not the user!SHA256:fp shape sshd logs", self)
	}
	// The exact line sshd writes when THIS key authenticates, parsed back through the REAL reader.
	at := time.Date(2026, 8, 12, 15, 0, 0, 0, time.UTC)
	line := `{"SYSLOG_IDENTIFIER":"sshd","_COMM":"sshd","MESSAGE":"Accepted publickey for root from 10.30.1.9 port 4242 ssh2: ED25519 ` +
		ssh.FingerprintSHA256(pub) + `","__REALTIME_TIMESTAMP":"` + itoaTest(at.UnixMicro()) + `","__CURSOR":"c1"}`
	got := journal.ParseForTest([]byte(line), "librespeed01", at.Add(-time.Hour), at.Add(time.Hour), true)
	if len(got) != 1 {
		t.Fatalf("the reader produced %d rows for a real sshd line, want 1", len(got))
	}
	if got[0].Actor != self {
		t.Fatalf("READER SELF-RECOGNITION IS BROKEN: the composition root derives %q from the hostdiag key, and "+
			"the journal reader parses %q from the line sshd writes for that same key — TG's own diagnostic "+
			"login would read attributed-suspicious (TG-453).", self, got[0].Actor)
	}
}

// A hostdiag deployment set can name many hosts on ONE key and some on another. The reader identities must
// de-duplicate (a shared key is ONE identity, not one-per-host), keep distinct keys distinct, and honour a
// non-root diagnostic login user (a `tgdiag` user would otherwise never match its own sshd lines).
func TestSelfSSHReadersDedupAndDistinct(t *testing.T) {
	keyA, pubA := writeKey(t)
	keyB, pubB := writeKey(t)
	readers := resolveSelfSSHReaders(envGetter(map[string]string{
		"TG_HOSTDIAG_DEPLOYMENTS": "dc1|host-a|root|file:" + keyA + ";" +
			"dc1|host-b|root|file:" + keyA + ";" +
			"dc1|host-c|tgdiag|file:" + keyB,
	}))
	want := map[string]bool{
		"root!" + ssh.FingerprintSHA256(pubA):   true,
		"tgdiag!" + ssh.FingerprintSHA256(pubB): true,
	}
	if len(readers) != len(want) {
		t.Fatalf("expected %d distinct reader identities (a shared key deduped to one), got %d: %v", len(want), len(readers), readers)
	}
	for _, r := range readers {
		if !want[r] {
			t.Errorf("unexpected reader identity %q", r)
		}
	}
}

// FAIL-SOFT, never a guess. A row whose key ref is absent, unreadable, or unparseable contributes NO identity —
// that reader's logins keep reading suspicious (the safe pre-TG-453 behaviour) rather than minting a fabricated
// self-identity that would be an amnesty for whatever matches it. And an unset deployment set (the actuation
// plane, which withholds the hostdiag keys) yields nothing — self-reader recognition is a triage-plane property.
func TestSelfSSHReadersFailSoftAndPlaneScoped(t *testing.T) {
	good, pub := writeKey(t)
	junk := filepath.Join(t.TempDir(), "not-a-key")
	if err := os.WriteFile(junk, []byte("this is not a private key\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	readers := resolveSelfSSHReaders(envGetter(map[string]string{
		"TG_HOSTDIAG_DEPLOYMENTS": "dc1|host-a|root|file:" + good + ";" +
			"dc1|host-b|root|file:" + filepath.Join(t.TempDir(), "absent") + ";" +
			"dc1|host-c|root|file:" + junk,
	}))
	if len(readers) != 1 || readers[0] != "root!"+ssh.FingerprintSHA256(pub) {
		t.Fatalf("only the resolvable key must yield an identity (bad rows skipped, not guessed), got %v", readers)
	}
	if got := resolveSelfSSHReaders(envGetter(map[string]string{})); got != nil {
		t.Errorf("no hostdiag deployments (e.g. the actuation plane) ⇒ no reader identities, got %v", got)
	}
}

// TG-457: the reader-self derivation must ALSO cover TG's SYSLOGNG log-collection identity — the read-only SSH
// login TG uses to pull a per-site syslog server's device logs during triage. When the fault is ON that syslog
// server, TG's own collection login lands in ITS auth journal; unrecognised, it reads attributed-suspicious and
// SECURITY-ESCALATES TG's own investigation — the TG-453 defect one lane over. THE KILLING CHECK: with only
// TG_SYSLOGNG_DEPLOYMENTS set, the resolver must yield the syslogng identity — before the derivation was widened
// to syslogng it yielded nothing (this asserts the syslogng loop exists), and both lanes together must carry
// BOTH identities (syslogng MERGED into the hostdiag set, never replacing it).
func TestSelfSSHReadersIncludeSyslogng(t *testing.T) {
	sgKey, sgPub := writeKey(t)
	wantSG := "root!" + ssh.FingerprintSHA256(sgPub)

	// site|sshhost|sshuser|keyref|basepath — neutral placeholder tokens, no estate specifics.
	syslogngOnly := resolveSelfSSHReaders(envGetter(map[string]string{
		"TG_SYSLOGNG_DEPLOYMENTS": "siteA|loghost|root|file:" + sgKey + "|/logs/syslog-ng",
	}))
	if len(syslogngOnly) != 1 || syslogngOnly[0] != wantSG {
		t.Fatalf("a syslogng deployment ALONE must yield exactly its reader identity %q (RED before the derivation was widened to syslogng), got %v", wantSG, syslogngOnly)
	}

	// Both lanes set (distinct keys ⇒ distinct fingerprints): the returned set carries BOTH identities.
	hdKey, hdPub := writeKey(t)
	wantHD := "root!" + ssh.FingerprintSHA256(hdPub)
	both := resolveSelfSSHReaders(envGetter(map[string]string{
		"TG_HOSTDIAG_DEPLOYMENTS": "siteA|*|root|file:" + hdKey,
		"TG_SYSLOGNG_DEPLOYMENTS": "siteA|loghost|root|file:" + sgKey,
	}))
	got := map[string]bool{}
	for _, r := range both {
		got[r] = true
	}
	if !got[wantHD] {
		t.Errorf("hostdiag identity %q missing — widening the derivation to syslogng must not drop the hostdiag lane", wantHD)
	}
	if !got[wantSG] {
		t.Errorf("syslogng identity %q missing when both lanes are set (TG-457)", wantSG)
	}
	if len(both) != 2 {
		t.Fatalf("two distinct keys ⇒ two identities, got %d: %v", len(both), both)
	}
}

// TG-457, the mirror of the TG-453 attribution round-trip one lane over: the syslogng reader identity the
// composition root derives must be EXACTLY what sshd logs for that key, and attribution must then recognise it
// as TG's own — no attributed-suspicious, no candidate minted — through the SAME SelfReaders["journal"]
// mechanism (core/attribution UNCHANGED). The oracle is a ROUND TRIP over a REAL key: derive → synthesise the
// exact sshd line → parse it back through the real journal reader → feed attribution, never two literals that
// could drift together.
func TestSyslogngReaderRecognisedEndToEnd(t *testing.T) {
	sgKey, sgPub := writeKey(t)
	readers := resolveSelfSSHReaders(envGetter(map[string]string{
		"TG_SYSLOGNG_DEPLOYMENTS": "siteA|loghost|root|file:" + sgKey + "|/logs/syslog-ng",
	}))
	if len(readers) != 1 {
		t.Fatalf("one syslogng key ⇒ one reader identity, got %v", readers)
	}
	self := readers[0]
	if !strings.HasPrefix(self, "root!SHA256:") {
		t.Errorf("syslogng reader identity %q is not the user!SHA256:fp shape sshd logs", self)
	}

	// (1) The exact line sshd writes when THIS syslogng key authenticates, parsed back through the REAL reader.
	// The 192.0.2.0/24 source is RFC 5737 documentation space (no estate IP); the reader keys off the FINGERPRINT.
	at := time.Date(2026, 8, 12, 15, 0, 0, 0, time.UTC)
	line := `{"SYSLOG_IDENTIFIER":"sshd","_COMM":"sshd","MESSAGE":"Accepted publickey for root from 192.0.2.10 port 4242 ssh2: ED25519 ` +
		ssh.FingerprintSHA256(sgPub) + `","__REALTIME_TIMESTAMP":"` + itoaTest(at.UnixMicro()) + `","__CURSOR":"c1"}`
	rows := journal.ParseForTest([]byte(line), "loghost", at.Add(-time.Hour), at.Add(time.Hour), true)
	if len(rows) != 1 {
		t.Fatalf("the reader produced %d rows for a real syslogng sshd line, want 1", len(rows))
	}
	if rows[0].Actor != self {
		t.Fatalf("SYSLOGNG READER SELF-RECOGNITION IS BROKEN: the composition root derives %q from the syslogng key, "+
			"and the journal reader parses %q from the line sshd writes for that same key — TG's own log-collection "+
			"login would read attributed-suspicious (TG-457).", self, rows[0].Actor)
	}

	// (2) attribution recognises that same identity as TG's own read-only investigation: not suspicious, and
	// alone it mints NO actor (a read is no remediation) ⇒ Unattributable — exactly the TG-453 SelfReaders path.
	cfg := attribution.Config{
		SelfActors:  map[string]string{},                     // triage plane withholds the actuation key
		SelfReaders: map[string][]string{"journal": readers}, // the widened set carrying the syslogng identity
		Sanctioned:  map[string][]string{},                   // the journal domain sanctions no one
		Window:      30 * time.Minute,
		Now:         func() time.Time { return at },
	}
	login := attribution.Evidence{Domain: "journal", Actor: self, ActionKind: "ssh-login", Target: "loghost", ObservedAt: at.Add(-3 * time.Minute), Ref: "c1"}
	f := attribution.Attribute("loghost", "start-service", []attribution.Evidence{login}, nil, cfg)
	if f.Taxonomy == attribution.AttributedSuspicious {
		t.Fatalf("TG's own syslogng reader login must NOT read attributed-suspicious (TG-457); got suspicious, candidates %v", f.Candidates)
	}
	if f.Taxonomy != attribution.Unattributable {
		t.Fatalf("TG's own syslogng reader login alone ⇒ unattributable (a read mints no actor), got %v", f.Taxonomy)
	}

	// (3) VACUITY GUARD: with the syslogng identity absent from SelfReaders (the pre-TG-457 behaviour), the
	// identical login falls through to suspicious — proving the recognition, not some unrelated clause, decides it.
	bare := cfg
	bare.SelfReaders = map[string][]string{}
	if fb := attribution.Attribute("loghost", "start-service", []attribution.Evidence{login}, nil, bare); fb.Taxonomy != attribution.AttributedSuspicious {
		t.Fatalf("without the syslogng identity in SelfReaders the login is indistinguishable from an intruder ⇒ suspicious (pre-TG-457), got %v", fb.Taxonomy)
	}
}
