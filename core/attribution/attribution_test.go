package attribution

import (
	"strings"
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

func hasWarningContaining(ws []string, sub string) bool {
	for _, w := range ws {
		if strings.Contains(w, sub) {
			return true
		}
	}
	return false
}

// TG-453: TG's OWN read-only investigation identity — hostdiag's classify-SSH login into the faulted host
// DURING triage — must NOT read attributed-suspicious. Before the fix it was neither the actuation self-actor
// (withheld on the triage plane), nor sanctioned (the journal domain sanctions no one), nor a carve-out, so it
// fell through to suspicious and SECURITY-ESCALATED TG's own investigation — refusing a legitimately-approved
// heal (the live defect). The fix recognises it from the reader CREDENTIAL (SelfReaders) and drops it from
// candidate-minting, WITHOUT narrowing the suspicious path for a genuine unknown actor.
func TestSelfReaderNotSuspicious(t *testing.T) {
	const reader = "root!SHA256:HOSTDIAGkeyFingerprintAAAA"   // TG's own hostdiag classify-SSH identity
	const intruder = "root!SHA256:ATTACKERkeyFingerprintZZZZ" // an unknown actor's login on the same host
	// The journal domain as this estate runs it: NO sanctioned principals; on the triage plane the actuation
	// self-actor does not resolve either. Self-recognition of a DIAGNOSTIC login rides entirely on SelfReaders.
	cfg := Config{
		SelfActors:  map[string]string{}, // triage plane withholds the actuation key (TG-153)
		SelfReaders: map[string][]string{"journal": {reader}},
		Sanctioned:  map[string][]string{},
		Window:      30 * time.Minute,
		Now:         nowFunc,
	}
	readerLogin := ev("journal", reader, "ssh-login", "librespeed01", epoch.Add(-3*time.Minute))
	intruderLogin := ev("journal", intruder, "ssh-login", "librespeed01", epoch.Add(-4*time.Minute))

	// (1) THE FIX. TG's own diagnostic login ALONE ⇒ Unattributable, never suspicious: a read is not an
	// actor-mutation, so nobody is attributed and the heal is not security-escalated.
	f := Attribute("librespeed01", "start-service", []Evidence{readerLogin}, nil, cfg)
	if f.Taxonomy == AttributedSuspicious {
		t.Fatalf("TG's own reader login must NOT read attributed-suspicious (TG-453); got suspicious with candidates %v", f.Candidates)
	}
	if f.Taxonomy != Unattributable {
		t.Fatalf("TG's own reader login alone ⇒ unattributable (no fault actor), got %v", f.Taxonomy)
	}
	// Transparency: the recognition names the identity + subject on the finding (not a silent drop).
	if !hasWarningContaining(f.Warnings, reader) {
		t.Fatalf("recognising TG's own reader identity must be surfaced in the finding warnings; got %v", f.Warnings)
	}

	// (2) SECURITY PRESERVED (REQ-2304). A genuine unknown actor on the SAME subject still dominates — the
	// recognition excludes only TG's own record, never the finding. The killing check that the fix did not
	// blunt the suspicious path: a co-occurring reader-self must not mask the intruder into a non-escalation.
	f = Attribute("librespeed01", "start-service", []Evidence{readerLogin, intruderLogin}, nil, cfg)
	if f.Taxonomy != AttributedSuspicious {
		t.Fatalf("an unknown actor co-occurring with TG's own reader login must STILL read suspicious (REQ-2304), got %v", f.Taxonomy)
	}

	// (3) VACUITY GUARD. With NO SelfReaders configured, the identical login falls through to suspicious —
	// proving the recognition (not some unrelated clause) is what changes the outcome, and that an
	// unconfigured reader-self is the safe pre-TG-453 behaviour, never a silent relaxation.
	bare := cfg
	bare.SelfReaders = map[string][]string{}
	f = Attribute("librespeed01", "start-service", []Evidence{readerLogin}, nil, bare)
	if f.Taxonomy != AttributedSuspicious {
		t.Fatalf("without SelfReaders the login is indistinguishable from an intruder ⇒ suspicious (the pre-fix behaviour the fix changes), got %v", f.Taxonomy)
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
