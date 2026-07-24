package attribution

import (
	"testing"
	"time"
)

// The deterministic attributor's REQ matrix: the taxonomy is derived from typed reader evidence only,
// evidence is admissible only when timestamped inside the window AND naming the subject (REQ-2312),
// absence degrades to the pre-feature ladder (REQ-2303), a carve-out resolves authorized-test only when
// temporally valid (REQ-2309), and a contradiction escalates with every candidate recorded (REQ-2310).

var (
	epoch   = time.Date(2026, 7, 23, 12, 0, 0, 0, time.UTC)
	nowFunc = func() time.Time { return epoch }
	baseCfg = Config{
		SelfActors: map[string]string{"pve": "root@pam!tg-actuate"},
		Sanctioned: map[string][]string{"pve": {"root@pam"}},
		Window:     30 * time.Minute,
		Now:        nowFunc,
	}
)

func ev(domain, actor, kind, target string, at time.Time) Evidence {
	return Evidence{Domain: domain, Actor: actor, ActionKind: kind, Target: target, ObservedAt: at, Ref: "UPID:1"}
}

func TestAttribute(t *testing.T) {
	cases := []struct {
		name     string
		subject  string
		ev       []Evidence
		cfg      Config
		want     Taxonomy
		wantRule string
		wantCand int // >1 ⇒ contradiction (escalate)
	}{
		{"REQ-2303: no evidence ⇒ unattributable (pre-feature ladder)", "web01", nil, baseCfg, Unattributable, "", 0},
		{"REQ-2301: sanctioned non-self principal ⇒ attributed-authorized", "web01",
			[]Evidence{ev("pve", "root@pam", "vzstop", "web01", epoch.Add(-5*time.Minute))}, baseCfg, AttributedAuthorized, "", 1},
		{"REQ-2302: platform's own identity ⇒ attributed-self", "web01",
			[]Evidence{ev("pve", "root@pam!tg-actuate", "vzstart", "web01", epoch.Add(-5*time.Minute))}, baseCfg, AttributedSelf, "", 0},
		{"REQ-2304: unsanctioned actor ⇒ attributed-suspicious", "web01",
			[]Evidence{ev("pve", "mallory@pam", "vzstop", "web01", epoch.Add(-5*time.Minute))}, baseCfg, AttributedSuspicious, "", 1},
		{"REQ-2312: evidence OUTSIDE the window is inadmissible", "web01",
			[]Evidence{ev("pve", "root@pam", "vzstop", "web01", epoch.Add(-2*time.Hour))}, baseCfg, Unattributable, "", 0},
		{"REQ-2312: evidence naming a DIFFERENT target is inadmissible", "web01",
			[]Evidence{ev("pve", "root@pam", "vzstop", "db02", epoch.Add(-5*time.Minute))}, baseCfg, Unattributable, "", 0},
		{"hardening: mixed-case subject folds to a lowercased evidence Target (journal normalises Target; matcher must not silent-drop)", "WEB01",
			[]Evidence{ev("pve", "root@pam", "vzstop", "web01", epoch.Add(-5*time.Minute))}, baseCfg, AttributedAuthorized, "", 1},
		{"hardening: folding does NOT over-match a genuinely different host", "web01",
			[]Evidence{ev("pve", "root@pam", "vzstop", "WEB02", epoch.Add(-5*time.Minute))}, baseCfg, Unattributable, "", 0},
		{"hardening: ACTOR identity stays case-SENSITIVE (ROOT@PAM != sanctioned root@pam ⇒ suspicious, not authorized)", "web01",
			[]Evidence{ev("pve", "ROOT@PAM", "vzstop", "web01", epoch.Add(-5*time.Minute))}, baseCfg, AttributedSuspicious, "", 1},
		{"hardening: carve-out host match is case-insensitive (subject POOLHOST01 vs carve-out Hosts poolhost01)", "POOLHOST01",
			[]Evidence{ev("pve", "root@pam", "vzstop", "poolhost01", epoch.Add(-5*time.Minute))},
			Config{SelfActors: baseCfg.SelfActors, Sanctioned: baseCfg.Sanctioned, Window: baseCfg.Window, Now: nowFunc,
				CarveOuts: []CarveOut{{ID: "pool-ci", Domain: "pve", Actors: []string{"root@pam"}, Hosts: []string{"poolhost01"},
					ValidFrom: epoch.Add(-time.Hour), ValidUntil: epoch.Add(time.Hour)}}},
			AuthorizedTest, "pool-ci", 0},
		{"REQ-2309: a currently-valid carve-out ⇒ authorized-test with the rule id", "poolhost01",
			[]Evidence{ev("pve", "root@pam", "vzstop", "poolhost01", epoch.Add(-5*time.Minute))},
			Config{SelfActors: baseCfg.SelfActors, Sanctioned: baseCfg.Sanctioned, Window: baseCfg.Window, Now: nowFunc,
				CarveOuts: []CarveOut{{ID: "shadowbench-pool", Domain: "pve", Actors: []string{"root@pam"}, Hosts: []string{"poolhost01"},
					ValidFrom: epoch.Add(-time.Hour), ValidUntil: epoch.Add(time.Hour)}}},
			AuthorizedTest, "shadowbench-pool", 0},
		{"REQ-2309: an EXPIRED carve-out never matches (reverts to authorized stand-down)", "poolhost01",
			[]Evidence{ev("pve", "root@pam", "vzstop", "poolhost01", epoch.Add(-5*time.Minute))},
			Config{SelfActors: baseCfg.SelfActors, Sanctioned: baseCfg.Sanctioned, Window: baseCfg.Window, Now: nowFunc,
				CarveOuts: []CarveOut{{ID: "old", Domain: "pve", Actors: []string{"root@pam"}, Hosts: []string{"poolhost01"},
					ValidFrom: epoch.Add(-2 * time.Hour), ValidUntil: epoch.Add(-time.Hour)}}},
			AttributedAuthorized, "", 1},
		{"REQ-2310: a non-suspicious contradiction (self + authorized) ⇒ escalate with both recorded", "web01",
			[]Evidence{
				ev("pve", "root@pam", "vzstop", "web01", epoch.Add(-6*time.Minute)),
				ev("pve", "root@pam!tg-actuate", "vzstart", "web01", epoch.Add(-5*time.Minute)),
			}, baseCfg, Unattributable, "", 2}, // both candidates recorded; Taxonomy stays zero (escalate)
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := Attribute(c.subject, "start-guest", c.ev, nil, c.cfg)
			if f.Taxonomy != c.want {
				t.Fatalf("taxonomy = %v, want %v (candidates=%v)", f.Taxonomy, c.want, f.Candidates)
			}
			if f.RuleID != c.wantRule {
				t.Fatalf("rule id = %q, want %q", f.RuleID, c.wantRule)
			}
			if c.wantCand > 0 && len(f.Candidates) != c.wantCand {
				t.Fatalf("candidates = %v, want %d", f.Candidates, c.wantCand)
			}
		})
	}
}

// REQ-2303's oracle: an absent attribution must leave the classification byte-identical to the
// pre-feature ladder. The attributor's contribution is that Unattributable is the zero value and sets no
// restrictive signal — proven by the zero-input case returning Unattributable with no candidates.
func TestUnattributableIsTheZeroValue(t *testing.T) {
	var f Finding
	if f.Taxonomy != Unattributable {
		t.Fatalf("the zero Finding must read unattributable, got %v", f.Taxonomy)
	}
	if Unattributable.String() != "unattributable" {
		t.Fatalf("the zero taxonomy must render unattributable, got %q", Unattributable.String())
	}
}

// REQ-2310 where one candidate IS suspicious: REQ-2304 governs — the suspicious reading dominates any
// mere contradiction (a hostile-actor signal must never be averaged away).
func TestSuspiciousDominatesContradiction(t *testing.T) {
	cfg := baseCfg
	f := Attribute("web01", "start-guest", []Evidence{
		ev("pve", "root@pam", "vzstop", "web01", epoch.Add(-6*time.Minute)),    // sanctioned
		ev("pve", "mallory@pam", "vzstop", "web01", epoch.Add(-5*time.Minute)), // unsanctioned
	}, nil, cfg)
	if f.Taxonomy != AttributedSuspicious {
		t.Fatalf("a suspicious candidate must dominate, got %v", f.Taxonomy)
	}
	if len(f.Candidates) != 2 {
		t.Fatalf("both candidates must be recorded, got %v", f.Candidates)
	}
}

// SECURITY REGRESSION (REQ-2304 dominates the carve-out): an unsanctioned actor acting on a pool host
// DURING an active carve-out window must resolve attributed-suspicious — NEVER authorized-test. The
// carve-out was evaluated before the suspicious tally and short-circuited on the first sanctioned/self
// record, masking a co-occurring intruder as a sanctioned test and auto-healing over a possible intrusion.
func TestSuspiciousDominatesCarveOut(t *testing.T) {
	cfg := baseCfg
	cfg.CarveOuts = []CarveOut{{ID: "pool", Domain: "pve", Actors: []string{"root@pam", "root@pam!tg-actuate"},
		Hosts: []string{"poolhost01"}, ValidFrom: epoch.Add(-time.Hour), ValidUntil: epoch.Add(time.Hour)}}
	f := Attribute("poolhost01", "start-guest", []Evidence{
		ev("pve", "root@pam!tg-actuate", "vzstart", "poolhost01", epoch.Add(-6*time.Minute)), // TG's own prior heal (matches carve-out)
		ev("pve", "mallory@pve", "vzstop", "poolhost01", epoch.Add(-5*time.Minute)),          // the intruder, co-occurring
	}, nil, cfg)
	if f.Taxonomy != AttributedSuspicious {
		t.Fatalf("an unsanctioned actor during a carve-out window must be suspicious, not masked as authorized-test, got %v (rule %q)", f.Taxonomy, f.RuleID)
	}
	if f.RuleID != "" {
		t.Fatalf("a suspicious dominance must not attribute to a carve-out rule, got %q", f.RuleID)
	}
}

// REQ-2302 self-recognition survives a carve-out: TG's OWN heal on a pool host resolves attributed-self
// (terminate already-remediated, no re-actuation) — NOT authorized-test — because the carve-out lists the
// sanctioned INJECTOR principal, never TG's own actuation identity (default_config STONITH'd tg-actuate).
func TestSelfHealOnPoolHostStaysSelf(t *testing.T) {
	cfg := baseCfg
	cfg.CarveOuts = []CarveOut{{ID: "pool", Domain: "pve", Actors: []string{"root@pam"}, // injector only, NOT tg-actuate
		Hosts: []string{"poolhost01"}, ValidFrom: epoch.Add(-time.Hour), ValidUntil: epoch.Add(time.Hour)}}
	f := Attribute("poolhost01", "start-guest", []Evidence{
		ev("pve", "root@pam!tg-actuate", "vzstart", "poolhost01", epoch.Add(-5*time.Minute)),
	}, nil, cfg)
	if f.Taxonomy != AttributedSelf {
		t.Fatalf("TG's own heal on a pool host must stay attributed-self, got %v (rule %q)", f.Taxonomy, f.RuleID)
	}
}
