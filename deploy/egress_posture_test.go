package deploy

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TG-324. The committed default said `meter` while production ran `enforce`.
//
// Measured on the live worker 2026-08-06:
//
//	TG_EGRESS_MODE=enforce                       (from the box's own .env)
//	boot: "outbound meter installed over http.DefaultTransport in enforce mode; 33 declared
//	       destinations … Off-allowlist connections are COUNTED and NAMED in the log; they are BLOCKED."
//	tg_egress_allowlist_rules            33 (worker) / 10 (worker-actuate)
//	tg_egress_offallowlist_requests_total 0 on both, flat across every sample held
//
// while deploy/docker-compose.yml carried `${TG_EGRESS_MODE:-meter}` in BOTH worker blocks. A clean
// redeploy — one that did not happen to carry the same .env — would have dropped the control back to
// counting, silently, with the boot log dutifully reporting "meter mode" to nobody looking.
//
// A security posture that lives only in an untracked file on one host is a posture that reverts on the
// next deploy.

// PER-SERVICE, NOT FILE-WIDE (rewritten by TG-324). The original counted the substring
// "TG_EGRESS_MODE: ${TG_EGRESS_MODE:-meter}" across the whole compose file and required zero. That was
// adequate while the two workers were the only processes reading the key, but it cannot say WHICH block
// it is talking about — it would have been equally satisfied by the workers dropping to meter and some
// other service being added at enforce. TG-324 wired the meter into the grounder as well, deliberately
// at `meter` while the workers stay at `enforce`, and a file-wide substring count cannot express that
// asymmetry at all: it can only demand uniformity, which is the wrong requirement.
//
// So this now parses the services and asserts a posture per service. Stricter, not looser.
//
// KILLING MUTATION: set either worker block to ${TG_EGRESS_MODE:-meter}. RED, naming the service.
func TestEveryWorkerBlockDefaultsEgressToEnforce(t *testing.T) {
	// The posture each TG-binary service must SHIP with. The grounder's `meter` is a staged position with
	// a stated exit condition, not a permanent exemption — see the compose comment and TG-324.
	want := map[string]string{
		"worker":         "enforce",
		"worker-actuate": "enforce",
		"grounder":       "meter",
	}

	doc := sidecarComposeDoc(t)
	svcs, _ := doc["services"].(map[string]any)
	if len(svcs) == 0 {
		t.Fatal("no services parsed from docker-compose.yml — every assertion below would be vacuous")
	}

	var checked int
	for name, posture := range want {
		svc, ok := svcs[name].(map[string]any)
		if !ok {
			t.Errorf("service %q is not in docker-compose.yml — this guard's subject is gone. If it was "+
				"renamed, rename it here; do not let the check go quiet.", name)
			continue
		}
		env, _ := svc["environment"].(map[string]any)
		raw, present := env["TG_EGRESS_MODE"]
		if !present {
			t.Errorf("service %q declares no TG_EGRESS_MODE. Its binary READS the key, so compose not "+
				"forwarding it means the process silently falls back to its compiled default while .env "+
				"looks configured — the exact gap deploy/envparity_test.go exists to kill.", name)
			continue
		}
		got, _ := raw.(string)
		checked++
		if want := "${TG_EGRESS_MODE:-" + posture + "}"; got != want {
			t.Errorf("service %q defaults TG_EGRESS_MODE to %q, want %q. A posture that lives only in an "+
				"untracked .env on one host is a posture that reverts on the next clean deploy.",
				name, got, want)
		}
	}
	if checked == 0 {
		t.Fatal("no service was actually checked — the loop asserted nothing and passed")
	}

	// The override must survive: an operator must still be able to drop to meter without editing the repo,
	// which is the escape hatch that makes committing the stricter default safe to do at all.
	if strings.Contains(stripYAMLCommentLines(composeFile(t)), "TG_EGRESS_MODE: enforce\n") {
		t.Error("TG_EGRESS_MODE is hardcoded rather than defaulted — an operator can no longer fall back to " +
			"meter without a code change, which is the wrong direction for an emergency")
	}
}

// WHY THE STRICTER DEFAULT IS SAFE, pinned. cmd/worker refuses to enforce against an EMPTY allowlist and
// falls back to meter with a loud log — so a deployment that declares no destinations degrades to counting
// rather than losing the model gateway, the estate and OpenBao at once.
//
// Without that fallback this default would be the classic fail-closed-gate-shipped-before-the-config
// mistake. The guard is what makes it a posture rather than an outage.
//
// IT NOW READS core/egress/install.go, NOT cmd/worker/egress.go (TG-324 moved the decision there so the
// grounder could share it rather than receive an almost-right copy). Repointing the guard is the whole
// job here: left aimed at the old file it would have found no enforce branch and t.Fatal'd, which is the
// loud failure — but a slightly different version of it would have found nothing to match and passed,
// and this repo has shipped exactly that (a guard whose subject moved out from under it).
//
// One refusal, one guard, both binaries — which is strictly more coverage than before, when the grounder
// had no meter to refuse with.
//
// KILLING MUTATION: delete the `allow.Size() == 0` branch in core/egress/install.go. RED.
func TestTheEmptyAllowlistFallbackStillExists(t *testing.T) {
	rel := filepath.Join("..", "core", "egress", "install.go")
	b, err := os.ReadFile(rel)
	if err != nil {
		t.Fatalf("read core/egress/install.go: %v — this guard's subject is the shared install path; if it "+
			"moved again, repoint this test rather than deleting it", err)
	}
	src := stripGoLineComments(string(b))
	// SCOPED to the enforce block. `allow.Size() == 0` appears TWICE in this file — the guard, and a later
	// informational log about an empty allowlist in meter mode. A file-wide check is satisfied by the
	// second one, and that exact mutation (gutting the guard, leaving the log) SURVIVED the first version
	// of this test.
	enforceBlock := src
	if i := strings.Index(src, "string(ModeEnforce))"); i >= 0 {
		enforceBlock = src[i:]
		if j := strings.Index(enforceBlock, "\n\tm := NewMeter"); j > 0 {
			enforceBlock = enforceBlock[:j]
		}
	} else {
		t.Fatal("cannot locate the enforce-mode branch in core/egress/install.go — this guard would otherwise " +
			"assert against the whole file and pass on an unrelated line")
	}
	if !strings.Contains(enforceBlock, "allow.Size() == 0") {
		t.Fatal("egress.Install no longer refuses to enforce against an EMPTY allowlist. With the committed " +
			"default now `enforce`, a deployment whose endpoint scan finds nothing would block EVERY " +
			"outbound call — the model gateway, the estate and OpenBao — and the operator would have asked " +
			"for exactly that, so nothing would look wrong.")
	}
	// It must fall back to METER, not to some other mode and not fatally: a stack with no connectors
	// configured is a legitimate state, and killing the process over it trades one outage for another.
	idx := strings.Index(enforceBlock, "allow.Size() == 0")
	window := enforceBlock[idx:]
	if end := strings.Index(window, "\n\t}\n"); end > 0 {
		window = window[:end]
	}
	if strings.Contains(window, "log.Fatal") {
		t.Errorf("the empty-allowlist path is fatal; it must degrade to meter and say so:\n%s", window)
	}
	if !strings.Contains(window, "meter") {
		t.Errorf("the empty-allowlist path does not say it is staying in meter mode:\n%s", window)
	}
}

// stripGoLineComments drops whole-line // comments so these assertions cannot match the prose that
// explains them — this package's own commentary quotes both the meter default and the guard.
func stripGoLineComments(s string) string {
	var out strings.Builder
	for _, l := range strings.Split(s, "\n") {
		if strings.HasPrefix(strings.TrimSpace(l), "//") {
			continue
		}
		out.WriteString(l)
		out.WriteByte('\n')
	}
	return out.String()
}
