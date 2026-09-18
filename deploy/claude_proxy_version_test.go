package deploy

// THE SIDECAR'S THREE VERSION SURFACES MOVE TOGETHER.
//
// The proxy states its version in three places, and they had drifted:
//
//   - deploy/claude-proxy/Cargo.toml       — the crate version, the source of truth
//   - deploy/claude-proxy/Cargo.lock       — the same package's pinned entry
//   - deploy/claude-proxy/compose.yml      — the local `image:` alias an operator reads
//
// A fourth, `--version`, needs no pin: main.rs prints env!("CARGO_PKG_VERSION"), so it cannot drift from
// the crate by construction. That is the shape the others should have had.
//
// WHY A GUARD RATHER THAN A HABIT. The compose comment used to say the alias was "NOT A VERSION" and that
// bumping it "does not version anything" — true about what DEPLOYS (the pipeline retags a signed
// <short-sha> image over this name) and false about what it COMMUNICATES. It sat at 1.0.0 while the tree
// had gained TG-279 (unauthenticated /probe-auth and /metrics), TG-287 (the model channel on TLS), TG-288
// (off host networking, cap_drop, no-new-privileges) and the multi-model ModelPolicy — and while the
// downstream agentic-agri-webapp-ng fork had already shipped the contributed AGRIOPS-208 work as
// claudecode-runner 1.1.0. A version an operator reads and cannot trust is worse than no version.

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// cargoVersion extracts the [package] version from a Cargo.toml. It reads the FIRST `version = "..."`
// after the [package] header rather than any version in the file, because dependency entries carry
// version keys too and a naive first-match would silently pin the guard to a dependency.
func cargoVersion(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	body := string(raw)
	i := strings.Index(body, "[package]")
	if i < 0 {
		t.Fatalf("%s has no [package] section — this guard is reading a file that no longer describes the "+
			"crate, and would otherwise compare nothing", path)
	}
	m := regexp.MustCompile(`(?m)^version\s*=\s*"([^"]+)"`).FindStringSubmatch(body[i:])
	if m == nil {
		t.Fatalf("%s declares no [package] version", path)
	}
	return m[1]
}

func TestSidecarVersionSurfacesAgree(t *testing.T) {
	crate := cargoVersion(t, "claude-proxy/Cargo.toml")
	if crate == "" {
		t.Fatal("vacuity floor: the crate version parsed empty, so every comparison below would trivially pass")
	}

	// 1. The compose alias an operator reads.
	compose, err := os.ReadFile("claude-proxy/compose.yml")
	if err != nil {
		t.Fatalf("read sidecar compose: %v", err)
	}
	want := "image: tg-claude-proxy:" + crate
	if !strings.Contains(string(compose), want) {
		got := regexp.MustCompile(`image: tg-claude-proxy:\S+`).FindString(string(compose))
		t.Errorf("compose declares %q but the crate is %s.\n"+
			"The alias does not change what DEPLOYS (the pipeline retags a signed <short-sha> image over "+
			"it), but it is the version an operator reads in this file. Bump them together.", got, crate)
	}

	// 2. The lock file's entry for this same package. A stale lock is how a `cargo build` in a clean
	//    checkout can disagree with the manifest the reviewer read.
	lock, err := os.ReadFile("claude-proxy/Cargo.lock")
	if err != nil {
		t.Fatalf("read sidecar Cargo.lock: %v", err)
	}
	locked := regexp.MustCompile(`name = "claudecode-runner"\nversion = "([^"]+)"`).FindStringSubmatch(string(lock))
	if locked == nil {
		t.Fatal("Cargo.lock has no claudecode-runner entry — the crate was renamed, or the lock is not " +
			"tracking this package; re-derive this guard rather than deleting it")
	}
	if locked[1] != crate {
		t.Errorf("Cargo.lock pins claudecode-runner %s but Cargo.toml declares %s", locked[1], crate)
	}
}

// `--version` is asserted to stay DERIVED rather than hand-written. If someone replaces the env! with a
// literal, the surface starts drifting again and no other test here would notice — the whole point is that
// this one surface is correct by construction.
func TestSidecarVersionFlagStaysDerivedFromTheCrate(t *testing.T) {
	src, err := os.ReadFile("claude-proxy/src/main.rs")
	if err != nil {
		t.Fatalf("read sidecar main.rs: %v", err)
	}
	if !strings.Contains(string(src), `env!("CARGO_PKG_VERSION")`) {
		t.Error("--version no longer prints env!(\"CARGO_PKG_VERSION\"). Printing a literal makes the flag a " +
			"fourth thing to remember to bump, and the one that lies to whoever is debugging a running " +
			"container.")
	}
}

// THE FOURTH SURFACE: THE CI RETAG TARGET (found the hard way).
//
// The three pins above were not enough. deploy-sidecar pulls the signed <short-sha> image and retags it
// with the local alias compose expects — and that target was the literal `tg-claude-proxy:1.0.0` in
// .gitlab-ci.yml. Releasing 1.2.0 moved compose and left the retag behind, so the deploy tagged 1.0.0,
// compose looked for 1.2.0, found it in no registry, tried to PULL a local-only alias, was denied, and
// died — AFTER stopping the TLS terminator. A version bump took the model channel down.
//
// The fix is not a fourth pin. It is to make the retag DERIVE the alias from the same compose file, so the
// two cannot disagree — the shape `--version` already had. This guard asserts that derivation survives,
// because re-hardcoding the tag is the obvious "simplification" and it fails only at deploy time, on the
// estate, after something has already been stopped.
func TestTheSidecarRetagTargetIsDerivedNotHardcoded(t *testing.T) {
	ci, err := os.ReadFile("../.gitlab-ci.yml")
	if err != nil {
		t.Fatalf("read .gitlab-ci.yml: %v", err)
	}
	var retag string
	for _, ln := range strings.Split(string(ci), "\n") {
		code := strings.TrimSpace(ln)
		if strings.HasPrefix(code, "#") {
			continue // prose about the retag is not the retag (TG-326/TG-143 taught this twice)
		}
		if strings.Contains(code, "docker tag ") && strings.Contains(code, "TG_SIDECAR_IMAGE") {
			retag = code
		}
	}
	if retag == "" {
		t.Fatal("no `docker tag ${TG_SIDECAR_IMAGE}…` line found in .gitlab-ci.yml — this guard is reading a " +
			"deploy that no longer retags, and would otherwise pass by checking nothing")
	}
	if regexp.MustCompile(`tg-claude-proxy:[0-9]`).MatchString(retag) {
		t.Errorf("the sidecar retag target is a hardcoded version: %s\n"+
			"It must be read from deploy/claude-proxy/compose.yml. A literal here disagrees with compose the "+
			"moment the crate is released, and the failure lands at DEPLOY time on the estate — after the "+
			"terminator has already been stopped.", retag)
	}
	if !strings.Contains(retag, "SIDECAR_ALIAS") {
		t.Errorf("the retag no longer uses the alias derived from compose.yml: %s", retag)
	}
}
