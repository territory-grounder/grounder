package cisco

// CONFIG-DRIFT SENSOR (TG-85 component 8): does the device still match the configuration the estate believes
// it has? READ-ONLY, mutation OFF — it produces an observation, never a change.
//
// The estate's network IaC repo auto-syncs FROM the live device every ~30 minutes (a scripted `show` →
// git commit), which makes the DEVICE the source of truth and the repo its follower. That is fine for
// bookkeeping and useless as a control: if someone edits the device by hand, the repo silently absorbs the
// edit at the next sync and nothing ever reports that the estate's intent and the device diverged. This
// sensor is the counterpart — TG holds a GOLDEN fingerprint and compares the live device against it, so an
// out-of-band change is visible for the window it matters rather than being ratified by the next sync.
//
// TWO PROPERTIES DO ALL THE WORK, and both are about not lying:
//
//  1. NORMALIZATION. A raw `show` diff is almost entirely false positives: counters, uptimes, timestamps,
//     "last cleared", byte totals and hit counts change on every single read. A sensor that reported those
//     as drift would be ignored within a day — and an ignored sensor is worse than none, because its silence
//     then means nothing either. So the comparison runs over a normalized projection that deliberately drops
//     the volatile fields and keeps the CONFIGURED ones.
//
//  2. AN EMPTY READ IS NOT AN EMPTY DEVICE. If a section comes back empty — a truncated session, a pager
//     that swallowed the output, a command the platform renamed — the naive comparison says every line was
//     REMOVED, which is the loudest possible false alarm ("the whole ACL is gone"). That is refused as a
//     collection failure instead. The sensor reports what it could not read; it never converts a failed read
//     into a maximal finding.
//
// The sensor never reads a credential-bearing section: it is built from the catalog's scoped diagnostics, and
// RefuseCredentialBearing gates every command it would run (a fingerprint is a comparison key, not a config
// backup — TG has no business holding pre-shared keys in one).

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// Fingerprint is a normalized, credential-free projection of a device's configured state, section by section.
// It is what TG compares against — not the raw config, which is both volatile and secret-bearing.
type Fingerprint struct {
	// Sections maps a section name (the catalog entry that produced it) to its normalized, sorted lines.
	Sections map[string][]string
}

// DriftKind classifies one difference.
type DriftKind string

const (
	DriftAdded   DriftKind = "added"   // present live, absent in golden — an out-of-band addition
	DriftRemoved DriftKind = "removed" // present in golden, absent live — an out-of-band deletion
)

// Drift is one observed difference in one section.
type Drift struct {
	Section string
	Kind    DriftKind
	Line    string
}

// volatile matches the fields that change on every read and therefore must never count as drift. Each is a
// MEASURED source of false positives on IOS/ASA `show` output, not a guess: hit/packet/byte counters, uptimes
// and elapsed times, "last cleared" stamps, and the connection/translation counts that move continuously.
var volatile = []*regexp.Regexp{
	regexp.MustCompile(`(?i)\(hitcnt=\d+\)`),
	regexp.MustCompile(`(?i)\bhits?\s*[:=]\s*\d+`),
	regexp.MustCompile(`(?i)\b\d+\s+packets?\b`),
	regexp.MustCompile(`(?i)\b\d+\s+bytes?\b`),
	regexp.MustCompile(`(?i)\buptime\b.*$`),
	regexp.MustCompile(`(?i)\blast\s+clear(ed|ing)\b.*$`),
	regexp.MustCompile(`(?i)\bidle\s+\d+`),
	regexp.MustCompile(`(?i)\bduration\b.*$`),
	regexp.MustCompile(`(?i)\b\d{2}:\d{2}:\d{2}\b`),                      // elapsed / wall clock
	regexp.MustCompile(`(?i)\b\d+\s+(seconds?|minutes?|hours?|days?)\b`), // "3 days", "12 minutes"
	// Traffic RATES move continuously on a live link — "5 minute input rate 12000 bits/sec" differs on every
	// read of an otherwise untouched interface. Found by the oracle below, not by inspection: the first
	// normalizer stripped the "5 minute" and left the rate, so an unchanged device still reported drift.
	regexp.MustCompile(`(?i)\brate\s+\d+\s*\S*`),
	regexp.MustCompile(`(?i)\b\d+\s*(bits|bytes|packets)/sec\b`),
}

// NormalizeSection projects raw `show` output into the comparable lines of a fingerprint: volatile fields
// stripped, whitespace collapsed, blank lines and the device's own prompt echo dropped, and the result sorted
// so a reordering by the platform is not reported as drift. Exported so an operator tool and the oracles can
// build a golden the same way the sensor builds the live side — a fingerprint compared against one built by a
// DIFFERENT normalizer would report drift that is purely an artifact of the two projections.
func NormalizeSection(raw string) []string {
	var out []string
	for _, line := range strings.Split(raw, "\n") {
		s := strings.TrimSpace(line)
		if s == "" {
			continue
		}
		for _, re := range volatile {
			s = re.ReplaceAllString(s, "")
		}
		s = strings.Join(strings.Fields(s), " ") // collapse whitespace left by the strips
		if s == "" {
			continue
		}
		// A prompt echo is the transport's artifact, not device config.
		if defaultPromptRE.MatchString(s) && len(strings.Fields(s)) == 1 {
			continue
		}
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}

// NewFingerprint builds a fingerprint from raw section output. It REFUSES a section that came back empty:
// an empty read is a collection failure, and treating it as an empty device would turn a truncated session
// into "every line was removed" — the loudest possible false alarm.
func NewFingerprint(sections map[string]string) (Fingerprint, error) {
	if len(sections) == 0 {
		return Fingerprint{}, fmt.Errorf("cisco drift: no sections collected — a fingerprint over nothing would compare as total drift (fail closed)")
	}
	fp := Fingerprint{Sections: make(map[string][]string, len(sections))}
	for name, raw := range sections {
		lines := NormalizeSection(raw)
		if len(lines) == 0 {
			return Fingerprint{}, fmt.Errorf("cisco drift: section %q read back empty — refusing to record it as an empty device; an empty read is a collection failure (a truncated session, a pager that swallowed the output, a renamed command), not a deleted configuration", name)
		}
		fp.Sections[name] = lines
	}
	return fp, nil
}

// Digest is a stable hex digest of the fingerprint — the cheap equality check an operator or a metric can
// carry without holding the lines.
func (f Fingerprint) Digest() string {
	names := make([]string, 0, len(f.Sections))
	for n := range f.Sections {
		names = append(names, n)
	}
	sort.Strings(names)
	h := sha256.New()
	for _, n := range names {
		h.Write([]byte(n))
		h.Write([]byte{0})
		for _, l := range f.Sections[n] {
			h.Write([]byte(l))
			h.Write([]byte{0})
		}
		h.Write([]byte{1})
	}
	return hex.EncodeToString(h.Sum(nil))
}

// CompareFingerprints reports how live differs from golden, section by section.
//
// It refuses a comparison it cannot make honestly: a section present in golden but MISSING from live is a
// collection gap, not a wholesale deletion, and is returned as an error rather than as a wall of "removed"
// findings. A section present live but not in golden is reported as a new section's worth of additions — that
// direction is safe, because it cannot manufacture a deletion.
func CompareFingerprints(golden, live Fingerprint) ([]Drift, error) {
	if len(golden.Sections) == 0 {
		return nil, fmt.Errorf("cisco drift: the golden fingerprint is empty — every live line would read as drift (fail closed)")
	}
	if len(live.Sections) == 0 {
		return nil, fmt.Errorf("cisco drift: the live fingerprint is empty — refusing to report the whole golden as removed (fail closed)")
	}
	var missing []string
	for name := range golden.Sections {
		if _, ok := live.Sections[name]; !ok {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		return nil, fmt.Errorf("cisco drift: section(s) %v are in the golden but were not collected live — that is a collection gap, not a deletion; refusing to report them as removed", missing)
	}

	var out []Drift
	names := make([]string, 0, len(live.Sections))
	for n := range live.Sections {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, name := range names {
		g := index(golden.Sections[name])
		l := index(live.Sections[name])
		for line := range l {
			if _, ok := g[line]; !ok {
				out = append(out, Drift{Section: name, Kind: DriftAdded, Line: line})
			}
		}
		for line := range g {
			if _, ok := l[line]; !ok {
				out = append(out, Drift{Section: name, Kind: DriftRemoved, Line: line})
			}
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Section != out[j].Section {
			return out[i].Section < out[j].Section
		}
		if out[i].Kind != out[j].Kind {
			return out[i].Kind < out[j].Kind
		}
		return out[i].Line < out[j].Line
	})
	return out, nil
}

func index(lines []string) map[string]struct{} {
	m := make(map[string]struct{}, len(lines))
	for _, l := range lines {
		m[l] = struct{}{}
	}
	return m
}
