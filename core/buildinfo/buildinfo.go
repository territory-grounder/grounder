// Package buildinfo exposes the binary's own VCS revision (TG-600): the deploy image is built from a git
// checkout with no .dockerignore, so Go's default -buildvcs=auto stamps vcs.revision / vcs.modified into
// every worker and grounder binary even under -trimpath -ldflags="-s -w" (verified 2026-10-23 on a local
// build of cmd/worker). The harness render record stores it beside each fingerprint so "which image
// rendered this configuration" is a row, not deploy-history archaeology. Read-only; never a boot knob.
package buildinfo

import (
	"runtime/debug"
	"sync"
)

var (
	once     sync.Once
	revision string
	modified bool
)

func load() {
	once.Do(func() {
		bi, ok := debug.ReadBuildInfo()
		if !ok {
			return
		}
		revision, modified = fromSettings(bi.Settings)
	})
}

// fromSettings is the pure read, kept apart so it can be tested without a stamped binary.
func fromSettings(settings []debug.BuildSetting) (rev string, dirty bool) {
	for _, s := range settings {
		switch s.Key {
		case "vcs.revision":
			rev = s.Value
		case "vcs.modified":
			dirty = s.Value == "true"
		}
	}
	return rev, dirty
}

// Revision is the full VCS revision the binary was built from, or "" when the build carried none
// (a `go run`, a build outside a checkout). Callers persist it verbatim; "" means unrecorded.
func Revision() string {
	load()
	return revision
}

// Modified reports whether the checkout was dirty at build time.
func Modified() bool {
	load()
	return modified
}

// Describe is the operator-facing form: the 12-hex prefix, "+dirty" when the tree was modified, or
// "unknown" when the build carried no revision.
func Describe() string {
	return describe(Revision(), Modified())
}

func describe(rev string, dirty bool) string {
	if rev == "" {
		return "unknown"
	}
	if len(rev) > 12 {
		rev = rev[:12]
	}
	if dirty {
		return rev + "+dirty"
	}
	return rev
}
