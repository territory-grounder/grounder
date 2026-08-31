package deploy_test

// WHAT THE REPOSITORY IS ALLOWED TO CARRY (TG-257).
//
// On 2026-07-30 ADR-0015 deleted the unreachable React `frontend/`. The commit that did it — subject "one
// console — delete the unreachable React frontend/", 4b0e9763 in !765 — ALSO deleted the .gitignore lines
// that had been hiding that build's output. So the deletion removed the 52 SOURCE files and left the
// build behind: frontend/ went from 52 tracked files to 6,706, of which 6,647 were node_modules, totalling
// 98.5 MB. It sat on main for four days. Every clone and every CI checkout carried it.
//
// The .gitignore rules are back, and they are not the protection. They were there before and a commit
// removed them; a rule that a commit can delete cannot be the thing standing between this repo and 98 MB
// of dependency tree. THIS is the protection: the index itself is checked, so re-adding the tree fails the
// build whatever .gitignore happens to say at the time.
//
// Scope note: this governs the tracked tree at HEAD. It does not and cannot shrink history — those blobs
// are still reachable from earlier commits, so `git clone` still transfers them. Removing them for real
// means rewriting published history, which is a destructive, coordinated act for the repository owner to
// decide, not a side effect of a test. TG-257 records that as the remaining half.

import (
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// repoRoot is where git must be run from.
//
// `go test` runs with the CWD set to the PACKAGE directory, so a bare `git ls-files` here lists deploy/
// and nothing else — 222 of the repository's files. The first draft of this gate did exactly that and
// reported a 3 MiB tree while 98.5 MB of node_modules sat one level up, passing green. Every git command
// below is therefore anchored with -C.
func repoRoot(t *testing.T) string {
	t.Helper()
	out, err := exec.Command("git", "rev-parse", "--show-toplevel").Output()
	if err != nil {
		t.Skipf("not a git checkout (%v) — this gate needs the index", err)
	}
	return strings.TrimSpace(string(out))
}

// trackedFiles returns every path in the git index, or skips when git is unavailable (a source tarball).
func trackedFiles(t *testing.T) []string {
	t.Helper()
	out, err := exec.Command("git", "-C", repoRoot(t), "ls-files").Output()
	if err != nil {
		t.Skipf("git ls-files unavailable (%v) — this gate needs the index, not the filesystem, because "+
			"an ignored-but-present directory is exactly the state it must not flag", err)
	}
	var files []string
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if line != "" {
			files = append(files, line)
		}
	}
	if len(files) == 0 {
		t.Fatal("the index is empty — this gate would pass vacuously")
	}
	return files
}

// KILLING MUTATION: `git add -f frontend/node_modules`, or re-add any dependency tree. RED.
func TestNoDependencyTreeIsTracked(t *testing.T) {
	var offenders []string
	for _, f := range trackedFiles(t) {
		for _, part := range strings.Split(f, "/") {
			if part == "node_modules" || part == "vendor" && strings.HasPrefix(f, "frontend/") {
				offenders = append(offenders, f)
				break
			}
		}
	}
	if len(offenders) > 0 {
		t.Fatalf("%d tracked file(s) inside a dependency tree — every clone and CI checkout carries these, "+
			"and .gitignore alone has already failed to prevent it once:\n  %s%s",
			len(offenders), strings.Join(offenders[:min(len(offenders), 10)], "\n  "),
			map[bool]string{true: "\n  …", false: ""}[len(offenders) > 10])
	}
}

// The React frontend is deleted by accepted ADR and preserved in the tag `archive/frontend-react`. Its
// build output must not creep back in the way it did last time.
//
// KILLING MUTATION: re-add frontend/dist. RED.
func TestTheDeletedFrontendStaysDeleted(t *testing.T) {
	var live []string
	for _, f := range trackedFiles(t) {
		if strings.HasPrefix(f, "frontend/") {
			live = append(live, f)
		}
	}
	if len(live) > 0 {
		t.Fatalf("%d tracked file(s) under frontend/, which ADR-0015 deleted on 2026-07-30. The source is "+
			"preserved in the tag archive/frontend-react; nothing here should be a build artefact:\n  %s",
			len(live), strings.Join(live[:min(len(live), 10)], "\n  "))
	}
}

// A single enormous blob is the other shape this defect takes — one vendored archive rather than 6,647
// small files. The cap is deliberately generous: it exists to catch a category error, not to police
// legitimately large assets, which belong on the allowlist with a reason.
//
// KILLING MUTATION: commit a multi-megabyte binary. RED.
func TestNoOversizedBlobIsTracked(t *testing.T) {
	const capBytes = 4 << 20 // 4 MiB

	// Named, reasoned exemptions. An entry here is a claim that the file is genuinely source.
	allowed := map[string]string{}

	root := repoRoot(t)
	out, err := exec.Command("git", "-C", root, "ls-files", "-s").Output()
	if err != nil {
		t.Skipf("git ls-files -s unavailable: %v", err)
	}
	type blob struct{ path, sha string }
	var blobs []blob
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		// mode SP sha SP stage TAB path
		tab := strings.IndexByte(line, '\t')
		if tab < 0 {
			continue
		}
		fields := strings.Fields(line[:tab])
		if len(fields) < 2 {
			continue
		}
		blobs = append(blobs, blob{path: line[tab+1:], sha: fields[1]})
	}
	if len(blobs) == 0 {
		t.Fatal("no blobs parsed — this gate would pass vacuously")
	}

	var in strings.Builder
	for _, b := range blobs {
		in.WriteString(b.sha)
		in.WriteByte('\n')
	}
	cmd := exec.Command("git", "-C", root, "cat-file", "--batch-check=%(objectsize)")
	cmd.Stdin = strings.NewReader(in.String())
	sizes, err := cmd.Output()
	if err != nil {
		t.Skipf("git cat-file unavailable: %v", err)
	}
	lines := strings.Fields(string(sizes))
	if len(lines) != len(blobs) {
		t.Fatalf("size lookup returned %d rows for %d blobs — refusing to guess which is which",
			len(lines), len(blobs))
	}

	var over []string
	var total int64
	for i, b := range blobs {
		n, err := strconv.ParseInt(lines[i], 10, 64)
		if err != nil {
			continue
		}
		total += n
		if n > capBytes {
			if _, ok := allowed[b.path]; ok {
				continue
			}
			over = append(over, b.path+" ("+strconv.FormatInt(n/(1<<20), 10)+" MiB)")
		}
	}
	t.Logf("tracked tree: %d files, %.1f MiB", len(blobs), float64(total)/(1<<20))
	if len(over) > 0 {
		t.Fatalf("%d tracked file(s) over %d MiB with no stated reason — add an entry to `allowed` "+
			"explaining why it is source, or stop tracking it:\n  %s",
			len(over), capBytes>>20, strings.Join(over, "\n  "))
	}
}

// A .gitignore that does not cover dependency trees is how this happened. The rules are not the primary
// protection — the index checks above are — but their absence is a signal worth failing on, because it
// means the next `git add -A` reintroduces the problem silently before any of those gates run.
//
// KILLING MUTATION: delete the node_modules line from .gitignore. RED.
func TestGitignoreCoversDependencyTrees(t *testing.T) {
	out, err := exec.Command("git", "-C", repoRoot(t), "check-ignore", "-q",
		filepath.Join("frontend", "node_modules", "x")).CombinedOutput()
	if err != nil && len(out) > 0 {
		t.Skipf("git check-ignore unavailable: %s", out)
	}
	if err != nil {
		t.Fatal("frontend/node_modules is NOT ignored — a `git add -A` would track it again, which is " +
			"exactly how 6,647 files and 98.5 MB reached main under TG-257. The rule must be unanchored: " +
			"the /dist/ rule above it matches only the repository root.")
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
