package journal

import (
	"os"
	"strings"
	"testing"
	"time"
)

// readSelf reads the reader's own source so the argv construction can be asserted structurally. A behaviour
// test cannot see this defect: a zone-less bound is a VALID argv that returns a valid empty result.
func readSelf() (string, error) {
	b, err := os.ReadFile("journal.go")
	return string(b), err
}

// A WINDOW WITHOUT A ZONE IS A DIFFERENT WINDOW ON EVERY HOST.
//
// journalctl reads a bare "2006-01-02 15:04:05" in the TARGET HOST'S local timezone. This reader rendered UTC
// values without a zone, so on any host that is not UTC it silently queried the wrong interval — never an
// error, never a warning, just an empty result indistinguishable from "nothing happened here".
//
// MEASURED LIVE 2026-07-29 on dc1ghostfolio01 (Europe/Amsterdam, CEST +0200), same 40-minute window:
//
//	--since '2026-07-29 08:05:12'      bare UTC value, read as LOCAL ->  0 rows
//	--since '2026-07-29 10:05:12'      the window in the host's zone -> 86 rows
//	--since '2026-07-29 08:05:12 UTC'  what this now emits           -> 92 rows, 19 harness logins
//
// The estate runs two hours ahead of UTC, so every read landed two hours in the past. This predates the SSH
// evidence source — the sudo path carried it too, an INDEPENDENT second reason the reader returned zero rows
// all-time, on top of the estate having no sudo. Either alone produces silence, and silence looks identical
// whichever caused it, which is how the "wrong allowlist" diagnosis survived for so long.
func TestTheJournalWindowCarriesAnExplicitZone(t *testing.T) {
	at := time.Date(2026, 7, 29, 8, 5, 12, 0, time.UTC)
	got := journalStamp(at)
	if !strings.HasSuffix(got, " UTC") {
		t.Fatalf("journalStamp(%v) = %q — a bare timestamp is read in the TARGET HOST's local zone, so on "+
			"this estate (+0200) every read lands two hours in the past and returns nothing", at, got)
	}
	if !strings.HasPrefix(got, "2026-07-29 08:05:12") {
		t.Errorf("journalStamp(%v) = %q — the instant itself changed", at, got)
	}
}

// The instant must be the same wherever the caller's clock is expressed. A caller handing in a +02:00 time
// must produce the same absolute bound as one handing in its UTC equivalent — otherwise the fix would only
// move the ambiguity from the host to the caller.
func TestTheWindowIsTheSameInstantWhateverZoneTheCallerUses(t *testing.T) {
	utc := time.Date(2026, 7, 29, 8, 5, 12, 0, time.UTC)
	cest := utc.In(time.FixedZone("CEST", 2*60*60))
	if a, b := journalStamp(utc), journalStamp(cest); a != b {
		t.Errorf("the same instant rendered two ways: %q vs %q", a, b)
	}
}

// The rendered bound must reach the ARGV. An oracle over journalStamp alone would prove the helper and
// nothing about the query the estate actually receives — the defect lived in the argv, not in a formatter.
func TestTheZonedStampReachesTheArgv(t *testing.T) {
	// Read() needs a runner and resolver to get as far as the argv, so this asserts the construction the
	// argv is built from instead: both bounds go through journalStamp, and neither is a bare format call.
	src, err := readSelf()
	if err != nil {
		t.Fatalf("read own source: %v", err)
	}
	if strings.Contains(src, `"--since", since.UTC().Format(`) || strings.Contains(src, `"--until", until.UTC().Format(`) {
		t.Error("the argv still formats a window bound inline instead of going through journalStamp — that is " +
			"the exact shape that produced a zone-less timestamp, and it will not fail any behaviour test")
	}
	if !strings.Contains(src, `"--since", journalStamp(since)`) || !strings.Contains(src, `"--until", journalStamp(until)`) {
		t.Error("the argv does not build BOTH window bounds with journalStamp")
	}
}
