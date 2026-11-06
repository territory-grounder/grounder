package deploy

// THE MIRROR'S LAST GATE MUST READ EVERYTHING IT PUBLISHES.
//
// github-sync/sync-to-github.sh ends with an abort-on-survivor pass: after replacements.txt has
// rewritten identifiers, every pattern in github-sync/denylist.txt is searched for, and a single hit
// refuses the push. That design is sound. Its reach was not.
//
// The search named nine extensions — .go .md .sh .yml .yaml .json .mod .txt .html. The tree tracks
// 2,117 files; that list reaches 1,839. The other 278 were published unread: every .rs, .sql, .py,
// .mjs, .feature, .conf, .hcl and .pub in the repo.
//
// The sidecar is Rust. So when deploy/claude-proxy/src/oauth_rotate.rs carried a full-length
// `sk-ant-oat01-` OAuth token as a test fixture — its own comment calling it the operator's real
// paste — no denylist entry could have stopped it. The file was never opened. Adding a pattern for
// that shape while leaving the reach alone would have produced a guard that reads as present and
// matches nothing, which is the failure mode this repository exists to prevent.
//
// The selection is now inverted: read what `git ls-files` publishes, subtract only binaries (which
// cannot carry a reviewable secret) and vendored trees (which are not ours to police). A file type
// added tomorrow is covered tomorrow, not whenever someone remembers to extend a list.
//
// These guards exist because re-narrowing the scan is the obvious "make it faster" edit, and it fails
// silently — the pipeline stays green, the gate keeps logging "all denylist patterns clean", and the
// next secret in a .rs or .sql file goes to the public mirror unread.

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

func mirrorSyncScript(t *testing.T) string {
	t.Helper()
	raw, err := os.ReadFile("../github-sync/sync-to-github.sh")
	if err != nil {
		t.Fatalf("read sync-to-github.sh: %v", err)
	}
	return string(raw)
}

// codeLines strips comment and blank lines. Prose describing the scan is not the scan — TG-326 and
// TG-143 were both guards satisfied by a comment that merely mentioned the token they searched for.
func codeLines(body string) []string {
	var out []string
	for _, ln := range strings.Split(body, "\n") {
		trimmed := strings.TrimSpace(ln)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		out = append(out, trimmed)
	}
	return out
}

func TestTheMirrorScanIsNotScopedToAnExtensionAllowlist(t *testing.T) {
	lines := codeLines(mirrorSyncScript(t))
	if len(lines) == 0 {
		t.Fatal("vacuity floor: sync-to-github.sh parsed to zero code lines, so every check below " +
			"would trivially pass")
	}

	var sawDenylistLoop bool
	for _, ln := range lines {
		if strings.Contains(ln, "DENYLIST[@]") {
			sawDenylistLoop = true
		}
		// --include=<glob> is how the scan was restricted. Any reappearance re-creates the blind spot.
		if strings.Contains(ln, "--include=") {
			t.Errorf("the mirror scan is restricted by extension again: %s\n"+
				"An --include allowlist is what let a full-length OAuth token publish from a .rs file: "+
				"the scan reached 1,839 of 2,117 tracked files and never opened the sidecar crate. "+
				"Select with `git ls-files` and subtract binaries/vendor instead.", ln)
		}
	}
	if !sawDenylistLoop {
		t.Fatal("no loop over ${DENYLIST[@]} found in sync-to-github.sh — this guard is reading a " +
			"sync script that no longer runs the abort-on-survivor pass, and would otherwise pass by " +
			"checking nothing at all")
	}
}

func TestTheMirrorScanSelectsFromTrackedFiles(t *testing.T) {
	lines := codeLines(mirrorSyncScript(t))
	var found bool
	for _, ln := range lines {
		if strings.Contains(ln, "git ls-files") {
			found = true
		}
	}
	if !found {
		t.Error("the mirror scan no longer enumerates tracked files with `git ls-files`. Whatever " +
			"replaced it must still cover every published file — the point of the change is that " +
			"coverage follows what is published rather than a hand-maintained list.")
	}
}

// The denylist must carry SHAPE patterns for provider credentials, not the one value that leaked. A
// value-based entry can only ever catch the token that already went public.
func TestTheDenylistCoversProviderCredentialShapes(t *testing.T) {
	raw, err := os.ReadFile("../github-sync/denylist.txt")
	if err != nil {
		t.Fatalf("read denylist: %v", err)
	}
	var patterns []string
	for _, ln := range strings.Split(string(raw), "\n") {
		trimmed := strings.TrimSpace(ln)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		patterns = append(patterns, trimmed)
	}
	if len(patterns) == 0 {
		t.Fatal("vacuity floor: the denylist parsed to zero patterns, so the mirror would publish blind")
	}

	// A token of the shape that leaked. Assembled here for the same reason the fixture it guards is
	// assembled: this file publishes to the same mirror.
	sample := "sk-ant-oat01-" + strings.Repeat("AbCdEf0123456789_-", 6)[:95]
	var covered bool
	for _, p := range patterns {
		re, err := regexp.Compile(p)
		if err != nil {
			continue // shell ERE that Go's syntax rejects; the sync script is the authority on those
		}
		if re.MatchString(sample) {
			covered = true
		}
	}
	if !covered {
		t.Errorf("no denylist pattern matches a full-length `sk-ant-oat01-` token.\n"+
			"One of these published from deploy/claude-proxy/src/oauth_rotate.rs. Cover the SHAPE "+
			"(prefix + long charset run), never the specific value. Patterns present: %d", len(patterns))
	}
}

// END-TO-END: the shapes must not merely be listed, they must come back clean against the real tree,
// and go red when a token is reintroduced. A pattern that is present but never runs is the same
// defect in a different place.
func TestTheDenylistShapesAreCleanAgainstTheRealTreeAndCatchAReintroducedToken(t *testing.T) {
	root, err := filepath.Abs("..")
	if err != nil {
		t.Fatalf("resolve repo root: %v", err)
	}
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git unavailable")
	}

	scan := func(pattern string) string {
		t.Helper()
		// Same selection the sync script uses: tracked files, minus binaries and vendored trees.
		cmd := exec.Command("bash", "-c",
			`git ls-files -z `+
				`| grep -zvE '\.(png|jpg|jpeg|gif|ico|pdf|zip|gz|tgz|woff2?|ttf|eot|bin|so|a|o|wasm)$' `+
				`| grep -zvE '(^|/)(vendor|node_modules|third_party)/' `+
				`| xargs -0 grep -nHE `+shellQuote(pattern)+` 2>/dev/null | head -3`)
		cmd.Dir = root
		out, _ := cmd.Output() // grep exits non-zero on no-match; the output is the signal
		return strings.TrimSpace(string(out))
	}

	const oatShape = `sk-ant-oat[0-9]+-[A-Za-z0-9_-]{40}`
	const apiShape = `sk-ant-api[0-9]+-[A-Za-z0-9_-]{40}`

	for _, shape := range []string{oatShape, apiShape} {
		if hit := scan(shape); hit != "" {
			t.Errorf("a token-shaped literal is present in the tree and would abort the mirror:\n%s\n"+
				"Assemble the fixture at runtime (prefix + generated tail) so prefix, length and "+
				"charset stay under test while no matching literal exists in the published source.", hit)
		}
	}

	// Killing mutation: write a token-shaped literal into a tracked-file path and confirm the scan
	// finds it. Without this, the two clean results above are indistinguishable from a scan that
	// silently matched nothing.
	canary := filepath.Join(root, "deploy", "claude-proxy", "src", "mirror_scan_canary.rs")
	body := "// generated by TestTheDenylistShapes…; removed before the test returns\n" +
		"const LEAK: &str = \"sk-ant-oat01-" + strings.Repeat("AbCdEf0123456789_-", 6)[:95] + "\";\n"
	if err := os.WriteFile(canary, []byte(body), 0o644); err != nil {
		t.Fatalf("write canary: %v", err)
	}
	defer os.Remove(canary)

	// git ls-files only reports tracked paths, so the canary must be staged to be visible — exactly
	// the state a real leak would be in when the mirror runs.
	add := exec.Command("git", "add", "--intent-to-add", canary)
	add.Dir = root
	if err := add.Run(); err != nil {
		t.Skipf("cannot stage canary (read-only or detached checkout): %v", err)
	}
	defer func() {
		rm := exec.Command("git", "rm", "--cached", "--quiet", canary)
		rm.Dir = root
		_ = rm.Run()
	}()

	if hit := scan(oatShape); hit == "" {
		t.Error("KILLING MUTATION SURVIVED: a full-length sk-ant-oat01 token was planted in a tracked " +
			".rs file and the denylist scan did not find it. The gate is blind — this is precisely the " +
			"state the mirror was in when the real token published.")
	}
}

// shellQuote single-quotes a pattern for `bash -c`. The shapes are ASCII regexes with no quotes of their
// own; this exists so the command is unambiguous rather than to sanitise untrusted input.
func shellQuote(pattern string) string { return "'" + pattern + "'" }
