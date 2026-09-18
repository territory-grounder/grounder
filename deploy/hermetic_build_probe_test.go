package deploy

import (
	"os"
	"strings"
	"testing"
)

// TG-174 step 1. The exposure, measured 2026-08-06 on dc1gitlabrunner01's
// /etc/gitlab-runner/config.toml:
//
//	executor   = "docker"
//	privileged = true
//	volumes    = ["/cache", "/var/run/docker.sock:/var/run/docker.sock"]
//
// Both settings are GLOBAL, so every job container on these shared runners is privileged and holds the host
// Docker socket — not only .image-build. The runner config is what closes that, and it is not in this
// repository; but dropping the socket there breaks .image-build on the next pipeline, so TG's build has to
// stop NEEDING it first.
//
// .image-build is gated on main, so an MR pipeline cannot exercise a rewrite of it: the first-ever run
// would be on main, where a failure reds main and blocks every deploy. hermetic-build-probe exists to fail
// somewhere it is cheap to fail.

// KILLING MUTATION: delete the hermetic-build-probe job, or give it a BLOCKING `main` rule. RED.
func TestTheHermeticProbeRunsOnMergeRequestsOnly(t *testing.T) {
	ci := stripYAMLCommentLines(ciFile(t))

	block, ok := jobBlock(ci, "hermetic-build-probe")
	if !ok {
		t.Fatal("there is no hermetic-build-probe job. Without one, the only way to learn whether a " +
			"daemon-less build works is to change .image-build and watch main")
	}
	if !strings.Contains(block, `$CI_PIPELINE_SOURCE == "merge_request_event"`) {
		t.Errorf("the probe does not run on merge requests, which is the only place it is useful:\n%s", block)
	}
	// AMENDED 2026-08-06 (TG-174). This used to forbid a main arm outright, for two stated reasons: it
	// "adds a second way to red main" and "proves nothing new". The first is real and is now answered by
	// allow_failure: true, asserted in TestTheProbeCannotRedMain. The second was refuted by measurement —
	// the probe proved nothing at ALL, because it ran ZERO times: its changes-filter needs an MR touching
	// one of four files, and roughly fifty merge requests went past that day without touching any of them.
	//
	// So a main arm is now permitted, and REQUIRED to be non-blocking. The original safety intent is
	// preserved; only the "MR-only" implementation of it changed.
	if strings.Contains(block, `$CI_COMMIT_BRANCH == "main"`) && !strings.Contains(block, "allow_failure: true") {
		t.Errorf("the probe runs on main WITHOUT allow_failure: true. That is a second way to red main, "+
			"for a job whose whole purpose is to gather evidence for a switch that has not happened:\n%s", block)
	}
	// Scoped to the build inputs — a full Go image build on every MR is a tax nobody asked for.
	if !strings.Contains(block, "changes:") {
		t.Errorf("the probe is unscoped and will build on every merge request:\n%s", block)
	}
	for _, want := range []string{"deploy/Dockerfile", "go.mod", "go.sum"} {
		if !strings.Contains(block, want) {
			t.Errorf("the probe does not trigger on %s, so a change to the build inputs would go unproven:\n%s", want, block)
		}
	}
}

// THE TWO PROPERTIES THAT MAKE IT SAFE TO ADD AT ALL: it needs no Docker daemon, and it pushes nothing.
//
// KILLING MUTATION: drop --no-push, or point the job at image: docker:28. RED.
func TestTheProbeNeedsNoDaemonAndPushesNothing(t *testing.T) {
	ci := stripYAMLCommentLines(ciFile(t))
	block, ok := jobBlock(ci, "hermetic-build-probe")
	if !ok {
		t.Fatal("no hermetic-build-probe job")
	}

	if !strings.Contains(block, "kaniko") {
		t.Errorf("the probe does not use a daemon-less builder, so it proves nothing about removing the "+
			"docker socket:\n%s", block)
	}
	if strings.Contains(block, "docker:28") || strings.Contains(block, "docker info") {
		t.Errorf("the probe reaches for the Docker daemon — it would then need the very socket this whole "+
			"ticket is about removing:\n%s", block)
	}
	if !strings.Contains(block, "--no-push") {
		t.Errorf("the probe does not pass --no-push. It runs on merge requests, including from forks; a "+
			"probe that can publish an image is a supply-chain hole, not a test:\n%s", block)
	}
	// And it must carry no registry credentials: nothing to leak, nothing to misuse.
	for _, cred := range []string{"REGISTRY_PASSWORD", "CI_REGISTRY_PASSWORD", "docker login"} {
		if strings.Contains(block, cred) {
			t.Errorf("the probe references %s. A --no-push build needs no credentials, and handing them to "+
				"an MR-triggered job is exactly the exposure this ticket is trying to shrink:\n%s", cred, block)
		}
	}
}

// The real build is UNCHANGED by this step. Converting it is the next commit, after the probe has been
// green across real changes — flipping both at once would put the untested path on main immediately, which
// is the thing being avoided.
//
// KILLING MUTATION: change .image-build to kaniko in this commit. RED.
func TestTheRealImageBuildIsUntouchedForNow(t *testing.T) {
	ci := stripYAMLCommentLines(ciFile(t))
	block, ok := jobBlock(ci, ".image-build")
	if !ok {
		t.Fatal("the .image-build template is gone")
	}
	if !strings.Contains(block, `$CI_COMMIT_BRANCH == "main"`) {
		t.Errorf("the real image build no longer runs on main:\n%s", block)
	}
	if strings.Contains(block, "kaniko") {
		t.Error("the real image build was switched to kaniko in the same change that introduces the probe. " +
			"Its first execution would still be on main — the probe buys nothing if the thing it de-risks " +
			"lands beside it")
	}
}

// ciFile reads .gitlab-ci.yml from the deploy package's parent.
func ciFile(t *testing.T) string {
	t.Helper()
	b, err := readRepoFile("../.gitlab-ci.yml")
	if err != nil {
		t.Fatalf("read .gitlab-ci.yml: %v", err)
	}
	return b
}

// jobBlock returns the YAML block for one top-level job key: from its `name:` line to the next line that
// starts at column 0. Scoped so an assertion about one job cannot be satisfied by another.
func jobBlock(ci, name string) (string, bool) {
	marker := "\n" + name + ":\n"
	i := strings.Index(ci, marker)
	if i < 0 {
		return "", false
	}
	rest := ci[i+1:]
	lines := strings.Split(rest, "\n")
	var out []string
	for n, l := range lines {
		if n > 0 && l != "" && !strings.HasPrefix(l, " ") && !strings.HasPrefix(l, "\t") {
			break
		}
		out = append(out, l)
	}
	return strings.Join(out, "\n"), true
}

// readRepoFile is a thin os.ReadFile wrapper kept here so the helper set above reads as one unit.
func readRepoFile(p string) (string, error) {
	b, err := os.ReadFile(p)
	return string(b), err
}
