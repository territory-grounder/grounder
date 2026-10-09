package deploy

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// THE SERVED-SURFACE GUARD.
//
// A console image can build, ship and TEST a bundle that no operator can ever reach, and nothing about it
// looks wrong: CI is green, the image builds, the container runs, the site loads. The only thing that is
// false is the accounting.
//
// MEASURED LIVE 2026-07-29 on dc1tg01, before this guard existed:
//   - the served /usr/share/nginx/html/index.html referenced the React bundle 0 times
//   - nginx had served index-*.js / index-*.css 0 times across every request it had logged
//   - /assets/ (973 KB: one JS bundle, one stylesheet, 56 font files) had been requested 0 times
//   - the served console inlines its own fonts as base64, so it needs none of them
//   - and ALL 8 spec/010 tasks — the UX Console spec — own ONLY frontend/ files, every one "completed"
//
// So the component's delivered% counted a React application, its tested% counted 8 scenarios certifying it,
// and an operator opening the console saw a different program entirely. That is the whole gap between
// "Operator surfaces 55 delivered" and "22 operational", and no test could see it because every test was
// pointed at the unreachable half.
//
// The rule this file enforces is deliberately narrow and decidable from the repo alone:
//
//	IF the served console entry point is SELF-CONTAINED — it pulls in no external script, stylesheet or
//	font — THEN the image must not copy any other web asset into the served root, because nothing can
//	reference it.
//
// It is not a style rule. A self-contained entry point plus a copied bundle is a provable contradiction:
// the bundle is unreachable by construction, whatever anyone intended.
const (
	consoleDockerfile = "console/Dockerfile"
	servedRoot        = "/usr/share/nginx/html"
)

// repoPath resolves a Dockerfile COPY source to a path this test can read. Those sources are relative to the
// BUILD CONTEXT, which is the repo root, while `go test` runs in deploy/ — reading them verbatim silently
// finds nothing, and a guard that cannot open the artifact it grades proves nothing at all.
func repoPath(src string) string { return filepath.Join("..", src) }

// copyLine matches a Dockerfile COPY, capturing any --from stage, the sources, and the destination.
var copyLine = regexp.MustCompile(`(?m)^\s*COPY\s+(?:(--from=\S+)\s+)?(.+?)\s+(\S+)\s*$`)

type dockerCopy struct {
	from string // "--from=build" or ""
	src  string
	dst  string
}

func consoleCopies(t *testing.T) []dockerCopy {
	t.Helper()
	b, err := os.ReadFile(consoleDockerfile)
	if err != nil {
		t.Fatalf("read %s: %v", consoleDockerfile, err)
	}
	var out []dockerCopy
	for _, m := range copyLine.FindAllStringSubmatch(string(b), -1) {
		out = append(out, dockerCopy{from: m[1], src: strings.TrimSpace(m[2]), dst: strings.TrimSpace(m[3])})
	}
	if len(out) == 0 {
		t.Fatalf("no COPY instructions parsed out of %s — the guard would pass vacuously", consoleDockerfile)
	}
	return out
}

// servedEntryPoint returns the repo path of the file that ends up at <servedRoot>/index.html. The LAST such
// COPY wins, exactly as Docker layers it — reading the first would have concluded, wrongly, that the React
// build was the served console.
func servedEntryPoint(t *testing.T) string {
	t.Helper()
	entry := ""
	for _, c := range consoleCopies(t) {
		if c.from != "" {
			continue // a build stage cannot be resolved to a repo path
		}
		if c.dst == servedRoot+"/index.html" {
			entry = c.src
		}
	}
	if entry == "" {
		t.Fatalf("no COPY in %s writes %s/index.html — this guard cannot tell what an operator is served, "+
			"and that is exactly the state it exists to prevent", consoleDockerfile, servedRoot)
	}
	return entry
}

// tagRef finds document-level references: <script src> and <link href>. These are meaningful anywhere in
// the file, so they are scanned over the whole entry point.
var tagRef = regexp.MustCompile(`(?i)(?:<script[^>]+src\s*=\s*["']([^"']+)["']|<link[^>]+href\s*=\s*["']([^"']+)["'])`)

// cssURL finds CSS url() asset references. It is scanned ONLY over CSS contexts — <style> blocks and inline
// style="" attributes — never over the whole document.
//
// ★ CSS url() IS IN HERE BECAUSE LEAVING IT OUT MADE A MUTATION CONTROL VACUOUS. The first version matched
// only <script src> and <link href>. Mutation BD — reclassify a data: URI as external — then changed nothing
// observable, because the served console's inline fonts live in `@font-face { src: url(data:...) }` and the
// regex never saw them. A control that mutates unreachable code is not a passing control, it is no control.
//
// ★ AND IT IS SCOPED TO CSS BECAUSE SCANNING THE WHOLE DOCUMENT MADE THE GUARD FIRE ON JAVASCRIPT (TG-270).
// The console is one file: its scripts are inline, and to a case-insensitive, unanchored url\( pattern, the
// JS URL constructor — `new URL(location.href)` — is indistinguishable from a stylesheet fetching
// `location.href` off the network. Measured 2026-08-03: it cost two red pipelines in one day, the second
// from a CODE COMMENT that merely described the first failure. A guard for "what does the browser fetch"
// must look where the browser fetches from: fetch-capable url() tokens live in CSS, and only CSS is scanned
// for them.
var cssURL = regexp.MustCompile(`(?i)url\(\s*["']?([^"')]+)["']?\s*\)`)

// cssContexts extracts the CSS of the document: every <style>…</style> body and every style="…" attribute
// value. Anything outside these cannot cause a CSS fetch.
var (
	styleBlock = regexp.MustCompile(`(?is)<style[^>]*>(.*?)</style>`)
	styleAttr  = regexp.MustCompile(`(?i)\sstyle\s*=\s*("([^"]*)"|'([^']*)')`)
)

func cssContexts(doc string) []string {
	var out []string
	for _, m := range styleBlock.FindAllStringSubmatch(doc, -1) {
		out = append(out, m[1])
	}
	for _, m := range styleAttr.FindAllStringSubmatch(doc, -1) {
		if m[2] != "" {
			out = append(out, m[2])
		} else {
			out = append(out, m[3])
		}
	}
	return out
}

func selfContained(t *testing.T, entry string) (bool, []string) {
	t.Helper()
	b, err := os.ReadFile(repoPath(entry))
	if err != nil {
		t.Fatalf("read served entry point %s: %v", entry, err)
	}
	return selfContainedBytes(string(b))
}

// selfContainedBytes is the decision, split from the file read so its edge cases are testable against
// synthetic documents — the two false-positive classes above are pinned by tests, not by memory.
func selfContainedBytes(doc string) (bool, []string) {
	var ext []string
	add := func(ref string) {
		ref = strings.TrimSpace(ref)
		if ref == "" || strings.HasPrefix(ref, "data:") || strings.HasPrefix(ref, "#") {
			return
		}
		// TRUNCATE. A reference can be a multi-hundred-kilobyte inline payload, and a failure that prints it
		// whole is a failure nobody reads — the first run of this oracle emitted 174 KB of base64 and buried
		// its own finding.
		if len(ref) > 60 {
			ref = ref[:60] + "…"
		}
		ext = append(ext, ref)
	}
	for _, m := range tagRef.FindAllStringSubmatch(doc, -1) {
		for _, g := range m[1:] {
			if g != "" {
				add(g)
				break
			}
		}
	}
	for _, css := range cssContexts(doc) {
		for _, m := range cssURL.FindAllStringSubmatch(css, -1) {
			add(m[1])
		}
	}
	return len(ext) == 0, ext
}

// THE GUARD. A self-contained served entry point plus any other web asset copied into the served root is a
// provable contradiction, and the contradiction is always resolved the same way in production: the asset is
// dead weight in every image and every pipeline, and any test pointed at it certifies nothing an operator
// can reach.
func TestTheImageShipsNoWebAssetTheServedConsoleCannotReach(t *testing.T) {
	entry := servedEntryPoint(t)
	_, ext := selfContained(t, entry)

	var extras []dockerCopy
	for _, c := range consoleCopies(t) {
		if !strings.HasPrefix(c.dst, servedRoot) {
			continue // nginx.conf and friends land outside the served document root
		}
		if c.dst == servedRoot+"/index.html" {
			continue // the entry point itself
		}
		extras = append(extras, c)
	}

	// ★ BOTH DIRECTIONS, AND NO SILENT EXIT. An earlier version returned early when the served page was not
	// self-contained, on the reasoning that a copied bundle might legitimately serve it. Mutation BD — count
	// a `data:` URI as an external reference — then left this whole file GREEN, because the guard exempted
	// itself before asserting anything. A control that can disarm itself by reclassifying its own input is
	// the shape this repo keeps finding: present, reachable, and unable to fail.
	switch {
	case len(ext) == 0 && len(extras) > 0:
		for _, c := range extras {
			t.Errorf("%s copies %q (from %q) into the served root %q, but the served entry point %s is "+
				"SELF-CONTAINED — it references no external script, stylesheet or font. Nothing an operator "+
				"loads can reach that payload.\n"+
				"Measured live before this guard existed: the React bundle and 973 KB of fonts had been "+
				"requested ZERO times, while all 8 spec/010 tasks certified them.\n"+
				"Either make the served console reference it, or stop shipping it.",
				consoleDockerfile, c.src, c.from, c.dst, entry)
		}
	case len(ext) > 0 && len(extras) == 0:
		// The converse defect, and it is worse for an operator: the page asks the browser for files the
		// image never ships, so the console renders half-built with 404s in the network log.
		t.Errorf("the served entry point %s references %d external asset(s) %v, but %s copies NOTHING else "+
			"into %s — an operator is served a page whose scripts and styles 404",
			entry, len(ext), ext, consoleDockerfile, servedRoot)
	case len(ext) > 0 && len(extras) > 0:
		// Both present. Whether they MATCH cannot be decided here: the assets come from a build stage whose
		// output filenames are content-hashed at build time. Said out loud rather than passed over — a guard
		// that quietly accepts a case it cannot check is how the original defect survived.
		t.Logf("served entry %s references %v and %s ships %d asset copy(ies); matching them needs a built "+
			"image and is NOT checked here", entry, ext, consoleDockerfile, len(extras))
	}
}

// ★ PINS THE PREMISE THE GUARD ABOVE BRANCHES ON.
//
// The guard behaves differently depending on whether the served console is self-contained. That premise is
// true today and is a deliberate property of the v2 console — it inlines its fonts as `data:` URIs precisely
// so it needs no asset directory. If it ever stops being true, the guard changes meaning, and the change
// should be a decision someone makes rather than something that happens quietly. So the premise is asserted
// on its own: this failing is not a bug report, it is a prompt to re-decide what the guard should enforce.
func TestTheServedConsoleIsSelfContainedToday(t *testing.T) {
	entry := servedEntryPoint(t)
	selfC, ext := selfContained(t, entry)
	if !selfC {
		t.Errorf("the served console %s now pulls in %d external reference(s) %v. It was self-contained when "+
			"REQ-617 was written, and TestTheImageShipsNoWebAssetTheServedConsoleCannotReach branches on that "+
			"premise. Re-decide what the guard enforces rather than letting it silently take the other branch.",
			entry, len(ext), ext)
	}
}

// The served console must be the one the repo can actually inspect — a build-stage artifact would leave every
// oracle in this repo unable to see what an operator is given.
func TestTheServedConsoleIsAFileThisRepoCanRead(t *testing.T) {
	entry := servedEntryPoint(t)
	if strings.Contains(entry, "*") {
		t.Fatalf("served entry point %q is a glob — no oracle can pin what is served", entry)
	}
	st, err := os.Stat(repoPath(entry))
	if err != nil {
		t.Fatalf("the served console entry point %q does not exist in the repo: %v", entry, err)
	}
	if st.Size() == 0 {
		t.Fatalf("the served console entry point %q is EMPTY — an operator would be served a blank page", entry)
	}
	// It must be the assembled artifact, not a module fragment: assemble.py composes console.html + modules/*
	// into it, and serving a fragment would render an operator a partial console with no error.
	if filepath.Base(entry) != "index.html" {
		t.Errorf("served entry point %q is not an index.html", entry)
	}
}

// ★ THE ACCOUNTING HALF, AND THE REASON THIS FILE IS NOT JUST A DOCKERFILE LINT.
//
// The defect was never only that dead bytes shipped — it is that the spec CERTIFYING the operator console
// pointed exclusively at the unserved half, so the component's tested% described a program no one runs. This
// asserts spec/010 states, in prose an operator or auditor can find, which artifact is actually served. If
// the Dockerfile is ever re-pointed at the React build, this fails until the spec says so too.
func TestSpecTenNamesTheArtifactThatIsActuallyServed(t *testing.T) {
	entry := servedEntryPoint(t)
	b, err := os.ReadFile("../spec/010-ux-console/requirements.md")
	if err != nil {
		t.Fatalf("read spec/010 requirements: %v", err)
	}
	if !strings.Contains(string(b), entry) {
		t.Errorf("spec/010 never names %q, which is the file an operator is actually served.\n"+
			"All 8 of its tasks own only frontend/ paths, and the image overwrites that build's entry point, "+
			"so the spec certifies a surface no operator can reach. A spec that documents the unserved half "+
			"of a component reports its own coverage as higher than it is.", entry)
	}
}
