package deploy

// PINS FOR THE SELF-CONTAINMENT DECISION (TG-270).
//
// selfContainedBytes decides whether the served console pulls anything off the network, and both of its
// failure directions have bitten for real:
//   - UNDER-match (mutation BD): the first version never saw CSS url() at all, so a data:-reclassification
//     mutation was unreachable and an external font would have passed as self-contained.
//   - OVER-match (TG-270): the second version scanned url\( over the WHOLE document, so the JS URL
//     constructor — and then a code COMMENT describing it — read as external stylesheets. Two red
//     pipelines in one day, each of which emails the owner.
//
// Every case here is one of those two directions on a synthetic document. KILLING MUTATION: widen the
// url() scan back to the whole document — the two JS cases go red. Narrow it to nothing — the style-block
// cases go red.

import (
	"strings"
	"testing"
)

func TestJavaScriptURLConstructorIsNotAStylesheet(t *testing.T) {
	ok, ext := selfContainedBytes(`<html><script>
		const u = new URL(location.href);
		u.searchParams.set("session", ref);
	</script></html>`)
	if !ok {
		t.Fatalf("the JS URL constructor was read as an external CSS asset %v — the TG-270 false positive, "+
			"which cost two red pipelines on 2026-08-03", ext)
	}
}

func TestACodeCommentDescribingTheConstructIsNotAStylesheet(t *testing.T) {
	ok, ext := selfContainedBytes(`<html><script>
		/* deploy/served_console_test.go scans for url( ... ) case-insensitively, so new URL(location.href)
		   reads to that guard as a stylesheet pulling location.href off the network. */
	</script></html>`)
	if !ok {
		t.Fatalf("PROSE inside a script comment was read as an external asset %v — the second red pipeline "+
			"of the pair: a comment explaining the first false positive triggered the same guard", ext)
	}
}

func TestAnExternalFontInAStyleBlockStillFires(t *testing.T) {
	ok, ext := selfContainedBytes(`<html><style>
		@font-face { font-family: X; src: url(https://cdn.example/font.woff2); }
	</style></html>`)
	if ok {
		t.Fatal("an external font url() inside <style> passed as self-contained — the guard was scoped into " +
			"vacuity; this is the hole the original mutation BD exposed, reopened")
	}
	if len(ext) != 1 || !strings.Contains(ext[0], "cdn.example") {
		t.Fatalf("the finding does not name the external asset: %v", ext)
	}
}

func TestAnInlineStyleAttributeStillFires(t *testing.T) {
	ok, _ := selfContainedBytes(`<html><div style="background: url('https://cdn.example/bg.png')"></div></html>`)
	if ok {
		t.Fatal("an external url() in an inline style attribute passed — style= is a fetch-capable CSS " +
			"context and must stay scanned")
	}
}

func TestInlineDataFontsRemainSelfContained(t *testing.T) {
	ok, ext := selfContainedBytes(`<html><style>
		@font-face { font-family: X; src: url(data:font/woff2;base64,AAAA); }
	</style></html>`)
	if !ok {
		t.Fatalf("a data: URI was classified external %v — mutation BD's exemption broke: the console's "+
			"whole design is fonts inlined as data URIs precisely so it needs no assets directory", ext)
	}
}

func TestScriptSrcAndLinkHrefFireAnywhereInTheDocument(t *testing.T) {
	if ok, _ := selfContainedBytes(`<html><script src="https://cdn.example/app.js"></script></html>`); ok {
		t.Fatal("an external <script src> passed — scoping url() to CSS must not have narrowed the tag scan")
	}
	if ok, _ := selfContainedBytes(`<html><link rel="stylesheet" href="/assets/index.css"></html>`); ok {
		t.Fatal("an external <link href> passed — the tag scan lost the stylesheet alternative")
	}
}
