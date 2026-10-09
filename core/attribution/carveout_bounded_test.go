package attribution

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

// A CARVE-OUT SUSPENDS THE SECURITY PATH, SO IT MUST EXPIRE.
//
// Inside a carve-out, an actor who would otherwise resolve to attributed-suspicious (→ security-escalate)
// resolves to authorized-test (→ ladder-unchanged, heal). That is the correct behaviour for a sanctioned
// fault injector, and it is why the requirement calls the rule TEMPORALLY BOUNDED (REQ-2309).
//
// Until 2026-07-29 the bound was optional in the schema and skipped when absent, so "no valid_until" meant
// "valid forever" — the exact inverse of the property. Measured on the live estate that day: BOTH deployed
// carve-outs omitted it. `shadowbench-pool` permanently sanctioned root@pam over 15 pool guests, and
// `shadowbench-pool-ssh` permanently sanctioned the operator's admin SSH key fingerprint over 15 guests.
// Since this estate's fault harness and its operator hold the SAME key, that made "the harness broke this
// guest" and "someone with the admin key broke this guest" permanently indistinguishable, and it is why
// attributed-suspicious read 0 rows for all time on those hosts.
//
// These oracles assert over the CLOSED SET of ways a bound can be under-specified rather than one sample,
// because the defect was one unchecked case standing in for all of them.
func TestCarveOutBoundsAreMandatoryAtLoad(t *testing.T) {
	// Every shape that leaves the window open at one or both ends, plus the shapes that satisfy the schema
	// while re-creating permanence. All must be REJECTED at load.
	cases := []struct {
		name       string
		from, till string
		wantErr    string
	}{
		{"both bounds absent — the live defect", "", "", "must declare BOTH"},
		{"upper bound absent — unbounded into the future", "2026-06-01T00:00:00Z", "", "must declare BOTH"},
		{"lower bound absent — unbounded into the past", "", "2026-08-30T00:00:00Z", "must declare BOTH"},
		{"a century-long window satisfies the schema and is still permanent",
			"2026-06-01T00:00:00Z", "2126-06-01T00:00:00Z", "over the 90-day maximum"},
		{"one day over the cap", "2026-06-01T00:00:00Z", "2026-08-31T00:00:00Z", "over the 90-day maximum"},
		{"inverted window is inert, not permissive", "2026-08-30T00:00:00Z", "2026-06-01T00:00:00Z", "not after"},
		{"empty window", "2026-06-01T00:00:00Z", "2026-06-01T00:00:00Z", "not after"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, _, err := ParseConfig([]byte(rulesetWithCarveOut(c.from, c.till)))
			if err == nil {
				t.Fatalf("ParseConfig ACCEPTED %s (from=%q until=%q) — a carve-out the operator cannot "+
					"see the end of is a permanent exemption from the security path", c.name, c.from, c.till)
			}
			if !strings.Contains(err.Error(), c.wantErr) {
				t.Fatalf("error should explain %q, got: %v", c.wantErr, err)
			}
		})
	}

	// And the boundary case must still LOAD — a cap that rejects a ruleset written exactly to the documented
	// maximum would push operators toward shorter, more frequently-lapsing windows.
	from := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	_, cfg, err := ParseConfig([]byte(rulesetWithCarveOut(
		from.Format(time.RFC3339), from.Add(MaxCarveOutWindow).Format(time.RFC3339))))
	if err != nil {
		t.Fatalf("a carve-out of exactly MaxCarveOutWindow must parse: %v", err)
	}
	if len(cfg.CarveOuts) != 1 {
		t.Fatalf("want the carve-out retained, got %+v", cfg.CarveOuts)
	}
}

// THE SECOND LAYER: the TYPE, not just the document. A Config assembled in code must not be able to obtain a
// permanent exemption by leaving a field at its zero value — the parser guards the data, this guards every
// caller that never routes through the parser.
func TestUnboundedCarveOutNeverMatches(t *testing.T) {
	now := time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC)
	base := CarveOut{ID: "harness", Domain: "pve", Actors: []string{"root@pam"}, Hosts: []string{"poolhost01"}}

	withFrom := base
	withFrom.ValidFrom = now.Add(-time.Hour)
	withTill := base
	withTill.ValidUntil = now.Add(time.Hour)
	both := base
	both.ValidFrom, both.ValidUntil = now.Add(-time.Hour), now.Add(time.Hour)

	for _, c := range []struct {
		name string
		co   CarveOut
		want bool
	}{
		{"no bounds at all", base, false},
		{"lower bound only — open-ended into the future", withFrom, false},
		{"upper bound only — open-ended into the past", withTill, false},
		{"fully bounded and current", both, true},
	} {
		if got := carveOutValid(c.co, now); got != c.want {
			t.Errorf("%s: carveOutValid = %v, want %v", c.name, got, c.want)
		}
	}

	// ★ THE CONSEQUENCE, NOT JUST THE PREDICATE. A unit check on carveOutValid would stay green if the
	// carve-out branch in Attribute stopped consulting it, so drive the real derivation: an UNSANCTIONED
	// actor on a host named by an unbounded carve-out must reach attributed-suspicious. This is the assertion
	// that would have caught the live defect, because it fails whenever an unbounded carve-out can still
	// mask a stranger.
	cfg := Config{
		Window:     time.Hour,
		Now:        func() time.Time { return now },
		Sanctioned: map[string][]string{},
		SelfActors: map[string]string{},
		CarveOuts:  []CarveOut{base}, // deliberately unbounded
	}
	ev := []Evidence{{
		Domain: "pve", Actor: "root@pam", ActionKind: "vzstop", Target: "poolhost01",
		ObservedAt: now.Add(-time.Minute), Ref: "UPID:1",
	}}
	if f := Attribute("poolhost01", "guest-down", ev, nil, cfg); f.Taxonomy != AttributedSuspicious {
		t.Fatalf("an unsanctioned actor on a host named by an UNBOUNDED carve-out resolved to %v, want "+
			"attributed-suspicious: the carve-out must not launder an unknown actor into authorized-test "+
			"on the strength of a bound nobody ever wrote", f.Taxonomy)
	}

	// The same evidence with a VALID carve-out must resolve authorized-test — otherwise this file would pass
	// on a build where carve-outs never match at all, which would silently end the learning regime.
	cfg.CarveOuts = []CarveOut{both}
	if f := Attribute("poolhost01", "guest-down", ev, nil, cfg); f.Taxonomy != AuthorizedTest {
		t.Fatalf("a CURRENT carve-out must still resolve authorized-test, got %v — a fix that simply "+
			"disabled carve-outs would stop the harness from ever accruing a clean run", f.Taxonomy)
	}
}

func rulesetWithCarveOut(from, till string) string {
	bounds := ""
	if from != "" {
		bounds += fmt.Sprintf(`,"valid_from":%q`, from)
	}
	if till != "" {
		bounds += fmt.Sprintf(`,"valid_until":%q`, till)
	}
	return `{"actor_attribution":{
		"mapping":[
			{"id":"a","taxonomy":"attributed-authorized","disposition":"stand-down-coordinate"},
			{"id":"b","taxonomy":"attributed-self","disposition":"self-noop"},
			{"id":"c","taxonomy":"unattributable","disposition":"ladder-unchanged"},
			{"id":"d","taxonomy":"attributed-suspicious","disposition":"security-escalate"},
			{"id":"e","taxonomy":"authorized-test","disposition":"ladder-unchanged"}
		],
		"carve_outs":[{"id":"pool","domain":"pve","actors":["root@pam"],"hosts":["poolhost01"]` + bounds + `}]
	}}`
}
