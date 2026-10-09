package risk

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// CanaryPin is one deployment-declared canary allowlist entry: an action whose host and op class match
// this pin is forced to POLL_PAUSE regardless of how reversible/low-risk the classifier would otherwise
// judge it. It is the mechanism that pins the FIRST staged mutations behind a human vote (spec/001
// REQ-009; the Phase-2 canary "band FORCED POLL_PAUSE, never AUTO"). Config-not-code: no hostnames or op
// classes live in the binary — the set is declared per-deployment via a file reference.
type CanaryPin struct {
	HostPattern string `json:"host_pattern"` // glob over the target host ("" or "*" = any)
	OpClass     string `json:"op_class"`     // glob over the op class ("" or "*" = any)
	Reason      string `json:"reason"`       // audit reason recorded on the pinned session
}

// CanaryPins is an immutable, loaded set of canary pins. Its zero value (and a nil receiver) matches
// nothing, so the pin is INERT until a policy file is declared — an un-configured deployment classifies
// byte-identically to one without this feature.
type CanaryPins struct {
	pins []CanaryPin
}

// Match reports whether (host, opClass) is on the canary allowlist, returning the FIRST matching pin's
// audit reason. Nil-safe: a nil/empty set never matches. First-match-wins keeps the outcome deterministic.
func (c *CanaryPins) Match(host, opClass string) (bool, string) {
	if c == nil {
		return false, ""
	}
	for _, p := range c.pins {
		if globMatch(host, p.HostPattern) && globMatch(opClass, p.OpClass) {
			return true, p.Reason
		}
	}
	return false, ""
}

// Len reports the number of loaded pins (nil-safe) — used at boot to decide whether to log the pin set.
func (c *CanaryPins) Len() int {
	if c == nil {
		return 0
	}
	return len(c.pins)
}

// globMatch treats "" and "*" as match-any; otherwise it is a filepath.Match glob. A malformed pattern
// fails CLOSED for the CANARY sense — i.e. it does NOT match, so a typo can never silently widen the pin
// (and, because the pin only ever RAISES review, a non-match is the safe direction here regardless).
func globMatch(s, pattern string) bool {
	if pattern == "" || pattern == "*" {
		return true
	}
	ok, err := filepath.Match(pattern, s)
	return err == nil && ok
}

// LoadCanaryPins reads a JSON array of CanaryPin from path. An empty path yields an inert (empty) set —
// the default, no canary pinned. A path that is present but unreadable or malformed is a HARD error: the
// policy is never silently dropped, because a dropped canary pin would let a staged mutation reach AUTO.
func LoadCanaryPins(path string) (*CanaryPins, error) {
	if path == "" {
		return &CanaryPins{}, nil
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("canary poll policy: read %q: %w", path, err)
	}
	var pins []CanaryPin
	if err := json.Unmarshal(b, &pins); err != nil {
		return nil, fmt.Errorf("canary poll policy: parse %q: %w", path, err)
	}
	// Validate every glob at LOAD time so a malformed pattern fails the BOOT rather than silently
	// never-matching at classify time (a typo'd pin that never fires would let a staged mutation reach
	// AUTO once the gate is on). filepath.Match reports a bad pattern via ErrBadPattern; "" and "*"
	// validate cleanly (they are the match-any sentinels handled in globMatch).
	for i, p := range pins {
		if _, err := filepath.Match(p.HostPattern, "probe"); err != nil {
			return nil, fmt.Errorf("canary poll policy: pin %d host_pattern %q: %w", i, p.HostPattern, err)
		}
		if _, err := filepath.Match(p.OpClass, "probe"); err != nil {
			return nil, fmt.Errorf("canary poll policy: pin %d op_class %q: %w", i, p.OpClass, err)
		}
	}
	return &CanaryPins{pins: pins}, nil
}
